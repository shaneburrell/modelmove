package receiver

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/scan"
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

func buildManifest(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	m, err := scan.Build(context.Background(), scan.Options{Root: dir, Tool: "test"})
	if err != nil {
		t.Fatalf("scan.Build: %v", err)
	}
	return m
}

// apply drives a receiver the way the engine does, reading needed chunks out
// of the source directory. It returns the plan and the summary.
func apply(t *testing.T, src, dst string, opt Options) (*Plan, *Summary) {
	t.Helper()
	m := buildManifest(t, src)
	return applyManifest(t, m, src, dst, opt)
}

func applyManifest(t *testing.T, m *manifest.Manifest, src, dst string, opt Options) (*Plan, *Summary) {
	t.Helper()
	opt.Root = dst
	r, err := New(opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plan, err := r.Plan(context.Background(), m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, fp := range plan.Files {
		if fp.Action == ActionSkip {
			continue
		}
		f := m.Lookup(fp.Path)
		if err := r.BeginFile(fp.Path); err != nil {
			t.Fatalf("BeginFile %s: %v", fp.Path, err)
		}
		srcFile, err := os.Open(filepath.Join(src, filepath.FromSlash(fp.Path)))
		if err != nil {
			t.Fatal(err)
		}
		where := map[chunk.Digest]manifest.Chunk{}
		for _, c := range f.Chunks {
			if _, ok := where[c.Digest]; !ok {
				where[c.Digest] = c
			}
		}
		for _, d := range fp.Need {
			c := where[d]
			buf := make([]byte, c.Length)
			if _, err := srcFile.ReadAt(buf, int64(c.Offset)); err != nil {
				t.Fatal(err)
			}
			if err := r.Chunk(d, buf); err != nil {
				t.Fatalf("Chunk: %v", err)
			}
		}
		srcFile.Close()
		if _, err := r.EndFile(); err != nil {
			t.Fatalf("EndFile %s: %v", fp.Path, err)
		}
	}
	sum, err := r.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return plan, sum
}

func defaults(dst string) Options {
	o := DefaultOptions(dst)
	o.Jobs = 2
	return o
}

func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "config.json", []byte(`{"model_type":"llama"}`))
	write(t, dir, "model-00001-of-00002.safetensors", randomBytes(300<<10, 1))
	write(t, dir, "model-00002-of-00002.safetensors", randomBytes(300<<10, 2))
	return dir
}

func TestApplyToEmptyDestination(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	plan, sum := apply(t, src, dst, defaults(dst))
	if plan.CopyFiles != 3 {
		t.Errorf("CopyFiles = %d, want 3", plan.CopyFiles)
	}
	if plan.ReuseBytes != 0 {
		t.Errorf("an empty destination should reuse nothing, got %d", plan.ReuseBytes)
	}
	if sum.FilesWritten != 3 {
		t.Errorf("FilesWritten = %d, want 3", sum.FilesWritten)
	}
	if sum.BytesReused != 0 {
		t.Errorf("BytesReused = %d, want 0", sum.BytesReused)
	}

	for _, rel := range []string{"config.json", "model-00001-of-00002.safetensors"} {
		if string(read(t, dst, rel)) != string(read(t, src, rel)) {
			t.Errorf("%s does not match the source", rel)
		}
	}
	// The applied manifest is recorded so that verify works with no arguments.
	if _, err := manifest.LoadApplied(dst); err != nil {
		t.Errorf("no manifest recorded at the destination: %v", err)
	}
}

func TestSecondApplyIsANoOp(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	plan, sum := apply(t, src, dst, defaults(dst))
	if plan.SkipFiles != 3 {
		t.Errorf("SkipFiles = %d, want 3", plan.SkipFiles)
	}
	if plan.NeedBytes != 0 {
		t.Errorf("NeedBytes = %d, want 0", plan.NeedBytes)
	}
	if sum.BytesReceived != 0 {
		t.Errorf("BytesReceived = %d, want 0", sum.BytesReceived)
	}
	if got := plan.Savings(); got != 1 {
		t.Errorf("Savings = %v, want 1", got)
	}
}

// TestSparseUpdate is the core promise: changing a few bytes of a weight file
// must move only the chunks that changed.
func TestSparseUpdate(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	data := read(t, src, "model-00001-of-00002.safetensors")
	copy(data[150<<10:], []byte("CHANGED"))
	write(t, src, "model-00001-of-00002.safetensors", data)

	plan, sum := apply(t, src, dst, defaults(dst))
	if plan.UpdateFiles != 1 {
		t.Errorf("UpdateFiles = %d, want 1", plan.UpdateFiles)
	}
	total := plan.TotalBytes
	if plan.NeedBytes > total/4 {
		t.Errorf("a 7-byte edit moved %d of %d bytes", plan.NeedBytes, total)
	}
	if sum.BytesReused == 0 {
		t.Error("no chunks were reused from the destination")
	}
	if string(read(t, dst, "model-00001-of-00002.safetensors")) != string(data) {
		t.Fatal("the updated file does not match the source")
	}
}

// TestCrossFileDedupe checks that a file copied to a new name costs nothing.
func TestCrossFileDedupe(t *testing.T) {
	src := t.TempDir()
	payload := randomBytes(400<<10, 3)
	write(t, src, "model-00001-of-00001.safetensors", payload)

	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	// Re-shard: same bytes, different filename.
	os.Remove(filepath.Join(src, "model-00001-of-00001.safetensors"))
	write(t, src, "model-00001-of-00002.safetensors", payload)

	plan, sum := apply(t, src, dst, defaults(dst))
	if plan.NeedBytes != 0 {
		t.Errorf("a rename cost %d bytes, should cost 0", plan.NeedBytes)
	}
	if sum.BytesReused != int64(len(payload)) {
		t.Errorf("BytesReused = %d, want %d", sum.BytesReused, len(payload))
	}
}

func TestNoDedupeDisablesCrossFileReuse(t *testing.T) {
	src := t.TempDir()
	payload := randomBytes(400<<10, 4)
	write(t, src, "a.safetensors", payload)

	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	os.Remove(filepath.Join(src, "a.safetensors"))
	write(t, src, "b.safetensors", payload)

	opt := defaults(dst)
	opt.Dedupe = false
	plan, _ := apply(t, src, dst, opt)
	if plan.NeedBytes != int64(len(payload)) {
		t.Errorf("with --no-dedupe the rename should cost the whole file, got %d", plan.NeedBytes)
	}
}

func TestDeleteRemovesExtraFiles(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))
	write(t, dst, "stale/leftover.safetensors", randomBytes(1024, 5))

	opt := defaults(dst)
	opt.Delete = true
	plan, sum := apply(t, src, dst, opt)

	if len(plan.Deletes) != 1 || plan.Deletes[0] != "stale/leftover.safetensors" {
		t.Fatalf("Deletes = %v", plan.Deletes)
	}
	if sum.FilesDeleted != 1 {
		t.Errorf("FilesDeleted = %d, want 1", sum.FilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Error("the emptied directory should have been pruned")
	}
}

func TestDeleteOffKeepsExtraFiles(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))
	write(t, dst, "keep.txt", []byte("mine"))

	_, sum := apply(t, src, dst, defaults(dst))
	if sum.FilesDeleted != 0 {
		t.Errorf("FilesDeleted = %d without --delete", sum.FilesDeleted)
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Error("an unrelated file was removed")
	}
}

func TestAtomicModelDefersReplacement(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	// Change every weight file, then apply with whole-directory atomicity.
	write(t, src, "model-00001-of-00002.safetensors", randomBytes(300<<10, 11))
	write(t, src, "model-00002-of-00002.safetensors", randomBytes(300<<10, 12))

	opt := defaults(dst)
	opt.Atomic = AtomicModel
	_, sum := apply(t, src, dst, opt)
	if sum.FilesWritten != 2 {
		t.Errorf("FilesWritten = %d, want 2", sum.FilesWritten)
	}
	for _, rel := range []string{"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors"} {
		if string(read(t, dst, rel)) != string(read(t, src, rel)) {
			t.Errorf("%s was not committed", rel)
		}
	}
	// No staging files should survive a clean run.
	if _, err := os.Stat(filepath.Join(dst, manifest.StateDir, "stage")); !os.IsNotExist(err) {
		t.Error("the staging directory was left behind")
	}
}

// TestResumeReusesStagedChunks simulates an interrupted transfer by writing a
// partial staging file, then checks the next run does not re-send it.
func TestResumeReusesStagedChunks(t *testing.T) {
	src := t.TempDir()
	payload := randomBytes(400<<10, 6)
	write(t, src, "model.safetensors", payload)
	m := buildManifest(t, src)

	dst := filepath.Join(t.TempDir(), "out")
	stage := filepath.Join(dst, manifest.StateDir, "stage", "model.safetensors.part")
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		t.Fatal(err)
	}
	// A staging file with the right size and the first half already written.
	partial := make([]byte, len(payload))
	f := m.Files[0]
	var filled int
	for _, c := range f.Chunks {
		if filled >= len(payload)/2 {
			break
		}
		copy(partial[c.Offset:], payload[c.Offset:c.End()])
		filled += int(c.Length)
	}
	if err := os.WriteFile(stage, partial, 0o644); err != nil {
		t.Fatal(err)
	}

	plan, sum := applyManifest(t, m, src, dst, defaults(dst))
	if plan.NeedBytes >= int64(len(payload)) {
		t.Errorf("resume sent %d of %d bytes; the staged half should have been kept", plan.NeedBytes, len(payload))
	}
	if sum.BytesReused == 0 {
		t.Error("resume reused nothing")
	}
	if string(read(t, dst, "model.safetensors")) != string(payload) {
		t.Fatal("the resumed file does not match the source")
	}
}

func TestResumeDisabledIgnoresStage(t *testing.T) {
	src := t.TempDir()
	payload := randomBytes(200<<10, 7)
	write(t, src, "model.safetensors", payload)
	m := buildManifest(t, src)

	dst := filepath.Join(t.TempDir(), "out")
	stage := filepath.Join(dst, manifest.StateDir, "stage", "model.safetensors.part")
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	opt := defaults(dst)
	opt.Resume = false
	plan, _ := applyManifest(t, m, src, dst, opt)
	if plan.NeedBytes != int64(len(payload)) {
		t.Errorf("--no-resume sent %d bytes, want the whole %d", plan.NeedBytes, len(payload))
	}
}

func TestFastTrustsSizeAndModTime(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	opt := defaults(dst)
	opt.Fast = true
	plan, _ := apply(t, src, dst, opt)
	if plan.ScannedDest != 0 {
		t.Errorf("--fast still read %d bytes from the destination", plan.ScannedDest)
	}
	if plan.SkipFiles != 3 {
		t.Errorf("SkipFiles = %d, want 3", plan.SkipFiles)
	}
}

func TestVerificationCatchesBadChunk(t *testing.T) {
	src := t.TempDir()
	write(t, src, "model.safetensors", randomBytes(100<<10, 8))
	m := buildManifest(t, src)

	dst := filepath.Join(t.TempDir(), "out")
	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Plan(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	fp := plan.Files[0]
	if err := r.BeginFile(fp.Path); err != nil {
		t.Fatal(err)
	}
	// Send every chunk but with the wrong payload for one of them.
	f := m.Lookup(fp.Path)
	srcData := read(t, src, "model.safetensors")
	for i, c := range f.Chunks {
		payload := srcData[c.Offset:c.End()]
		if i == 0 {
			bad := append([]byte(nil), payload...)
			bad[0] ^= 0xff
			if err := r.Chunk(c.Digest, bad); err == nil {
				t.Fatal("Chunk accepted a payload that does not match its digest")
			}
			// Send the correct bytes so the rest of the file can proceed.
			payload = srcData[c.Offset:c.End()]
		}
		if err := r.Chunk(c.Digest, payload); err != nil {
			t.Fatalf("Chunk: %v", err)
		}
	}
	res, err := r.EndFile()
	if err != nil {
		t.Fatalf("EndFile: %v", err)
	}
	if !res.Verified {
		t.Error("the result should be marked verified")
	}
	if _, err := r.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestChunkRejectsUnknownDigest(t *testing.T) {
	src := t.TempDir()
	write(t, src, "model.safetensors", randomBytes(50<<10, 9))
	m := buildManifest(t, src)

	dst := filepath.Join(t.TempDir(), "out")
	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Plan(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk(chunk.Sum([]byte("x")), []byte("x")); err == nil {
		t.Error("Chunk succeeded with no file open")
	}
	if err := r.BeginFile("model.safetensors"); err != nil {
		t.Fatal(err)
	}
	if err := r.Chunk(chunk.Sum([]byte("stranger")), []byte("stranger")); err == nil {
		t.Error("Chunk accepted a digest that is not part of the file")
	}
	r.Abort()
}

func TestMissingChunkFailsCleanly(t *testing.T) {
	src := t.TempDir()
	write(t, src, "model.safetensors", randomBytes(100<<10, 10))
	m := buildManifest(t, src)

	dst := filepath.Join(t.TempDir(), "out")
	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Plan(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginFile("model.safetensors"); err != nil {
		t.Fatal(err)
	}
	// Close the file without sending anything.
	res, err := r.EndFile()
	if err == nil {
		t.Fatal("EndFile succeeded without any chunks")
	}
	if res.Status != "failed" {
		t.Errorf("Status = %q, want failed", res.Status)
	}
	if _, statErr := os.Stat(filepath.Join(dst, "model.safetensors")); !os.IsNotExist(statErr) {
		t.Error("a failed file must not be committed")
	}
}

func TestBeginFileRejects(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	m := buildManifest(t, src)

	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.BeginFile("config.json"); err == nil {
		t.Error("BeginFile succeeded before Plan")
	}
	if _, err := r.Plan(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginFile("../escape"); err == nil {
		t.Error("BeginFile accepted a path outside the root")
	}
	if err := r.BeginFile("not-in-manifest"); err == nil {
		t.Error("BeginFile accepted an unknown path")
	}
	if err := r.BeginFile("config.json"); err != nil {
		t.Fatal(err)
	}
	if err := r.BeginFile("config.json"); err == nil {
		t.Error("BeginFile succeeded while another file was open")
	}
	if _, err := r.Finish(); err == nil {
		t.Error("Finish succeeded with a file still open")
	}
	r.Abort()
}

func TestPlanRejectsInvalidManifest(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out")
	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	bad := manifest.New("t", chunk.SmallFileOptions())
	bad.Files = []*manifest.File{{Path: "../escape", Size: 0}}
	if _, err := r.Plan(context.Background(), bad); err == nil {
		t.Fatal("Plan accepted a manifest with an escaping path")
	}
}

func TestNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted an empty root")
	}
}

func TestParseAtomicMode(t *testing.T) {
	for _, s := range []string{"", "file"} {
		if got, err := ParseAtomicMode(s); err != nil || got != AtomicFile {
			t.Errorf("ParseAtomicMode(%q) = (%v, %v)", s, got, err)
		}
	}
	for _, s := range []string{"model", "dir", "ALL"} {
		if got, err := ParseAtomicMode(s); err != nil || got != AtomicModel {
			t.Errorf("ParseAtomicMode(%q) = (%v, %v)", s, got, err)
		}
	}
	if _, err := ParseAtomicMode("sometimes"); err == nil {
		t.Error("ParseAtomicMode accepted a bogus mode")
	}
}

func TestPreservesModeAndTimes(t *testing.T) {
	src := t.TempDir()
	write(t, src, "run.sh", []byte("#!/bin/sh\n"))
	if err := os.Chmod(filepath.Join(src, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))

	srcInfo, err := os.Stat(filepath.Join(src, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
		t.Errorf("mode = %v, want %v", dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
	}
	if !dstInfo.ModTime().Equal(srcInfo.ModTime().UTC()) {
		t.Errorf("mod time = %v, want %v", dstInfo.ModTime(), srcInfo.ModTime())
	}
}

func TestPlanWorkAndRoot(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	m := buildManifest(t, src)
	r, err := New(defaults(dst))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Plan(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Work()) != 3 {
		t.Errorf("Work() returned %d files, want 3", len(plan.Work()))
	}
	if !filepath.IsAbs(r.Root()) {
		t.Errorf("Root() = %q, want an absolute path", r.Root())
	}
	if len(r.SortedResults()) != 0 {
		t.Error("SortedResults should be empty before anything is written")
	}
	r.Abort()
}

func TestNestedDirectoriesAreCreated(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a/b/c/deep.safetensors", randomBytes(2048, 13))
	dst := filepath.Join(t.TempDir(), "out")
	apply(t, src, dst, defaults(dst))
	if string(read(t, dst, "a/b/c/deep.safetensors")) != string(read(t, src, "a/b/c/deep.safetensors")) {
		t.Error("the nested file was not written correctly")
	}
}
