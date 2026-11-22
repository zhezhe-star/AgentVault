package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
)

// registerRunCommands attaches run to the root.
func registerRunCommands(root *cobra.Command, out io.Writer) {
	root.AddCommand(runCmd(out))
}

func runCmd(out io.Writer) *cobra.Command {
	var envs []string
	cmd := &cobra.Command{
		Use:   "run --env VAR=ref:provider/key [--env ...] -- <command> [args...]",
		Short: "Run a command with keychain-backed secrets injected as environment variables",
		Long: `Decrypts each referenced key from the OS keychain in-process and
injects it into the child environment only. Config files never carry the
plaintext; typical use is wrapping MCP server startup commands.`,
		Example: `  agentvault run --env ARK_API_KEY=ref:ark/ark_api_key -- my-mcp-server
  agentvault run --env Z_AI_API_KEY=ref:zai/primary -- codex exec "hi"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			childEnv, err := buildInjectedEnv(cfg, envs, os.Environ())
			if err != nil {
				return err
			}
			// Pass stdio through untouched; forward interrupts so the child
			// behaves as if launched directly; propagate its exit code.
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Env = childEnv
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			sig := make(chan os.Signal, 1)
			signal.Notify(sig)
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				return fmt.Errorf("run %s: %w", args[0], err)
			}
			go func() {
				done <- cmd.Wait()
			}()
			for {
				select {
				case s := <-sig:
					_ = cmd.Process.Signal(s)
				case err := <-done:
					if err == nil {
						return nil
					}
					var exitErr *exec.ExitError
					if ok := asExitError(err, &exitErr); ok {
						os.Exit(exitErr.ExitCode())
					}
					return err
				}
			}
		},
	}
	cmd.Flags().StringArrayVar(&envs, "env", nil, "injection spec VAR=ref:provider/key (repeatable)")
	return cmd
}

// buildInjectedEnv resolves each VAR=ref:spec against the vault and layers
// the plaintext onto the parent environment (parent env never contains it).
func buildInjectedEnv(cfg app.Config, specs, parent []string) ([]string, error) {
	over := map[string]string{}
	for _, spec := range specs {
		name, ref, ok := splitEnvSpec(spec)
		if !ok {
			return nil, fmt.Errorf("bad --env %q: want VAR=ref:provider/key", spec)
		}
		v, err := cfg.Secret.Get(cfg.Service, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve ref:%s: %w", ref, err)
		}
		over[name] = v
	}
	out := make([]string, 0, len(parent)+len(over))
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		if _, injected := over[name]; !injected {
			out = append(out, kv)
		}
	}
	for name, v := range over {
		out = append(out, name+"="+v)
	}
	return out, nil
}

// splitEnvSpec parses "VAR=ref:provider/key".
func splitEnvSpec(spec string) (name, ref string, ok bool) {
	name, rest, found := strings.Cut(spec, "=")
	if !found || name == "" {
		return "", "", false
	}
	if !strings.HasPrefix(rest, "ref:") {
		return "", "", false
	}
	ref = strings.TrimPrefix(rest, "ref:")
	return name, ref, ref != ""
}

// asExitError is a tiny wrapper so the exit-code path stays testable.
func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

var _ = syscall.SIGTERM // keep syscall import for signal forwarding semantics
