package filecache

import (
	"io"
	"io/ioutil"
	"os"
	"sync"
)

// FileStorage is an interface for putting and retrieving immutable files.
//
// Has returns true or false, depending on whether a file with the associated
// key exists. If this function returns true, subsequent non-failing requests
// should also return true.
//
// Get retrieves a file and stores it in the file at localPath. If a file with
// the associated key does not exist in the file store, then io.ErrNotExist
// should be returned.
//
// Put sends a file to the file store and stores it there. If a file associated
// with the key already exist in the file storage, then io.ErrExist should be
// thrown.
type FileStorage interface {
	Has(key string) (bool, error)
	Get(dst io.Writer, key string) error
	Put(key, src io.Reader) error
}

type Size int64

const (
	Byte Size = 1
	KB        = 1000 * Byte
	KiB       = 1024 * Byte
	MB        = 1000 * KB
	MiB       = 1024 * KiB
	GB        = 1000 * MB
	GiB       = 1024 * MiB
	TB        = 1000 * GB
	TiB       = 1024 * GiB
)

func NewFilecache(maxSize Size) *Filecache {
	return nil
}

type Filecache struct {
	lock       sync.RWMutex
	subStorage FileStorage
	inTransit  map[string]*cacheEntry
	cache      map[string]*cacheEntry
	tmpFolder  string
	root       *cacheEntry
	size       int64
	maxSize    int64
}

type cacheEntry struct {
	sync.RWMutex
	key        string
	removed    bool
	path       string
	fileSize   int64
	next, prev *cacheEntry
}

func (fc *Filecache) Has(key string) (res bool, err error) {
	// Cannot check the ones in transit. Although that would be preferable, we may
	// have a race condition where we attempt to download a file, then call Has,
	// then the download finds out the file does not exist and bails out.
	fc.lock.RLock()
	_, res = fc.cache[key]
	fc.lock.RUnlock()
	if res {
		return
	}
	res, err = fc.subStorage.Has(key)
	return
}

// getEntry retrieves a cache entry. It can only be used when the caller has a
// lock on it.
func (fc *Filecache) getEntry(key string) *cacheEntry {
	entry := fc.cache[key]
	if entry == nil {
		// maybe it's in transit? In that case, wait for the existing transfer to
		// complete to avoid potential network saturation.
		entry = fc.inTransit[key]
	}
	return entry
}

func (fc *Filecache) Get(dst io.Writer, key string) (err error) {
	// Check in cache first, to avoid the tiny overhead of making a cache entry
	// (this is probably slightly premature)
	fc.lock.RLock()
	existingEntry := fc.getEntry(key)
	fc.lock.RUnlock()
	if existingEntry != nil {
		var notDeleted bool
		notDeleted, err = existingEntry.copyTo(dst)
		if notDeleted {
			fc.hit(existingEntry)
			return
		}
	}

	// Create a new temporary entry and prepare to put it in transit:
	goodEntry := false
	newEntry := &cacheEntry{
		key: key,
	}
	newEntry.Lock() // lock it immediately to avoid premature reads.
	defer func() {
		if !goodEntry {
			newEntry.removed = true
		}
		newEntry.Unlock()
	}()

	fc.lock.Lock()
	// Race condition: There may now be an entry in either the cache or in
	// transit, so let's check those before we add the new entry.
	existingEntry = fc.getEntry(key)
	if existingEntry == nil {
		fc.inTransit[key] = newEntry
	}
	fc.lock.Unlock()
	if existingEntry != nil {
		var notDeleted bool
		notDeleted, err = existingEntry.copyTo(dst)
		if notDeleted {
			fc.hit(existingEntry)
			return
		}
	}

	// Not in cache and we just added an in-transit entry, so we should probably
	// download the thing.

	// Create temp file to store contents in.
	var entryFile *os.File
	entryFile, err = ioutil.TempFile(fc.tmpFolder, key)
	if err != nil {
		// TODO: wrap this one
		return err
	}
	newEntry.path = entryFile.Name()
	defer func() {
		if !goodEntry {
			entryFile.Close()
			os.Remove(entryFile.Name())
			// Should we do anything with this error? Presumably it's okay to
			// leave it be, as the temp file will be empty and in the temp dir, so
			// it should be cleaned up after some time.
		}
	}()

	// now we can actually try to retrieve the file
	err = fc.subStorage.Get(entryFile, key)
	if err != nil {
		return err
	}
	// Horray, we did it! Now find the size of the file.
	var stat os.FileInfo
	stat, err = entryFile.Stat()
	if err != nil {
		// TODO: Wrap
		return err
	}
	fileSize := stat.Size()
	newEntry.fileSize = fileSize

	// LATER: Should we perhaps check the file size here? If it exceeds the
	// total cache size, we should probably bail and complain.

	err = entryFile.Close()
	if err != nil {
		// TODO: wrap
		return err
	}

	// Then the last part: Insert it into the cache
	fc.lock.Lock()
	fc.size += fileSize
	delete(fc.inTransit, key)
	fc.insertEntry(newEntry)
	fc.lock.Unlock()

	// At this point we have a sound entry in the cache. We can go on with our
	// lives and copy it over to the local file.
	goodEntry = true

	// Oh yeah, now we can do what we wanted actually was supposed to do: Move
	// this into the file we were given.

	// We still have the write rights on the entry, so we can safely read from
	// the file and push it into the actual file we wanted to push data into.
	// Actually this is what we'd prefer, because if we tried to read it from
	// the entry, then we may, in theory, end up with a case where it has
	// already been evicted from the cache and all the download work was for
	// nothing. However, it also means we have a slight penalty for multiple
	// requests on the exact same time that appear close to simultaneously and
	// are not in the cache..

	err = copyFile(dst, newEntry.path)
	return err
}

func (fc *Filecache) hit(entry *cacheEntry) {
	// update this entry's location in the file cache.

	// Ughhh: We need a lock on
}

func (fc *Filecache) insertEntry(entry *cacheEntry) {
	// TODO: Is this sound? Presumably: Users will only pull out an entry via
	// getEntry, and whenever a value is put in transit, we explicitly check that
	// there are no other entries. Only values put in transit (and removed) will
	// be given here, so it is sound.
	fc.cache[entry.key] = entry

	// Put into LRU.

	// Then remove excessive entries.
	//fc.removeLRU()
}

func (ce *cacheEntry) copyTo(dst io.Writer) (bool, error) {
	ce.RLock()
	defer ce.RUnlock()
	if ce.removed {
		// while we waited for the lock, the entry may have been removed. In that
		// case, bail out instead.
		return false, nil
	}

	err := copyFile(dst, ce.path)
	return true, err
}

func copyFile(dst io.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		// TODO: Wrap?
		return err
	}
	defer f.Close() // Ugh, I'm not sure how to handle this particular error. If
	// io.copy doesn't

	_, err = io.Copy(dst, f)
	// TODO: Wrap?
	return err
}
