package chunk

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/zeebo/blake3"
)

// MaxJobs caps the hashing pool. Past this point the bottleneck is always the
// storage device, not the CPU.
const MaxJobs = 64

// Sink receives every chunk payload as it is hashed. Implementations must be
// safe for concurrent use, and must not retain data past the call.
type Sink interface {
	Chunk(c Chunk, data []byte) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(c Chunk, data []byte) error

// Chunk calls f.
func (f SinkFunc) Chunk(c Chunk, data []byte) error { return f(c, data) }

// Config tunes a single Hash call.
type Config struct {
	// Jobs is the number of parallel BLAKE3 workers. Zero means NumCPU.
	Jobs int
	// Sink, if set, observes each chunk payload.
	Sink Sink
	// Progress, if set, is called with the byte count of each hashed chunk.
	// It must be safe for concurrent use.
	Progress func(n int64)
	// SkipFileDigest omits the whole-file digest, saving one pass of BLAKE3
	// over the data.
	SkipFileDigest bool
}

func (c Config) jobs() int {
	n := c.Jobs
	if n <= 0 {
		n = runtime.NumCPU()
	}
	if n > MaxJobs {
		n = MaxJobs
	}
	return n
}

// block is one chunk payload in flight. It carries a reference count because
// the payload is consumed by both the whole-file hasher and a chunk worker,
// and the backing buffer only returns to the pool once both are finished.
type block struct {
	buf  *[]byte
	data []byte
	idx  int
	off  uint64
	refs atomic.Int32
	pool *sync.Pool
}

func (b *block) release() {
	if b.refs.Add(-1) == 0 {
		b.pool.Put(b.buf)
	}
}

// Hash chunks r and returns its signature. Chunk boundaries are found
// sequentially (they must be, they depend on the byte stream) while the
// per-chunk BLAKE3 digests and the whole-file digest are computed in parallel
// with the scan.
func Hash(ctx context.Context, r io.Reader, opt Options, cfg Config) (Signature, error) {
	opt = opt.Normalized()
	if err := opt.Validate(); err != nil {
		return Signature{}, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := cfg.jobs()
	bufSize := int(opt.MaxSize)
	pool := &sync.Pool{New: func() any {
		b := make([]byte, bufSize)
		return &b
	}}

	work := make(chan *block, jobs*2)
	results := make(chan Chunk, jobs*2)
	var fileCh chan *block
	if !cfg.SkipFileDigest {
		fileCh = make(chan *block, jobs*2)
	}

	var (
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	// Whole-file digest: a single goroutine consuming blocks in order.
	var fileDigest Digest
	fileDone := make(chan struct{})
	if fileCh != nil {
		go func() {
			defer close(fileDone)
			h := blake3.New()
			for b := range fileCh {
				_, _ = h.Write(b.data)
				b.release()
			}
			copy(fileDigest[:], h.Sum(nil))
		}()
	} else {
		close(fileDone)
	}

	// Chunk digest workers.
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				c := Chunk{
					Index:  b.idx,
					Offset: b.off,
					Length: uint32(len(b.data)),
					Digest: Sum(b.data),
				}
				if cfg.Sink != nil {
					if err := cfg.Sink.Chunk(c, b.data); err != nil {
						fail(err)
						b.release()
						continue
					}
				}
				if cfg.Progress != nil {
					cfg.Progress(int64(len(b.data)))
				}
				b.release()
				select {
				case results <- c:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Collector.
	var chunks []Chunk
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for c := range results {
			chunks = append(chunks, c)
		}
	}()

	// Scanner.
	refs := int32(1)
	if fileCh != nil {
		refs = 2
	}
	ck := NewChunker(r, opt)
	var total int64
	for idx := 0; ; idx++ {
		if err := ctx.Err(); err != nil {
			fail(err)
			break
		}
		view, off, err := ck.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail(err)
			break
		}
		bp := pool.Get().(*[]byte)
		data := (*bp)[:len(view)]
		copy(data, view)
		b := &block{buf: bp, data: data, idx: idx, off: off, pool: pool}
		b.refs.Store(refs)
		total += int64(len(view))

		if fileCh != nil {
			select {
			case fileCh <- b:
			case <-ctx.Done():
				fail(ctx.Err())
			}
		}
		if ctx.Err() != nil {
			break
		}
		select {
		case work <- b:
		case <-ctx.Done():
			fail(ctx.Err())
		}
		if ctx.Err() != nil {
			break
		}
	}

	close(work)
	if fileCh != nil {
		close(fileCh)
	}
	wg.Wait()
	close(results)
	<-collected
	<-fileDone

	if firstErr != nil {
		return Signature{}, firstErr
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Index < chunks[j].Index })
	return Signature{Size: total, Digest: fileDigest, Options: opt, Chunks: chunks}, nil
}

// HashPath chunks the file at path.
func HashPath(ctx context.Context, path string, opt Options, cfg Config) (Signature, error) {
	f, err := os.Open(path)
	if err != nil {
		return Signature{}, err
	}
	defer f.Close()
	return Hash(ctx, f, opt, cfg)
}
