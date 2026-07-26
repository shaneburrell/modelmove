package transport

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
	"github.com/shaneburrell/modelmove/internal/scan"
)

func TestParseLocal(t *testing.T) {
	for _, in := range []string{"/models/llama", "./llama", "llama", "../llama", "file:///models/llama"} {
		e, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if !e.IsLocal() {
			t.Errorf("Parse(%q) = %v, want a local path", in, e.Scheme)
		}
		if e.Path == "" {
			t.Errorf("Parse(%q) produced an empty path", in)
		}
	}
}

func TestParseSSH(t *testing.T) {
	cases := []struct {
		in   string
		user string
		host string
		port int
		path string
	}{
		{"gpu-box:/srv/models", "", "gpu-box", 0, "/srv/models"},
		{"shane@gpu-box:/srv/models", "shane", "gpu-box", 0, "/srv/models"},
		{"gpu-box:models/llama", "", "gpu-box", 0, "models/llama"},
		{"ssh://gpu-box/srv/models", "", "gpu-box", 0, "/srv/models"},
		{"ssh://shane@gpu-box:2222/srv/models", "shane", "gpu-box", 2222, "/srv/models"},
		{"SSH://gpu-box/srv/m", "", "gpu-box", 0, "/srv/m"},
	}
	for _, c := range cases {
		e, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		if e.Scheme != SchemeSSH {
			t.Errorf("Parse(%q).Scheme = %q", c.in, e.Scheme)
		}
		if e.User != c.user || e.Host != c.host || e.Port != c.port || e.Path != c.path {
			t.Errorf("Parse(%q) = %+v, want user=%q host=%q port=%d path=%q",
				c.in, e, c.user, c.host, c.port, c.path)
		}
		if e.IsLocal() {
			t.Errorf("Parse(%q) reported local", c.in)
		}
		if !strings.Contains(e.String(), c.host) {
			t.Errorf("String() = %q, missing the host", e.String())
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "ssh://", "ssh://host", "ssh://host:99999/p", "ssh://host:abc/p", "ssh://:22/p"} {
		if e, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %+v, want an error", in, e)
		}
	}
}

func TestParseAmbiguousColons(t *testing.T) {
	// A relative path containing a colon is not a host spec.
	e, err := Parse("./weird:name")
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsLocal() {
		t.Errorf("./weird:name parsed as %v", e.Scheme)
	}
	// A host with an empty path is a local path, not a remote root.
	e, err = Parse("host:")
	if err != nil {
		t.Fatal(err)
	}
	if !e.IsLocal() {
		t.Errorf("host: parsed as %v", e.Scheme)
	}
	if runtime.GOOS == "windows" {
		e, err := Parse(`C:\models`)
		if err != nil {
			t.Fatal(err)
		}
		if !e.IsLocal() {
			t.Error("a Windows drive letter parsed as a remote host")
		}
	}
}

func TestLocalPath(t *testing.T) {
	e, _ := Parse("/models/./llama/")
	p, err := e.LocalPath()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/models/llama" {
		t.Errorf("LocalPath = %q", p)
	}
	remote, _ := Parse("host:/p")
	if _, err := remote.LocalPath(); err == nil {
		t.Error("LocalPath succeeded for a remote endpoint")
	}
	if got := (Endpoint{Scheme: SchemeFile, Path: "/x"}).String(); got != "/x" {
		t.Errorf("local String() = %q", got)
	}
}

func randomBytes(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	r.Read(b)
	return b
}

func sourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), randomBytes(150<<10, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func defaultConfig() Config {
	return Config{
		Atomic: receiver.AtomicFile, Dedupe: true, Resume: true,
		Verify: true, PreserveTimes: true, Jobs: 2, Tool: "modelmove/test",
	}
}

// pushAll drives a session the way the engine does.
func pushAll(t *testing.T, sess Session, m *manifest.Manifest, src string) *receiver.Summary {
	t.Helper()
	ctx := context.Background()
	plan, err := sess.Plan(ctx, m)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, fp := range plan.Files {
		if fp.Action == receiver.ActionSkip {
			continue
		}
		if err := sess.BeginFile(ctx, fp.Path); err != nil {
			t.Fatalf("BeginFile: %v", err)
		}
		f := m.Lookup(fp.Path)
		fh, err := os.Open(filepath.Join(src, filepath.FromSlash(fp.Path)))
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
			if _, err := fh.ReadAt(buf, int64(c.Offset)); err != nil {
				t.Fatal(err)
			}
			if err := sess.SendChunk(ctx, d, buf); err != nil {
				t.Fatalf("SendChunk: %v", err)
			}
		}
		fh.Close()
		if _, err := sess.EndFile(ctx); err != nil {
			t.Fatalf("EndFile: %v", err)
		}
	}
	sum, err := sess.Finish(ctx)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return sum
}

func TestLocalSession(t *testing.T) {
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Parse(dst)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := Dial(context.Background(), e, defaultConfig())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sess.Close()

	sum := pushAll(t, sess, m, src)
	if sum.FilesWritten != 2 {
		t.Errorf("FilesWritten = %d, want 2", sum.FilesWritten)
	}
	if !filepath.IsAbs(sess.Root()) {
		t.Errorf("Root() = %q", sess.Root())
	}
	got, err := os.ReadFile(filepath.Join(dst, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(src, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("the transferred file does not match the source")
	}
}

func TestDialRejectsUnknownScheme(t *testing.T) {
	if _, err := Dial(context.Background(), Endpoint{Scheme: "ftp", Path: "/x"}, defaultConfig()); err == nil {
		t.Fatal("Dial accepted an unknown scheme")
	}
}

func TestDialRemoteEndpointAsLocalFails(t *testing.T) {
	e := Endpoint{Scheme: SchemeFile}
	if _, err := newLocalSession(e, defaultConfig()); err == nil {
		t.Fatal("a local session with an empty path should fail")
	}
}

// fakeSSH writes a script that stands in for ssh: it drops the host argument
// and runs the command locally, which exercises the real subprocess and
// framing without needing a server.
func fakeSSH(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh shim is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "fakessh")
	script := "#!/bin/sh\nshift\nexec /bin/sh -c \"$*\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	helperOnce sync.Once
	helperPath string
	helperErr  error
)

// helperBinary builds the modelmove command once per test run so that the SSH
// transport can be exercised end to end against a real subprocess.
func helperBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the ssh transport test in short mode")
	}
	helperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "modelmove-helper-")
		if err != nil {
			helperErr = err
			return
		}
		out := filepath.Join(dir, "modelmove")
		cmd := exec.CommandContext(context.Background(), "go", "build", "-o", out,
			"github.com/shaneburrell/modelmove/cmd/modelmove")
		if combined, err := cmd.CombinedOutput(); err != nil {
			helperErr = fmt.Errorf("go build: %w: %s", err, combined)
			return
		}
		helperPath = out
	})
	if helperErr != nil {
		t.Skipf("cannot build the helper binary: %v", helperErr)
	}
	return helperPath
}

func TestSSHSession(t *testing.T) {
	bin := helperBinary(t)
	src := sourceDir(t)
	dst := filepath.Join(t.TempDir(), "out")

	m, err := scan.Build(context.Background(), scan.Options{Root: src, Tool: "test"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := Parse("fakehost:" + dst)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.SSHCommand = fakeSSH(t)
	cfg.RemoteBin = bin

	sess, err := Dial(context.Background(), e, cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sum := pushAll(t, sess, m, src)
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	if sum.FilesWritten != 2 {
		t.Errorf("FilesWritten = %d, want 2", sum.FilesWritten)
	}
	if sess.Root() != dst {
		t.Errorf("Root() = %q, want %q", sess.Root(), dst)
	}
}

func TestSSHMissingRemoteBinary(t *testing.T) {
	cfg := defaultConfig()
	cfg.SSHCommand = fakeSSH(t)
	cfg.RemoteBin = "definitely-not-installed-modelmove"

	e, err := Parse("fakehost:/tmp/nowhere")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), e, cfg)
	if err == nil {
		t.Fatal("Dial succeeded with no helper on the remote")
	}
	// The error has to say something more useful than "unexpected EOF".
	if !strings.Contains(err.Error(), "remote") {
		t.Errorf("error = %v, want it to mention the remote", err)
	}
}

func TestSSHEmptyCommand(t *testing.T) {
	cfg := defaultConfig()
	cfg.SSHCommand = "   "
	e, _ := Parse("host:/p")
	if _, err := Dial(context.Background(), e, cfg); err == nil {
		t.Fatal("Dial accepted an empty ssh command")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/srv/models":    `'/srv/models'`,
		"/srv/my models": `'/srv/my models'`,
		"/srv/it's":      `'/srv/it'\''s'`,
		"; rm -rf /":     `'; rm -rf /'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteHelperCommand(t *testing.T) {
	got := remoteHelperCommand("modelmove", "/srv/models", Config{})
	if !strings.Contains(got, "remote-helper --root '/srv/models'") {
		t.Errorf("command = %q", got)
	}
	if strings.Contains(got, "--allow-delete") {
		t.Error("--allow-delete should only appear when deletes are requested")
	}
	got = remoteHelperCommand("modelmove", "/srv/models", Config{Delete: true})
	if !strings.Contains(got, "--allow-delete") {
		t.Errorf("command = %q, want --allow-delete", got)
	}
}

func TestStderrTail(t *testing.T) {
	var tail stderrTail
	if _, err := tail.Write([]byte("  hello\n")); err != nil {
		t.Fatal(err)
	}
	if got := tail.String(); got != "hello" {
		t.Errorf("String() = %q", got)
	}
	// Only the tail is kept, so a chatty remote cannot use unbounded memory.
	big := make([]byte, stderrTailLimit*2)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := tail.Write(big); err != nil {
		t.Fatal(err)
	}
	if len(tail.String()) > stderrTailLimit {
		t.Errorf("tail kept %d bytes, limit is %d", len(tail.String()), stderrTailLimit)
	}
}

func TestLookPathRemoteHint(t *testing.T) {
	if LookPathRemoteHint() == "" {
		t.Error("LookPathRemoteHint returned nothing")
	}
}
