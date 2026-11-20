package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/proxy"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// registerServeCommands attaches serve/stop/status to the root.
func registerServeCommands(root *cobra.Command) {
	root.AddCommand(serveCmd(), stopCmd(), statusCmd())
}

// decodeJSON reads and decodes a JSON body.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// pidFilePath is where serve records the running proxy pid.
func pidFilePath(cfg app.Config) string { return filepath.Join(cfg.DataDir, "proxy.pid") }

// defaultListenAddr is the loopback bind address for the proxy.
const defaultListenAddr = "127.0.0.1:8787"

func serveCmd() *cobra.Command {
	var addr string
	var daemon bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local proxy and control-plane console (loopback only)",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			if daemon {
				return spawnDaemon(cfg, addr)
			}
			return runProxy(cfg, addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultListenAddr, "loopback address to bind")
	cmd.Flags().BoolVar(&daemon, "daemon", false, "fork into background and return")
	return cmd
}

// spawnDaemon re-executes this binary with --daemon detached, writing the
// child's pid file, then returns immediately.
func spawnDaemon(cfg app.Config, addr string) error {
	if running := readPid(cfg); running > 0 {
		return fmt.Errorf("proxy already running (pid %d)", running)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cmd := exec.Command(exe, "serve", "--addr", addr)
	cmd.Stdout, cmd.Stderr = nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fork proxy: %w", err)
	}
	pid := cmd.Process.Pid
	if err := os.WriteFile(pidFilePath(cfg), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	fmt.Printf("proxy starting in background (pid %d, %s)\n", pid, addr)
	return nil
}

// runProxy runs the server in the foreground until interrupted.
func runProxy(cfg app.Config, addr string) error {
	store, err := cfg.OpenStore()
	if err != nil {
		return err
	}
	defer store.Close()

	// The proxy reads keys from the configured secret backend and records
	// per-call usage into the same database file (WAL allows both).
	usageStore, err := usage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer usageStore.Close()

	proxySrv := proxy.New(store, cfg.Secret, cfg.Service)
	proxySrv.EnableUsage(usageStore)
	proxySrv.EnableQuotas(store, usageStore)
	proxySrv.SetUsageSink(func(r usage.Record) {
		if err := usageStore.Insert(r); err != nil {
			// Usage recording must never take the request down; log-free
			// drop is acceptable (structural no-secret logging).
			_ = err
		}
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Best-effort pid file so status/stop work for foreground runs too.
	pid := os.Getpid()
	_ = os.WriteFile(pidFilePath(cfg), []byte(strconv.Itoa(pid)), 0o600)
	defer os.Remove(pidFilePath(cfg))

	fmt.Printf("agentvault proxy listening on http://%s (pid %d)\n", addr, pid)
	return proxySrv.ListenAndServe(ctx, addr)
}

// readPid returns the recorded pid, or 0 when absent.
func readPid(cfg app.Config) int {
	data, err := os.ReadFile(pidFilePath(cfg))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// pidAlive reports whether the process is still running.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running proxy",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			pid := readPid(cfg)
			if pid == 0 || !pidAlive(pid) {
				fmt.Println("proxy not running")
				_ = os.Remove(pidFilePath(cfg))
				return nil
			}
			p, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find pid %d: %w", pid, err)
			}
			if err := p.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("signal pid %d: %w", pid, err)
			}
			fmt.Printf("proxy stopped (pid %d)\n", pid)
			_ = os.Remove(pidFilePath(cfg))
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show proxy health, providers and cooldown state",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			pid := readPid(cfg)
			alive := pidAlive(pid)
			fmt.Printf("pid: %d (running=%v)\n", pid, alive)
			if !alive {
				return nil
			}
			// Ask the proxy itself for routing observation.
			client := &http.Client{Timeout: 2 * time.Second}
			resp, err := client.Get("http://" + defaultListenAddr + "/admin/routing")
			if err != nil {
				fmt.Println("routing status: unreachable", err)
				return nil
			}
			defer resp.Body.Close()
			var out struct {
				CoolingKeys int `json:"cooling_keys"`
				BreakerKeys int `json:"breaker_keys"`
				Providers   []struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Keys   int    `json:"keys_active"`
					Models int    `json:"models"`
				} `json:"providers"`
			}
			if err := decodeJSON(resp.Body, &out); err != nil {
				return fmt.Errorf("decode routing status: %w", err)
			}
			fmt.Printf("cooling keys: %d\nbreaker keys: %d\n", out.CoolingKeys, out.BreakerKeys)
			for _, p := range out.Providers {
				fmt.Printf("provider %s (%s): %d active keys, %d models\n", p.ID, p.Name, p.Keys, p.Models)
			}
			return nil
		},
	}
}
