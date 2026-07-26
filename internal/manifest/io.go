package manifest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Encoding selects the manifest wire format.
type Encoding int

// Supported encodings.
const (
	// EncodingAuto infers the encoding from the file extension when writing
	// and from the leading bytes when reading.
	EncodingAuto Encoding = iota
	EncodingJSON
	EncodingBinary
)

func (e Encoding) String() string {
	switch e {
	case EncodingBinary:
		return "binary"
	case EncodingJSON:
		return "json"
	default:
		return "auto"
	}
}

// ParseEncoding maps a user-supplied name to an Encoding.
func ParseEncoding(s string) (Encoding, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return EncodingAuto, nil
	case "json":
		return EncodingJSON, nil
	case "binary", "bin", "mmm":
		return EncodingBinary, nil
	default:
		return EncodingAuto, fmt.Errorf("manifest: unknown format %q (want json or binary)", s)
	}
}

// EncodingForPath guesses an encoding from a filename extension.
func EncodingForPath(p string) Encoding {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mmm", ".bin", ".manifest":
		return EncodingBinary
	default:
		return EncodingJSON
	}
}

// Write encodes m to w.
func Write(w io.Writer, m *Manifest, enc Encoding) error {
	if enc == EncodingBinary {
		return EncodeBinary(w, m)
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	e := json.NewEncoder(bw)
	e.SetIndent("", "  ")
	if err := e.Encode(m); err != nil {
		return err
	}
	return bw.Flush()
}

// Read decodes a manifest, detecting the encoding from the leading bytes.
func Read(r io.Reader) (*Manifest, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	if magic, err := br.Peek(len(Magic)); err == nil && string(magic) == string(Magic[:]) {
		return DecodeBinary(br)
	}
	var m Manifest
	if err := json.NewDecoder(br).Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: decode: %w", err)
	}
	return &m, nil
}

// Save writes m to path, creating parent directories. A path of "-" writes to
// stdout. The file is written to a temporary name and renamed so that a
// crashed run never leaves a half-written manifest behind.
func Save(path string, m *Manifest, enc Encoding) error {
	if enc == EncodingAuto {
		enc = EncodingForPath(path)
	}
	if path == "-" {
		return Write(os.Stdout, m, enc)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := Write(tmp, m, enc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Load reads a manifest from path, or from stdin when path is "-".
func Load(path string) (*Manifest, error) {
	if path == "-" {
		return Read(os.Stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// StatePath returns the path of a file inside a destination's state directory.
func StatePath(root string, parts ...string) string {
	return filepath.Join(append([]string{root, StateDir}, parts...)...)
}

// LoadApplied reads the manifest modelmove recorded in a destination root, if
// one is present.
func LoadApplied(root string) (*Manifest, error) {
	return Load(StatePath(root, ManifestName))
}
