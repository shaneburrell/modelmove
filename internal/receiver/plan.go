package receiver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/scan"
)

// Action describes what will happen to one path.
type Action string

// Plan actions.
const (
	ActionCopy   Action = "copy"   // not present at the destination
	ActionUpdate Action = "update" // present but different
	ActionSkip   Action = "skip"   // already byte-identical
)

// FilePlan is the transfer plan for a single file.
type FilePlan struct {
	Path        string         `json:"path"`
	Action      Action         `json:"action"`
	Size        int64          `json:"size"`
	Role        layout.Role    `json:"role,omitempty"`
	Need        []chunk.Digest `json:"need,omitempty"`
	NeedChunks  int            `json:"need_chunks"`
	NeedBytes   int64          `json:"need_bytes"`
	ReuseBytes  int64          `json:"reuse_bytes"`
	StagedBytes int64          `json:"staged_bytes,omitempty"`
}

// Plan is the full transfer plan for a manifest against a destination.
type Plan struct {
	Root        string     `json:"root"`
	Files       []FilePlan `json:"files"`
	Deletes     []string   `json:"deletes,omitempty"`
	TotalFiles  int        `json:"total_files"`
	TotalBytes  int64      `json:"total_bytes"`
	NeedBytes   int64      `json:"need_bytes"`
	ReuseBytes  int64      `json:"reuse_bytes"`
	SkipFiles   int        `json:"skip_files"`
	SkipBytes   int64      `json:"skip_bytes"`
	CopyFiles   int        `json:"copy_files"`
	UpdateFiles int        `json:"update_files"`
	ScannedDest int64      `json:"scanned_dest_bytes"`
}

// Savings returns the fraction of bytes the plan avoids moving.
func (p *Plan) Savings() float64 {
	if p.TotalBytes == 0 {
		return 1
	}
	return 1 - float64(p.NeedBytes)/float64(p.TotalBytes)
}

// Work returns only the files that need bytes written.
func (p *Plan) Work() []FilePlan {
	out := make([]FilePlan, 0, len(p.Files))
	for _, f := range p.Files {
		if f.Action != ActionSkip {
			out = append(out, f)
		}
	}
	return out
}

// destFile is one file discovered at the destination during planning.
type destFile struct {
	rel     string
	abs     string
	size    int64
	modTime int64
	sig     chunk.Signature
	hashed  bool
}

// Plan inspects the destination and computes what has to move. It is the
// expensive half of a sync: every candidate destination file is chunked so
// that its contents can be reused.
func (r *Receiver) Plan(ctx context.Context, m *manifest.Manifest) (*Plan, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	r.manifest = m

	if err := os.MkdirAll(r.root, 0o755); err != nil {
		return nil, fmt.Errorf("receiver: create destination: %w", err)
	}

	existing, err := scan.Walk(scan.Options{
		Root:           r.root,
		IncludeHidden:  true,
		FollowSymlinks: false,
		Warn:           r.opt.warnf,
	})
	if err != nil {
		return nil, err
	}

	wanted := make(map[string]*manifest.File, len(m.Files))
	for _, f := range m.Files {
		wanted[f.Path] = f
	}

	// Decide which destination files are worth hashing. Without --dedupe we
	// only care about the paths the manifest mentions.
	var candidates []*destFile
	byPath := make(map[string]*destFile, len(existing))
	for _, e := range existing {
		want, inManifest := wanted[e.Path]
		if !inManifest && !r.opt.Dedupe {
			continue
		}
		df := &destFile{
			rel:     e.Path,
			abs:     filepath.Join(r.root, filepath.FromSlash(e.Path)),
			size:    e.Size,
			modTime: e.Info.ModTime().UnixNano(),
		}
		// --fast trusts size and mtime for files the manifest already
		// describes, which skips reading the whole destination model.
		if r.opt.Fast && inManifest && want.Size == e.Size && want.ModTime.UnixNano() == df.modTime {
			byPath[e.Path] = df
			continue
		}
		candidates = append(candidates, df)
		byPath[e.Path] = df
	}

	r.hashDest(ctx, candidates, wanted)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	plan := &Plan{Root: r.root}
	for _, df := range candidates {
		plan.ScannedDest += df.size
	}

	// Chunks that stay readable for the whole run go in the global index.
	// Under per-file atomicity the previous contents of a file that is about
	// to be replaced are only usable while that file is being written.
	replaced := make(map[string]*index)

	for _, df := range byPath {
		want, inManifest := wanted[df.rel]
		switch {
		case !inManifest:
			if r.opt.Dedupe && df.hashed {
				r.addSignature(r.global, df.abs, df.sig)
			}
		case !df.hashed:
			// Trusted by --fast: index it using the manifest's own chunk list.
			r.addManifestChunks(r.global, df.abs, want)
		case df.sig.Digest == want.Digest && df.sig.Size == want.Size:
			r.addSignature(r.global, df.abs, df.sig)
		case r.opt.Atomic == AtomicModel:
			// Nothing is replaced until Finish, so old contents stay usable.
			r.addSignature(r.global, df.abs, df.sig)
		default:
			idx := newIndex()
			r.addSignature(idx, df.abs, df.sig)
			replaced[df.rel] = idx
		}
	}
	r.replaced = replaced

	// Partially written files from an interrupted run are just another source
	// of local chunks, and they are verified by digest like any other.
	if r.opt.Resume {
		r.indexStage(ctx, m)
	}

	// Simulate the transfer in order. Each finished file makes its chunks
	// available to the ones after it.
	available := newIndexView(r.global, r.staged)
	order := transferOrder(m)
	plan.Files = make([]FilePlan, 0, len(m.Files))

	for _, rel := range order {
		f := wanted[rel]
		fp := FilePlan{Path: rel, Action: ActionCopy, Size: f.Size, Role: f.Role}

		df, exists := byPath[rel]
		switch {
		case !exists:
			fp.Action = ActionCopy
		case !df.hashed:
			fp.Action = ActionSkip
		case df.sig.Digest == f.Digest && df.sig.Size == f.Size:
			fp.Action = ActionSkip
		default:
			fp.Action = ActionUpdate
		}

		if fp.Action == ActionSkip {
			plan.SkipFiles++
			plan.SkipBytes += f.Size
			plan.TotalBytes += f.Size
			plan.Files = append(plan.Files, fp)
			// A skipped file's contents are already correct and available.
			continue
		}

		local := replaced[rel]
		seen := make(map[chunk.Digest]struct{})
		for _, c := range f.Chunks {
			if _, dup := seen[c.Digest]; dup {
				fp.ReuseBytes += int64(c.Length)
				continue
			}
			seen[c.Digest] = struct{}{}
			if available.has(c.Digest) || (local != nil && local.has(c.Digest)) {
				fp.ReuseBytes += int64(c.Length)
				continue
			}
			fp.Need = append(fp.Need, c.Digest)
			fp.NeedChunks++
			fp.NeedBytes += int64(c.Length)
		}
		switch fp.Action {
		case ActionCopy:
			plan.CopyFiles++
		case ActionUpdate:
			plan.UpdateFiles++
		}
		plan.TotalBytes += f.Size
		plan.NeedBytes += fp.NeedBytes
		plan.ReuseBytes += fp.ReuseBytes
		plan.Files = append(plan.Files, fp)

		// After this file lands, all of its chunks exist at the destination.
		for _, c := range f.Chunks {
			available.mark(c.Digest)
		}
	}

	plan.TotalFiles = len(plan.Files)
	if r.opt.Delete {
		for _, e := range existing {
			if _, ok := wanted[e.Path]; !ok {
				plan.Deletes = append(plan.Deletes, e.Path)
			}
		}
	}

	r.plan = plan
	r.planByPath = make(map[string]FilePlan, len(plan.Files))
	for _, fp := range plan.Files {
		r.planByPath[fp.Path] = fp
	}
	return plan, nil
}

// transferOrder returns manifest paths ordered so that small metadata lands
// before multi-gigabyte weight shards.
func transferOrder(m *manifest.Manifest) []string {
	files := make([]layout.File, len(m.Files))
	roles := make(map[string]layout.Role, len(m.Files))
	for i, f := range m.Files {
		files[i] = layout.File{Path: f.Path, Size: f.Size}
		roles[f.Path] = f.Role
	}
	return layout.TransferOrder(files, roles)
}

// hashDest chunks candidate destination files in parallel.
func (r *Receiver) hashDest(ctx context.Context, files []*destFile, wanted map[string]*manifest.File) {
	if len(files) == 0 {
		return
	}
	jobs := r.opt.Jobs
	if jobs <= 0 {
		jobs = runtime.NumCPU()
		if jobs > 8 {
			jobs = 8
		}
	}
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup

	for _, df := range files {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(df *destFile) {
			defer wg.Done()
			defer func() { <-sem }()

			// Boundaries only line up if both sides used the same parameters,
			// so prefer the ones the source recorded for this path. Files the
			// manifest does not mention fall back to the size-derived rule,
			// which is what the source would have used for them too.
			var opt chunk.Options
			if want, ok := wanted[df.rel]; ok {
				opt = want.Options()
			} else {
				opt = scan.ChunkOptionsFor(roleGuess(df.size), df.size, r.opt.Pin)
			}
			sig, err := chunk.HashPath(ctx, df.abs, opt, chunk.Config{Jobs: 2})
			if err != nil {
				r.opt.warnf("cannot read destination file %s: %v", df.rel, err)
				return
			}
			df.sig = sig
			df.hashed = true
			r.bar.Add(sig.Size)
		}(df)
	}
	wg.Wait()
}

func roleGuess(size int64) layout.Role {
	if size >= chunk.LargeFileThreshold {
		return layout.RoleWeight
	}
	return layout.RoleOther
}

func (r *Receiver) addSignature(idx *index, abs string, sig chunk.Signature) {
	for _, c := range sig.Chunks {
		idx.add(c.Digest, location{path: abs, offset: int64(c.Offset), length: c.Length})
	}
}

func (r *Receiver) addManifestChunks(idx *index, abs string, f *manifest.File) {
	for _, c := range f.Chunks {
		idx.add(c.Digest, location{path: abs, offset: int64(c.Offset), length: c.Length})
	}
}

// indexStage hashes leftover staging files so an interrupted transfer resumes
// where it stopped instead of starting the file again.
func (r *Receiver) indexStage(ctx context.Context, m *manifest.Manifest) {
	for _, f := range m.Files {
		stage := r.stagePath(f.Path)
		fi, err := os.Stat(stage)
		if err != nil || fi.Size() != f.Size {
			continue
		}
		sig, err := chunk.HashPath(ctx, stage, f.Options(), chunk.Config{Jobs: 2, SkipFileDigest: true})
		if err != nil {
			r.opt.warnf("ignoring unreadable staging file for %s: %v", f.Path, err)
			continue
		}
		// A staging file is written chunk-aligned, so matching offsets are the
		// only ones that count.
		want := make(map[uint64]manifest.Chunk, len(f.Chunks))
		for _, c := range f.Chunks {
			want[c.Offset] = c
		}
		n := 0
		for _, c := range sig.Chunks {
			w, ok := want[c.Offset]
			if !ok || w.Length != c.Length || w.Digest != c.Digest {
				continue
			}
			r.staged.add(c.Digest, location{path: stage, offset: int64(c.Offset), length: c.Length})
			n++
		}
		if n > 0 {
			r.opt.warnf("resuming %s with %d/%d chunks already staged", f.Path, n, len(f.Chunks))
		}
	}
}

// indexView is the union of the indexes visible while planning, plus the
// digests that earlier files in the plan will have produced by the time a
// later file is written.
type indexView struct {
	sources []*index
	future  map[chunk.Digest]struct{}
}

func newIndexView(sources ...*index) *indexView {
	return &indexView{sources: sources, future: make(map[chunk.Digest]struct{})}
}

func (v *indexView) has(d chunk.Digest) bool {
	if _, ok := v.future[d]; ok {
		return true
	}
	for _, s := range v.sources {
		if s.has(d) {
			return true
		}
	}
	return false
}

func (v *indexView) mark(d chunk.Digest) { v.future[d] = struct{}{} }
