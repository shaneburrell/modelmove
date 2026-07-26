package units

import (
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"512", 512},
		{"1k", 1024},
		{"1K", 1024},
		{"64K", 64 * 1024},
		{"1KiB", 1024},
		{"1kb", 1000},
		{"1MiB", 1 << 20},
		{"1m", 1 << 20},
		{"2.5g", 2.5 * (1 << 30)},
		{"1t", 1 << 40},
		{"4096b", 4096},
		{" 8M ", 8 << 20},
		{"1MB", 1000 * 1000},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseSizeErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "K", "-5", "1x2", "NaN", "1e400"} {
		if v, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", in, v)
		}
	}
}

func TestBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{150 << 20, "150 MiB"},
		{1 << 30, "1.0 GiB"},
		{-2048, "-2.0 KiB"},
	}
	for _, c := range cases {
		if got := Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRate(t *testing.T) {
	if got := Rate(0); got != "0 B/s" {
		t.Errorf("Rate(0) = %q", got)
	}
	if got := Rate(-1); got != "0 B/s" {
		t.Errorf("Rate(-1) = %q", got)
	}
	if got := Rate(1 << 20); got != "1.0 MiB/s" {
		t.Errorf("Rate(1MiB) = %q", got)
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{2*time.Hour + 4*time.Minute, "2h04m"},
	}
	for _, c := range cases {
		if got := Duration(c.in); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPercent(t *testing.T) {
	if got := Percent(1, 0); got != "100%" {
		t.Errorf("Percent with a zero total = %q", got)
	}
	if got := Percent(1, 4); got != "25%" {
		t.Errorf("Percent(1,4) = %q", got)
	}
}
