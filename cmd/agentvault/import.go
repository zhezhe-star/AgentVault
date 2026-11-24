package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	var report bool
	var only []string
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
			if report {
				var refs []string
				if yes {
					cfg, err := app.Resolve()
					if err != nil {
						return err
					}
					rep, err := migrate.Apply(cfg.Secret, cfg.Service, migrate.BuildPlanFiltered(found, resolveOnly(home, only)))
					if err != nil {
						return err
					}
					refs = rep.Imported
				}
				migrate.WriteReport(out, found, refs)
				return nil
			}
			plan := migrate.BuildPlanFiltered(found, resolveOnly(home, only))
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
	cmd.Flags().BoolVar(&report, "report", false, "print the three-class migration report instead of the plan")
	cmd.Flags().StringArrayVar(&only, "only", nil, "restrict the plan to this file (repeatable; default: all found files)")
	return cmd
}

// resolveOnly turns --only args into absolute paths (relative resolved
// against the scanned home).
func resolveOnly(home string, only []string) []string {
	var out []string
	for _, o := range only {
		if !filepath.IsAbs(o) {
			o = filepath.Join(home, o)
		}
		if abs, err := filepath.Abs(o); err == nil {
			o = abs
		}
		out = append(out, o)
	}
	return out
}
