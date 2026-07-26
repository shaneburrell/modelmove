package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/chunk"
	"github.com/shaneburrell/modelmove/internal/engine"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/receiver"
	"github.com/shaneburrell/modelmove/internal/units"
)

// transferFlags holds the flags shared by copy and sync.
type transferFlags struct {
	exclude        []string
	includeHidden  bool
	noFollowLinks  bool
	avgSize        string
	minSize        string
	maxSize        string
	jobs           int
	dryRun         bool
	delete         bool
	fast           bool
	noResume       bool
	noVerify       bool
	noDedupe       bool
	atomic         string
	noTimes        bool
	sshCommand     string
	sshOptions     []string
	remoteBin      string
	manifestOut    string
	manifestFormat string
	json           bool
	noProgress     bool
}

func (t *transferFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringSliceVar(&t.exclude, "exclude", nil, "glob patterns to skip (repeatable)")
	f.BoolVar(&t.includeHidden, "include-hidden", false, "include dot-files and dot-directories")
	f.BoolVar(&t.noFollowLinks, "no-follow-symlinks", false, "skip symlinks instead of copying their targets")
	f.StringVar(&t.avgSize, "avg-size", "", "pin the average chunk size (e.g. 1MiB); default scales with file size")
	f.StringVar(&t.minSize, "min-size", "", "pin the minimum chunk size")
	f.StringVar(&t.maxSize, "max-size", "", "pin the maximum chunk size")
	f.IntVarP(&t.jobs, "jobs", "j", 0, "parallel hashing jobs (0 = auto)")
	f.BoolVarP(&t.dryRun, "dry-run", "n", false, "show the transfer plan and stop")
	f.BoolVar(&t.fast, "fast", false, "trust size and mtime instead of re-hashing the destination")
	f.BoolVar(&t.noResume, "no-resume", false, "ignore partially staged files from an interrupted run")
	f.BoolVar(&t.noVerify, "no-verify", false, "skip the BLAKE3 check before replacing a file (not recommended)")
	f.BoolVar(&t.noDedupe, "no-dedupe", false, "only reuse chunks from the same path, not from other files")
	f.StringVar(&t.atomic, "atomic", "file", "when to swap in staged files: file|model")
	f.BoolVar(&t.noTimes, "no-times", false, "do not preserve modification times")
	f.StringVar(&t.sshCommand, "ssh", "", "ssh command to use (default \"ssh\")")
	f.StringSliceVar(&t.sshOptions, "ssh-option", nil, "extra argument passed to ssh (repeatable)")
	f.StringVar(&t.remoteBin, "remote-bin", "", "path to modelmove on the remote host (default \"modelmove\")")
	f.StringVar(&t.manifestOut, "manifest", "", "also write the source manifest to this path")
	f.StringVar(&t.manifestFormat, "manifest-format", "auto", "manifest encoding: auto|json|binary")
	f.BoolVar(&t.json, "json", false, "emit machine-readable JSON")
	f.BoolVar(&t.noProgress, "no-progress", false, "disable the progress bar")
}

// pinnedOptions turns the size flags into chunker parameters. Any of the three
// may be given; the rest are derived.
func (t *transferFlags) pinnedOptions() (chunk.Options, error) {
	var opt chunk.Options
	for _, spec := range []struct {
		name  string
		value string
		dst   *uint32
	}{
		{"--avg-size", t.avgSize, &opt.AvgSize},
		{"--min-size", t.minSize, &opt.MinSize},
		{"--max-size", t.maxSize, &opt.MaxSize},
	} {
		if spec.value == "" {
			continue
		}
		n, err := units.ParseSize(spec.value)
		if err != nil {
			return opt, fmt.Errorf("%s: %w", spec.name, err)
		}
		if n > uint64(^uint32(0)) {
			return opt, fmt.Errorf("%s: %s is too large", spec.name, spec.value)
		}
		*spec.dst = uint32(n)
	}
	if opt.IsZero() {
		return chunk.Options{}, nil
	}
	opt = opt.Normalized()
	if err := opt.Validate(); err != nil {
		return opt, err
	}
	return opt, nil
}

func (t *transferFlags) config(src, dst string) (engine.Config, error) {
	pin, err := t.pinnedOptions()
	if err != nil {
		return engine.Config{}, err
	}
	atomic, err := receiver.ParseAtomicMode(t.atomic)
	if err != nil {
		return engine.Config{}, err
	}
	enc, err := manifest.ParseEncoding(t.manifestFormat)
	if err != nil {
		return engine.Config{}, err
	}
	return engine.Config{
		Source:         src,
		Dest:           dst,
		Exclude:        t.exclude,
		IncludeHidden:  t.includeHidden,
		FollowSymlinks: !t.noFollowLinks,
		Pin:            pin,
		Jobs:           t.jobs,
		DryRun:         t.dryRun,
		Delete:         t.delete,
		Fast:           t.fast,
		Resume:         !t.noResume,
		Verify:         !t.noVerify,
		Dedupe:         !t.noDedupe,
		Atomic:         atomic,
		PreserveTimes:  !t.noTimes,
		SSHCommand:     t.sshCommand,
		SSHOptions:     t.sshOptions,
		RemoteBin:      t.remoteBin,
		ManifestOut:    t.manifestOut,
		ManifestFormat: enc,
		Tool:           Tool(),
		Progress:       !t.noProgress && !t.json && !quiet,
		Warn:           warnf,
	}, nil
}
