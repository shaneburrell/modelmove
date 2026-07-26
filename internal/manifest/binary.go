package manifest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
)

// Magic identifies the compact binary encoding. A 70 GB model chunked at 4 MiB
// still only produces ~18k chunks, but a Hugging Face cache full of small
// files can reach millions, and JSON costs roughly 3x per chunk.
var Magic = [4]byte{'M', 'M', 'M', '1'}

const (
	binHashBLAKE3      = 1
	binChunkerFastCDC  = 1
	maxStringLen       = 1 << 16
	initialAllocCap    = 4096
	binaryTrailerBytes = chunk.DigestSize
)

// Decode limits. These are far above anything a real model produces (the
// largest checkpoints run to a few thousand files and a few million chunks)
// and exist so that a corrupt length prefix cannot turn into a huge
// allocation or a long parse.
const (
	maxFileCount  = 1 << 24
	maxChunkCount = 1 << 28
	maxShardCount = 1 << 16
	maxPartCount  = 1 << 16
)

// ErrBadMagic is returned when a stream is not a binary manifest.
var ErrBadMagic = errors.New("manifest: not a binary manifest")

type binWriter struct {
	w   *bufio.Writer
	h   hash.Hash
	buf []byte
	err error
}

func newBinWriter(w io.Writer) *binWriter {
	hasher := chunk.NewHasher()
	bw := bufio.NewWriterSize(io.MultiWriter(w, hasher), 64<<10)
	return &binWriter{w: bw, h: hasher, buf: make([]byte, binary.MaxVarintLen64)}
}

func (b *binWriter) raw(p []byte) {
	if b.err != nil {
		return
	}
	_, b.err = b.w.Write(p)
}

func (b *binWriter) u8(v uint8) {
	if b.err != nil {
		return
	}
	b.err = b.w.WriteByte(v)
}

func (b *binWriter) uvarint(v uint64) {
	if b.err != nil {
		return
	}
	n := binary.PutUvarint(b.buf, v)
	_, b.err = b.w.Write(b.buf[:n])
}

func (b *binWriter) varint(v int64) {
	if b.err != nil {
		return
	}
	n := binary.PutVarint(b.buf, v)
	_, b.err = b.w.Write(b.buf[:n])
}

func (b *binWriter) u32(v uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	b.raw(tmp[:])
}

func (b *binWriter) str(s string) {
	if len(s) > maxStringLen {
		b.err = fmt.Errorf("manifest: string of %d bytes exceeds limit", len(s))
		return
	}
	b.uvarint(uint64(len(s)))
	b.raw([]byte(s))
}

func (b *binWriter) digest(d chunk.Digest) { b.raw(d[:]) }

// EncodeBinary writes m in the compact binary encoding, terminated by a
// BLAKE3 digest of everything before it.
func EncodeBinary(w io.Writer, m *Manifest) error {
	b := newBinWriter(w)

	b.raw(Magic[:])
	b.u8(uint8(Version))
	b.u8(binHashBLAKE3)
	b.u8(binChunkerFastCDC)
	b.u8(0) // flags, reserved
	b.varint(m.CreatedAt.Unix())
	b.str(m.Tool)
	b.u32(m.Chunker.AvgSize)
	b.u32(m.Chunker.MinSize)
	b.u32(m.Chunker.MaxSize)

	b.str(m.Model.Name)
	b.str(string(m.Model.Kind))
	b.str(m.Model.Root)
	b.digest(m.Model.Digest)
	b.uvarint(uint64(len(m.Model.Shards)))
	for _, s := range m.Model.Shards {
		b.str(s.Name)
		b.uvarint(uint64(s.Total))
		b.uvarint(uint64(s.Size))
		b.uvarint(uint64(len(s.Parts)))
		for _, p := range s.Parts {
			b.str(p)
		}
	}

	b.uvarint(uint64(len(m.Files)))
	for _, f := range m.Files {
		b.str(f.Path)
		b.uvarint(uint64(f.Size))
		b.uvarint(uint64(f.Mode))
		b.varint(f.ModTime.UnixNano())
		b.str(string(f.Role))
		b.digest(f.Digest)
		b.u32(f.Chunker.AvgSize)
		b.u32(f.Chunker.MinSize)
		b.u32(f.Chunker.MaxSize)
		b.uvarint(uint64(len(f.Chunks)))
		for _, c := range f.Chunks {
			b.uvarint(uint64(c.Length))
			b.digest(c.Digest)
		}
	}

	if b.err != nil {
		return b.err
	}
	if err := b.w.Flush(); err != nil {
		return err
	}
	sum := b.h.Sum(nil)
	_, err := w.Write(sum[:binaryTrailerBytes])
	return err
}

type binReader struct {
	r   *bufio.Reader
	h   hash.Hash
	err error
}

func (b *binReader) read(p []byte) {
	if b.err != nil {
		return
	}
	if _, err := io.ReadFull(b.r, p); err != nil {
		b.err = wrapEOF(err)
		return
	}
	_, _ = b.h.Write(p)
}

func (b *binReader) u8() uint8 {
	var p [1]byte
	b.read(p[:])
	return p[0]
}

func (b *binReader) u32() uint32 {
	var p [4]byte
	b.read(p[:])
	return binary.BigEndian.Uint32(p[:])
}

func (b *binReader) uvarint() uint64 {
	if b.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(byteReaderFunc(b.byteAndHash))
	if err != nil {
		b.err = wrapEOF(err)
		return 0
	}
	return v
}

func (b *binReader) varint() int64 {
	if b.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(byteReaderFunc(b.byteAndHash))
	if err != nil {
		b.err = wrapEOF(err)
		return 0
	}
	return v
}

func (b *binReader) byteAndHash() (byte, error) {
	c, err := b.r.ReadByte()
	if err != nil {
		return 0, err
	}
	_, _ = b.h.Write([]byte{c})
	return c, nil
}

func (b *binReader) str() string {
	n := b.uvarint()
	if b.err != nil {
		return ""
	}
	if n > maxStringLen {
		b.err = fmt.Errorf("manifest: string length %d exceeds limit", n)
		return ""
	}
	p := make([]byte, n)
	b.read(p)
	return string(p)
}

func (b *binReader) digest() chunk.Digest {
	var d chunk.Digest
	b.read(d[:])
	return d
}

// count reads a length prefix and rejects absurd values before anything is
// allocated, so that a corrupt or hostile manifest cannot exhaust memory.
func (b *binReader) count(what string, limit uint64) int {
	n := b.uvarint()
	if b.err != nil {
		return 0
	}
	if n > limit {
		b.err = fmt.Errorf("manifest: %s count %d exceeds limit %d", what, n, limit)
		return 0
	}
	return int(n)
}

func allocCap(n int) int {
	if n > initialAllocCap {
		return initialAllocCap
	}
	return n
}

type byteReaderFunc func() (byte, error)

func (f byteReaderFunc) ReadByte() (byte, error) { return f() }

func wrapEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// DecodeBinary reads a manifest in the compact binary encoding and verifies
// its trailing digest.
func DecodeBinary(r io.Reader) (*Manifest, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	magic, err := br.Peek(len(Magic))
	if err != nil || string(magic) != string(Magic[:]) {
		return nil, ErrBadMagic
	}

	b := &binReader{r: br, h: chunk.NewHasher()}
	var got [4]byte
	b.read(got[:])
	version := b.u8()
	hashAlgo := b.u8()
	chunkerAlgo := b.u8()
	_ = b.u8() // flags
	if b.err != nil {
		return nil, b.err
	}
	if int(version) != Version {
		return nil, fmt.Errorf("manifest: unsupported binary version %d", version)
	}
	if hashAlgo != binHashBLAKE3 {
		return nil, fmt.Errorf("manifest: unsupported hash id %d", hashAlgo)
	}
	if chunkerAlgo != binChunkerFastCDC {
		return nil, fmt.Errorf("manifest: unsupported chunker id %d", chunkerAlgo)
	}

	m := &Manifest{Format: Format, Version: Version, Hash: chunk.HashAlgorithm}
	m.CreatedAt = time.Unix(b.varint(), 0).UTC()
	m.Tool = b.str()
	m.Chunker = Chunker{Algorithm: chunk.ChunkerAlgorithm, AvgSize: b.u32(), MinSize: b.u32(), MaxSize: b.u32()}

	m.Model.Name = b.str()
	m.Model.Kind = layout.Kind(b.str())
	m.Model.Root = b.str()
	m.Model.Digest = b.digest()

	shardCount := b.count("shard", maxShardCount)
	if b.err != nil {
		return nil, b.err
	}
	m.Model.Shards = make([]layout.ShardSet, 0, allocCap(shardCount))
	for i := 0; i < shardCount && b.err == nil; i++ {
		s := layout.ShardSet{Name: b.str()}
		s.Total = int(b.uvarint())
		s.Size = int64(b.uvarint())
		parts := b.count("shard part", maxPartCount)
		if b.err != nil {
			break
		}
		s.Parts = make([]string, 0, allocCap(parts))
		for j := 0; j < parts && b.err == nil; j++ {
			s.Parts = append(s.Parts, b.str())
		}
		m.Model.Shards = append(m.Model.Shards, s)
	}

	fileCount := b.count("file", maxFileCount)
	if b.err != nil {
		return nil, b.err
	}
	m.Files = make([]*File, 0, allocCap(fileCount))
	for i := 0; i < fileCount && b.err == nil; i++ {
		f := &File{}
		f.Path = b.str()
		f.Size = int64(b.uvarint())
		f.Mode = uint32(b.uvarint())
		f.ModTime = time.Unix(0, b.varint()).UTC()
		f.Role = layout.Role(b.str())
		f.Digest = b.digest()
		f.Chunker = Chunker{Algorithm: chunk.ChunkerAlgorithm, AvgSize: b.u32(), MinSize: b.u32(), MaxSize: b.u32()}
		n := b.count("chunk", maxChunkCount)
		if b.err != nil {
			break
		}
		f.Chunks = make([]Chunk, 0, allocCap(n))
		var off uint64
		for j := 0; j < n && b.err == nil; j++ {
			length := b.uvarint()
			if length > uint64(^uint32(0)) {
				b.err = fmt.Errorf("manifest: %s: chunk length %d out of range", f.Path, length)
				break
			}
			f.Chunks = append(f.Chunks, Chunk{Offset: off, Length: uint32(length), Digest: b.digest()})
			off += length
		}
		m.Files = append(m.Files, f)
	}
	if b.err != nil {
		return nil, b.err
	}

	want := b.h.Sum(nil)[:binaryTrailerBytes]
	var trailer [binaryTrailerBytes]byte
	if _, err := io.ReadFull(br, trailer[:]); err != nil {
		return nil, fmt.Errorf("manifest: reading trailer: %w", wrapEOF(err))
	}
	if string(trailer[:]) != string(want) {
		return nil, errors.New("manifest: binary manifest is corrupt (digest mismatch)")
	}
	m.Recount()
	return m, nil
}
