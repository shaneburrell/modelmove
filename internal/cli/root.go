// Package cli implements the modelmove command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

// Build information, overwritten at link time with -X.
var (
	version = "0.1.0"
	commit  = "none"
	date    = "unknown"
)

// Version returns the build version.
func Version() string { return version }

// Tool is the identifier recorded in manifests.
func Tool() string { return "modelmove/" + version }

// versionLine is what "modelmove --version" prints.
func versionLine() string {
	return fmt.Sprintf("modelmove %s (commit %s, built %s, %s/%s, %s)",
		version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Exit codes. Anything that is not a usage or runtime error gets its own code
// so that scripts can tell "the model is corrupt" from "the command failed".
const (
	ExitOK       = 0
	ExitError    = 1
	ExitMismatch = 2
)

// exitError carries a specific process exit code.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// mismatch marks an error as a content mismatch rather than a failure to run,
// so that a script can tell "the model is corrupt" from "the command broke".
func mismatch(err error) error { return &exitError{code: ExitMismatch, err: err} }

// Code maps an error returned by Execute to a process exit code.
func Code(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return ExitError
}

// Execute runs the CLI with the process arguments.
func Execute() error { return ExecuteArgs(os.Args[1:]) }

// ExecuteContext runs the CLI with the process arguments under ctx, so that
// an interrupt stops an in-flight transfer instead of killing it mid-write.
func ExecuteContext(ctx context.Context) error {
	return executeArgs(ctx, os.Args[1:])
}

// ExecuteArgs runs the CLI with explicit arguments, excluding the program
// name. It exists so tests can drive the CLI without a subprocess.
func ExecuteArgs(args []string) error {
	return executeArgs(context.Background(), args)
}

func executeArgs(ctx context.Context, args []string) error {
	root := newRoot()
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "modelmove: "+err.Error())
		return err
	}
	return nil
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "modelmove",
		Short: "Sparse-delta, verified transfer of LLM model weights",
		Long: `modelmove copies and syncs model directories between machines.

It understands the layouts models actually ship in (Hugging Face, GGUF,
Ollama), splits weight files with FastCDC so that updating a fine-tune moves
only the tensors that changed, and stages every file with a BLAKE3 check
before it replaces the original.`,
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           version,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
	}
	root.SetVersionTemplate(versionLine() + "\n")
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "log skipped files and other details")
	root.PersistentFlags().BoolVar(&quiet, "quiet", false, "suppress progress and non-error output")

	root.AddCommand(newCopyCmd())
	root.AddCommand(newSyncCmd())
	root.AddCommand(newVerifyCmd())
	root.AddCommand(newManifestCmd())
	root.AddCommand(newDiffCmd())
	root.AddCommand(newRemoteHelperCmd())
	return root
}

var (
	verbose bool
	quiet   bool
)

// warnf prints a warning to stderr. Warnings are only shown with --verbose,
// because a Hugging Face cache full of symlinks would otherwise bury the
// output.
func warnf(format string, args ...any) {
	if !verbose || quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "modelmove: "+format+"\n", args...)
}
