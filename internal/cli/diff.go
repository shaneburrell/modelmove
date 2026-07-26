package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/manifest"
	"github.com/shaneburrell/modelmove/internal/units"
)

func newDiffCmd() *cobra.Command {
	var (
		asJSON   bool
		limit    int
		exitCode bool
	)
	cmd := &cobra.Command{
		Use:   "diff <old-manifest> <new-manifest>",
		Short: "Show what a sync between two manifests would move",
		Long: `Compare two manifests and report the delta.

This is the same calculation a transfer performs, so it answers "how much will
this fine-tune actually cost me to ship?" without touching the network.`,
		Example: `  modelmove diff base.manifest finetune.manifest
  modelmove diff base.manifest finetune.manifest --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldM, err := manifest.Load(args[0])
			if err != nil {
				return err
			}
			newM, err := manifest.Load(args[1])
			if err != nil {
				return err
			}
			if err := oldM.Validate(); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			if err := newM.Validate(); err != nil {
				return fmt.Errorf("%s: %w", args[1], err)
			}

			d := manifest.Compare(oldM, newM)
			if asJSON {
				if err := writeJSON(cmd.OutOrStdout(), d); err != nil {
					return err
				}
			} else {
				printDiff(cmd.OutOrStdout(), args[0], args[1], d, limit)
			}
			if exitCode && !d.Identical {
				return mismatch(fmt.Errorf("manifests differ"))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	f.IntVar(&limit, "limit", 20, "maximum number of changed files to print (0 = all)")
	f.BoolVar(&exitCode, "exit-code", false, "exit with status 2 when the manifests differ")
	return cmd
}

func printDiff(w io.Writer, oldName, newName string, d manifest.Diff, limit int) {
	fmt.Fprintf(w, "old   %s  (%s)\n", oldName, units.Bytes(d.OldBytes))
	fmt.Fprintf(w, "new   %s  (%s)\n", newName, units.Bytes(d.NewBytes))
	fmt.Fprintf(w, "files %d added, %d removed, %d modified, %d unchanged\n",
		d.Added, d.Removed, d.Modified, d.Unchanged)
	fmt.Fprintf(w, "bytes %s to transfer, %s reusable (%.1f%% of the new model already exists)\n",
		units.Bytes(d.TransferByte), units.Bytes(d.ReusableByte), 100*d.Similarity())

	if d.Identical {
		fmt.Fprintln(w, "\nmanifests are identical")
		return
	}

	changed := make([]manifest.FileDiff, 0, len(d.Files))
	for _, f := range d.Files {
		if f.Change != manifest.ChangeUnchanged {
			changed = append(changed, f)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].NewBytes != changed[j].NewBytes {
			return changed[i].NewBytes > changed[j].NewBytes
		}
		return changed[i].Path < changed[j].Path
	})

	fmt.Fprintln(w)
	for i, f := range changed {
		if limit > 0 && i >= limit {
			fmt.Fprintf(w, "  ... and %d more\n", len(changed)-i)
			break
		}
		switch f.Change {
		case manifest.ChangeRemoved:
			fmt.Fprintf(w, "  %-9s %-52s %10s\n", f.Change, truncate(f.Path, 52), units.Bytes(f.OldSize))
		default:
			fmt.Fprintf(w, "  %-9s %-52s %10s of %10s\n",
				f.Change, truncate(f.Path, 52), units.Bytes(f.NewBytes), units.Bytes(f.NewSize))
		}
	}
}
