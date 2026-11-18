// Command agentvault is the single entry point for the AgentVault CLI:
// provider/key management, the local OpenAI-compatible proxy, and the
// built-in control-plane console.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := newRootCmd(os.Stdout).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// newRootCmd assembles the full command tree writing to out.
func newRootCmd(out io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "agentvault",
		Short: "Local encrypted API key vault with an OpenAI-compatible proxy",
		Long: `AgentVault manages provider API keys in the OS keychain and serves a
local OpenAI-compatible proxy so plaintext keys never live in config files.`,
		SilenceUsage: true,
	}
	root.SetOut(out)
	root.AddCommand(versionCmd(out), keyCmd(), providerCmd())
	registerServeCommands(root)
	return root
}

func versionCmd(out io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agentvault version",
		Run: func(*cobra.Command, []string) {
			fmt.Fprintln(out, "agentvault", version)
		},
	}
}
