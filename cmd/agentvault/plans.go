package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// registerPlansCommands attaches the plan-quota commands used to seed the
// plans table (auto mode M6-1): agentvault plans set / plans list.
func registerPlansCommands(root *cobra.Command) {
	root.AddCommand(plansCmd())
}

func plansCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plans",
		Short: "Manage subscription plan quota rows (auto mode quota awareness)",
	}
	cmd.AddCommand(plansSetCmd(), plansListCmd())
	return cmd
}

// plansSetCmd upserts one plan row. Values are in the plan's own unit
// (e.g. GLM coding-plan credits); remaining may be given as a percentage
// with --used-pct instead of an absolute number.
func plansSetCmd() *cobra.Command {
	var tier, window string
	var limit, remaining float64
	var usedPct float64
	var verified string
	cmd := &cobra.Command{
		Use:   "set <provider-id>",
		Short: "Set a plan quota row (--tier, --window, --limit and --remaining or --used-pct)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tier == "" || window == "" || limit <= 0 {
				return fmt.Errorf("%w: --tier, --window and --limit (>0) are required", app.ErrFlagMissing)
			}
			if remaining == 0 && usedPct == 0 {
				return fmt.Errorf("%w: give --remaining or --used-pct (0 excluded: pass an explicit value)", app.ErrFlagMissing)
			}
			if usedPct != 0 {
				if usedPct < 0 || usedPct > 100 {
					return fmt.Errorf("--used-pct must be within [0,100], got %v", usedPct)
				}
				remaining = limit * (1 - usedPct/100)
			}
			var verifiedAt time.Time
			if verified != "" {
				t, err := time.Parse(time.RFC3339, verified)
				if err != nil {
					return fmt.Errorf("--verified-at must be RFC3339: %w", err)
				}
				verifiedAt = t
			}
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			store, err := cfg.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()
			p := provider.Plan{
				Provider: args[0], PlanTier: tier, WindowType: window,
				Limit: limit, Remaining: remaining, VerifiedAt: verifiedAt,
			}
			if err := store.SetPlan(p); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "plan %s/%s/%s set: limit %s remaining %s\n",
				p.Provider, p.PlanTier, p.WindowType,
				strconv.FormatFloat(p.Limit, 'f', -1, 64),
				strconv.FormatFloat(p.Remaining, 'f', -1, 64))
			return nil
		},
	}
	cmd.Flags().StringVar(&tier, "tier", "", "plan tier (e.g. max)")
	cmd.Flags().StringVar(&window, "window", "", "quota window type (5h | weekly)")
	cmd.Flags().Float64Var(&limit, "limit", 0, "quota limit in plan units (e.g. credits)")
	cmd.Flags().Float64Var(&remaining, "remaining", 0, "remaining quota in plan units")
	cmd.Flags().Float64Var(&usedPct, "used-pct", 0, "used percentage (alternative to --remaining)")
	cmd.Flags().StringVar(&verified, "verified-at", "", "RFC3339 time of the last manual price cross-check (default: now)")
	_ = cmd.MarkFlagRequired("tier")
	_ = cmd.MarkFlagRequired("window")
	_ = cmd.MarkFlagRequired("limit")
	return cmd
}

// plansListCmd prints every plan row, flagging rows whose manual
// verification is older than 30 days (design §6 verified_at reminder).
func plansListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plan quota rows (verify older than 30 days is flagged)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			store, err := cfg.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()
			ps, err := store.ListPlans()
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no plan rows")
				return nil
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "provider\ttier\twindow\tlimit\tremaining\trefreshed\tverified")
			for _, p := range ps {
				stale := ""
				if time.Since(p.VerifiedAt) > 30*24*time.Hour {
					stale = " (verify: >30d)"
				}
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n",
					p.Provider, p.PlanTier, p.WindowType,
					strconv.FormatFloat(p.Limit, 'f', -1, 64),
					strconv.FormatFloat(p.Remaining, 'f', -1, 64),
					p.RefreshedAt.Format(time.RFC3339),
					p.VerifiedAt.Format(time.RFC3339), stale)
			}
			return nil
		},
	}
}
