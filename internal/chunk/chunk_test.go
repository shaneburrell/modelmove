package chunk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// deterministicData returns pseudo-random bytes that are stable across runs so
// that chunk boundaries are reproducible in assertions.
func deterministicData(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestDigestText(t *testing.T) {
	d := Sum([]byte("hello"))
	if d.IsZero() {
		t.Fatal("digest of non-empty input is zero")
	}
	if len(d.String()) != 64 {
		t.Fatalf("String() = %d chars, want 64", len(d.String()))
	}
	if len(d.Short()) != 12 {
		t.Fatalf("Short() = %q, want 12 chars", d.Short())
	}

	parsed, err := ParseDigest(d.String())
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}
	if parsed != d {
		t.Fatal("round trip changed the digest")
	}

	blob, err := json.Marshal(map[string]Digest{"d": d})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back map[string]Digest
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back["d"] != d {
		t.Fatal("JSON round trip changed the digest")
	}
}

func TestParseDigestErrors(t *testing.T) {
	for _, s := range []string{"", "abc", strings.Repeat("z", 64), strings.Repeat("ab", 40)} {
		if _, err := ParseDigest(s); err == nil {
			t.Errorf("ParseDigest(%q) accepted an invalid digest", s)
		}
	}
	var d Digest
	if err := d.UnmarshalText([]byte("xx")); !errors.Is(err, ErrShortDigest) {
		t.Errorf("short digest error = %v, want ErrShortDigest", err)
	}
}

func TestOptionsNormalizeAndValidate(t *testing.T) {
	var zero Options
	if !zero.IsZero() {
		t.Fatal("zero Options should report IsZero")
	}
	n := zero.Normalized()
	if n.AvgSize == 0 || n.MinSize == 0 || n.MaxSize == 0 {
		t.Fatalf("Normalized left a zero field: %+v", n)
	}
	if err := n.Validate(); err != nil {
		t.Fatalf("normalized defaults are invalid: %v", err)
	}

	// Only the average given: the others are derived around it.
	derived := Options{AvgSize: 1 << 20}.Normalized()
	if derived.MinSize != 1<<18 || derived.MaxSize != 1<<22 {
		t.Fatalf("derived sizes = %+v", derived)
	}

	bad := []Options{
		{AvgSize: 1024, MinSize: 8, MaxSize: 4096},               // min too small
		{AvgSize: 1024, MinSize: 2048, MaxSize: 4096},            // min above avg
		{AvgSize: 8192, MinSize: 1024, MaxSize: 4096},            // avg above max
		{AvgSize: 1 << 20, MinSize: 1 << 18, MaxSize: 1<<30 + 1}, // max too large
	}
	for _, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("Validate accepted %+v", o)
		}
	}
	if s := n.String(); !strings.Contains(s, "avg=") {
		t.Errorf("String() = %q", s)
	}
}

func TestOptionsForSize(t *testing.T) {
	cases := []struct {
		size int64
		want Options
	}{
		{1024, SmallFileOptions()},
		{LargeFileThreshold - 1, SmallFileOptions()},
		{LargeFileThreshold, LargeFileOptions()},
		{HugeFileThreshold, HugeFileOptions()},
		{40 << 30, HugeFileOptions()},
	}
	for _, c := range cases {
		if got := OptionsForSize(c.size); got != c.want {
			t.Errorf("OptionsForSize(%d) = %+v, want %+v", c.size, got, c.want)
		}
	}
	if DefaultOptions() != SmallFileOptions() {
		t.Error("DefaultOptions should be the small-file preset")
	}
}

func TestChunkerRespectsBounds(t *testing.T) {
	opt := Options{AvgSize: 4096, MinSize: 1024, MaxSize: 16384}
	data := deterministicData(1<<20, 1)
	ck := NewChunker(bytes.NewReader(data), opt)

	var total int
	var count int
	for {
		piece, off, err := ck.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if uint64(total) != off {
			t.Fatalf("chunk %d reported offset %d, expected %d", count, off, total)
		}
		if len(piece) > int(opt.MaxSize) {
			t.Fatalf("chunk %d is %d bytes, over the %d max", count, len(piece), opt.MaxSize)
		}
		// Every chunk but the last must reach the minimum size.
		if total+len(piece) < len(data) && len(piece) < int(opt.MinSize) {
			t.Fatalf("chunk %d is %d bytes, under the %d min", count, len(piece), opt.MinSize)
		}
		total += len(piece)
		count++
	}
	if total != len(data) {
		t.Fatalf("chunks cover %d bytes, input was %d", total, len(data))
	}
	if count < 10 {
		t.Fatalf("only %d chunks for 1 MiB at a 4 KiB average", count)
	}
	if ck.Options().AvgSize != opt.AvgSize {
		t.Error("Options() did not report the configured average")
	}
}

func TestChunkerIsDeterministic(t *testing.T) {
	data := deterministicData(512<<10, 2)
	first := chunkAll(t, data, DefaultOptions())
	second := chunkAll(t, data, DefaultOptions())
	if len(first) != len(second) {
		t.Fatalf("chunk counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

// TestChunkerBoundaryShift is the property that makes content-defined chunking
// worth using: inserting bytes near the start must not change the chunks after
// the insertion point.
func TestChunkerBoundaryShift(t *testing.T) {
	opt := Options{AvgSize: 4096, MinSize: 1024, MaxSize: 16384}
	original := deterministicData(1<<20, 3)

	modified := make([]byte, 0, len(original)+64)
	modified = append(modified, original[:5000]...)
	modified = append(modified, bytes.Repeat([]byte("INSERTED"), 8)...)
	modified = append(modified, original[5000:]...)

	before := chunkAll(t, original, opt)
	after := chunkAll(t, modified, opt)

	shared := map[Digest]bool{}
	for _, d := range before {
		shared[d] = true
	}
	common := 0
	for _, d := range after {
		if shared[d] {
			common++
		}
	}
	// A fixed-size chunker would share almost nothing here.
	if ratio := float64(common) / float64(len(after)); ratio < 0.9 {
		t.Fatalf("only %.0f%% of chunks survived a 64-byte insertion (%d/%d)",
			100*ratio, common, len(after))
	}
}

func chunkAll(t *testing.T, data []byte, opt Options) []Digest {
	t.Helper()
	ck := NewChunker(bytes.NewReader(data), opt)
	var out []Digest
	for {
		piece, _, err := ck.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, Sum(piece))
	}
}

func TestHashMatchesChunker(t *testing.T) {
	data := deterministicData(700<<10, 4)
	opt := Options{AvgSize: 8192, MinSize: 2048, MaxSize: 32768}

	sig, err := Hash(context.Background(), bytes.NewReader(data), opt, Config{Jobs: 4})
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if sig.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", sig.Size, len(data))
	}
	whole, _, err := HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if sig.Digest != whole {
		t.Fatal("whole-file digest does not match a plain BLAKE3 of the input")
	}

	want := chunkAll(t, data, opt)
	if len(sig.Chunks) != len(want) {
		t.Fatalf("Hash produced %d chunks, the chunker produced %d", len(sig.Chunks), len(want))
	}
	var off uint64
	for i, c := range sig.Chunks {
		if c.Index != i {
			t.Fatalf("chunk %d has index %d", i, c.Index)
		}
		if c.Offset != off {
			t.Fatalf("chunk %d offset = %d, want %d", i, c.Offset, off)
		}
		if c.Digest != want[i] {
			t.Fatalf("chunk %d digest differs from the sequential chunker", i)
		}
		if c.End() != off+uint64(c.Length) {
			t.Fatalf("chunk %d End() is inconsistent", i)
		}
		off += uint64(c.Length)
	}
}

func TestHashJobCountDoesNotChangeResult(t *testing.T) {
	data := deterministicData(300<<10, 5)
	var reference Signature
	for _, jobs := range []int{1, 2, 8, 64, 1000} {
		sig, err := Hash(context.Background(), bytes.NewReader(data), DefaultOptions(), Config{Jobs: jobs})
		if err != nil {
			t.Fatalf("jobs=%d: %v", jobs, err)
		}
		if reference.Size == 0 {
			reference = sig
			continue
		}
		if sig.Digest != reference.Digest || len(sig.Chunks) != len(reference.Chunks) {
			t.Fatalf("jobs=%d produced a different result", jobs)
		}
		for i := range sig.Chunks {
			if sig.Chunks[i] != reference.Chunks[i] {
				t.Fatalf("jobs=%d chunk %d differs", jobs, i)
			}
		}
	}
}

func TestHashEmptyInput(t *testing.T) {
	sig, err := Hash(context.Background(), bytes.NewReader(nil), DefaultOptions(), Config{})
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if sig.Size != 0 || len(sig.Chunks) != 0 {
		t.Fatalf("empty input produced %d bytes in %d chunks", sig.Size, len(sig.Chunks))
	}
	empty, _, _ := HashReader(bytes.NewReader(nil))
	if sig.Digest != empty {
		t.Fatal("empty file digest is wrong")
	}
}

func TestHashSinkAndProgress(t *testing.T) {
	data := deterministicData(200<<10, 6)

	var (
		mu       sync.Mutex
		seen     int64
		progress int64
	)
	sig, err := Hash(context.Background(), bytes.NewReader(data), DefaultOptions(), Config{
		Jobs: 3,
		Sink: SinkFunc(func(c Chunk, payload []byte) error {
			if Sum(payload) != c.Digest {
				t.Error("sink received a payload that does not match its digest")
			}
			mu.Lock()
			seen += int64(len(payload))
			mu.Unlock()
			return nil
		}),
		Progress: func(n int64) {
			mu.Lock()
			progress += n
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if seen != sig.Size {
		t.Fatalf("sink saw %d bytes, file is %d", seen, sig.Size)
	}
	if progress != sig.Size {
		t.Fatalf("progress reported %d bytes, file is %d", progress, sig.Size)
	}
}

func TestHashSinkErrorPropagates(t *testing.T) {
	sentinel := errors.New("sink is full")
	_, err := Hash(context.Background(), bytes.NewReader(deterministicData(500<<10, 7)),
		DefaultOptions(), Config{
			Jobs: 2,
			Sink: SinkFunc(func(Chunk, []byte) error { return sentinel }),
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the sink error", err)
	}
}

func TestHashSkipFileDigest(t *testing.T) {
	sig, err := Hash(context.Background(), bytes.NewReader(deterministicData(100<<10, 8)),
		DefaultOptions(), Config{SkipFileDigest: true})
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !sig.Digest.IsZero() {
		t.Fatal("SkipFileDigest still produced a whole-file digest")
	}
	if len(sig.Chunks) == 0 {
		t.Fatal("SkipFileDigest lost the chunk list")
	}
}

func TestHashRejectsBadOptions(t *testing.T) {
	_, err := Hash(context.Background(), bytes.NewReader(nil), Options{AvgSize: 1024, MinSize: 4096, MaxSize: 8192}, Config{})
	if err == nil {
		t.Fatal("Hash accepted min > avg")
	}
}

func TestHashReaderError(t *testing.T) {
	sentinel := errors.New("disk on fire")
	_, err := Hash(context.Background(), &failingReader{err: sentinel}, DefaultOptions(), Config{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the read error", err)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestHashContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Hash(ctx, bytes.NewReader(deterministicData(4<<20, 9)), DefaultOptions(), Config{Jobs: 1})
	if err == nil {
		t.Fatal("Hash ignored a cancelled context")
	}
}

func TestHashAt(t *testing.T) {
	data := deterministicData(64<<10, 10)
	ra := bytes.NewReader(data)
	got, err := HashAt(ra, 1000, 500, nil)
	if err != nil {
		t.Fatalf("HashAt: %v", err)
	}
	if want := Sum(data[1000:1500]); got != want {
		t.Fatal("HashAt hashed the wrong range")
	}
	// A supplied buffer that is too small must be grown, not truncated.
	got, err = HashAt(ra, 0, 4096, make([]byte, 8))
	if err != nil {
		t.Fatalf("HashAt with a short buffer: %v", err)
	}
	if want := Sum(data[:4096]); got != want {
		t.Fatal("HashAt with a short buffer produced the wrong digest")
	}
	if _, err := HashAt(ra, int64(len(data)-10), 100, nil); err == nil {
		t.Fatal("HashAt past the end should fail")
	}
}

func TestHashPath(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/blob.bin"
	data := deterministicData(128<<10, 11)
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	sig, err := HashPath(context.Background(), path, DefaultOptions(), Config{})
	if err != nil {
		t.Fatalf("HashPath: %v", err)
	}
	if sig.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", sig.Size, len(data))
	}
	if _, err := HashPath(context.Background(), dir+"/missing", DefaultOptions(), Config{}); err == nil {
		t.Fatal("HashPath on a missing file should fail")
	}
}

func TestLog2AndMask(t *testing.T) {
	if log2(1) != 0 || log2(1024) != 10 || log2(1<<20) != 20 {
		t.Fatal("log2 is wrong")
	}
	if mask(0) != 1 {
		t.Fatalf("mask(0) = %d, want 1", mask(0))
	}
	if mask(40) != (1<<31)-1 {
		t.Fatalf("mask(40) = %d, want the 31-bit mask", mask(40))
	}
}
