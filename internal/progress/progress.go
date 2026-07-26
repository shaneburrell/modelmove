// Package progress renders a single-line transfer progress bar on a terminal.
// It degrades to silence when the output is not a terminal, so that piping a
// manifest to a file stays clean.
package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaneburrell/modelmove/internal/units"
)

// DefaultInterval is how often an active bar repaints.
const DefaultInterval = 120 * time.Millisecond

// Options configures a Bar.
type Options struct {
	// Total is the expected byte count. It may be updated later with SetTotal.
	Total int64
	// Label is shown at the start of the line.
	Label string
	// Interval overrides DefaultInterval.
	Interval time.Duration
	// Width overrides the default bar width in characters.
	Width int
}

// Bar is a byte-oriented progress bar. All methods are safe for concurrent
// use, and every method tolerates a nil receiver so callers can pass a
// disabled bar around without nil checks.
type Bar struct {
	w        io.Writer
	label    string
	width    int
	interval time.Duration

	total   atomic.Int64
	current atomic.Int64
	extra   atomic.Value // string

	start time.Time
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once

	mu      sync.Mutex
	lastLen int
}

// New starts a bar writing to w. Use Disabled for a no-op bar.
func New(w io.Writer, opts Options) *Bar {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Width <= 0 {
		opts.Width = 24
	}
	b := &Bar{
		w:        w,
		label:    opts.Label,
		width:    opts.Width,
		interval: opts.Interval,
		start:    time.Now(),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	b.total.Store(opts.Total)
	b.extra.Store("")
	go b.loop()
	return b
}

// NewFor starts a bar on stderr when stderr is a terminal and enabled is true,
// and returns nil otherwise.
func NewFor(enabled bool, opts Options) *Bar {
	if !enabled || !IsTerminal(os.Stderr) {
		return nil
	}
	return New(os.Stderr, opts)
}

// Disabled returns a bar that renders nothing.
func Disabled() *Bar { return nil }

// IsTerminal reports whether f refers to a character device.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Add advances the bar by n bytes.
func (b *Bar) Add(n int64) {
	if b == nil {
		return
	}
	b.current.Add(n)
}

// Set sets the absolute byte count.
func (b *Bar) Set(n int64) {
	if b == nil {
		return
	}
	b.current.Store(n)
}

// SetTotal updates the expected byte count.
func (b *Bar) SetTotal(n int64) {
	if b == nil {
		return
	}
	b.total.Store(n)
}

// SetLabel replaces the trailing detail text, typically the current filename.
func (b *Bar) SetLabel(s string) {
	if b == nil {
		return
	}
	b.extra.Store(s)
}

// Current returns the bytes counted so far.
func (b *Bar) Current() int64 {
	if b == nil {
		return 0
	}
	return b.current.Load()
}

// Finish stops the bar and clears its line.
func (b *Bar) Finish() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		close(b.stop)
		<-b.done
		b.clear()
	})
}

func (b *Bar) loop() {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.render()
		}
	}
}

func (b *Bar) render() {
	cur := b.current.Load()
	total := b.total.Load()
	elapsed := time.Since(b.start)

	var rate float64
	if elapsed > 0 {
		rate = float64(cur) / elapsed.Seconds()
	}

	var line strings.Builder
	if b.label != "" {
		fmt.Fprintf(&line, "%s ", b.label)
	}
	if total > 0 {
		filled := int(float64(b.width) * float64(cur) / float64(total))
		if filled > b.width {
			filled = b.width
		}
		if filled < 0 {
			filled = 0
		}
		fmt.Fprintf(&line, "[%s%s] %4s  %s / %s  %s",
			strings.Repeat("=", filled),
			strings.Repeat(" ", b.width-filled),
			units.Percent(cur, total),
			units.Bytes(cur), units.Bytes(total), units.Rate(rate))
		if rate > 0 && cur < total {
			eta := time.Duration(float64(total-cur)/rate) * time.Second
			fmt.Fprintf(&line, "  eta %s", units.Duration(eta))
		}
	} else {
		fmt.Fprintf(&line, "%s  %s", units.Bytes(cur), units.Rate(rate))
	}
	if extra, _ := b.extra.Load().(string); extra != "" {
		fmt.Fprintf(&line, "  %s", extra)
	}

	b.write(line.String())
}

func (b *Bar) write(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pad := ""
	if n := b.lastLen - len([]rune(s)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(b.w, "\r%s%s", s, pad)
	b.lastLen = len([]rune(s))
}

func (b *Bar) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastLen == 0 {
		return
	}
	fmt.Fprintf(b.w, "\r%s\r", strings.Repeat(" ", b.lastLen))
	b.lastLen = 0
}
