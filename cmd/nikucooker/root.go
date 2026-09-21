package main

import "github.com/spf13/cobra"

const appName = "nikucooker"

// newRootCmd builds the command tree.
//
// Commands are constructed here rather than registered from init() so that a
// fresh, independent tree can be built per test run, with no shared global
// state leaking between them.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   appName,
		Short: "Local-first AI fansubbing pipeline",
		Long: `NikuCooker turns raw, untranslated video into translated, reviewed and
ready-to-watch releases.

Speech recognition and media processing run entirely on this machine; audio and
video never leave it. Only subtitle text is sent to an LLM, and only to the
provider you configure.`,
		// A usage dump after every runtime error buries the actual message.
		SilenceUsage: true,
	}

	root.AddCommand(
		newServeCmd(),
		newVersionCmd(),
	)

	return root
}
