package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/migrate"
)

// registerScanCommands attaches scan to the root.
func registerScanCommands(root *cobra.Command, out io.Writer) {
	root.AddCommand(scanCmd(out))
}

func scanCmd(out io.Writer) *cobra.Command {
	var home string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan local dotfiles for plaintext API keys (read-only, masked output)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if home == "" {
				var err error
				home, err = os.UserHomeDir()
				if err != nil {
					return err
				}
			}
			found, err := migrate.NewScanner(home).Scan()
			if err != nil {
				return err
			}
			return printFindings(out, found, asJSON)
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "home directory to scan (default: current user home)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// printFindings renders the scan report; masked values only by construction.
func printFindings(w io.Writer, found []migrate.Finding, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(found)
	}
	if len(found) == 0 {
		fmt.Fprintln(w, "no plaintext keys found")
		return nil
	}
	fmt.Fprintf(w, "%-4s %-40s %-22s %-12s %-13s %s\n", "LINE", "FILE", "VAR", "KIND", "CLASS", "VALUE")
	for _, f := range found {
		fmt.Fprintf(w, "%-4d %-40s %-22s %-12s %-13s %s\n",
			f.Line, f.Path, f.Var, f.Kind, f.Class, f.MaskedValue)
	}
	fmt.Fprintf(w, "\n%d plaintext key(s) found\n", len(found))
	return nil
}
