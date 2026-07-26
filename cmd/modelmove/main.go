// Command modelmove transfers and verifies LLM model directories between
// machines, moving only the chunks that changed.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/shaneburrell/modelmove/internal/cli"
)

func main() {
	os.Exit(run())
}

// run exists so that the signal handler is torn down before os.Exit, which
// never runs deferred functions.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Code(cli.ExecuteContext(ctx))
}
