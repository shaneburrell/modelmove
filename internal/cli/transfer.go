package cli

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/engine"
	"github.com/shaneburrell/modelmove/internal/receiver"
	"github.com/shaneburrell/modelmove/internal/units"
)

func newCopyCmd() *cobra.Command {
	flags := &transferFlags{}
	cmd := &cobra.Command{
		Use:   "copy <src-dir> <dst>",
		Short: "Copy a model directory to a local path or over SSH",
		Long: `Copy a model directory to another location.

The destination may be a local path, "host:/path" or "ssh://user@host:port/path".
Files already present and identical at the destination are skipped, and files
that only partly changed transfer only their changed chunks.`,
		Example: `  modelmove copy ./llama-3-8b /models/llama-3-8b
  modelmove copy ./llama-3-8b gpu-box:/srv/models/llama-3-8b
  modelmove copy ./llama-3-8b gpu-box:/srv/models/llama-3-8b --dry-run`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTransfer(cmd, flags, args[0], args[1])
		},
	}
	flags.register(cmd)
	return cmd
}

func newSyncCmd() *cobra.Command {
	flags := &transferFlags{}
	cmd := &cobra.Command{
		Use:   "sync <src-dir> <dst>",
		Short: "Mirror a model directory, moving only changed chunks",
		Long: `Sync a model directory to another location.

sync is copy plus mirroring: with --delete it also removes destination files
the source no longer has. Both commands move only the chunks the destination
is missing.`,
		Example: `  modelmove sync ./llama-3-8b-ft gpu-box:/srv/models/llama-3-8b-ft
  modelmove sync ./llama-3-8b-ft /models/llama-3-8b-ft --delete`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTransfer(cmd, flags, args[0], args[1])
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&flags.delete, "delete", false, "remove destination files that the source does not have")
	return cmd
}

func runTransfer(cmd *cobra.Command, flags *transferFlags, src, dst string) error {
	cfg, err := flags.config(src, dst)
	if err != nil {
		return err
	}
	res, err := engine.Run(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if flags.json {
		return writeJSON(cmd.OutOrStdout(), res)
	}
	printTransfer(cmd.OutOrStdout(), res)
	return nil
}

func printTransfer(w io.Writer, res *engine.Result) {
	if quiet {
		return
	}
	m := res.Model
	fmt.Fprintf(w, "source  %s\n", res.Source)
	fmt.Fprintf(w, "dest    %s\n", res.Dest)
	fmt.Fprintf(w, "model   %s", m.Kind)
	if m.Name != "" {
		fmt.Fprintf(w, " %s", m.Name)
	}
	fmt.Fprintf(w, ", %d files, %s", m.Files, units.Bytes(m.Bytes))
	if m.WeightFiles > 0 {
		fmt.Fprintf(w, " (%d weight files, %s)", m.WeightFiles, units.Bytes(m.WeightBytes))
	}
	if m.Shards > 0 {
		fmt.Fprintf(w, ", %d shard set(s)", m.Shards)
	}
	fmt.Fprintln(w)

	p := res.Plan
	if p == nil {
		return
	}
	fmt.Fprintf(w, "plan    %d new, %d updated, %d unchanged\n", p.CopyFiles, p.UpdateFiles, p.SkipFiles)
	fmt.Fprintf(w, "        %s to send, %s reusable at the destination (%.1f%% saved)\n",
		units.Bytes(p.NeedBytes), units.Bytes(p.ReuseBytes+p.SkipBytes), 100*p.Savings())
	if len(p.Deletes) > 0 {
		fmt.Fprintf(w, "        %d file(s) to delete\n", len(p.Deletes))
	}

	if res.DryRun {
		printPlanDetail(w, p)
		fmt.Fprintf(w, "\ndry run: nothing was written\n")
		return
	}

	s := res.Summary
	if s == nil {
		return
	}
	fmt.Fprintf(w, "sent    %s in %s (%s)\n",
		units.Bytes(s.BytesReceived), units.Duration(secondsToDuration(res.Seconds)),
		units.Rate(float64(s.BytesReceived)/max(res.Seconds, 0.001)))
	fmt.Fprintf(w, "done    %d written, %d unchanged", s.FilesWritten, s.FilesSkipped)
	if s.FilesDeleted > 0 {
		fmt.Fprintf(w, ", %d deleted", s.FilesDeleted)
	}
	fmt.Fprintf(w, "; %s verified at the destination\n", units.Bytes(s.BytesTotal))
}

func printPlanDetail(w io.Writer, p *receiver.Plan) {
	work := p.Work()
	if len(work) == 0 {
		fmt.Fprintln(w, "\nnothing to transfer")
		return
	}
	sort.Slice(work, func(i, j int) bool { return work[i].NeedBytes > work[j].NeedBytes })
	fmt.Fprintln(w)
	for _, f := range work {
		fmt.Fprintf(w, "  %-6s %-52s %10s of %10s\n",
			f.Action, truncate(f.Path, 52), units.Bytes(f.NeedBytes), units.Bytes(f.Size))
	}
	for _, d := range p.Deletes {
		fmt.Fprintf(w, "  %-6s %s\n", "delete", d)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return "..." + s[len(s)-(n-3):]
}

func newRemoteHelperCmd() *cobra.Command {
	var (
		root        string
		allowDelete bool
	)
	cmd := &cobra.Command{
		Use:    "remote-helper",
		Short:  "Serve a transfer on stdin/stdout (invoked over SSH)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if root == "" {
				return fmt.Errorf("remote-helper requires --root")
			}
			return serveRemote(cmd, root, allowDelete)
		},
	}
	cmd.Flags().StringVar(&root, "root", "", "destination directory")
	cmd.Flags().BoolVar(&allowDelete, "allow-delete", false, "permit the client to delete files")
	return cmd
}

func secondsToDuration(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
