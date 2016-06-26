package filecache

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
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

func TestSerialGetting(t *testing.T) {
	fc, _ := NewFilecache(100*Byte, newKeyCounter())
	for i := 0; i < 1000; i++ {
		key := strconv.Itoa(i)
		getKeyCounter(t, fc, key)
	}
	// Ensure we give the  flush out entries
	time.Sleep(100 * time.Millisecond)
}

func TestConcurrentGetting(t *testing.T) {
	fc, _ := NewFilecache(100*Byte, newKeyCounter())

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
	// Ensure we give the  flush out entries
	time.Sleep(100 * time.Millisecond)
}

func TestCached(t *testing.T) {
	// Tests that lru actually happened
	kc := newKeyCounter()
	fc, _ := NewFilecache(50*Byte, kc)
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
