package progress

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer that is safe to read while the bar's render
// goroutine writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestNilBarIsSafe(t *testing.T) {
	var b *Bar
	b.Add(10)
	b.Set(20)
	b.SetTotal(30)
	b.SetLabel("x")
	b.Finish()
	if b.Current() != 0 {
		t.Error("a nil bar should report no progress")
	}
	if Disabled() != nil {
		t.Error("Disabled should return nil")
	}
}

func TestBarRenders(t *testing.T) {
	out := &syncBuffer{}
	b := New(out, Options{Total: 1000, Label: "send", Interval: 5 * time.Millisecond})
	b.Add(500)
	b.SetLabel("weights.safetensors")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "50%") {
		time.Sleep(5 * time.Millisecond)
	}
	got := out.String()
	b.Finish()

	if !strings.Contains(got, "50%") {
		t.Fatalf("bar never rendered 50%%: %q", got)
	}
	if !strings.Contains(got, "send") {
		t.Errorf("bar is missing its label: %q", got)
	}
	if b.Current() != 500 {
		t.Errorf("Current() = %d, want 500", b.Current())
	}
	// Finish clears the line so that following output starts clean.
	if !strings.HasSuffix(out.String(), "\r") {
		t.Error("Finish did not clear the line")
	}
}

func TestBarWithoutTotal(t *testing.T) {
	out := &syncBuffer{}
	b := New(out, Options{Interval: 5 * time.Millisecond})
	b.Set(4096)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "KiB") {
		time.Sleep(5 * time.Millisecond)
	}
	got := out.String()
	b.Finish()
	if !strings.Contains(got, "4.0 KiB") {
		t.Fatalf("bar without a total did not show the byte count: %q", got)
	}
}

func TestBarFinishIsIdempotent(t *testing.T) {
	b := New(&syncBuffer{}, Options{Total: 10, Interval: time.Millisecond})
	b.Finish()
	b.Finish()
}

func TestBarSetTotalLate(t *testing.T) {
	out := &syncBuffer{}
	b := New(out, Options{Interval: 5 * time.Millisecond})
	b.SetTotal(200)
	b.Add(200)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "100%") {
		time.Sleep(5 * time.Millisecond)
	}
	got := out.String()
	b.Finish()
	if !strings.Contains(got, "100%") {
		t.Fatalf("bar did not reach 100%%: %q", got)
	}
}

func TestNewForDisabled(t *testing.T) {
	if b := NewFor(false, Options{}); b != nil {
		b.Finish()
		t.Error("NewFor(false) should return nil")
	}
	// Tests never run with stderr on a terminal, so this stays nil too.
	if b := NewFor(true, Options{}); b != nil {
		b.Finish()
		t.Error("NewFor should return nil when stderr is not a terminal")
	}
}

func TestIsTerminal(t *testing.T) {
	if IsTerminal(nil) {
		t.Error("nil is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("a regular file is not a terminal")
	}
	closed, err := os.CreateTemp(t.TempDir(), "y")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if IsTerminal(closed) {
		t.Error("a closed file is not a terminal")
	}
}
