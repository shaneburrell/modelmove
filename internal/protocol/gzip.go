package protocol

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
)

func gzipBytes(p []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(p); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(p []byte) ([]byte, error) {
	return gunzipBytesLimit(p, MaxFrame)
}

// gunzipBytesLimit decompresses p and rejects output larger than max so a
// small gzip frame cannot expand into an unbounded allocation on the helper.
func gunzipBytesLimit(p []byte, max int64) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(p))
	if err != nil {
		return nil, fmt.Errorf("protocol: gzip manifest: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("protocol: gzip manifest: %w", err)
	}
	if int64(len(out)) > max {
		return nil, fmt.Errorf("protocol: gzip manifest exceeds %d bytes", max)
	}
	return out, nil
}
