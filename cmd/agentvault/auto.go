package main

// The `agentvault auto` command group: the decision-action face of the
// owner channel (design docs/design/auto-mode.md §7, e247). The event
// file lets the owner KNOW something is pending (the proxy writes it);
// these commands complete the decision; the console is the third face.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

func registerAutoCommands(root *cobra.Command) {
	root.AddCommand(autoCmd())
}

func autoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Owner decision channel for auto-mode overflow candidates",
	}
	cmd.AddCommand(autoPendingCmd(), autoApproveCmd(), autoDenyCmd())
	return cmd
}

func autoPendingCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "pending",
		Short: "List overflow candidates awaiting the owner decision",
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
			status := provider.PendingOpen
			if all {
				status = ""
			}
			list, err := store.ListPendingDecisions(status)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no pending decisions")
				return nil
			}
			for _, d := range list {
				fmt.Fprintf(cmd.OutOrStdout(),
					"#%d [%s] tier=%s target=%s/%s est=~%d tok (~$%.4f)\n  rule: %s\n  reason: %s\n",
					d.ID, d.Status, d.TaskTier, d.Provider, d.Model, d.EstTokens, d.EstCost,
					d.MatchedRule, oneLine(d.Reason))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include already-decided rows")
	return cmd
}

// decisionCmd builds the approve/deny commands: `auto approve <id|all>`.
func decisionCmd(use, short, status string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <id|all>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			store, err := cfg.OpenStore()
			if err != nil {
				return err
			}
			defer store.Close()
			events := auto.NewEventLog(eventFilePath(cfg))
			arg := strings.TrimSpace(args[0])
			if arg == "all" {
				n, err := store.DecideAllPending(status)
				if err != nil {
					return err
				}
				_ = events.Append(auto.Event{Type: auto.EventPendingDecided, Decision: status, Reason: fmt.Sprintf("bulk: %d rows", n)})
				fmt.Fprintf(cmd.OutOrStdout(), "%d pending decision(s) %s\n", n, status)
				return nil
			}
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil {
				return fmt.Errorf("decision id must be a number or \"all\": %q", arg)
			}
			d, err := store.GetPendingDecision(id)
			if err != nil {
				return fmt.Errorf("pending decision #%d: %w", id, err)
			}
			if err := store.DecidePending(id, status); err != nil {
				return err
			}
			_ = events.Append(auto.Event{
				Type: auto.EventPendingDecided, ID: id, Decision: status,
				Tier: d.TaskTier, Provider: d.Provider, Model: d.Model,
			})
			fmt.Fprintf(cmd.OutOrStdout(), "pending decision #%d %s\n", id, status)
			return nil
		},
	}
}

func autoApproveCmd() *cobra.Command {
	return decisionCmd("approve", "Approve an overflow candidate (by id, or all)", provider.PendingApproved)
}

func autoDenyCmd() *cobra.Command {
	return decisionCmd("deny", "Deny an overflow candidate (by id, or all)", provider.PendingDenied)
}

// eventFilePath is the shared event-file location (serve writes creation
// events, the CLI writes decision events): <data-dir>/auto-events.jsonl.
func eventFilePath(cfg app.Config) string {
	return auto.DefaultEventPath(cfg.DataDir)
}

// oneLine collapses a multi-line reason for the list output.
func oneLine(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}
