package filecache

import (
	"io"
	"os"
)

// NoopStore is a file storage that does not send or receive any data from a
// remote file storage. While it technically doesn't satisfy the constraints a
// backing store should satisfy, it will make the filecache implementation a
// transient immutable cache.
//
//    fcache := filecache.New(30 * filecache.GiB, filecache.NoopStore{})
//    defer fcache.Close()
type NoopStore struct{}

// Has from a NoopStore will always return false, nil.
func (NoopStore) Has(key string) (bool, error) {
	return false, nil
}

// Get will return os.ErrNotExist.
func (NoopStore) Get(_ io.Writer, key string) error {
	return os.ErrNotExist
}

// Put does nothing and returns nil.
func (NoopStore) Put(key string, _ io.Reader) error {
	return nil
}
