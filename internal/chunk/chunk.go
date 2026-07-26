// Package chunk implements FastCDC content-defined chunking with BLAKE3-256
// digests. It is tuned for the access pattern modelmove cares about: a small
// number of very large, high-entropy weight files alongside a long tail of
// small metadata files.
package chunk

import (
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/zeebo/blake3"
)

// Algorithm identifiers recorded in manifests so that a reader can tell
// whether it is able to reproduce the digests it is looking at.
const (
	HashAlgorithm    = "blake3-256"
	ChunkerAlgorithm = "fastcdc-gear-nc2"
)

// DigestSize is the length in bytes of a BLAKE3-256 digest.
const DigestSize = 32

// Bounds accepted by Options.Validate.
const (
	MinChunkSize = 1 << 6  // 64 B
	MaxChunkSize = 1 << 30 // 1 GiB
)

// ErrShortDigest is returned when parsing a digest of the wrong length.
var ErrShortDigest = errors.New("chunk: digest must be 64 hex characters")

// Digest is a BLAKE3-256 digest.
type Digest [DigestSize]byte

func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// Short returns the first 12 hex characters, for human-facing output.
func (d Digest) Short() string { return hex.EncodeToString(d[:6]) }

// IsZero reports whether d is the zero digest, which never occurs naturally.
func (d Digest) IsZero() bool { return d == Digest{} }

// MarshalText encodes the digest as lowercase hex.
func (d Digest) MarshalText() ([]byte, error) {
	out := make([]byte, hex.EncodedLen(DigestSize))
	hex.Encode(out, d[:])
	return out, nil
}

// UnmarshalText decodes a lowercase or uppercase hex digest.
func (d *Digest) UnmarshalText(text []byte) error {
	if len(text) != hex.EncodedLen(DigestSize) {
		return ErrShortDigest
	}
	if _, err := hex.Decode(d[:], text); err != nil {
		return fmt.Errorf("chunk: parse digest: %w", err)
	}
	return nil
}

// ParseDigest decodes a hex-encoded digest.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	err := d.UnmarshalText([]byte(s))
	return d, err
}

// Sum returns the BLAKE3-256 digest of b.
func Sum(b []byte) Digest { return blake3.Sum256(b) }

// NewHasher returns a streaming BLAKE3-256 hasher.
func NewHasher() hash.Hash { return blake3.New() }

// Chunk describes one content-defined chunk of a file. Data is never carried
// here; chunk payloads are streamed separately so that manifests stay small.
type Chunk struct {
	Index  int
	Offset uint64
	Length uint32
	Digest Digest
}

// End returns the offset one past the last byte of the chunk.
func (c Chunk) End() uint64 { return c.Offset + uint64(c.Length) }

// Signature is the full content-addressed description of one file.
type Signature struct {
	Size    int64
	Digest  Digest
	Options Options
	Chunks  []Chunk
}

// Options controls the FastCDC parameters. Zero values are filled in by
// Normalized with the small-file defaults.
type Options struct {
	AvgSize uint32 `json:"avg_size"`
	MinSize uint32 `json:"min_size"`
	MaxSize uint32 `json:"max_size"`
}

// Chunk size presets. Model weight files are frequently tens of gigabytes, and
// a 64 KiB average would produce hundreds of thousands of manifest entries for
// no deduplication benefit: a changed tensor spans megabytes, not kilobytes.
func smallOptions() Options {
	return Options{AvgSize: 64 << 10, MinSize: 16 << 10, MaxSize: 256 << 10}
}

func largeOptions() Options {
	return Options{AvgSize: 1 << 20, MinSize: 256 << 10, MaxSize: 4 << 20}
}

func hugeOptions() Options {
	return Options{AvgSize: 4 << 20, MinSize: 1 << 20, MaxSize: 16 << 20}
}

// SmallFileOptions returns the defaults used for configs, tokenizers and other
// metadata files.
func SmallFileOptions() Options { return smallOptions() }

// LargeFileOptions returns the defaults used for weight files up to 1 GiB.
func LargeFileOptions() Options { return largeOptions() }

// HugeFileOptions returns the defaults used for weight files past 1 GiB.
func HugeFileOptions() Options { return hugeOptions() }

// DefaultOptions returns the small-file preset.
func DefaultOptions() Options { return smallOptions() }

// Thresholds at which OptionsForSize escalates to a larger preset.
const (
	LargeFileThreshold = 64 << 20 // 64 MiB
	HugeFileThreshold  = 1 << 30  // 1 GiB
)

// OptionsForSize picks chunker parameters from a file size. The choice is a
// pure function of size so that two machines independently chunking the same
// file always agree on boundaries.
func OptionsForSize(size int64) Options {
	switch {
	case size >= HugeFileThreshold:
		return hugeOptions()
	case size >= LargeFileThreshold:
		return largeOptions()
	default:
		return smallOptions()
	}
}

// IsZero reports whether no parameter has been set.
func (o Options) IsZero() bool { return o == Options{} }

// Normalized fills in unset fields with the small-file defaults.
func (o Options) Normalized() Options {
	d := smallOptions()
	if o.AvgSize == 0 {
		o.AvgSize = d.AvgSize
	}
	if o.MinSize == 0 {
		o.MinSize = o.AvgSize / 4
	}
	if o.MaxSize == 0 {
		o.MaxSize = o.AvgSize * 4
	}
	return o
}

// Validate rejects parameter sets that would produce a degenerate or
// non-terminating chunker.
func (o Options) Validate() error {
	switch {
	case o.MinSize < MinChunkSize:
		return fmt.Errorf("chunk: min size %d is below %d", o.MinSize, MinChunkSize)
	case o.MaxSize > MaxChunkSize:
		return fmt.Errorf("chunk: max size %d is above %d", o.MaxSize, MaxChunkSize)
	case o.MinSize > o.AvgSize:
		return fmt.Errorf("chunk: min size %d exceeds average size %d", o.MinSize, o.AvgSize)
	case o.AvgSize > o.MaxSize:
		return fmt.Errorf("chunk: average size %d exceeds max size %d", o.AvgSize, o.MaxSize)
	}
	return nil
}

func (o Options) String() string {
	return fmt.Sprintf("avg=%d min=%d max=%d", o.AvgSize, o.MinSize, o.MaxSize)
}

// HashReader returns the BLAKE3-256 digest and byte count of r.
func HashReader(r io.Reader) (Digest, int64, error) {
	h := blake3.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return Digest{}, n, err
	}
	var d Digest
	copy(d[:], h.Sum(nil))
	return d, n, nil
}

// HashAt returns the digest of the length bytes of ra starting at offset.
func HashAt(ra io.ReaderAt, offset int64, length int, buf []byte) (Digest, error) {
	if cap(buf) < length {
		buf = make([]byte, length)
	}
	buf = buf[:length]
	if _, err := io.ReadFull(newSectionReader(ra, offset, int64(length)), buf); err != nil {
		return Digest{}, err
	}
	return Sum(buf), nil
}

func newSectionReader(ra io.ReaderAt, off, n int64) io.Reader {
	return io.NewSectionReader(ra, off, n)
}
