package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/progress"
	"github.com/shaneburrell/modelmove/internal/scan"
)

// Problem classifies a verification failure.
type Problem string

// Verification problems.
const (
	ProblemMissing    Problem = "missing"
	ProblemSize       Problem = "size-mismatch"
	ProblemDigest     Problem = "digest-mismatch"
	ProblemUnreadable Problem = "unreadable"
)

// BadChunk locates a chunk-sized range that does not match the manifest.
type BadChunk struct {
	Index  int    `json:"index"`
	Offset uint64 `json:"offset"`
	Length uint32 `json:"length"`
	Want   string `json:"want"`
	Got    string `json:"got,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FileProblem reports everything wrong with one file.
type FileProblem struct {
	Path      string     `json:"path"`
	Problem   Problem    `json:"problem"`
	WantSize  int64      `json:"want_size,omitempty"`
	GotSize   int64      `json:"got_size,omitempty"`
	Want      string     `json:"want,omitempty"`
	Got       string     `json:"got,omitempty"`
	Error     string     `json:"error,omitempty"`
	BadChunks []BadChunk `json:"bad_chunks,omitempty"`
	BadBytes  int64      `json:"bad_bytes,omitempty"`
}

// VerifyConfig describes a verification run.
type VerifyConfig struct {
	Root         string
	ManifestPath string
	Jobs         int
	// Quick skips the per-chunk breakdown of a failing file.
	Quick bool
	// Extra reports files present in the directory but absent from the
	// manifest.
	Extra    bool
	Progress bool
	Warn     func(format string, args ...any)
}

// VerifyResult is the outcome of a verification run.
type VerifyResult struct {
	Root         string        `json:"root"`
	ManifestPath string        `json:"manifest_path"`
	Model        ModelInfo     `json:"model"`
	OK           bool          `json:"ok"`
	Files        int           `json:"files"`
	FilesOK      int           `json:"files_ok"`
	BytesChecked int64         `json:"bytes_checked"`
	Problems     []FileProblem `json:"problems,omitempty"`
	Extra        []string      `json:"extra,omitempty"`
	Seconds      float64       `json:"seconds"`
}

// ErrNoManifest is returned when a directory has no recorded manifest and none
// was supplied.
var ErrNoManifest = errors.New("no manifest: pass --manifest, or copy the model with modelmove first")

// LoadVerifyManifest resolves which manifest to verify against.
func LoadVerifyManifest(cfg VerifyConfig) (*manifest.Manifest, string, error) {
	if cfg.ManifestPath != "" {
		m, err := manifest.Load(cfg.ManifestPath)
		return m, cfg.ManifestPath, err
	}
	applied := manifest.StatePath(cfg.Root, manifest.ManifestName)
	if _, err := os.Stat(applied); err != nil {
		return nil, "", ErrNoManifest
	}
	m, err := manifest.Load(applied)
	return m, applied, err
}

// Verify re-reads a model directory and checks it against a manifest.
func Verify(ctx context.Context, cfg VerifyConfig) (*VerifyResult, error) {
	m, path, err := LoadVerifyManifest(cfg)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}

	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(root); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("engine: %s is not a directory", cfg.Root)
	}

	res := &VerifyResult{
		Root:         root,
		ManifestPath: path,
		Model:        modelInfo(m),
		Files:        len(m.Files),
	}

	bar := progress.NewFor(cfg.Progress, progress.Options{Label: "verify", Total: m.TotalBytes()})
	defer bar.Finish()

	jobs := cfg.Jobs
	if jobs <= 0 {
		jobs = runtime.NumCPU()
		if jobs > 8 {
			jobs = 8
		}
	}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		problems []FileProblem
		okFiles  int
		checked  int64
	)
	sem := make(chan struct{}, jobs)

	for _, f := range m.Files {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(f *manifest.File) {
			defer wg.Done()
			defer func() { <-sem }()

			bar.SetLabel(f.Path)
			p, n := verifyFile(ctx, root, f, cfg.Quick)
			bar.Add(n)

			mu.Lock()
			defer mu.Unlock()
			checked += n
			if p == nil {
				okFiles++
				return
			}
			problems = append(problems, *p)
		}(f)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
	res.Problems = problems
	res.FilesOK = okFiles
	res.BytesChecked = checked
	res.OK = len(problems) == 0

	if cfg.Extra {
		res.Extra = extraFiles(root, m, cfg.Warn)
		if len(res.Extra) > 0 {
			res.OK = false
		}
	}
	return res, nil
}

func verifyFile(ctx context.Context, root string, f *manifest.File, quick bool) (*FileProblem, int64) {
	abs := filepath.Join(root, filepath.FromSlash(f.Path))
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileProblem{Path: f.Path, Problem: ProblemMissing, WantSize: f.Size}, 0
		}
		return &FileProblem{Path: f.Path, Problem: ProblemUnreadable, Error: err.Error()}, 0
	}
	if fi.Size() != f.Size {
		return &FileProblem{
			Path: f.Path, Problem: ProblemSize,
			WantSize: f.Size, GotSize: fi.Size(),
		}, 0
	}

	fh, err := os.Open(abs)
	if err != nil {
		return &FileProblem{Path: f.Path, Problem: ProblemUnreadable, Error: err.Error()}, 0
	}
	defer fh.Close()

	got, n, err := chunk.HashReader(fh)
	if err != nil {
		return &FileProblem{Path: f.Path, Problem: ProblemUnreadable, Error: err.Error()}, n
	}
	if got == f.Digest {
		return nil, n
	}

	p := &FileProblem{
		Path: f.Path, Problem: ProblemDigest,
		Want: f.Digest.String(), Got: got.String(),
		WantSize: f.Size, GotSize: fi.Size(),
	}
	if !quick {
		p.BadChunks, p.BadBytes = badChunks(ctx, fh, f)
	}
	return p, n
}

// badChunks re-hashes the exact ranges the manifest recorded, which pinpoints
// the damaged region instead of just saying the file is wrong.
func badChunks(ctx context.Context, ra *os.File, f *manifest.File) ([]BadChunk, int64) {
	var (
		out    []BadChunk
		bytes  int64
		maxLen uint32
	)
	for _, c := range f.Chunks {
		if c.Length > maxLen {
			maxLen = c.Length
		}
	}
	buf := make([]byte, maxLen)

	for i, c := range f.Chunks {
		if ctx.Err() != nil {
			break
		}
		got, err := chunk.HashAt(ra, int64(c.Offset), int(c.Length), buf)
		if err != nil {
			out = append(out, BadChunk{Index: i, Offset: c.Offset, Length: c.Length,
				Want: c.Digest.String(), Error: err.Error()})
			bytes += int64(c.Length)
			continue
		}
		if got != c.Digest {
			out = append(out, BadChunk{Index: i, Offset: c.Offset, Length: c.Length,
				Want: c.Digest.String(), Got: got.String()})
			bytes += int64(c.Length)
		}
	}
	return out, bytes
}

func extraFiles(root string, m *manifest.Manifest, warn func(string, ...any)) []string {
	entries, err := scan.Walk(scan.Options{Root: root, IncludeHidden: true, Warn: warn})
	if err != nil {
		return nil
	}
	known := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		known[f.Path] = struct{}{}
	}
	var extra []string
	for _, e := range entries {
		if _, ok := known[e.Path]; !ok {
			extra = append(extra, e.Path)
		}
	}
	sort.Strings(extra)
	return extra
}
