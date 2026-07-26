package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/engine"
	"github.com/shaneburrell/modelmove/internal/units"
)

func newVerifyCmd() *cobra.Command {
	var (
		manifestPath string
		jobs         int
		quick        bool
		extra        bool
		asJSON       bool
		noProgress   bool
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "verify <dir>",
		Short: "Re-hash a model directory and check it against a manifest",
		Long: `Verify that a model directory still matches a manifest.

With no --manifest, the manifest modelmove recorded in <dir>/.modelmove is
used. A mismatch exits with status 2, so scripts can tell corruption apart
from a failed command.`,
		Example: `  modelmove verify /models/llama-3-8b
  modelmove verify /models/llama-3-8b --manifest model.manifest
  modelmove verify /models/llama-3-8b --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := engine.Verify(cmd.Context(), engine.VerifyConfig{
				Root:         args[0],
				ManifestPath: manifestPath,
				Jobs:         jobs,
				Quick:        quick,
				Extra:        extra,
				Progress:     !noProgress && !asJSON && !quiet,
				Warn:         warnf,
			})
			if err != nil {
				return err
			}
			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				printVerify(cmd.OutOrStdout(), res, limit)
			}
			if !res.OK {
				return mismatch(fmt.Errorf("verification failed: %d of %d files do not match",
					len(res.Problems), res.Files))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&manifestPath, "manifest", "m", "", "manifest to check against (default: <dir>/.modelmove/manifest.json)")
	f.IntVarP(&jobs, "jobs", "j", 0, "parallel hashing jobs (0 = auto)")
	f.BoolVar(&quick, "quick", false, "report failing files without locating the bad chunks")
	f.BoolVar(&extra, "extra", false, "also report files present in the directory but not the manifest")
	f.BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	f.BoolVar(&noProgress, "no-progress", false, "disable the progress bar")
	f.IntVar(&limit, "limit", 20, "maximum number of problems to print (0 = all)")
	return cmd
}

func printVerify(w io.Writer, res *engine.VerifyResult, limit int) {
	if quiet && res.OK {
		return
	}
	fmt.Fprintf(w, "dir       %s\n", res.Root)
	fmt.Fprintf(w, "manifest  %s\n", res.ManifestPath)
	fmt.Fprintf(w, "model     %s", res.Model.Kind)
	if res.Model.Name != "" {
		fmt.Fprintf(w, " %s", res.Model.Name)
	}
	fmt.Fprintf(w, ", %d files, %s\n", res.Model.Files, units.Bytes(res.Model.Bytes))
	fmt.Fprintf(w, "checked   %d/%d files, %s read\n", res.FilesOK, res.Files, units.Bytes(res.BytesChecked))

	if res.OK {
		fmt.Fprintln(w, "result    ok")
		return
	}

	fmt.Fprintf(w, "result    FAILED (%d problem file(s))\n", len(res.Problems))
	shown := res.Problems
	if limit > 0 && len(shown) > limit {
		shown = shown[:limit]
	}
	for _, p := range shown {
		fmt.Fprintf(w, "\n  %s: %s\n", p.Path, p.Problem)
		switch p.Problem {
		case engine.ProblemSize:
			fmt.Fprintf(w, "    size %d, manifest says %d\n", p.GotSize, p.WantSize)
		case engine.ProblemDigest:
			fmt.Fprintf(w, "    want %s\n    got  %s\n", p.Want, p.Got)
			if n := len(p.BadChunks); n > 0 {
				fmt.Fprintf(w, "    %d bad chunk(s), %s affected\n", n, units.Bytes(p.BadBytes))
				for i, c := range p.BadChunks {
					if i >= 5 {
						fmt.Fprintf(w, "      ... and %d more\n", n-i)
						break
					}
					fmt.Fprintf(w, "      offset %d length %d\n", c.Offset, c.Length)
				}
			}
		case engine.ProblemUnreadable:
			fmt.Fprintf(w, "    %s\n", p.Error)
		}
	}
	if len(shown) < len(res.Problems) {
		fmt.Fprintf(w, "\n  ... and %d more problem file(s)\n", len(res.Problems)-len(shown))
	}
	if len(res.Extra) > 0 {
		fmt.Fprintf(w, "\n  %d file(s) not in the manifest:\n", len(res.Extra))
		for i, p := range res.Extra {
			if limit > 0 && i >= limit {
				fmt.Fprintf(w, "    ... and %d more\n", len(res.Extra)-i)
				break
			}
			fmt.Fprintf(w, "    %s\n", p)
		}
	}
}
