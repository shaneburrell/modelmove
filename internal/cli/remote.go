package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/shaneburrell/modelmove/internal/protocol"
)

// serveRemote runs the destination side of a transfer over stdin and stdout.
// Nothing may be printed to stdout here: it carries the protocol.
func serveRemote(cmd *cobra.Command, root string, allowDelete bool) error {
	return protocol.Serve(cmd.Context(), os.Stdin, os.Stdout, protocol.ServerOptions{
		Root:        root,
		Tool:        Tool(),
		AllowDelete: allowDelete,
	})
}
