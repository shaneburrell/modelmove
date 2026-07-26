package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
)

// run executes the CLI with the given arguments and captures stdout.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetGlobals()
	root := newRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// resetGlobals clears the package-level flag state between runs, which the
// real binary never needs because it exits.
func resetGlobals() {
	verbose = false
	quiet = false
}

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

func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "config.json", []byte(`{"model_type":"llama"}`))
	write(t, dir, "model-00001-of-00002.safetensors", randomBytes(150<<10, 1))
	write(t, dir, "model-00002-of-00002.safetensors", randomBytes(150<<10, 2))
	return dir
}

func TestRootHelp(t *testing.T) {
	out, err := run(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"copy", "sync", "verify", "manifest", "diff"} {
		if !strings.Contains(out, want) {
			t.Errorf("help is missing the %s command", want)
		}
	}
	if strings.Contains(out, "remote-helper") {
		t.Error("remote-helper should be hidden")
	}
}

func TestVersion(t *testing.T) {
	if Version() == "" {
		t.Error("Version is empty")
	}
	if !strings.HasPrefix(Tool(), "modelmove/") {
		t.Errorf("Tool = %q", Tool())
	}
	out, err := run(t, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, Version()) {
		t.Errorf("--version printed %q", out)
	}
}

func TestCopyAndVerify(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	out, err := run(t, "copy", src, dst, "--no-progress")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !strings.Contains(out, "huggingface") {
		t.Errorf("copy output did not name the layout: %q", out)
	}
	if !strings.Contains(out, "3 written") {
		t.Errorf("copy output = %q", out)
	}

	out, err = run(t, "verify", dst, "--no-progress")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(out, "result    ok") {
		t.Errorf("verify output = %q", out)
	}
}

func TestCopyJSON(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	out, err := run(t, "copy", src, dst, "--json")
	if err != nil {
		t.Fatalf("copy --json: %v", err)
	}
	var res struct {
		Source string `json:"source"`
		Model  struct {
			Kind  string `json:"kind"`
			Files int    `json:"files"`
		} `json:"model"`
		Summary struct {
			FilesWritten int `json:"files_written"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("copy --json emitted invalid JSON: %v\n%s", err, out)
	}
	if res.Model.Kind != "huggingface" || res.Model.Files != 3 {
		t.Errorf("decoded %+v", res)
	}
	if res.Summary.FilesWritten != 3 {
		t.Errorf("FilesWritten = %d", res.Summary.FilesWritten)
	}
}

func TestSyncSparseAndDelete(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := run(t, "copy", src, dst, "--no-progress"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(src, "model-00001-of-00002.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	copy(data[70<<10:], []byte("EDIT"))
	write(t, src, "model-00001-of-00002.safetensors", data)
	write(t, dst, "leftover.bin", []byte("old"))

	out, err := run(t, "sync", src, dst, "--delete", "--no-progress")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !strings.Contains(out, "1 deleted") {
		t.Errorf("sync output = %q", out)
	}
	if !strings.Contains(out, "saved") {
		t.Errorf("sync did not report the savings: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dst, "leftover.bin")); !os.IsNotExist(err) {
		t.Error("--delete did not remove the extra file")
	}
}

func TestDryRun(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	out, err := run(t, "copy", src, dst, "--dry-run", "--no-progress")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "dry run: nothing was written") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "copy") {
		t.Errorf("the plan detail is missing: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.json")); !os.IsNotExist(err) {
		t.Error("a dry run wrote to disk")
	}
}

func TestDryRunWithNothingToDo(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := run(t, "copy", src, dst, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "sync", src, dst, "--dry-run", "--no-progress")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to transfer") {
		t.Errorf("output = %q", out)
	}
}

func TestManifestToStdout(t *testing.T) {
	src := sourceDir(t)
	out, err := run(t, "manifest", src, "--no-progress")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	m, err := manifest.Read(strings.NewReader(out))
	if err != nil {
		t.Fatalf("stdout was not a valid manifest: %v", err)
	}
	if len(m.Files) != 3 {
		t.Errorf("got %d files, want 3", len(m.Files))
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestManifestToFile(t *testing.T) {
	src := sourceDir(t)
	out := filepath.Join(t.TempDir(), "model.mmm")
	if _, err := run(t, "manifest", src, "--out", out, "--format", "binary", "--no-progress"); err != nil {
		t.Fatalf("manifest --out: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, manifest.Magic[:]) {
		t.Error("--format binary did not write a binary manifest")
	}
	m, err := manifest.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 3 {
		t.Errorf("got %d files", len(m.Files))
	}
}

func TestManifestSummary(t *testing.T) {
	src := sourceDir(t)
	out, err := run(t, "manifest", src, "--summary", "--no-progress")
	if err != nil {
		t.Fatalf("manifest --summary: %v", err)
	}
	var s summaryOut
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("--summary emitted invalid JSON: %v\n%s", err, out)
	}
	if s.Kind != "huggingface" || s.Files != 3 || s.WeightFiles != 2 {
		t.Errorf("summary = %+v", s)
	}
	if s.Digest == "" {
		t.Error("the summary has no model digest")
	}
	if s.UniqueChunk == 0 || s.UniqueBytes == 0 {
		t.Error("the summary is missing chunk statistics")
	}
}

func TestManifestPinnedChunkSize(t *testing.T) {
	src := sourceDir(t)
	out, err := run(t, "manifest", src, "--avg-size", "8KiB", "--no-progress")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	m, err := manifest.Read(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if m.Chunker.AvgSize != 8<<10 {
		t.Errorf("AvgSize = %d, want 8192", m.Chunker.AvgSize)
	}
	for _, f := range m.Files {
		if f.Chunker.AvgSize != 8<<10 {
			t.Errorf("%s used AvgSize %d", f.Path, f.Chunker.AvgSize)
		}
	}
}

func TestVerifyFailureExitCode(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := run(t, "copy", src, dst, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "model-00001-of-00002.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	copy(data[50<<10:], []byte("BROKEN"))
	write(t, dst, "model-00001-of-00002.safetensors", data)

	out, err := run(t, "verify", dst, "--no-progress")
	if err == nil {
		t.Fatal("verify passed on a corrupt directory")
	}
	if got := Code(err); got != ExitMismatch {
		t.Errorf("exit code = %d, want %d", got, ExitMismatch)
	}
	if !strings.Contains(out, "FAILED") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, "bad chunk") {
		t.Errorf("the bad chunk was not located: %q", out)
	}
}

func TestVerifyJSON(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := run(t, "copy", src, dst, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "verify", dst, "--json")
	if err != nil {
		t.Fatalf("verify --json: %v", err)
	}
	var res struct {
		OK      bool `json:"ok"`
		FilesOK int  `json:"files_ok"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.OK || res.FilesOK != 3 {
		t.Errorf("decoded %+v", res)
	}
}

func TestVerifyWithoutManifest(t *testing.T) {
	if _, err := run(t, "verify", t.TempDir(), "--no-progress"); err == nil {
		t.Fatal("verify succeeded with no manifest")
	}
}

func TestDiff(t *testing.T) {
	src := sourceDir(t)
	oldPath := filepath.Join(t.TempDir(), "old.json")
	newPath := filepath.Join(t.TempDir(), "new.json")

	if _, err := run(t, "manifest", src, "--out", oldPath, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(src, "model-00002-of-00002.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	copy(data[60<<10:], []byte("CHANGED"))
	write(t, src, "model-00002-of-00002.safetensors", data)
	if _, err := run(t, "manifest", src, "--out", newPath, "--no-progress"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "diff", oldPath, newPath)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "1 modified") {
		t.Errorf("diff output = %q", out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("diff did not report reuse: %q", out)
	}

	// --exit-code turns a difference into a non-zero status.
	_, err = run(t, "diff", oldPath, newPath, "--exit-code")
	if err == nil {
		t.Fatal("--exit-code did not fail on differing manifests")
	}
	if got := Code(err); got != ExitMismatch {
		t.Errorf("exit code = %d, want %d", got, ExitMismatch)
	}
}

func TestDiffIdentical(t *testing.T) {
	src := sourceDir(t)
	path := filepath.Join(t.TempDir(), "m.json")
	if _, err := run(t, "manifest", src, "--out", path, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "diff", path, path, "--exit-code")
	if err != nil {
		t.Fatalf("diff of a manifest with itself: %v", err)
	}
	if !strings.Contains(out, "identical") {
		t.Errorf("output = %q", out)
	}
}

func TestDiffJSON(t *testing.T) {
	src := sourceDir(t)
	path := filepath.Join(t.TempDir(), "m.json")
	if _, err := run(t, "manifest", src, "--out", path, "--no-progress"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "diff", path, path, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var d manifest.Diff
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !d.Identical {
		t.Error("a manifest should be identical to itself")
	}
}

func TestDiffErrors(t *testing.T) {
	if _, err := run(t, "diff", "absent-a.json", "absent-b.json"); err == nil {
		t.Error("diff accepted missing files")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	write(t, dir, "bad.json", []byte(`{"format":"nope","version":1}`))
	good := filepath.Join(dir, "good.json")
	if err := manifest.Save(good, manifest.New("t", chunk.SmallFileOptions()), manifest.EncodingJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "diff", bad, good); err == nil {
		t.Error("diff accepted an invalid manifest as the old side")
	}
	if _, err := run(t, "diff", good, bad); err == nil {
		t.Error("diff accepted an invalid manifest as the new side")
	}
}

func TestFlagValidation(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	cases := [][]string{
		{"copy", src, dst, "--avg-size", "nonsense"},
		{"copy", src, dst, "--min-size", "1MiB", "--avg-size", "4KiB"},
		{"copy", src, dst, "--atomic", "sometimes"},
		{"copy", src, dst, "--manifest", "m.bin", "--manifest-format", "yaml"},
		{"manifest", src, "--format", "yaml"},
		{"manifest", src, "--avg-size", "-1"},
	}
	for _, args := range cases {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestArgCountValidation(t *testing.T) {
	for _, args := range [][]string{
		{"copy"}, {"copy", "a"}, {"copy", "a", "b", "c"},
		{"sync", "a"}, {"verify"}, {"manifest"}, {"diff", "a"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestRemoteHelperRequiresRoot(t *testing.T) {
	if _, err := run(t, "remote-helper"); err == nil {
		t.Fatal("remote-helper ran without --root")
	}
}

func TestQuietSuppressesOutput(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", src, dst, "--quiet", "--no-progress")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("--quiet still printed %q", out)
	}
}

func TestExitCodeMapping(t *testing.T) {
	if Code(nil) != ExitOK {
		t.Error("nil should map to exit 0")
	}
	if Code(errPlain{}) != ExitError {
		t.Error("a plain error should map to exit 1")
	}
	if Code(mismatch(errPlain{})) != ExitMismatch {
		t.Error("a coded error should keep its code")
	}
	wrapped := mismatch(errPlain{})
	if wrapped.Error() != "plain" {
		t.Errorf("Error() = %q", wrapped.Error())
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "plain" }

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abcdefghij", 5); got != "...ij" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abcdefghij", 2); got != "ab" {
		t.Errorf("truncate = %q", got)
	}
}

func TestExecuteArgsPropagatesErrors(t *testing.T) {
	if err := ExecuteArgs([]string{"verify", filepath.Join(t.TempDir(), "absent")}); err == nil {
		t.Fatal("ExecuteArgs hid an error")
	}
	if err := ExecuteArgs([]string{"--help"}); err != nil {
		t.Fatalf("--help returned %v", err)
	}
}

func TestWarnfRespectsVerbosity(t *testing.T) {
	// warnf writes to the process stderr, so this only checks that it does not
	// panic in either mode.
	resetGlobals()
	warnf("quiet path %d", 1)
	verbose = true
	quiet = true
	warnf("still quiet %d", 2)
	resetGlobals()
}

func TestIsTerminalOnFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file is not a terminal")
	}
}

func TestCommandsHaveExamples(t *testing.T) {
	root := newRoot()
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		if c.Example == "" {
			t.Errorf("%s has no example", c.Name())
		}
		if c.Short == "" {
			t.Errorf("%s has no summary", c.Name())
		}
	}
}
