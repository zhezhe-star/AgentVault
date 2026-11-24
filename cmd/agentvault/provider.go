package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/provider"
)

// registerProviderCommands attaches the control-plane provider commands.
func registerProviderCommands(root *cobra.Command) {
	root.AddCommand(providerCmd())
}

func providerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Manage model providers and their keys",
	}
	cmd.AddCommand(providerAddCmd(), providerListCmd(), providerShowCmd(), providerRevokeCmd(), providerAddModelCmd(), providerQuotaCmd())
	return cmd
}

func providerAddCmd() *cobra.Command {
	var name, baseURL, anthropicBaseURL string
	cmd := &cobra.Command{
		Use:   "add <provider-id>",
		Short: "Register a provider (metadata only; add keys separately)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || baseURL == "" {
				return fmt.Errorf("%w: --name and --base-url are required", app.ErrFlagMissing)
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
			if err := store.CreateProvider(provider.Provider{
				ID: args[0], Name: name, BaseURL: baseURL, AnthropicBaseURL: anthropicBaseURL,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "provider %s added\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "display name")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "provider API base URL")
	cmd.Flags().StringVar(&anthropicBaseURL, "anthropic-base-url", "",
		"optional Anthropic-compatible endpoint for /v1/messages passthrough")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("base-url")
	return cmd
}

func providerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered providers",
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
			ps, err := store.ListProviders()
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no providers registered")
				return nil
			}
			for _, p := range ps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", p.ID, p.Name, p.BaseURL)
			}
			return nil
		},
	}
}

func providerShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <provider-id>",
		Short: "Show a provider with its keys and models",
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
			p, err := store.GetProvider(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "provider: %s (%s)\nbase-url: %s\nanthropic-base-url: %s\ncreated: %s\n",
				p.ID, p.Name, p.BaseURL, p.AnthropicBaseURL, p.CreatedAt.Format(time.RFC3339))
			keys, err := store.ListKeys(p.ID)
			if err != nil {
				return err
			}
			for _, k := range keys {
				// Only the secret reference is printed, never a key value.
				fmt.Fprintf(cmd.OutOrStdout(), "key %s\tstatus=%s\tweight=%d\tref=%s\n",
					k.ID, k.Status, k.Weight, app.KeyRef(p.ID, k.ID))
				models, err := store.ListModels(p.ID)
				if err != nil {
					return err
				}
				for _, m := range models {
					if m.KeyID == k.ID {
						fmt.Fprintf(cmd.OutOrStdout(), "  model %s -> %s\n", m.Name, m.Upstream)
					}
				}
			}
			return nil
		},
	}
}

func providerRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <provider-id> <key-id>",
		Short: "Revoke a key (marks it revoked; revocation at the provider console stays manual)",
		Args:  cobra.ExactArgs(2),
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
			if err := store.SetKeyStatus(args[1], provider.KeyRevoked); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "key %s/%s revoked\n", args[0], args[1])
			return nil
		},
	}
}

// keyCmd adds a key to a provider. The secret is read from stdin without
// echo; it goes straight into the secret store.
func keyCmd() *cobra.Command {
	var keyID string
	cmd := &cobra.Command{
		Use:   "add-key <provider-id>",
		Short: "Add an API key to a provider (secret read from stdin, no echo)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyID == "" {
				return fmt.Errorf("%w: --key-id is required", app.ErrFlagMissing)
			}
			raw, err := readSecret(cmd)
			if err != nil {
				return err
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
			if err := store.CreateKey(provider.Key{
				ID: keyID, ProviderID: args[0],
				SecretID: app.KeyRef(args[0], keyID),
				Status:   provider.KeyActive, Weight: 1,
			}); err != nil {
				return err
			}
			if err := cfg.Secret.Set(cfg.Service, args[0]+"/"+keyID, string(raw)); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "key %s/%s stored (secret in keychain)\n", args[0], keyID)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "identifier for the key slot")
	_ = cmd.MarkFlagRequired("key-id")
	return cmd
}

// readSecret reads the API key: hidden input on a TTY, plain line reading
// otherwise (pipes/tests). The value must still not be echoed on a TTY.
func readSecret(cmd *cobra.Command) (string, error) {
	in := cmd.InOrStdin()
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(os.Stderr, "paste API key (input hidden): ")
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read key: %w", err)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	raw, err := io.ReadAll(io.LimitReader(in, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func providerAddModelCmd() *cobra.Command {
	var keyID, upstream string
	cmd := &cobra.Command{
		Use:   "add-model <provider-id> <model-name>",
		Short: "Register a model mapping under a provider key (model-name is what clients request)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keyID == "" {
				return fmt.Errorf("%w: --key-id is required", app.ErrFlagMissing)
			}
			if upstream == "" {
				upstream = args[1] // same id at the provider by default
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
			m := provider.Model{
				ID:         args[0] + "/" + keyID + "/" + args[1],
				ProviderID: args[0], KeyID: keyID,
				Name: args[1], Upstream: upstream,
			}
			if err := store.CreateModel(m); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "model %s -> %s (provider %s, key %s) added\n",
				m.Name, m.Upstream, m.ProviderID, m.KeyID)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyID, "key-id", "", "provider key slot this model routes through")
	cmd.Flags().StringVar(&upstream, "upstream", "", "model id at the provider (default: the requested model name)")
	_ = cmd.MarkFlagRequired("key-id")
	return cmd
}

func providerQuotaCmd() *cobra.Command {
	var window, scope string
	var maxTokens int64
	cmd := &cobra.Command{
		Use:   "quota <ref-id>",
		Short: "Set a rolling-window token quota (window like 1h/30m; scope provider|key)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if maxTokens <= 0 || window == "" || (scope != "provider" && scope != "key") {
				return fmt.Errorf("%w: --max-tokens (>0), --window and --scope=provider|key are required", app.ErrFlagMissing)
			}
			d, err := time.ParseDuration(window)
			if err != nil || d <= 0 {
				return fmt.Errorf("--window must be a positive duration: %w", err)
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
			if err := store.SetQuota(provider.Quota{
				Scope: scope, RefID: args[0],
				WindowSeconds: int(d.Seconds()), MaxTokens: maxTokens,
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "quota set: %s %s max %d tokens / %s\n", scope, args[0], maxTokens, window)
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "provider", "quota scope: provider or key")
	cmd.Flags().StringVar(&window, "window", "1h", "rolling window duration (e.g. 1h, 30m)")
	cmd.Flags().Int64Var(&maxTokens, "max-tokens", 0, "max tokens (in+out) within the window")
	_ = cmd.MarkFlagRequired("max-tokens")
	return cmd
}
