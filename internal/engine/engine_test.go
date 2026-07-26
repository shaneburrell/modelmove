package engine

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
)

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func write(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "config.json", []byte(`{"model_type":"llama"}`))
	write(t, dir, "tokenizer.json", []byte(`{"vocab":[]}`))
	write(t, dir, "model-00001-of-00002.safetensors", randomBytes(250<<10, 1))
	write(t, dir, "model-00002-of-00002.safetensors", randomBytes(250<<10, 2))
	return dir
}

func baseConfig(src, dst string) Config {
	return Config{
		Source:         src,
		Dest:           dst,
		FollowSymlinks: true,
		Resume:         true,
		Verify:         true,
		Dedupe:         true,
		Atomic:         receiver.AtomicFile,
		PreserveTimes:  true,
		Tool:           "modelmove/test",
		Jobs:           2,
	}
}

func TestRunCopiesEverything(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	res, err := Run(context.Background(), baseConfig(src, dst))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Model.Kind != layout.KindHuggingFace {
		t.Errorf("Kind = %q", res.Model.Kind)
	}
	if res.Model.Files != 4 {
		t.Errorf("Files = %d, want 4", res.Model.Files)
	}
	if res.Plan.CopyFiles != 4 {
		t.Errorf("CopyFiles = %d, want 4", res.Plan.CopyFiles)
	}
	if res.Summary.FilesWritten != 4 {
		t.Errorf("FilesWritten = %d, want 4", res.Summary.FilesWritten)
	}
	if res.Manifests() == nil {
		t.Error("Manifests() returned nil")
	}
	if res.ScanSeconds <= 0 || res.Seconds <= 0 {
		t.Error("timings were not recorded")
	}
	if res.TransferRate() <= 0 {
		t.Error("TransferRate is zero")
	}
	for _, rel := range []string{"config.json", "model-00001-of-00002.safetensors"} {
		if string(read(t, dst, rel)) != string(read(t, src, rel)) {
			t.Errorf("%s does not match the source", rel)
		}
	}
}

func TestRunSparseUpdate(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}

	data := read(t, src, "model-00002-of-00002.safetensors")
	copy(data[120<<10:], []byte("FINE-TUNED"))
	write(t, src, "model-00002-of-00002.safetensors", data)

	res, err := Run(context.Background(), baseConfig(src, dst))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Plan.UpdateFiles != 1 || res.Plan.SkipFiles != 3 {
		t.Errorf("plan = %d updated, %d skipped", res.Plan.UpdateFiles, res.Plan.SkipFiles)
	}
	if res.Summary.BytesReceived >= res.Plan.TotalBytes/4 {
		t.Errorf("moved %d of %d bytes for a 10-byte edit",
			res.Summary.BytesReceived, res.Plan.TotalBytes)
	}
	if res.Summary.Savings() < 0.7 {
		t.Errorf("Savings = %.2f, want most of the model to be reused", res.Summary.Savings())
	}
	if string(read(t, dst, "model-00002-of-00002.safetensors")) != string(data) {
		t.Fatal("the destination file is wrong after the sparse update")
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	cfg := baseConfig(src, dst)
	cfg.DryRun = true
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.DryRun {
		t.Error("DryRun was not recorded in the result")
	}
	if res.Summary != nil {
		t.Error("a dry run should not produce a summary")
	}
	if res.Plan.NeedBytes == 0 {
		t.Error("the plan should still say what would move")
	}
	if _, err := os.Stat(filepath.Join(dst, "config.json")); !os.IsNotExist(err) {
		t.Error("a dry run wrote a file")
	}
}

func TestRunWritesManifest(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	out := filepath.Join(t.TempDir(), "model.mmm")

	cfg := baseConfig(src, dst)
	cfg.ManifestOut = out
	cfg.ManifestFormat = manifest.EncodingAuto
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest != out {
		t.Errorf("Manifest = %q, want %q", res.Manifest, out)
	}
	loaded, err := manifest.Load(out)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Files) != 4 {
		t.Errorf("the written manifest has %d files", len(loaded.Files))
	}
}

func TestRunDeleteRemovesExtras(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}
	write(t, dst, "obsolete.safetensors", randomBytes(1024, 9))

	cfg := baseConfig(src, dst)
	cfg.Delete = true
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", res.Summary.FilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "obsolete.safetensors")); !os.IsNotExist(err) {
		t.Error("the extra file survived --delete")
	}
}

func TestRunExcludes(t *testing.T) {
	src := sourceDir(t)
	write(t, src, "notes.md", []byte("hello"))
	dst := filepath.Join(t.TempDir(), "out")

	cfg := baseConfig(src, dst)
	cfg.Exclude = []string{"*.md"}
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model.Files != 4 {
		t.Errorf("Files = %d, want 4 after excluding the markdown file", res.Model.Files)
	}
	if _, err := os.Stat(filepath.Join(dst, "notes.md")); !os.IsNotExist(err) {
		t.Error("an excluded file was transferred")
	}
}

func TestRunRejectsBadEndpoints(t *testing.T) {
	src := sourceDir(t)
	dst := t.TempDir()

	cases := map[string]Config{
		"remote source":      {Source: "host:/models", Dest: dst},
		"same directory":     {Source: src, Dest: src},
		"dest inside source": {Source: src, Dest: filepath.Join(src, "inner")},
		"source inside dest": {Source: filepath.Join(dst, "inner"), Dest: dst},
		"empty source":       {Source: "", Dest: dst},
		"empty dest":         {Source: src, Dest: ""},
		"missing source":     {Source: filepath.Join(dst, "absent"), Dest: filepath.Join(dst, "out")},
	}
	for name, cfg := range cases {
		cfg.Tool = "test"
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Errorf("Run accepted %s", name)
		}
	}
}

func TestRunRejectsEmptySource(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	_, err := Run(context.Background(), baseConfig(src, dst))
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Fatalf("error = %v, want a complaint about an empty source", err)
	}
}

func TestRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(ctx, baseConfig(src, dst)); err == nil {
		t.Fatal("Run ignored a cancelled context")
	}
}

func TestBuildManifest(t *testing.T) {
	src := sourceDir(t)
	m, err := BuildManifest(context.Background(), Config{Source: src, Tool: "test", FollowSymlinks: true})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("invalid manifest: %v", err)
	}
	if len(m.Files) != 4 {
		t.Errorf("got %d files, want 4", len(m.Files))
	}
	if _, err := BuildManifest(context.Background(), Config{Source: "host:/remote"}); err == nil {
		t.Error("BuildManifest accepted a remote source")
	}
	if _, err := BuildManifest(context.Background(), Config{Source: ""}); err == nil {
		t.Error("BuildManifest accepted an empty source")
	}
}

func TestVerifyPasses(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(context.Background(), VerifyConfig{Root: dst, Jobs: 2})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("verification failed: %+v", res.Problems)
	}
	if res.FilesOK != 4 {
		t.Errorf("FilesOK = %d, want 4", res.FilesOK)
	}
	if res.BytesChecked == 0 {
		t.Error("no bytes were read")
	}
	if !strings.HasSuffix(res.ManifestPath, manifest.ManifestName) {
		t.Errorf("ManifestPath = %q", res.ManifestPath)
	}
}

func TestVerifyFindsCorruption(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}

	data := read(t, dst, "model-00001-of-00002.safetensors")
	copy(data[80<<10:], []byte("CORRUPTED"))
	write(t, dst, "model-00001-of-00002.safetensors", data)

	res, err := Verify(context.Background(), VerifyConfig{Root: dst, Jobs: 2})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("Verify missed a corrupted file")
	}
	if len(res.Problems) != 1 {
		t.Fatalf("got %d problems, want 1", len(res.Problems))
	}
	p := res.Problems[0]
	if p.Problem != ProblemDigest {
		t.Errorf("Problem = %q", p.Problem)
	}
	if len(p.BadChunks) == 0 {
		t.Fatal("no bad chunks were located")
	}
	// The reported range must actually contain the damage.
	bad := p.BadChunks[0]
	if bad.Offset > 80<<10 || bad.Offset+uint64(bad.Length) < 80<<10 {
		t.Errorf("bad chunk at [%d,%d) does not cover the edit at %d",
			bad.Offset, bad.Offset+uint64(bad.Length), 80<<10)
	}
	if p.BadBytes == 0 {
		t.Error("BadBytes is zero")
	}
}

func TestVerifyQuickSkipsChunkDetail(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}
	data := read(t, dst, "model-00001-of-00002.safetensors")
	copy(data[10<<10:], []byte("X"))
	write(t, dst, "model-00001-of-00002.safetensors", data)

	res, err := Verify(context.Background(), VerifyConfig{Root: dst, Quick: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("Verify missed the corruption")
	}
	if len(res.Problems[0].BadChunks) != 0 {
		t.Error("--quick should not locate individual chunks")
	}
}

func TestVerifyDetectsMissingAndResized(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dst, "config.json")); err != nil {
		t.Fatal(err)
	}
	write(t, dst, "tokenizer.json", []byte("truncated"))

	res, err := Verify(context.Background(), VerifyConfig{Root: dst})
	if err != nil {
		t.Fatal(err)
	}
	found := map[Problem]bool{}
	for _, p := range res.Problems {
		found[p.Problem] = true
	}
	if !found[ProblemMissing] {
		t.Error("a deleted file was not reported as missing")
	}
	if !found[ProblemSize] {
		t.Error("a resized file was not reported")
	}
}

func TestVerifyExtraFiles(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Run(context.Background(), baseConfig(src, dst)); err != nil {
		t.Fatal(err)
	}
	write(t, dst, "stowaway.bin", []byte("surprise"))

	res, err := Verify(context.Background(), VerifyConfig{Root: dst, Extra: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("an extra file should fail verification when --extra is set")
	}
	if len(res.Extra) != 1 || res.Extra[0] != "stowaway.bin" {
		t.Errorf("Extra = %v", res.Extra)
	}

	// Without --extra the same directory passes.
	res, err = Verify(context.Background(), VerifyConfig{Root: dst})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Error("an extra file should be ignored without --extra")
	}
}

func TestVerifyWithExplicitManifest(t *testing.T) {
	src := sourceDir(t)
	out := filepath.Join(t.TempDir(), "model.json")
	m, err := BuildManifest(context.Background(), Config{Source: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(out, m, manifest.EncodingJSON); err != nil {
		t.Fatal(err)
	}
	res, err := Verify(context.Background(), VerifyConfig{Root: src, ManifestPath: out})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Errorf("verifying the source against its own manifest failed: %+v", res.Problems)
	}
}

func TestVerifyWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := Verify(context.Background(), VerifyConfig{Root: dir}); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("error = %v, want ErrNoManifest", err)
	}
	if _, err := Verify(context.Background(), VerifyConfig{Root: dir, ManifestPath: "absent.json"}); err == nil {
		t.Error("Verify accepted a missing manifest file")
	}
}

func TestVerifyRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "file.txt", []byte("x"))
	m := manifest.New("t", chunk.SmallFileOptions())
	out := filepath.Join(dir, "m.json")
	if err := manifest.Save(out, m, manifest.EncodingJSON); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(context.Background(), VerifyConfig{
		Root: filepath.Join(dir, "file.txt"), ManifestPath: out,
	})
	if err == nil {
		t.Fatal("Verify accepted a regular file as the model directory")
	}
}
