// Package engine orchestrates a transfer: it scans the source into a
// manifest, asks the destination what it is missing, and streams only that.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/layout"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/progress"
	"github.com/shaneburrell/modelmove/internal/receiver"
	"github.com/shaneburrell/modelmove/internal/scan"
	"github.com/shaneburrell/modelmove/internal/transport"
)

// Config describes one copy or sync run.
type Config struct {
	Source string
	Dest   string

	Exclude        []string
	IncludeHidden  bool
	FollowSymlinks bool
	Pin            chunk.Options
	Jobs           int

	DryRun        bool
	Delete        bool
	Fast          bool
	Resume        bool
	Verify        bool
	Dedupe        bool
	Atomic        receiver.AtomicMode
	PreserveTimes bool

	SSHCommand string
	SSHOptions []string
	RemoteBin  string

	// ManifestOut, if set, receives the source manifest.
	ManifestOut    string
	ManifestFormat manifest.Encoding

	Tool     string
	Progress bool
	Warn     func(format string, args ...any)
}

// Result is the outcome of a transfer.
type Result struct {
	Source      string             `json:"source"`
	Dest        string             `json:"dest"`
	DryRun      bool               `json:"dry_run"`
	Model       ModelInfo          `json:"model"`
	Plan        *receiver.Plan     `json:"plan,omitempty"`
	Summary     *receiver.Summary  `json:"summary,omitempty"`
	ScanSeconds float64            `json:"scan_seconds"`
	Seconds     float64            `json:"seconds"`
	Manifest    string             `json:"manifest,omitempty"`
	manifestObj *manifest.Manifest `json:"-"`
}

// ModelInfo summarises what was found at the source.
type ModelInfo struct {
	Name        string       `json:"name,omitempty"`
	Kind        layout.Kind  `json:"kind"`
	Files       int          `json:"files"`
	Bytes       int64        `json:"bytes"`
	WeightFiles int          `json:"weight_files"`
	WeightBytes int64        `json:"weight_bytes"`
	Shards      int          `json:"shard_sets"`
	Chunks      int          `json:"chunks"`
	Digest      chunk.Digest `json:"digest"`
}

// Manifest returns the source manifest produced by the run.
func (r *Result) Manifests() *manifest.Manifest { return r.manifestObj }

// TransferRate returns the effective bytes per second of the whole run.
func (r *Result) TransferRate() float64 {
	if r.Summary == nil || r.Seconds <= 0 {
		return 0
	}
	return float64(r.Summary.BytesTotal) / r.Seconds
}

// Run scans the source and applies it to the destination.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	start := time.Now()

	srcEndpoint, err := transport.Parse(cfg.Source)
	if err != nil {
		return nil, err
	}
	if !srcEndpoint.IsLocal() {
		return nil, fmt.Errorf("engine: the source must be a local directory (got %s); run modelmove on the machine that holds the model", cfg.Source)
	}
	srcPath, err := srcEndpoint.LocalPath()
	if err != nil {
		return nil, err
	}
	dstEndpoint, err := transport.Parse(cfg.Dest)
	if err != nil {
		return nil, err
	}
	if dstEndpoint.IsLocal() {
		if err := checkNotNested(srcPath, dstEndpoint.Path); err != nil {
			return nil, err
		}
	}

	// Phase 1: hash the source.
	scanStart := time.Now()
	bar := progress.NewFor(cfg.Progress, progress.Options{Label: "scan  "})
	m, err := scan.Build(ctx, scan.Options{
		Root:           srcPath,
		Exclude:        cfg.Exclude,
		IncludeHidden:  cfg.IncludeHidden,
		FollowSymlinks: cfg.FollowSymlinks,
		Pin:            cfg.Pin,
		Jobs:           cfg.Jobs,
		Tool:           cfg.Tool,
		Bar:            bar,
		Warn:           cfg.Warn,
	})
	bar.Finish()
	if err != nil {
		return nil, err
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("engine: %s contains no files to transfer", srcPath)
	}
	scanSeconds := time.Since(scanStart).Seconds()

	res := &Result{
		Source:      srcPath,
		Dest:        dstEndpoint.String(),
		DryRun:      cfg.DryRun,
		Model:       modelInfo(m),
		ScanSeconds: scanSeconds,
		manifestObj: m,
	}

	if cfg.ManifestOut != "" {
		if err := manifest.Save(cfg.ManifestOut, m, cfg.ManifestFormat); err != nil {
			return nil, fmt.Errorf("engine: writing manifest: %w", err)
		}
		res.Manifest = cfg.ManifestOut
	}

	// Phase 2: ask the destination what it needs.
	planBar := progress.NewFor(cfg.Progress && dstEndpoint.IsLocal(), progress.Options{Label: "index "})
	sess, err := transport.Dial(ctx, dstEndpoint, transport.Config{
		Atomic:        cfg.Atomic,
		Dedupe:        cfg.Dedupe,
		Fast:          cfg.Fast,
		Resume:        cfg.Resume,
		Delete:        cfg.Delete,
		Verify:        cfg.Verify,
		PreserveTimes: cfg.PreserveTimes,
		Jobs:          cfg.Jobs,
		Pin:           cfg.Pin,
		Bar:           planBar,
		Warn:          cfg.Warn,
		SSHCommand:    cfg.SSHCommand,
		SSHOptions:    cfg.SSHOptions,
		RemoteBin:     cfg.RemoteBin,
		Tool:          cfg.Tool,
	})
	if err != nil {
		planBar.Finish()
		return nil, err
	}
	defer sess.Close()

	plan, err := sess.Plan(ctx, m)
	planBar.Finish()
	if err != nil {
		return nil, err
	}
	res.Plan = plan
	res.Dest = sess.Root()

	if cfg.DryRun {
		res.Seconds = time.Since(start).Seconds()
		return res, nil
	}

	// Phase 3: stream the missing chunks.
	xferBar := progress.NewFor(cfg.Progress, progress.Options{Label: "send  ", Total: plan.NeedBytes})
	err = transfer(ctx, sess, m, plan, xferBar)
	xferBar.Finish()
	if err != nil {
		return nil, err
	}

	sum, err := sess.Finish(ctx)
	if err != nil {
		return nil, err
	}
	res.Summary = sum
	res.Seconds = time.Since(start).Seconds()
	return res, nil
}

// transfer streams the chunks the plan asked for, file by file.
func transfer(ctx context.Context, sess transport.Session, m *manifest.Manifest, plan *receiver.Plan, bar *progress.Bar) error {
	for _, fp := range plan.Files {
		if fp.Action == receiver.ActionSkip {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f := m.Lookup(fp.Path)
		if f == nil {
			return fmt.Errorf("engine: plan references unknown file %q", fp.Path)
		}
		bar.SetLabel(fp.Path)
		if err := sendFile(ctx, sess, m.Model.Root, f, fp, bar); err != nil {
			return fmt.Errorf("%s: %w", fp.Path, err)
		}
	}
	return nil
}

func sendFile(ctx context.Context, sess transport.Session, root string, f *manifest.File, fp receiver.FilePlan, bar *progress.Bar) error {
	if err := sess.BeginFile(ctx, f.Path); err != nil {
		return err
	}

	if len(fp.Need) > 0 {
		src, err := os.Open(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		defer src.Close()

		// The plan lists digests in first-occurrence order, so reads walk the
		// file forwards even though they are random access.
		where := make(map[chunk.Digest]manifest.Chunk, len(f.Chunks))
		for _, c := range f.Chunks {
			if _, ok := where[c.Digest]; !ok {
				where[c.Digest] = c
			}
		}

		var buf []byte
		for _, d := range fp.Need {
			if err := ctx.Err(); err != nil {
				return err
			}
			c, ok := where[d]
			if !ok {
				return fmt.Errorf("chunk %s is not part of this file", d.Short())
			}
			if cap(buf) < int(c.Length) {
				buf = make([]byte, c.Length)
			}
			buf = buf[:c.Length]
			if _, err := src.ReadAt(buf, int64(c.Offset)); err != nil {
				return fmt.Errorf("reading offset %d: %w", c.Offset, err)
			}
			if got := chunk.Sum(buf); got != d {
				return fmt.Errorf("source changed while reading (offset %d)", c.Offset)
			}
			if err := sess.SendChunk(ctx, d, buf); err != nil {
				return err
			}
			bar.Add(int64(c.Length))
		}
	}

	res, err := sess.EndFile(ctx)
	if err != nil {
		return err
	}
	if res != nil && res.Status != "ok" {
		return fmt.Errorf("destination reported %s: %s", res.Status, res.Error)
	}
	return nil
}

func modelInfo(m *manifest.Manifest) ModelInfo {
	return ModelInfo{
		Name:        m.Model.Name,
		Kind:        m.Model.Kind,
		Files:       m.Model.Files,
		Bytes:       m.Model.Size,
		WeightFiles: m.Model.WeightFiles,
		WeightBytes: m.Model.WeightBytes,
		Shards:      len(m.Model.Shards),
		Chunks:      m.TotalChunks(),
		Digest:      m.Model.Digest,
	}
}

// checkNotNested refuses transfers where one side contains the other, which
// would otherwise scan its own output.
func checkNotNested(src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if absSrc == absDst {
		return fmt.Errorf("engine: source and destination are the same directory")
	}
	if rel, err := filepath.Rel(absSrc, absDst); err == nil && rel != ".." && !filepath.IsAbs(rel) && rel[0] != '.' {
		return fmt.Errorf("engine: destination %s is inside source %s", absDst, absSrc)
	}
	if rel, err := filepath.Rel(absDst, absSrc); err == nil && rel != ".." && !filepath.IsAbs(rel) && rel[0] != '.' {
		return fmt.Errorf("engine: source %s is inside destination %s", absSrc, absDst)
	}
	return nil
}

// BuildManifest scans a directory and returns its manifest.
func BuildManifest(ctx context.Context, cfg Config) (*manifest.Manifest, error) {
	e, err := transport.Parse(cfg.Source)
	if err != nil {
		return nil, err
	}
	if !e.IsLocal() {
		return nil, fmt.Errorf("engine: manifests can only be built from local directories")
	}
	path, err := e.LocalPath()
	if err != nil {
		return nil, err
	}
	bar := progress.NewFor(cfg.Progress, progress.Options{Label: "hash  "})
	defer bar.Finish()
	return scan.Build(ctx, scan.Options{
		Root:           path,
		Exclude:        cfg.Exclude,
		IncludeHidden:  cfg.IncludeHidden,
		FollowSymlinks: cfg.FollowSymlinks,
		Pin:            cfg.Pin,
		Jobs:           cfg.Jobs,
		Tool:           cfg.Tool,
		Bar:            bar,
		Warn:           cfg.Warn,
	})
}
