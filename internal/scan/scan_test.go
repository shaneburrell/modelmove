package scan

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
	"github.com/shaneburrell/modelmove/internal/manifest"
)

// modelDir builds a small directory that looks like a Hugging Face checkpoint.
func modelDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "config.json", []byte(`{"model_type":"llama"}`))
	write(t, dir, "tokenizer.json", []byte(`{"vocab":[]}`))
	write(t, dir, "model.safetensors.index.json", []byte(`{"weight_map":{}}`))
	write(t, dir, "model-00001-of-00002.safetensors", randomBytes(200<<10, 1))
	write(t, dir, "model-00002-of-00002.safetensors", randomBytes(200<<10, 2))
	return dir
}

func write(t *testing.T, dir, rel string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestWalkFindsRegularFiles(t *testing.T) {
	dir := modelDir(t)
	entries, err := Walk(Options{Root: dir})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("found %d files, want 5", len(entries))
	}
	// Results must be sorted so that manifests are reproducible.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path >= entries[i].Path {
			t.Fatalf("Walk returned unsorted output: %q then %q", entries[i-1].Path, entries[i].Path)
		}
	}
}

func TestWalkSkipsHiddenAndStateDir(t *testing.T) {
	dir := modelDir(t)
	write(t, dir, ".hidden", []byte("x"))
	write(t, dir, ".git/objects/abc", []byte("x"))
	write(t, dir, manifest.StateDir+"/manifest.json", []byte("{}"))

	entries, err := Walk(Options{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Path == ".hidden" || e.Path == ".git/objects/abc" {
			t.Errorf("Walk included the hidden path %q", e.Path)
		}
		if e.Path == manifest.StateDir+"/manifest.json" {
			t.Errorf("Walk included the state directory")
		}
	}

	withHidden, err := Walk(Options{Root: dir, IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawHidden bool
	for _, e := range withHidden {
		if e.Path == ".hidden" {
			sawHidden = true
		}
		if e.Path == manifest.StateDir+"/manifest.json" {
			t.Error("--include-hidden must still skip the state directory")
		}
	}
	if !sawHidden {
		t.Error("--include-hidden did not include a dot-file")
	}
}

func TestWalkExcludes(t *testing.T) {
	dir := modelDir(t)
	write(t, dir, "extra/notes.txt", []byte("x"))

	entries, err := Walk(Options{Root: dir, Exclude: []string{"*.json", "extra"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Path) == ".json" {
			t.Errorf("exclude *.json missed %q", e.Path)
		}
		if e.Path == "extra/notes.txt" {
			t.Errorf("excluding the directory did not skip %q", e.Path)
		}
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want the 2 safetensors files", len(entries))
	}
}

func TestWalkSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	dir := t.TempDir()
	target := write(t, dir, "real.safetensors", randomBytes(1024, 3))
	if err := os.Symlink(target, filepath.Join(dir, "linked.safetensors")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nope"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}

	followed, err := Walk(Options{Root: dir, FollowSymlinks: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawLink bool
	for _, e := range followed {
		if e.Path == "linked.safetensors" {
			sawLink = true
			if e.Size != 1024 {
				t.Errorf("followed symlink reported size %d, want 1024", e.Size)
			}
		}
		if e.Path == "dangling" {
			t.Error("a dangling symlink should be skipped")
		}
	}
	if !sawLink {
		t.Error("FollowSymlinks did not include the linked file")
	}

	var warnings int
	skipped, err := Walk(Options{Root: dir, FollowSymlinks: false, Warn: func(string, ...any) { warnings++ }})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 {
		t.Errorf("without FollowSymlinks got %d entries, want 1", len(skipped))
	}
	if warnings == 0 {
		t.Error("skipping a symlink should warn")
	}
}

func TestWalkErrors(t *testing.T) {
	if _, err := Walk(Options{Root: filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Error("Walk succeeded on a missing directory")
	}
	file := write(t, t.TempDir(), "f", []byte("x"))
	if _, err := Walk(Options{Root: file}); err == nil {
		t.Error("Walk succeeded on a regular file")
	}
}

func TestChunkOptionsFor(t *testing.T) {
	if got := ChunkOptionsFor(layout.RoleWeight, 100<<20, chunk.Options{}); got != chunk.LargeFileOptions() {
		t.Errorf("a 100 MiB weight got %+v", got)
	}
	if got := ChunkOptionsFor(layout.RoleWeight, 4<<30, chunk.Options{}); got != chunk.HugeFileOptions() {
		t.Errorf("a 4 GiB weight got %+v", got)
	}
	// Config files stay finely chunked no matter what, so a one-line edit
	// does not resend the whole file.
	if got := ChunkOptionsFor(layout.RoleConfig, 4<<30, chunk.Options{}); got != chunk.SmallFileOptions() {
		t.Errorf("a config file got %+v", got)
	}
	pin := chunk.Options{AvgSize: 8192, MinSize: 2048, MaxSize: 32768}
	if got := ChunkOptionsFor(layout.RoleWeight, 4<<30, pin); got != pin {
		t.Errorf("a pinned size was ignored: %+v", got)
	}
}

func TestBuildProducesValidManifest(t *testing.T) {
	dir := modelDir(t)
	m, err := Build(context.Background(), Options{Root: dir, Tool: "test"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Build produced an invalid manifest: %v", err)
	}
	if m.Model.Kind != layout.KindHuggingFace {
		t.Errorf("Kind = %q", m.Model.Kind)
	}
	if m.Model.Files != 5 {
		t.Errorf("Files = %d, want 5", m.Model.Files)
	}
	if m.Model.WeightFiles != 2 {
		t.Errorf("WeightFiles = %d, want 2", m.Model.WeightFiles)
	}
	if len(m.Model.Shards) != 1 {
		t.Errorf("Shards = %+v", m.Model.Shards)
	}
	if m.Model.Digest.IsZero() {
		t.Error("no model digest")
	}
	if m.Tool != "test" {
		t.Errorf("Tool = %q", m.Tool)
	}

	// Every file digest must match a plain hash of its bytes.
	for _, f := range m.Files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if got := chunk.Sum(data); got != f.Digest {
			t.Errorf("%s: digest does not match its contents", f.Path)
		}
		if f.Size != int64(len(data)) {
			t.Errorf("%s: size %d, file is %d", f.Path, f.Size, len(data))
		}
	}
}

func TestBuildIsReproducible(t *testing.T) {
	dir := modelDir(t)
	a, err := Build(context.Background(), Options{Root: dir, Jobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(context.Background(), Options{Root: dir, Jobs: 8})
	if err != nil {
		t.Fatal(err)
	}
	if a.Model.Digest != b.Model.Digest {
		t.Fatal("the model digest depends on the job count")
	}
	for i := range a.Files {
		if len(a.Files[i].Chunks) != len(b.Files[i].Chunks) {
			t.Fatalf("%s: chunk counts differ between runs", a.Files[i].Path)
		}
	}
}

func TestBuildEmptyDirectory(t *testing.T) {
	m, err := Build(context.Background(), Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(m.Files) != 0 {
		t.Errorf("an empty directory produced %d files", len(m.Files))
	}
	if err := m.Validate(); err != nil {
		t.Errorf("an empty manifest should still validate: %v", err)
	}
}

func TestBuildEmptyFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "empty.json", nil)
	m, err := Build(context.Background(), Options{Root: dir})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a zero-byte file broke validation: %v", err)
	}
	if len(m.Files[0].Chunks) != 0 {
		t.Errorf("a zero-byte file produced %d chunks", len(m.Files[0].Chunks))
	}
}

func TestBuildCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, Options{Root: modelDir(t), Jobs: 1}); err == nil {
		t.Fatal("Build ignored a cancelled context")
	}
}

func TestBuildPinnedChunkSize(t *testing.T) {
	dir := modelDir(t)
	pin := chunk.Options{AvgSize: 4096, MinSize: 1024, MaxSize: 16384}
	m, err := Build(context.Background(), Options{Root: dir, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		if f.Chunker.Options() != pin {
			t.Fatalf("%s used %+v instead of the pinned %+v", f.Path, f.Chunker.Options(), pin)
		}
	}
	if m.Chunker.Options() != pin {
		t.Errorf("the manifest header did not record the pinned size")
	}
}
