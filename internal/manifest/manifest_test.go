package manifest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
)

func digest(seed byte) chunk.Digest {
	var d chunk.Digest
	for i := range d {
		d[i] = seed + byte(i)
	}
	return d
}

// sample builds a small but structurally complete manifest.
func sample() *Manifest {
	m := New("modelmove/test", chunk.SmallFileOptions())
	m.Model.Name = "tiny"
	m.Model.Root = "/models/tiny"
	m.Model.Shards = []layout.ShardSet{{
		Name:  "model.safetensors",
		Parts: []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"},
		Total: 2,
		Size:  300,
	}}
	m.Files = []*File{
		{
			Path: "config.json", Size: 10, Mode: 0o644,
			ModTime: time.Unix(1700000000, 0).UTC(),
			Role:    layout.RoleConfig, Digest: digest(1),
			Chunker: ChunkerFrom(chunk.SmallFileOptions()),
			Chunks:  []Chunk{{Offset: 0, Length: 10, Digest: digest(2)}},
		},
		{
			Path: "model-00001-of-00002.safetensors", Size: 300, Mode: 0o644,
			ModTime: time.Unix(1700000001, 0).UTC(),
			Role:    layout.RoleWeight, Digest: digest(3),
			Chunker: ChunkerFrom(chunk.LargeFileOptions()),
			Chunks: []Chunk{
				{Offset: 0, Length: 100, Digest: digest(4)},
				{Offset: 100, Length: 100, Digest: digest(5)},
				{Offset: 200, Length: 100, Digest: digest(4)}, // repeated chunk
			},
		},
	}
	m.Recount()
	return m
}

func TestValidateAcceptsWellFormed(t *testing.T) {
	if err := sample().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(*Manifest){
		"bad format":        func(m *Manifest) { m.Format = "something-else" },
		"bad version":       func(m *Manifest) { m.Version = 99 },
		"bad hash":          func(m *Manifest) { m.Hash = "sha256" },
		"nil file":          func(m *Manifest) { m.Files = append(m.Files, nil) },
		"duplicate path":    func(m *Manifest) { m.Files = append(m.Files, m.Files[0]) },
		"negative size":     func(m *Manifest) { m.Files[0].Size = -1 },
		"bad chunker":       func(m *Manifest) { m.Files[0].Chunker.Algorithm = "rabin" },
		"gap in chunks":     func(m *Manifest) { m.Files[1].Chunks[1].Offset = 999 },
		"zero length":       func(m *Manifest) { m.Files[0].Chunks[0].Length = 0 },
		"coverage mismatch": func(m *Manifest) { m.Files[0].Size = 11 },
	}
	for name, mutate := range cases {
		m := sample()
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("Validate accepted a manifest with a %s", name)
		}
	}
}

func TestValidatePath(t *testing.T) {
	ok := []string{"a", "a/b", "dir/file.safetensors", "weird name.bin"}
	for _, p := range ok {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{
		"", "/abs", "../escape", "..", "a/../../b", "./a", "a//b", "a/",
		`win\path`, StateDir, StateDir + "/x", "a\x00b",
	}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) accepted a dangerous path", p)
		}
	}
}

func TestTotalsAndIndexes(t *testing.T) {
	m := sample()
	if got := m.TotalBytes(); got != 310 {
		t.Errorf("TotalBytes = %d, want 310", got)
	}
	if got := m.TotalChunks(); got != 4 {
		t.Errorf("TotalChunks = %d, want 4", got)
	}
	// digest(4) appears twice, so only three chunks are unique.
	count, bytes := m.UniqueChunks()
	if count != 3 || bytes != 210 {
		t.Errorf("UniqueChunks = (%d, %d), want (3, 210)", count, bytes)
	}
	if len(m.DigestSet()) != 3 {
		t.Errorf("DigestSet has %d entries, want 3", len(m.DigestSet()))
	}
	if m.Lookup("config.json") == nil {
		t.Error("Lookup missed an existing path")
	}
	if m.Lookup("absent") != nil {
		t.Error("Lookup invented a file")
	}
	if paths := m.Paths(); len(paths) != 2 || paths[0] != "config.json" {
		t.Errorf("Paths = %v", paths)
	}
	if m.Model.WeightFiles != 1 || m.Model.WeightBytes != 300 {
		t.Errorf("Recount got weights wrong: %+v", m.Model)
	}
}

func TestFinalizeSortsAndDigests(t *testing.T) {
	m := sample()
	m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
	info := layout.Detect("/models/tiny", []layout.File{
		{Path: "config.json", Size: 10},
		{Path: "model-00001-of-00002.safetensors", Size: 300},
	})
	m.Finalize(info)

	if m.Files[0].Path != "config.json" {
		t.Error("Finalize did not sort the files")
	}
	if m.Model.Digest.IsZero() {
		t.Error("Finalize did not compute a model digest")
	}
	if m.Model.Files != 2 || m.Model.Size != 310 {
		t.Errorf("Finalize totals = %+v", m.Model)
	}

	// The model digest must change if any file digest changes.
	before := m.Model.Digest
	m.Files[0].Digest = digest(200)
	m.Finalize(info)
	if m.Model.Digest == before {
		t.Error("the model digest ignored a changed file digest")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	m := sample()
	var buf bytes.Buffer
	if err := Write(&buf, m, EncodingJSON); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.Contains(buf.String(), `"format": "modelmove-manifest"`) {
		t.Error("JSON output is missing the format field")
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertSame(t, m, got)
}

func TestBinaryRoundTrip(t *testing.T) {
	m := sample()
	var buf bytes.Buffer
	if err := EncodeBinary(&buf, m); err != nil {
		t.Fatalf("EncodeBinary: %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), Magic[:]) {
		t.Fatal("binary manifest is missing its magic")
	}

	// Read must detect the encoding without being told.
	got, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertSame(t, m, got)

	if got.Model.Files != m.Model.Files || got.Model.Size != m.Model.Size {
		t.Errorf("decoded model totals = %+v, want %+v", got.Model, m.Model)
	}
	if len(got.Model.Shards) != 1 || got.Model.Shards[0].Total != 2 {
		t.Errorf("decoded shards = %+v", got.Model.Shards)
	}
}

// TestBinaryIsCompact guards the reason the binary encoding exists.
func TestBinaryIsCompact(t *testing.T) {
	m := New("t", chunk.SmallFileOptions())
	f := &File{Path: "big.safetensors", Role: layout.RoleWeight, Chunker: ChunkerFrom(chunk.LargeFileOptions())}
	for i := 0; i < 5000; i++ {
		f.Chunks = append(f.Chunks, Chunk{Offset: uint64(i) * 1024, Length: 1024, Digest: digest(byte(i))})
	}
	f.Size = int64(len(f.Chunks)) * 1024
	m.Files = []*File{f}
	m.Recount()

	var jsonBuf, binBuf bytes.Buffer
	if err := Write(&jsonBuf, m, EncodingJSON); err != nil {
		t.Fatal(err)
	}
	if err := Write(&binBuf, m, EncodingBinary); err != nil {
		t.Fatal(err)
	}
	if binBuf.Len() >= jsonBuf.Len()/2 {
		t.Errorf("binary is %d bytes vs JSON %d; expected less than half", binBuf.Len(), jsonBuf.Len())
	}
}

func TestBinaryDetectsCorruption(t *testing.T) {
	m := sample()
	var buf bytes.Buffer
	if err := EncodeBinary(&buf, m); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	corrupt := append([]byte(nil), raw...)
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := DecodeBinary(bytes.NewReader(corrupt)); err == nil {
		t.Error("DecodeBinary accepted a corrupt body")
	}

	truncated := raw[:len(raw)-5]
	if _, err := DecodeBinary(bytes.NewReader(truncated)); err == nil {
		t.Error("DecodeBinary accepted a truncated manifest")
	}

	if _, err := DecodeBinary(bytes.NewReader([]byte("not a manifest"))); !errors.Is(err, ErrBadMagic) {
		t.Errorf("bad magic error = %v, want ErrBadMagic", err)
	}

	badVersion := append([]byte(nil), raw...)
	badVersion[4] = 99
	if _, err := DecodeBinary(bytes.NewReader(badVersion)); err == nil {
		t.Error("DecodeBinary accepted an unknown version")
	}
}

func TestReadRejectsGarbageJSON(t *testing.T) {
	if _, err := Read(strings.NewReader("{not json")); err == nil {
		t.Error("Read accepted invalid JSON")
	}
}

func TestEncodingHelpers(t *testing.T) {
	cases := map[string]Encoding{
		"": EncodingAuto, "auto": EncodingAuto,
		"json": EncodingJSON, "JSON": EncodingJSON,
		"binary": EncodingBinary, "bin": EncodingBinary, "mmm": EncodingBinary,
	}
	for in, want := range cases {
		got, err := ParseEncoding(in)
		if err != nil || got != want {
			t.Errorf("ParseEncoding(%q) = (%v, %v)", in, got, err)
		}
	}
	if _, err := ParseEncoding("yaml"); err == nil {
		t.Error("ParseEncoding accepted yaml")
	}
	if EncodingForPath("m.mmm") != EncodingBinary || EncodingForPath("m.json") != EncodingJSON {
		t.Error("EncodingForPath guessed wrong")
	}
	if EncodingJSON.String() != "json" || EncodingBinary.String() != "binary" || EncodingAuto.String() != "auto" {
		t.Error("Encoding.String is wrong")
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	m := sample()

	jsonPath := filepath.Join(dir, "nested", "model.json")
	if err := Save(jsonPath, m, EncodingAuto); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(jsonPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertSame(t, m, loaded)

	binPath := filepath.Join(dir, "model.mmm")
	if err := Save(binPath, m, EncodingAuto); err != nil {
		t.Fatalf("Save binary: %v", err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, Magic[:]) {
		t.Error("Save did not pick the binary encoding from the .mmm extension")
	}

	// No temporary files must be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("Save left a temporary file: %s", e.Name())
		}
	}

	if _, err := Load(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("Load succeeded on a missing file")
	}
}

func TestStatePathAndLoadApplied(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, StateDir, ManifestName)
	if got := StatePath(dir, ManifestName); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
	if _, err := LoadApplied(dir); err == nil {
		t.Error("LoadApplied succeeded on a directory with no manifest")
	}
	if err := Save(want, sample(), EncodingJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApplied(dir); err != nil {
		t.Errorf("LoadApplied: %v", err)
	}
}

func TestChunkerConversion(t *testing.T) {
	o := chunk.LargeFileOptions()
	c := ChunkerFrom(o)
	if c.Algorithm != chunk.ChunkerAlgorithm {
		t.Errorf("Algorithm = %q", c.Algorithm)
	}
	if c.Options() != o {
		t.Error("Chunker round trip changed the options")
	}
	f := &File{Chunker: c}
	if f.Options() != o {
		t.Error("File.Options is wrong")
	}
	if got := (Chunk{Offset: 5, Length: 7}).End(); got != 12 {
		t.Errorf("Chunk.End = %d, want 12", got)
	}
}

func assertSame(t *testing.T, want, got *Manifest) {
	t.Helper()
	if got.Format != want.Format || got.Version != want.Version || got.Hash != want.Hash {
		t.Errorf("header differs: %+v", got)
	}
	if got.Tool != want.Tool {
		t.Errorf("Tool = %q, want %q", got.Tool, want.Tool)
	}
	if got.Chunker != want.Chunker {
		t.Errorf("Chunker = %+v, want %+v", got.Chunker, want.Chunker)
	}
	if got.Model.Name != want.Model.Name || got.Model.Digest != want.Model.Digest {
		t.Errorf("Model = %+v, want %+v", got.Model, want.Model)
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("got %d files, want %d", len(got.Files), len(want.Files))
	}
	for i := range want.Files {
		a, b := want.Files[i], got.Files[i]
		if a.Path != b.Path || a.Size != b.Size || a.Mode != b.Mode || a.Role != b.Role || a.Digest != b.Digest {
			t.Errorf("file %d differs:\n want %+v\n got  %+v", i, a, b)
		}
		if !a.ModTime.Equal(b.ModTime) {
			t.Errorf("file %d mod time %v, want %v", i, b.ModTime, a.ModTime)
		}
		if a.Chunker != b.Chunker {
			t.Errorf("file %d chunker = %+v, want %+v", i, b.Chunker, a.Chunker)
		}
		if len(a.Chunks) != len(b.Chunks) {
			t.Fatalf("file %d has %d chunks, want %d", i, len(b.Chunks), len(a.Chunks))
		}
		for j := range a.Chunks {
			if a.Chunks[j] != b.Chunks[j] {
				t.Errorf("file %d chunk %d = %+v, want %+v", i, j, b.Chunks[j], a.Chunks[j])
			}
		}
	}
}
