// Package units parses and formats the human-facing byte sizes, rates and
// durations used throughout the CLI.
package units

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

var suffixes = []struct {
	name  string
	scale uint64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"kb", 1000}, {"mb", 1000 * 1000}, {"gb", 1000 * 1000 * 1000}, {"tb", 1000 * 1000 * 1000 * 1000},
	{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
	{"b", 1},
}

// ParseSize parses a byte size such as "4096", "64K", "1MiB" or "2.5g". Bare
// numbers are bytes; single-letter and IEC suffixes are powers of two, and SI
// suffixes ("kb", "mb") are powers of ten.
func ParseSize(s string) (uint64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, fmt.Errorf("units: empty size")
	}
	lower := strings.ToLower(raw)
	for _, suf := range suffixes {
		if !strings.HasSuffix(lower, suf.name) {
			continue
		}
		num := strings.TrimSpace(lower[:len(lower)-len(suf.name)])
		if num == "" {
			return 0, fmt.Errorf("units: %q has no number", raw)
		}
		return scale(raw, num, suf.scale)
	}
	return scale(raw, lower, 1)
}

func scale(raw, num string, mul uint64) (uint64, error) {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("units: invalid size %q", raw)
	}
	if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("units: invalid size %q", raw)
	}
	v := f * float64(mul)
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("units: size %q is too large", raw)
	}
	return uint64(v), nil
}

var byteUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}

// Bytes formats a byte count with an IEC suffix.
func Bytes(n int64) string {
	if n < 0 {
		return "-" + Bytes(-n)
	}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(byteUnits)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", f, byteUnits[i])
	}
	return fmt.Sprintf("%.1f %s", f, byteUnits[i])
}

// Rate formats a transfer rate in bytes per second.
func Rate(bytesPerSec float64) string {
	if bytesPerSec <= 0 || math.IsNaN(bytesPerSec) || math.IsInf(bytesPerSec, 0) {
		return "0 B/s"
	}
	return Bytes(int64(bytesPerSec)) + "/s"
}

// Duration formats a duration compactly: "45s", "3m12s", "1h04m".
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Percent formats part/total as a percentage, tolerating a zero total.
func Percent(part, total int64) string {
	if total <= 0 {
		return "100%"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(part)/float64(total))
}
