package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/engine"
	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/units"
)

func newManifestCmd() *cobra.Command {
	var (
		out        string
		format     string
		jobs       int
		exclude    []string
		hidden     bool
		noLinks    bool
		avgSize    string
		minSize    string
		maxSize    string
		noProgress bool
		summary    bool
	)
	cmd := &cobra.Command{
		Use:   "manifest <dir>",
		Short: "Hash a model directory and write its manifest",
		Long: `Produce the content-addressed manifest of a model directory.

The manifest lists every file with its BLAKE3 digest and its FastCDC chunk
boundaries, which is everything another machine needs to work out what it is
missing. It goes to stdout unless --out is given, so progress is written to
stderr and "modelmove manifest DIR > model.manifest" works as expected.`,
		Example: `  modelmove manifest ./llama-3-8b > model.manifest
  modelmove manifest ./llama-3-8b --out model.mmm --format binary
  modelmove manifest ./llama-3-8b --summary`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := &transferFlags{
				exclude:       exclude,
				includeHidden: hidden,
				noFollowLinks: noLinks,
				avgSize:       avgSize,
				minSize:       minSize,
				maxSize:       maxSize,
			}
			pin, err := flags.pinnedOptions()
			if err != nil {
				return err
			}
			enc, err := manifest.ParseEncoding(format)
			if err != nil {
				return err
			}

			m, err := engine.BuildManifest(cmd.Context(), engine.Config{
				Source:         args[0],
				Exclude:        exclude,
				IncludeHidden:  hidden,
				FollowSymlinks: !noLinks,
				Pin:            pin,
				Jobs:           jobs,
				Tool:           Tool(),
				Progress:       !noProgress && !quiet,
				Warn:           warnf,
			})
			if err != nil {
				return err
			}

			if summary {
				return writeJSON(cmd.OutOrStdout(), manifestSummary(m))
			}
			if out == "" || out == "-" {
				if enc == manifest.EncodingAuto {
					enc = manifest.EncodingJSON
				}
				if enc == manifest.EncodingBinary && isTerminal(os.Stdout) {
					return fmt.Errorf("refusing to write binary manifest to a terminal; use --out")
				}
				return manifest.Write(cmd.OutOrStdout(), m, enc)
			}
			if err := manifest.Save(out, m, enc); err != nil {
				return err
			}
			if !quiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s (%d files, %d chunks, %s)\n",
					out, len(m.Files), m.TotalChunks(), units.Bytes(m.TotalBytes()))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&out, "out", "o", "", "write to this path instead of stdout")
	f.StringVar(&format, "format", "auto", "encoding: auto|json|binary")
	f.IntVarP(&jobs, "jobs", "j", 0, "parallel hashing jobs (0 = auto)")
	f.StringSliceVar(&exclude, "exclude", nil, "glob patterns to skip (repeatable)")
	f.BoolVar(&hidden, "include-hidden", false, "include dot-files and dot-directories")
	f.BoolVar(&noLinks, "no-follow-symlinks", false, "skip symlinks instead of hashing their targets")
	f.StringVar(&avgSize, "avg-size", "", "pin the average chunk size (e.g. 1MiB)")
	f.StringVar(&minSize, "min-size", "", "pin the minimum chunk size")
	f.StringVar(&maxSize, "max-size", "", "pin the maximum chunk size")
	f.BoolVar(&noProgress, "no-progress", false, "disable the progress bar")
	f.BoolVar(&summary, "summary", false, "print a JSON summary instead of the full manifest")
	return cmd
}

type summaryOut struct {
	Name        string `json:"name,omitempty"`
	Kind        string `json:"kind"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	WeightFiles int    `json:"weight_files"`
	WeightBytes int64  `json:"weight_bytes"`
	ShardSets   int    `json:"shard_sets"`
	Chunks      int    `json:"chunks"`
	UniqueChunk int    `json:"unique_chunks"`
	UniqueBytes int64  `json:"unique_bytes"`
	Digest      string `json:"digest"`
}

func manifestSummary(m *manifest.Manifest) summaryOut {
	uniq, uniqBytes := m.UniqueChunks()
	return summaryOut{
		Name:        m.Model.Name,
		Kind:        string(m.Model.Kind),
		Files:       m.Model.Files,
		Bytes:       m.Model.Size,
		WeightFiles: m.Model.WeightFiles,
		WeightBytes: m.Model.WeightBytes,
		ShardSets:   len(m.Model.Shards),
		Chunks:      m.TotalChunks(),
		UniqueChunk: uniq,
		UniqueBytes: uniqBytes,
		Digest:      m.Model.Digest.String(),
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
