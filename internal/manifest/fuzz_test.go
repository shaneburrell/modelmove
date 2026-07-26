package manifest

import (
	"bytes"
	"testing"
)

// FuzzDecodeBinary checks that a hostile or corrupt manifest is rejected
// rather than causing a panic or a huge allocation. Binary manifests arrive
// over the network from the other side of a transfer, so this is the parser
// most exposed to untrusted input.
func FuzzDecodeBinary(f *testing.F) {
	var buf bytes.Buffer
	if err := EncodeBinary(&buf, sample()); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add(Magic[:])
	f.Add([]byte("MMM1"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeBinary(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Anything that decodes must survive the structural checks and a
		// re-encode without changing shape.
		if m.Format != Format {
			t.Fatalf("decoded manifest has format %q", m.Format)
		}
		if m.TotalChunks() > 1<<16 {
			return // keep each fuzz execution cheap
		}
		var out bytes.Buffer
		if err := EncodeBinary(&out, m); err != nil {
			t.Fatalf("re-encoding a decoded manifest failed: %v", err)
		}
		again, err := DecodeBinary(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatalf("re-decoding failed: %v", err)
		}
		if len(again.Files) != len(m.Files) {
			t.Fatalf("round trip changed the file count: %d then %d", len(m.Files), len(again.Files))
		}
	})
}
