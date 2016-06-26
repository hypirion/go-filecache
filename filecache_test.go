// Copyright 2016 Jean Niklas L'orange.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package filecache

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

func printLink(fc *Filecache) {
	fc.lock.Lock()
	defer fc.lock.Unlock()
	fmt.Print(fc.root.key)
	for node := fc.root.next; node != fc.root; node = node.next {
		fmt.Printf(" -> %s", node.key)
	}
	fmt.Println()
}

type keyCounter struct {
	sync.Mutex
	vals map[string]int
}

func newKeyCounter() *keyCounter {
	return &keyCounter{vals: make(map[string]int)}
}

func (_ *keyCounter) Has(_ string) (bool, error) {
	return true, nil
}

func (kc *keyCounter) Get(dst io.Writer, key string) error {
	kc.Lock()
	kc.vals[key] += 1
	kc.Unlock()
	_, err := io.WriteString(dst, key)
	return err
}

func (kc *keyCounter) Put(key string, src io.Reader) error {
	return os.ErrExist
}

var errUnstable = errors.New("unstability  hit in")

type unstableSyncMap struct {
	sync.RWMutex
	failureRate float32
	vals        map[string][]byte
}

func newUnstableSyncMap(failureRate float32) *unstableSyncMap {
	return &unstableSyncMap{
		failureRate: failureRate,
		vals:        make(map[string][]byte),
	}
}

func getKeyCounter(t *testing.T, fs FileStorage, key string) {
	var b bytes.Buffer
	err := fs.Get(&b, key)
	if err != nil {
		t.Fatalf("Error retrieving key %q", key)
	}
	if b.String() != key {
		t.Errorf("Expected to get back %q, got %q", key, b.String())
	}
}

func (m *unstableSyncMap) Has(key string) (bool, error) {
	if rand.Float32() < m.failureRate {
		return false, errUnstable
	}
	m.RLock()
	_, ok := m.vals[key]
	m.RUnlock()
	return ok, nil
}

func (m *unstableSyncMap) Put(key string, src io.Reader) error {
	if rand.Float32() < m.failureRate {
		return errUnstable
	}
	var b bytes.Buffer
	_, err := io.Copy(&b, src)
	if err != nil {
		return err
	}
	m.Lock()
	_, ok := m.vals[key]
	if !ok {
		m.vals[key] = b.Bytes()
	}
	m.Unlock()
	if ok {
		return os.ErrExist
	}
	return nil
}

func (m *unstableSyncMap) Get(dst io.Writer, key string) error {
	if rand.Float32() < m.failureRate {
		return errUnstable
	}
	m.RLock()
	buf, ok := m.vals[key]
	m.RUnlock()
	if !ok {
		return os.ErrNotExist
	}
	_, err := io.Copy(dst, bytes.NewBuffer(buf))
	return err
}

func TestSerialGetting(t *testing.T) {
	fc, _ := New(100*Byte, newKeyCounter())
	defer fc.Close()
	for i := 0; i < 1000; i++ {
		key := strconv.Itoa(i)
		getKeyCounter(t, fc, key)
	}

}

func TestConcurrentGetting(t *testing.T) {
	fc, _ := New(100*Byte, newKeyCounter())
	defer fc.Close()

	rand.Seed(time.Now().UnixNano())
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				key := strconv.Itoa(rand.Intn(1000))
				getKeyCounter(t, fc, key)
			}
		}()
	}

	wg.Wait()
}

func TestSerialUnstable(t *testing.T) {
	fc, _ := New(100*Byte, newUnstableSyncMap(0.5))
	defer fc.Close()

	rand.Seed(time.Now().UnixNano())

	for i := 0; i < 100; i++ {
		key := strconv.Itoa(i)
		for {
			hasIt, err := fc.Has(key)
			if err == nil {
				if hasIt {
					t.Fatalf("Unexpected: Should not have key %d yet", i)
				}
				break
			}
			if err != errUnstable {
				t.Fatalf("Unexpected error looking up key %d: %s", i, err)
			}
		}
		for {
			err := fc.Put(key, bytes.NewBufferString(key))
			if err == nil {
				break
			}
			if err != errUnstable {
				t.Fatalf("Unexpected error when putting key %d: %s", i, err)
			}
		}
		for {
			hasIt, err := fc.Has(key)
			if err == nil {
				if !hasIt {
					t.Fatalf("Unexpected: Should have key %d", i)
				}
				break
			}
			if err != errUnstable {
				t.Fatalf("Unexpected error looking up key %d: %s", i, err)
			}
		}
		for {
			var b bytes.Buffer
			err := fc.Get(&b, key)
			if err == nil {
				if key != b.String() {
					t.Fatalf("Expected value for entry %d to be %q, was %q", i, key,
						b.String())
				}
				break
			}
			if err != errUnstable {
				t.Fatalf("Unexpected error when putting getting %d: %s", i, err)
			}
		}
	}
}

func TestConcurrentUnstableAbruptClose(t *testing.T) {
	fc, _ := New(100*Byte, newUnstableSyncMap(0.5))

	rand.Seed(time.Now().UnixNano())

	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var err error
				key := strconv.Itoa(rand.Intn(1000))
				switch rand.Intn(3) {
				case 0: // Put
					err := fc.Put(key, bytes.NewBufferString(key))
					switch {
					case err == ErrCacheClosed:
						return
					case err == nil || err == errUnstable || err == os.ErrExist:
						continue
					default:
						t.Fatalf("Unexpected error when putting key %d: %s", i, err)
					}
				case 1: // Get
					var b bytes.Buffer
					err = fc.Get(&b, key)
					switch {
					case err == ErrCacheClosed:
						return
					case err == nil || err == errUnstable || err == os.ErrNotExist:
						continue
					default:
						t.Fatalf("Unexpected error when getting key %d: %s", i, err)
					}
				case 2: // Has
					_, err = fc.Has(key)
					if err == ErrCacheClosed {
						return
					}
					if err == nil {
						continue
					}
					if err != errUnstable {
						t.Fatalf("Unexpected error when putting key %d: %s", i, err)
					}
				}
			}
		}()
	}
	// Let them play around for a while
	time.Sleep(100 * time.Millisecond)
	fc.Close()
	wg.Wait()
}

func TestCached(t *testing.T) {
	// Tests that lru actually happens
	kc := newKeyCounter()
	fc, _ := New(50*Byte, kc)
	defer fc.Close()
	for i := 0; i < 100; i++ {
		for j := 0; j < 3; j++ {
			getKeyCounter(t, fc, "foo")
		}
		key := strconv.Itoa(i)
		getKeyCounter(t, fc, key)
	}
	if kc.vals["foo"] != 1 {
		t.Errorf(`Expected "foo" to be looked up only once, was looked up %d times`,
			kc.vals["foo"])
	}
}

func TestAbruptClose(t *testing.T) {
	fc, _ := New(100*Byte, newKeyCounter())

	rand.Seed(time.Now().UnixNano())
	var wgDone sync.WaitGroup
	var wgStarted sync.WaitGroup

	for i := 0; i < 100; i++ {
		wgStarted.Add(1)
		wgDone.Add(1)
		go func() {
			defer wgDone.Done()
			started := false
			for {
				key := strconv.Itoa(rand.Intn(1000))
				var b bytes.Buffer
				err := fc.Get(&b, key)
				if !started {
					started = true
					wgStarted.Done()
				}
				if err == ErrCacheClosed {
					return // as expected
				}
				if err != nil {
					t.Fatalf("Error retrieving key %q", key)
				}
				if b.String() != key {
					t.Errorf("Expected to get back %q, got %q", key, b.String())
				}
			}
		}()
	}

	wgStarted.Wait()
	fc.Close()
	wgDone.Wait()
}
