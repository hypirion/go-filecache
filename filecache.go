package filecache

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
	Get(key string, localPath string) error
	Put(localPath string, key string) error
}
