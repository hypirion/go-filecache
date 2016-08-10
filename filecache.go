// Copyright 2016 Jean Niklas L'orange.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filecache

import (
	"errors"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrCacheClosed = errors.New("filecache is closed")

// FileStorage is an interface for putting and retrieving immutable files.
//
// Has returns true or false, depending on whether a file with the associated
// key exists. If this function returns true, subsequent non-failing requests
// should also return true.
//
// Get retrieves a file and stores it in the file at localPath. If a file with
// the associated key does not exist in the file store, then an error
// should be returned.
//
// Put sends a file to the file store and stores it there. If a file associated
// with the key already exist in the file storage, then an error should be
// returned.
type FileStorage interface {
	Has(key string) (bool, error)
	Get(dst io.Writer, key string) error
	Put(key string, src io.Reader) error
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

func New(maxSize Size, storage FileStorage) (*Filecache, error) {
	tmpDir, err := ioutil.TempDir("", "filecache-")
	if err != nil {
		return nil, err
	}
	return &Filecache{
		inTransit:  make(map[string]*cacheEntry),
		cache:      make(map[string]*cacheEntry),
		tmpFolder:  tmpDir,
		maxSize:    int64(maxSize),
		subStorage: storage,
	}, nil
}

func (fc *Filecache) Close() {
	entries := []*cacheEntry{}
	fc.lock.Lock()
	fc.closed = true
	fc.maxSize = 0
	// find all elements
	for _, entry := range fc.inTransit {
		entries = append(entries, entry)
	}
	if fc.root != nil {
		entries = append(entries, fc.root)
		for node := fc.root.next; node != fc.root; node = node.next {
			entries = append(entries, node)
		}
	}
	fc.deleters.Add(1)
	go fc.deleteEntries(entries)
	fc.lock.Unlock()
	fc.deleters.Wait()
	os.RemoveAll(fc.tmpFolder)
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
	closed     bool
	deleters   sync.WaitGroup
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
	closed := fc.closed
	if !closed {
		_, res = fc.cache[key]
	}
	fc.lock.RUnlock()
	if closed {
		return false, ErrCacheClosed
	}
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
	closed := fc.closed
	var existingEntry *cacheEntry
	if !closed {
		existingEntry = fc.getEntry(key)
	}
	fc.lock.RUnlock()
	if closed {
		return ErrCacheClosed
	}
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
	closed = fc.closed
	// Race condition: There may now be an entry in either the cache or in
	// transit, so let's check those before we add the new entry.
	if !closed {
		existingEntry = fc.getEntry(key)
		if existingEntry == nil {
			fc.inTransit[key] = newEntry
		}
	}
	fc.lock.Unlock()
	if closed {
		return ErrCacheClosed
	}
	if existingEntry != nil {
		var notDeleted bool
		notDeleted, err = existingEntry.copyTo(dst)
		if notDeleted {
			fc.hit(existingEntry)
			return
		}
	}
	defer func() {
		if !goodEntry {
			fc.lock.Lock()
			delete(fc.inTransit, key)
			fc.lock.Unlock()
		}
	}()

	// Not in cache and we just added an in-transit entry, so we should probably
	// retrieve the thing.

	// Create temp file to store contents in.
	var entryFile *os.File
	entryFile, err = ioutil.TempFile(fc.tmpFolder, filifyKey(key))
	if err != nil {
		// TODO: wrap this one
		return err
	}
	newEntry.path = entryFile.Name()
	defer func() {
		if !goodEntry {
			entryFile.Close()
			os.Remove(newEntry.path)
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

	fc.insertEntry(newEntry)

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

// hit places the cache entry at the top of the lru-cache if it is still in the
// cache.
func (fc *Filecache) hit(entry *cacheEntry) {
	fc.lock.Lock()
	closed := fc.closed
	// The entry may have already been evicted by others, so check if we're still
	// in the cache. Since we nil the links, we can just check those.

	// There's another edge case: We are already the root. In that case, we don't
	// have to do anything either.
	if !closed && entry.prev != nil && fc.root != entry {
		// This is similar to insertEntry, except that we also have to remove our
		// original position first. This is done by connecting prev and next to
		// eachother.
		oldPrev := entry.prev
		oldNext := entry.next
		oldPrev.next = oldNext
		oldNext.prev = oldPrev

		// then just do as in insertEntry. We don't have to check root here because
		// we have the links, which guarantees a root.
		entry.next = fc.root
		entry.prev = fc.root.prev
		entry.prev.next = entry
		entry.next.prev = entry
		fc.root = entry
	}
	fc.lock.Unlock()
}

func (fc *Filecache) insertEntry(entry *cacheEntry) {
	// TODO(?): Is this sound? Presumably: Users will only pull out an entry via
	// getEntry, and whenever a value is put in transit, we explicitly check that
	// there are no other entries. Only values put in transit (and removed) will
	// be given here, so it is sound.
	fc.lock.Lock()
	closed := fc.closed
	// If the cache was closed while we were in transit, then we're already
	// scheduled for deletion, so we don't have to worry about deleting ourselves.
	if !closed {
		// No defer here, because this should NOT fail.
		fc.size += entry.fileSize
		delete(fc.inTransit, entry.key)
		fc.cache[entry.key] = entry

		// Put into LRU.
		if fc.root == nil {
			fc.root = entry
			entry.prev = entry
			entry.next = entry
		} else {
			// Explanation:
			//              root
			//               |
			//               v
			// +------+   +------+   +------+
			// |r.prev|<->|   r  |<->|r.next|
			// +------+   +------+   +------+
			//
			// Add replace entry with r, by bumping it to the right
			//
			//              root
			//               |
			//               v
			// +------+   +------+   +------+    +------+
			// |r.prev|<->| entry|<->|   r  |<-> |r.next|
			// +------+   +------+   +------+    +------+
			//
			// (Obviously, r.prev is not r.prev anymore, but rather entry.prev)

			entry.next = fc.root
			entry.prev = fc.root.prev
			entry.prev.next = entry
			entry.next.prev = entry
			fc.root = entry
		}

		// Then remove excessive entries.
		fc.removeLRU()
	}
	fc.lock.Unlock()
}

func (fc *Filecache) removeLRU() {
	toDelete := []*cacheEntry{}
	last := fc.root.prev
	for fc.maxSize < fc.size {
		fc.size -= last.fileSize

		lastPrev := last.prev
		toDelete = append(toDelete, last)
		// remove last from cache by removing prev and next on it, and connecting
		// lastPrev to root.
		delete(fc.cache, last.key)

		last.prev = nil
		last.next = nil
		// If last == lastPrev then we only have a single entry. Which means that
		// the file itself is larger than the cache, which is silly for usage, but
		// oh well.
		if last == lastPrev {
			fc.root = nil
		} else {
			lastPrev.next = fc.root
			fc.root.prev = lastPrev
			last = lastPrev
		}

	}
	fc.deleters.Add(1)
	go fc.deleteEntries(toDelete)
}

// deletesEntries deletes cache entries synchronously. This must not be done on
// entries that are inside the cache, only the ones that are explicitly evicted.
func (fc *Filecache) deleteEntries(entries []*cacheEntry) {
	defer fc.deleters.Done()
	for _, entry := range entries {
		deleteEntry(entry)
	}
}

// deleteEntry deletes a single cache entry from disk. It must not be done on
// entries inside the cache, just the ones explicitly evicted from it.
func deleteEntry(entry *cacheEntry) {
	entry.Lock()
	defer entry.Unlock()
	if !entry.removed {
		entry.removed = true
		os.Remove(entry.path)
		// TODO?: can this panic? If not, we may avoid the defer entry and we may as
		// well inline it into deleteEntries
	}
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
	// io.copy doesn't error out, then this shouldn't be an issue unless we end up
	// being unable to read from the file again in the future.

	_, err = io.Copy(dst, f)
	// TODO: Wrap?
	return err
}

func (fc *Filecache) Put(key string, src io.Reader) (err error) {
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
	closed := fc.closed
	var existingEntry *cacheEntry
	if !closed {
		existingEntry = fc.getEntry(key)
		if existingEntry == nil {
			fc.inTransit[key] = newEntry
		}
	}
	fc.lock.Unlock()
	if closed {
		return ErrCacheClosed
	}
	if existingEntry != nil {
		return os.ErrExist
	}
	defer func() {
		if !goodEntry {
			fc.lock.Lock()
			delete(fc.inTransit, key)
			fc.lock.Unlock()
		}
	}()

	// Create temp file to store contents in.
	var entryFile *os.File
	entryFile, err = ioutil.TempFile(fc.tmpFolder, filifyKey(key))
	if err != nil {
		// TODO: wrap this one
		return err
	}
	newEntry.path = entryFile.Name()
	defer func() {
		if !goodEntry {
			entryFile.Close()
			os.Remove(newEntry.path)
			// Should we do anything with this error? Presumably it's okay to
			// leave it be, as the temp file will be empty and in the temp dir, so
			// it should be cleaned up after some time.
		}
	}()

	var fileSize int64
	fileSize, err = io.Copy(entryFile, src)
	if err != nil {
		return err
	}
	newEntry.fileSize = fileSize

	err = entryFile.Close()
	if err != nil {
		return err
	}

	// Reopen and call the inner put
	entryFile, err = os.Open(newEntry.path)
	if err != nil {
		return err
	}
	err = fc.subStorage.Put(key, entryFile)
	if err != nil {
		return err
	}

	err = entryFile.Close()
	if err != nil {
		return err
	}

	// alright, entry is good
	fc.insertEntry(newEntry)

	goodEntry = true
	return nil
}

func filifyKey(key string) string {
	return strings.Replace(key, string(filepath.Separator), "_", -1) + "-"
}
