package receiver

import (
	"fmt"
	"os"
	"sync"

	"github.com/shaneburrell/modelmove/internal/chunk"
)

// location points at a byte range that is known to hold a given chunk.
type location struct {
	path   string
	offset int64
	length uint32
}

// index maps chunk digests to somewhere on the destination disk that already
// holds those bytes. This is what turns a transfer into a sparse update: any
// chunk found here never crosses the network.
type index struct {
	mu sync.RWMutex
	m  map[chunk.Digest]location
}

func newIndex() *index { return &index{m: make(map[chunk.Digest]location)} }

func (i *index) add(d chunk.Digest, loc location) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.m[d]; !ok {
		i.m[d] = loc
	}
}

func (i *index) get(d chunk.Digest) (location, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	loc, ok := i.m[d]
	return loc, ok
}

func (i *index) has(d chunk.Digest) bool {
	_, ok := i.get(d)
	return ok
}

// dropPath removes every entry backed by a file that is about to be replaced.
func (i *index) dropPath(path string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	for d, loc := range i.m {
		if loc.path == path {
			delete(i.m, d)
		}
	}
}

// readers caches open file handles used to copy reusable chunks.
type readers struct {
	mu sync.Mutex
	m  map[string]*os.File
}

func newReaders() *readers { return &readers{m: make(map[string]*os.File)} }

func (r *readers) get(path string) (*os.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.m[path]; ok {
		return f, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r.m[path] = f
	return f, nil
}

// forget closes and drops a handle, which must happen before the file is
// replaced by a rename.
func (r *readers) forget(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.m[path]; ok {
		f.Close()
		delete(r.m, path)
	}
}

func (r *readers) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p, f := range r.m {
		f.Close()
		delete(r.m, p)
	}
}

// fetch reads and verifies the bytes described by loc. A digest mismatch means
// the file changed under us, so the caller must fall back to the network.
func (r *readers) fetch(loc location, want chunk.Digest, buf []byte) ([]byte, error) {
	f, err := r.get(loc.path)
	if err != nil {
		return nil, err
	}
	if cap(buf) < int(loc.length) {
		buf = make([]byte, loc.length)
	}
	buf = buf[:loc.length]
	if _, err := f.ReadAt(buf, loc.offset); err != nil {
		return nil, err
	}
	if got := chunk.Sum(buf); got != want {
		return nil, fmt.Errorf("local chunk at %s+%d no longer matches %s", loc.path, loc.offset, want.Short())
	}
	return buf, nil
}
