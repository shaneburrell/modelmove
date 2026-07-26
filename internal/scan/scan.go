// Package scan walks a model directory and turns it into a manifest, hashing
// files in parallel with FastCDC + BLAKE3.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/progress"
)

// Options controls a scan.
type Options struct {
	// Root is the model directory.
	Root string
	// Exclude holds glob patterns matched against relative paths and against
	// each path's basename.
	Exclude []string
	// IncludeHidden includes dot-files other than the modelmove state dir.
	IncludeHidden bool
	// FollowSymlinks materialises symlinked files as regular files. Hugging
	// Face caches link snapshots at blobs, so this is on by default.
	FollowSymlinks bool
	// Pin overrides the per-file chunker sizing with fixed parameters.
	Pin chunk.Options
	// Jobs bounds concurrent file hashing. Zero means NumCPU.
	Jobs int
	// Tool is recorded in the manifest.
	Tool string
	// Bar, if non-nil, is advanced by the number of bytes hashed.
	Bar *progress.Bar
	// Warn, if set, receives non-fatal problems such as skipped symlinks.
	Warn func(format string, args ...any)
}

func (o Options) jobs() int {
	if o.Jobs > 0 {
		return o.Jobs
	}
	n := runtime.NumCPU()
	if n > 8 {
		// Whole-file hashing is already internally parallel; too many files at
		// once just thrashes the disk.
		n = 8
	}
	return n
}

func (o Options) warn(format string, args ...any) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

// Entry is one file discovered by Walk.
type Entry struct {
	Path string // slash-separated, relative to root
	Size int64
	Mode os.FileMode
	Info os.FileInfo
}

// Walk lists the regular files under opt.Root without hashing anything.
func Walk(opt Options) ([]Entry, error) {
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("scan: %s is not a directory", opt.Root)
	}

	var entries []Entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		base := path.Base(rel)

		if d.IsDir() {
			if base == manifest.StateDir {
				return fs.SkipDir
			}
			if !opt.IncludeHidden && strings.HasPrefix(base, ".") {
				return fs.SkipDir
			}
			if opt.excluded(rel, base) {
				return fs.SkipDir
			}
			return nil
		}
		if !opt.IncludeHidden && strings.HasPrefix(base, ".") {
			return nil
		}
		if opt.excluded(rel, base) {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if !opt.FollowSymlinks {
				opt.warn("skipping symlink %s", rel)
				return nil
			}
			target, err := os.Stat(p)
			if err != nil {
				opt.warn("skipping unresolvable symlink %s: %v", rel, err)
				return nil
			}
			if target.IsDir() {
				opt.warn("skipping directory symlink %s", rel)
				return nil
			}
			fi = target
		}
		if !fi.Mode().IsRegular() {
			opt.warn("skipping non-regular file %s", rel)
			return nil
		}
		entries = append(entries, Entry{Path: rel, Size: fi.Size(), Mode: fi.Mode(), Info: fi})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (o Options) excluded(rel, base string) bool {
	for _, pat := range o.Exclude {
		if ok, err := path.Match(pat, rel); err == nil && ok {
			return true
		}
		if ok, err := path.Match(pat, base); err == nil && ok {
			return true
		}
	}
	return false
}

// ChunkOptionsFor picks the chunker parameters for a file. The choice depends
// only on role and size so that source and destination always agree.
func ChunkOptionsFor(role layout.Role, size int64, pin chunk.Options) chunk.Options {
	if !pin.IsZero() {
		return pin.Normalized()
	}
	if role == layout.RoleWeight {
		return chunk.OptionsForSize(size)
	}
	// Non-weight files are configs and tokenizers: small, and worth chunking
	// finely because a one-line config edit should not resend the file.
	return chunk.SmallFileOptions()
}

// Build walks the root, hashes every file and returns the manifest.
func Build(ctx context.Context, opt Options) (*manifest.Manifest, error) {
	entries, err := Walk(opt)
	if err != nil {
		return nil, err
	}

	lfiles := make([]layout.File, len(entries))
	for i, e := range entries {
		lfiles[i] = layout.File{Path: e.Path, Size: e.Size}
	}
	info := layout.Detect(opt.Root, lfiles)

	var totalBytes int64
	for _, e := range entries {
		totalBytes += e.Size
	}
	opt.Bar.SetTotal(totalBytes)

	m := manifest.New(opt.Tool, defaultChunker(entries, info, opt.Pin))
	m.Model.Root = opt.Root
	m.Files = make([]*manifest.File, len(entries))

	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	sem := make(chan struct{}, opt.jobs())

	for i, e := range entries {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			if firstErr != nil {
				return nil, firstErr
			}
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(i int, e Entry) {
			defer wg.Done()
			defer func() { <-sem }()

			f, err := hashEntry(ctx, root, e, info.RoleOf(e.Path), opt)
			if err != nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("%s: %w", e.Path, err)
					cancel()
				})
				return
			}
			m.Files[i] = f
		}(i, e)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.Finalize(info)
	return m, nil
}

// defaultChunker records a representative parameter set in the manifest
// header. Per-file parameters always win; the header is informational.
func defaultChunker(entries []Entry, info layout.Info, pin chunk.Options) chunk.Options {
	if !pin.IsZero() {
		return pin.Normalized()
	}
	var largest int64
	for _, e := range entries {
		if info.RoleOf(e.Path) == layout.RoleWeight && e.Size > largest {
			largest = e.Size
		}
	}
	if largest == 0 {
		return chunk.SmallFileOptions()
	}
	return chunk.OptionsForSize(largest)
}

func hashEntry(ctx context.Context, root string, e Entry, role layout.Role, opt Options) (*manifest.File, error) {
	abs := filepath.Join(root, filepath.FromSlash(e.Path))
	copt := ChunkOptionsFor(role, e.Size, opt.Pin)

	cfg := chunk.Config{
		// Each file already gets a worker; keep per-file parallelism modest so
		// that a directory of many files and a single huge file both saturate.
		Jobs:     hashJobs(e.Size),
		Progress: func(n int64) { opt.Bar.Add(n) },
	}
	sig, err := chunk.HashPath(ctx, abs, copt, cfg)
	if err != nil {
		return nil, err
	}
	if sig.Size != e.Size {
		opt.warn("%s changed while being read (%d -> %d bytes)", e.Path, e.Size, sig.Size)
	}

	f := &manifest.File{
		Path:    e.Path,
		Size:    sig.Size,
		Mode:    uint32(e.Info.Mode().Perm()),
		ModTime: e.Info.ModTime().UTC(),
		Role:    role,
		Digest:  sig.Digest,
		Chunker: manifest.ChunkerFrom(copt),
		Chunks:  make([]manifest.Chunk, len(sig.Chunks)),
	}
	for i, c := range sig.Chunks {
		f.Chunks[i] = manifest.Chunk{Offset: c.Offset, Length: c.Length, Digest: c.Digest}
	}
	return f, nil
}

func hashJobs(size int64) int {
	switch {
	case size >= 1<<30:
		return 8
	case size >= 64<<20:
		return 4
	default:
		return 1
	}
}

// ErrNotDir reports that a path expected to be a model directory is not one.
var ErrNotDir = errors.New("scan: not a directory")
