package chunk

import (
	"bytes"
	"context"
	"testing"
)

// FuzzChunker asserts the invariants that everything downstream relies on:
// chunks are contiguous, they cover the input exactly, and they stay within
// the configured bounds.
func FuzzChunker(f *testing.F) {
	f.Add([]byte(""), uint32(1024), uint32(256), uint32(4096))
	f.Add(bytes.Repeat([]byte("ab"), 5000), uint32(512), uint32(128), uint32(2048))
	f.Add(deterministicData(70000, 1), uint32(4096), uint32(1024), uint32(16384))

	f.Fuzz(func(t *testing.T, data []byte, avg, minSize, maxSize uint32) {
		opt := Options{AvgSize: avg, MinSize: minSize, MaxSize: maxSize}.Normalized()
		if opt.Validate() != nil {
			t.Skip()
		}
		if opt.MaxSize > 1<<20 {
			t.Skip() // keep fuzz allocations small
		}

		ck := NewChunker(bytes.NewReader(data), opt)
		var total uint64
		for {
			piece, off, err := ck.Next()
			if err != nil {
				break
			}
			if off != total {
				t.Fatalf("offset %d, expected %d", off, total)
			}
			if len(piece) == 0 {
				t.Fatal("zero-length chunk")
			}
			if uint32(len(piece)) > opt.MaxSize {
				t.Fatalf("chunk of %d bytes exceeds max %d", len(piece), opt.MaxSize)
			}
			if !bytes.Equal(piece, data[total:total+uint64(len(piece))]) {
				t.Fatal("chunk contents do not match the input")
			}
			total += uint64(len(piece))
		}
		if total != uint64(len(data)) {
			t.Fatalf("chunks cover %d bytes, input is %d", total, len(data))
		}

		sig, err := Hash(context.Background(), bytes.NewReader(data), opt, Config{Jobs: 2})
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}
		if sig.Size != int64(len(data)) {
			t.Fatalf("Hash size %d, input %d", sig.Size, len(data))
		}
	})
}
