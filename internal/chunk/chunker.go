package chunk

import (
	"io"
)

// normalizationLevel is the FastCDC "NC" level: the number of bits added to
// the cut mask before the average size and removed after it. Level 2 is what
// the paper recommends and keeps the chunk size distribution tight around the
// target average instead of the exponential spread of plain gear hashing.
const normalizationLevel = 2

// Chunker splits a stream into content-defined chunks. It is not safe for
// concurrent use.
type Chunker struct {
	r     io.Reader
	opt   Options
	buf   []byte
	start int
	end   int
	eof   bool
	off   uint64
	maskS uint32
	maskL uint32
}

// NewChunker returns a Chunker reading from r. Options are normalized but not
// validated; call Options.Validate first if the values came from a user.
func NewChunker(r io.Reader, opt Options) *Chunker {
	opt = opt.Normalized()
	bits := log2(opt.AvgSize)
	return &Chunker{
		r:     r,
		opt:   opt,
		buf:   make([]byte, int(opt.MaxSize)*2),
		maskS: mask(bits + normalizationLevel),
		maskL: mask(bits - normalizationLevel),
	}
}

// Options returns the normalized parameters in use.
func (c *Chunker) Options() Options { return c.opt }

// Next returns the next chunk and its offset. The returned slice aliases an
// internal buffer and is only valid until the following call to Next; copy it
// if it must outlive that. io.EOF is returned once the stream is exhausted.
func (c *Chunker) Next() ([]byte, uint64, error) {
	if err := c.fill(); err != nil {
		return nil, 0, err
	}
	if c.start == c.end {
		return nil, c.off, io.EOF
	}
	cut := c.findCut(c.buf[c.start:c.end])
	out := c.buf[c.start : c.start+cut]
	off := c.off
	c.start += cut
	c.off += uint64(cut)
	return out, off, nil
}

// fill tops the buffer up so that findCut always sees at least MaxSize bytes
// unless the stream has ended.
func (c *Chunker) fill() error {
	if c.eof || c.end-c.start >= int(c.opt.MaxSize) {
		return nil
	}
	if c.start > 0 {
		c.end = copy(c.buf, c.buf[c.start:c.end])
		c.start = 0
	}
	for c.end < len(c.buf) {
		n, err := c.r.Read(c.buf[c.end:])
		c.end += n
		if err == io.EOF {
			c.eof = true
			return nil
		}
		if err != nil {
			return err
		}
		if n > 0 && c.end >= int(c.opt.MaxSize) {
			return nil
		}
	}
	return nil
}

// findCut returns the length of the next chunk within data, implementing
// FastCDC with normalized chunking: a strict mask is used before the target
// average size and a lenient one after it.
func (c *Chunker) findCut(data []byte) int {
	n := len(data)
	minSize := int(c.opt.MinSize)
	if n <= minSize {
		return n
	}
	if maxSize := int(c.opt.MaxSize); n > maxSize {
		n = maxSize
	}
	normal := int(c.opt.AvgSize)
	if normal > n {
		normal = n
	}

	var h uint32
	i := minSize
	for ; i < normal; i++ {
		h = (h << 1) + gear[data[i]]
		if h&c.maskS == 0 {
			return i
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + gear[data[i]]
		if h&c.maskL == 0 {
			return i
		}
	}
	return n
}

// log2 returns the position of the highest set bit of v.
func log2(v uint32) uint {
	var n uint
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

func mask(bits uint) uint32 {
	if bits < 1 {
		bits = 1
	}
	if bits > 31 {
		bits = 31
	}
	return (1 << bits) - 1
}
