package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// registerUsageCommands attaches the usage query command.
func registerUsageCommands(root *cobra.Command) {
	cfg, err := app.Resolve()
	if err != nil {
		panic("usage: resolve config: " + err.Error())
	}
	root.AddCommand(newUsageCmd(cfg, os.Stdout))
}

// newUsageCmd builds the usage command against a concrete config and
// output writer (injected so tests can drive it directly).
func newUsageCmd(cfg app.Config, out io.Writer) *cobra.Command {
	var providerID, keyID, model, since, until string
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Query recorded proxy call usage (filter by provider/key/model/time window)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := usage.Open(cfg.DBPath)
			if err != nil {
				return err
			}
			defer store.Close()

			f := usage.Filter{ProviderID: providerID, KeyID: keyID, Model: model, Limit: limit}
			if since != "" {
				t, err := parseTimeArg(since)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				f.Since = t
			}
			if until != "" {
				t, err := parseTimeArg(until)
				if err != nil {
					return fmt.Errorf("--until: %w", err)
				}
				f.Until = t
			}
			records, err := store.QueryFilter(f)
			if err != nil {
				return err
			}

			if asJSON {
				return json.NewEncoder(out).Encode(records)
			}
			if len(records) == 0 {
				fmt.Fprintln(out, "no usage records match")
				return nil
			}
			fmt.Fprintln(out, "time\tprovider\tkey\tmodel\tproto\tstatus\ttokens_in\ttokens_out\tlatency_ms")
			for _, r := range records {
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
					r.CreatedAt.Format(time.RFC3339), r.ProviderID, r.KeyID, r.Model,
					r.Protocol, r.Status, r.TokensIn, r.TokensOut, r.LatencyMS)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&providerID, "provider", "", "filter by provider id")
	cmd.Flags().StringVar(&keyID, "key", "", "filter by key id")
	cmd.Flags().StringVar(&model, "model", "", "filter by model name")
	cmd.Flags().StringVar(&since, "since", "", "start of window (RFC3339 or duration like 1h/30m ago)")
	cmd.Flags().StringVar(&until, "until", "", "end of window (RFC3339 or duration)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max records to show")
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

// parseTimeArg accepts RFC3339 or a relative duration (interpreted as
// "now minus duration").
func parseTimeArg(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("expected RFC3339 timestamp or duration, got %q", s)
}
