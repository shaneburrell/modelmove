// Package receiver implements the destination side of a transfer: it decides
// which chunks it already has, assembles files from a mix of local and
// received chunks, verifies them with BLAKE3 and only then replaces the
// originals.
package receiver

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/progress"
)

// AtomicMode controls when staged files replace their originals.
type AtomicMode string

// Atomicity modes.
const (
	// AtomicFile replaces each file as soon as it verifies. Needs staging
	// space for one file at a time.
	AtomicFile AtomicMode = "file"
	// AtomicModel stages every file and swaps them all in at the end, so the
	// directory is never a mix of old and new shards. Needs staging space for
	// the whole changed set, and in exchange every old file stays available
	// as a source of reusable chunks for the entire run.
	AtomicModel AtomicMode = "model"
)

// ParseAtomicMode maps a user-supplied name to an AtomicMode.
func ParseAtomicMode(s string) (AtomicMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "file":
		return AtomicFile, nil
	case "model", "dir", "all":
		return AtomicModel, nil
	default:
		return "", fmt.Errorf("receiver: unknown atomic mode %q (want file or model)", s)
	}
}

// Options configures a Receiver.
type Options struct {
	// Root is the destination model directory.
	Root string
	// Atomic selects per-file or whole-directory replacement.
	Atomic AtomicMode
	// Dedupe allows chunks to be reused from destination files the manifest
	// does not mention.
	Dedupe bool
	// Fast trusts size and mtime instead of re-hashing unchanged files.
	Fast bool
	// Resume reuses chunks left in staging files by an interrupted run.
	Resume bool
	// Delete removes destination files the manifest does not list.
	Delete bool
	// Verify re-reads each staged file and checks its whole-file digest
	// before the replace. Disabling it is not recommended.
	Verify bool
	// PreserveTimes applies the source modification time to written files.
	PreserveTimes bool
	// Pin overrides chunker sizing for destination files not in the manifest.
	Pin chunk.Options
	// Jobs bounds parallel destination hashing.
	Jobs int
	// Bar, if non-nil, is advanced as destination files are hashed during
	// planning. Transfer progress is tracked by the caller, which is the only
	// side that knows how many bytes actually crossed the network.
	Bar *progress.Bar
	// Warn receives non-fatal problems.
	Warn func(format string, args ...any)
}

func (o Options) warnf(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// DefaultOptions returns the options used when nothing is overridden.
func DefaultOptions(root string) Options {
	return Options{
		Root:          root,
		Atomic:        AtomicFile,
		Dedupe:        true,
		Resume:        true,
		Verify:        true,
		PreserveTimes: true,
	}
}

// FileResult reports the outcome for one file.
type FileResult struct {
	Path     string `json:"path"`
	Action   Action `json:"action"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Size     int64  `json:"size"`
	Received int64  `json:"received_bytes"`
	Reused   int64  `json:"reused_bytes"`
	Verified bool   `json:"verified"`
}

// Summary reports the outcome of a whole transfer.
type Summary struct {
	Root          string       `json:"root"`
	Files         int          `json:"files"`
	FilesWritten  int          `json:"files_written"`
	FilesSkipped  int          `json:"files_skipped"`
	FilesDeleted  int          `json:"files_deleted"`
	BytesTotal    int64        `json:"bytes_total"`
	BytesReceived int64        `json:"bytes_received"`
	BytesReused   int64        `json:"bytes_reused"`
	Results       []FileResult `json:"results,omitempty"`
}

// Savings returns the fraction of the model that did not cross the network.
func (s Summary) Savings() float64 {
	if s.BytesTotal == 0 {
		return 1
	}
	return 1 - float64(s.BytesReceived)/float64(s.BytesTotal)
}

// Receiver applies a manifest to a destination directory.
type Receiver struct {
	opt  Options
	root string
	bar  *progress.Bar

	manifest   *manifest.Manifest
	plan       *Plan
	planByPath map[string]FilePlan

	global   *index
	staged   *index
	replaced map[string]*index
	readers  *readers

	active  *activeFile
	pending []stagedFile
	results []FileResult

	started time.Time
}

type stagedFile struct {
	rel   string
	stage string
	final string
	mode  os.FileMode
	mtime time.Time
}

// New creates a Receiver for the given destination.
func New(opt Options) (*Receiver, error) {
	if opt.Root == "" {
		return nil, errors.New("receiver: no destination root")
	}
	if opt.Atomic == "" {
		opt.Atomic = AtomicFile
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	return &Receiver{
		opt:      opt,
		root:     root,
		bar:      opt.Bar,
		global:   newIndex(),
		staged:   newIndex(),
		replaced: map[string]*index{},
		readers:  newReaders(),
		started:  time.Now(),
	}, nil
}

// Root returns the absolute destination path.
func (r *Receiver) Root() string { return r.root }

func (r *Receiver) stagePath(rel string) string {
	return filepath.Join(r.root, manifest.StateDir, "stage", filepath.FromSlash(rel)+".part")
}

func (r *Receiver) finalPath(rel string) string {
	return filepath.Join(r.root, filepath.FromSlash(rel))
}

// activeFile is the file currently being assembled.
type activeFile struct {
	plan     FilePlan
	file     *manifest.File
	stage    string
	f        *os.File
	covered  []bool
	byDigest map[chunk.Digest][]int
	received int64
	reused   int64
}

// BeginFile opens a staging file for rel and prepares to receive its chunks.
func (r *Receiver) BeginFile(rel string) error {
	if r.active != nil {
		return fmt.Errorf("receiver: %s is still open", r.active.plan.Path)
	}
	if r.manifest == nil {
		return errors.New("receiver: no plan; call Plan first")
	}
	if err := manifest.ValidatePath(rel); err != nil {
		return err
	}
	f := r.manifest.Lookup(rel)
	if f == nil {
		return fmt.Errorf("receiver: %s is not in the manifest", rel)
	}
	fp, ok := r.planByPath[rel]
	if !ok {
		return fmt.Errorf("receiver: %s is not in the plan", rel)
	}
	if fp.Action == ActionSkip {
		return fmt.Errorf("receiver: %s was planned as a skip", rel)
	}

	stage := r.stagePath(rel)
	if err := os.MkdirAll(filepath.Dir(stage), 0o755); err != nil {
		return err
	}

	reuse := false
	if r.opt.Resume {
		if fi, err := os.Stat(stage); err == nil && fi.Size() == f.Size {
			reuse = true
		}
	}
	flags := os.O_RDWR | os.O_CREATE
	if !reuse {
		flags |= os.O_TRUNC
	}
	sf, err := os.OpenFile(stage, flags, 0o644)
	if err != nil {
		return err
	}
	if err := sf.Truncate(f.Size); err != nil {
		sf.Close()
		return err
	}

	byDigest := make(map[chunk.Digest][]int, len(f.Chunks))
	for i, c := range f.Chunks {
		byDigest[c.Digest] = append(byDigest[c.Digest], i)
	}
	r.active = &activeFile{
		plan:     fp,
		file:     f,
		stage:    stage,
		f:        sf,
		covered:  make([]bool, len(f.Chunks)),
		byDigest: byDigest,
	}
	return nil
}

// Chunk accepts one chunk payload for the open file and writes it to every
// offset in that file where the digest occurs.
func (r *Receiver) Chunk(d chunk.Digest, data []byte) error {
	a := r.active
	if a == nil {
		return errors.New("receiver: no file is open")
	}
	if got := chunk.Sum(data); got != d {
		return fmt.Errorf("receiver: chunk payload does not match digest %s", d.Short())
	}
	idxs, ok := a.byDigest[d]
	if !ok {
		return fmt.Errorf("receiver: %s does not contain chunk %s", a.plan.Path, d.Short())
	}
	for _, i := range idxs {
		if a.covered[i] {
			continue
		}
		c := a.file.Chunks[i]
		if int(c.Length) != len(data) {
			return fmt.Errorf("receiver: chunk %s is %d bytes, expected %d", d.Short(), len(data), c.Length)
		}
		if _, err := a.f.WriteAt(data, int64(c.Offset)); err != nil {
			return err
		}
		a.covered[i] = true
		a.received += int64(len(data))
	}
	return nil
}

// statusFailed marks a per-file result whose transfer or verification broke.
const statusFailed = "failed"

// EndFile fills any chunks that were not sent from local sources, verifies the
// result and, under per-file atomicity, replaces the original.
func (r *Receiver) EndFile() (*FileResult, error) {
	a := r.active
	if a == nil {
		return nil, errors.New("receiver: no file is open")
	}
	r.active = nil

	res := &FileResult{Path: a.plan.Path, Action: a.plan.Action, Size: a.file.Size}
	fail := func(err error) (*FileResult, error) {
		if a.f != nil {
			a.f.Close()
			a.f = nil
		}
		res.Status = statusFailed
		res.Error = err.Error()
		r.results = append(r.results, *res)
		return res, err
	}

	if err := r.fill(a); err != nil {
		return fail(err)
	}
	res.Received = a.received
	res.Reused = a.reused

	if r.opt.Verify {
		if err := verifyStage(a); err != nil {
			return fail(err)
		}
		res.Verified = true
	}

	if err := a.f.Sync(); err != nil {
		return fail(err)
	}
	if err := a.f.Close(); err != nil {
		a.f = nil
		res.Status = statusFailed
		res.Error = err.Error()
		r.results = append(r.results, *res)
		return res, err
	}
	a.f = nil

	mode := os.FileMode(a.file.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := os.Chmod(a.stage, mode); err != nil {
		r.opt.warnf("cannot set mode on %s: %v", a.plan.Path, err)
	}

	sf := stagedFile{
		rel:   a.plan.Path,
		stage: a.stage,
		final: r.finalPath(a.plan.Path),
		mode:  mode,
		mtime: a.file.ModTime,
	}
	if r.opt.Atomic == AtomicModel {
		// Keep the staged copy addressable so later files can reuse it.
		r.indexFile(a.stage, a.file)
		r.pending = append(r.pending, sf)
	} else {
		if err := r.commit(sf); err != nil {
			res.Status = statusFailed
			res.Error = err.Error()
			r.results = append(r.results, *res)
			return res, err
		}
		r.indexFile(sf.final, a.file)
	}

	res.Status = "ok"
	r.results = append(r.results, *res)
	return res, nil
}

// fill writes every chunk that was not received, sourcing it from the local
// index. This is where the sparse update actually pays off.
func (r *Receiver) fill(a *activeFile) error {
	var buf []byte
	local := r.replaced[a.plan.Path]

	for i, c := range a.file.Chunks {
		if a.covered[i] {
			continue
		}
		loc, ok := r.lookup(local, c.Digest)
		if !ok {
			return fmt.Errorf("missing chunk %s for %s at offset %d", c.Digest.Short(), a.plan.Path, c.Offset)
		}
		data, err := r.readers.fetch(loc, c.Digest, buf)
		if err != nil {
			return fmt.Errorf("%s: %w", a.plan.Path, err)
		}
		buf = data[:0]
		if _, err := a.f.WriteAt(data, int64(c.Offset)); err != nil {
			return err
		}
		a.covered[i] = true
		a.reused += int64(c.Length)
	}
	return nil
}

func (r *Receiver) lookup(local *index, d chunk.Digest) (location, bool) {
	if local != nil {
		if loc, ok := local.get(d); ok {
			return loc, true
		}
	}
	if loc, ok := r.staged.get(d); ok {
		return loc, true
	}
	return r.global.get(d)
}

// verifyStage re-reads the assembled file and compares its BLAKE3 digest with
// the manifest. Nothing is replaced until this passes.
func verifyStage(a *activeFile) error {
	if _, err := a.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got, n, err := chunk.HashReader(a.f)
	if err != nil {
		return err
	}
	if n != a.file.Size {
		return fmt.Errorf("%s: staged %d bytes, manifest says %d", a.plan.Path, n, a.file.Size)
	}
	if got != a.file.Digest {
		return fmt.Errorf("%s: verification failed (got %s, want %s)", a.plan.Path, got.Short(), a.file.Digest.Short())
	}
	return nil
}

// commit moves a verified staging file over its destination.
func (r *Receiver) commit(sf stagedFile) error {
	if err := os.MkdirAll(filepath.Dir(sf.final), 0o755); err != nil {
		return err
	}
	// The old file may still be open as a chunk source; close it first so the
	// rename does not leave a stale handle behind.
	r.readers.forget(sf.final)
	if err := os.Rename(sf.stage, sf.final); err != nil {
		return err
	}
	if r.opt.PreserveTimes && !sf.mtime.IsZero() {
		if err := os.Chtimes(sf.final, sf.mtime, sf.mtime); err != nil {
			r.opt.warnf("cannot set times on %s: %v", sf.rel, err)
		}
	}
	syncDir(filepath.Dir(sf.final))
	return nil
}

func (r *Receiver) indexFile(path string, f *manifest.File) {
	r.global.dropPath(r.finalPath(f.Path))
	delete(r.replaced, f.Path)
	for _, c := range f.Chunks {
		r.global.add(c.Digest, location{path: path, offset: int64(c.Offset), length: c.Length})
	}
}

// Finish commits any deferred renames, applies deletions and records the
// manifest that the destination now matches.
func (r *Receiver) Finish() (*Summary, error) {
	if r.active != nil {
		return nil, fmt.Errorf("receiver: %s was never closed", r.active.plan.Path)
	}
	defer r.readers.closeAll()

	for _, sf := range r.pending {
		if err := r.commit(sf); err != nil {
			return nil, err
		}
	}
	r.pending = nil

	sum := &Summary{Root: r.root, Results: r.results}
	for _, res := range r.results {
		sum.BytesReceived += res.Received
		sum.BytesReused += res.Reused
		if res.Status == "ok" {
			sum.FilesWritten++
		}
	}
	if r.plan != nil {
		sum.Files = r.plan.TotalFiles
		sum.FilesSkipped = r.plan.SkipFiles
		sum.BytesTotal = r.plan.TotalBytes
	}

	if r.opt.Delete && r.plan != nil {
		for _, rel := range r.plan.Deletes {
			p := r.finalPath(rel)
			r.readers.forget(p)
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				r.opt.warnf("cannot delete %s: %v", rel, err)
				continue
			}
			sum.FilesDeleted++
			pruneEmptyDirs(filepath.Dir(p), r.root)
		}
	}

	if r.manifest != nil {
		applied := *r.manifest
		applied.Model.Root = r.root
		if err := manifest.Save(manifest.StatePath(r.root, manifest.ManifestName), &applied, manifest.EncodingJSON); err != nil {
			r.opt.warnf("cannot record manifest: %v", err)
		}
	}
	r.cleanStage()
	return sum, nil
}

// Abort closes the open file and releases handles without committing.
func (r *Receiver) Abort() {
	if r.active != nil {
		r.active.f.Close()
		r.active = nil
	}
	r.readers.closeAll()
}

// cleanStage removes empty directories left under the staging root after successful
// renames, leaving any partial .part file from a failed transfer for resume.
func (r *Receiver) cleanStage() {
	stageRoot := filepath.Join(r.root, manifest.StateDir, "stage")
	if _, err := os.Stat(stageRoot); err != nil {
		return
	}
	// Collect dirs depth-first, then remove empty ones from the deepest upward
	// so nested staging paths like stage/a/b/ do not linger after a rename.
	var dirs []string
	_ = filepath.WalkDir(stageRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := os.ReadDir(dirs[i])
		if err != nil || len(entries) > 0 {
			continue
		}
		_ = os.Remove(dirs[i])
	}
}

// underRoot reports whether dir is root or a path strictly inside it.
func underRoot(dir, root string) bool {
	if dir == root {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(dir, root+sep)
}

func pruneEmptyDirs(dir, stop string) {
	for underRoot(dir, stop) && dir != stop {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// SortedResults returns the per-file results ordered by path.
func (r *Receiver) SortedResults() []FileResult {
	out := make([]FileResult, len(r.results))
	copy(out, r.results)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
