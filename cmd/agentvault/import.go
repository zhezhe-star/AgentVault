package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/migrate"
)

// registerImportCommands attaches import to the root.
func registerImportCommands(root *cobra.Command, out io.Writer) {
	root.AddCommand(importCmd(out))
}

func importCmd(out io.Writer) *cobra.Command {
	var home string
	var yes bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import scanned plaintext keys into the vault and rewrite the files (.bak backups)",
		Long: `Default is preview: prints the planned keychain entries and rewrites
without touching anything. Pass --yes to apply. Skipped classes:
compliant entries are never touched.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			if home == "" {
				var err error
				if home, err = os.UserHomeDir(); err != nil {
					return err
				}
			}
			found, err := migrate.NewScanner(home).Scan()
			if err != nil {
				return err
			}
			plan := migrate.BuildPlan(found)
			plan.Preview(out)
			if !yes {
				fmt.Fprintln(out, "\npreview only — rerun with --yes to apply")
				return nil
			}
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			rep, err := migrate.Apply(cfg.Secret, cfg.Service, plan)
			if err != nil {
				return err
			}
			for _, ref := range rep.Imported {
				fmt.Fprintf(out, "stored ref:%s\n", ref)
			}
			for _, f := range rep.Files {
				fmt.Fprintf(out, "rewrote %s (backup alongside)\n", f)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "home directory to operate on (default: current user home)")
	cmd.Flags().BoolVar(&yes, "yes", false, "apply the plan (default: preview only)")
	return cmd
}
