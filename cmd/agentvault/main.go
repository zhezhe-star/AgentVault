// Command agentvault is the single entry point for the AgentVault CLI:
// provider/key management, the local OpenAI-compatible proxy, and the
// built-in control-plane console.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:   "agentvault",
		Short: "Local encrypted API key vault with an OpenAI-compatible proxy",
		Long: `AgentVault manages provider API keys in the OS keychain and serves a
local OpenAI-compatible proxy so plaintext keys never live in config files.`,
		SilenceUsage: true,
	}
	root.AddCommand(versionCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agentvault version",
		Run: func(*cobra.Command, []string) {
			fmt.Println("agentvault", version)
		},
	}
}
