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
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/provider"
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
	var queueTimeout time.Duration
	var quotaPoll bool
	var quotaProvider string
	var deepseekProvider string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local proxy and control-plane console (loopback only)",
		RunE: func(_ *cobra.Command, _ []string) error {
			if queueTimeout < 0 {
				return fmt.Errorf("--queue-timeout must be non-negative")
			}
			cfg, err := app.Resolve()
			if err != nil {
				return err
			}
			opts := serveOptions{
				QueueTimeout:     queueTimeout,
				QuotaPoll:        quotaPoll,
				QuotaProvider:    quotaProvider,
				DeepSeekProvider: deepseekProvider,
			}
			if daemon {
				return spawnDaemon(cfg, addr, opts)
			}
			return runProxy(cfg, addr, opts)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultListenAddr, "loopback address to bind")
	cmd.Flags().BoolVar(&daemon, "daemon", false, "fork into background and return")
	cmd.Flags().DurationVar(&queueTimeout, "queue-timeout", proxy.DefaultQueueTimeout,
		"max wait in the concurrency queue before answering 429 (e.g. 120s)")
	cmd.Flags().BoolVar(&quotaPoll, "quota-poll", true,
		"poll the GLM hidden quota endpoint into plans (disable for offline/dev smoke)")
	cmd.Flags().StringVar(&quotaProvider, "quota-provider", "bigmodel",
		"provider id whose first active key authenticates the quota endpoint")
	cmd.Flags().StringVar(&deepseekProvider, "deepseek-provider", "deepseek",
		"provider id whose first active key authenticates the DeepSeek balance probe")
	return cmd
}

// serveOptions carries the serve flag values through the daemon re-exec.
type serveOptions struct {
	QueueTimeout     time.Duration
	QuotaPoll        bool
	QuotaProvider    string
	DeepSeekProvider string
}

func (o serveOptions) execArgs(addr string) []string {
	return []string{"serve", "--addr", addr,
		"--queue-timeout", o.QueueTimeout.String(),
		"--quota-poll=" + strconv.FormatBool(o.QuotaPoll),
		"--quota-provider", o.QuotaProvider,
		"--deepseek-provider", o.DeepSeekProvider}
}

// spawnDaemon re-executes this binary with --daemon detached, writing the
// child's pid file, then returns immediately.
func spawnDaemon(cfg app.Config, addr string, opts serveOptions) error {
	if running := readPid(cfg); running > 0 {
		return fmt.Errorf("proxy already running (pid %d)", running)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cmd := exec.Command(exe, opts.execArgs(addr)...)
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
func runProxy(cfg app.Config, addr string, opts serveOptions) error {
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
	proxySrv.SetQueueTimeout(opts.QueueTimeout)
	proxySrv.EnableUsage(usageStore)
	proxySrv.EnableQuotas(store, usageStore)
	proxySrv.SetUsageSink(func(r usage.Record) {
		if err := usageStore.Insert(r); err != nil {
			// Usage recording must never take the request down; log-free
			// drop is acceptable (structural no-secret logging).
			_ = err
		}
	})
	// The quota band always reads the local plans table — no network. The
	// poller (below) is what refreshes it; without it the rows are only
	// what `agentvault plans set` wrote, and an empty table reads as
	// "unknown" (band annotation skipped).
	proxySrv.SetQuotaSource(proxySrv.PlanUsedPct)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.QuotaPoll {
		startQuotaMonitor(ctx, store, cfg, opts, proxySrv)
	}

	// Best-effort pid file so status/stop work for foreground runs too.
	pid := os.Getpid()
	_ = os.WriteFile(pidFilePath(cfg), []byte(strconv.Itoa(pid)), 0o600)
	defer os.Remove(pidFilePath(cfg))

	fmt.Printf("agentvault proxy listening on http://%s (pid %d)\n", addr, pid)
	return proxySrv.ListenAndServe(ctx, addr)
}

// startQuotaMonitor launches the M6-3 background pollers on the serve
// signal context: the GLM quota poller (hidden endpoint, file fallback)
// writing the plans table, and the DeepSeek availability probe feeding the
// /admin/routing status bit. It is only called when --quota-poll is on —
// the offline/dev-smoke path never touches the external endpoints.
func startQuotaMonitor(ctx context.Context, store *provider.Store, cfg app.Config, opts serveOptions, proxySrv *proxy.Server) {
	keyFor := func(providerID string) func() (string, error) {
		return func() (string, error) {
			keys, err := store.ListKeys(providerID)
			if err != nil {
				return "", err
			}
			for _, k := range keys {
				if k.Status == provider.KeyActive {
					// Same lookup the proxy keycache uses: service +
					// provider/key-id. The value stays in this process.
					return cfg.Secret.Get(cfg.Service, providerID+"/"+k.ID)
				}
			}
			return "", fmt.Errorf("no active key for provider %s", providerID)
		}
	}

	poller := &auto.QuotaPoller{
		Client: &auto.GLMQuotaClient{Key: keyFor(opts.QuotaProvider)},
		Apply: func(snap auto.QuotaSnapshot) error {
			return applyQuotaSnapshot(store, opts.QuotaProvider, snap)
		},
	}
	go func() { _ = poller.Run(ctx) }()

	dsHolder := &deepSeekHolder{}
	proxySrv.SetDeepSeekStatus(dsHolder.get)
	go func() {
		client := &auto.DeepSeekClient{Key: keyFor(opts.DeepSeekProvider)}
		for {
			if st, err := client.IsAvailable(ctx); err == nil {
				dsHolder.set(st)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(auto.DefaultQuotaInterval):
			}
		}
	}()
}

// applyQuotaSnapshot writes one snapshot into the plans table: the window
// length selects the row (300 min = 5h window, 7d = weekly credits).
func applyQuotaSnapshot(store *provider.Store, providerID string, snap auto.QuotaSnapshot) error {
	window := "5h"
	switch snap.WindowMinutes {
	case 7 * 24 * 60:
		window = "weekly"
	case 0:
		window = "default"
	}
	return store.SetPlan(provider.Plan{
		Provider:   providerID,
		PlanTier:   "coding",
		WindowType: window,
		Limit:      100,
		Remaining:  100 - snap.UsedPct,
		RefreshedAt: func() time.Time {
			if snap.NextReset.IsZero() {
				return time.Now()
			}
			return snap.NextReset
		}(),
		VerifiedAt: time.Now(),
	})
}

// deepSeekHolder caches the latest DeepSeek availability probe for the
// routing status endpoint (mutex-guarded; writer and HTTP handler run on
// different goroutines).
type deepSeekHolder struct {
	mu sync.Mutex
	st auto.DeepSeekStatus
}

func (h *deepSeekHolder) set(st auto.DeepSeekStatus) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.st = st
}

func (h *deepSeekHolder) get() (auto.DeepSeekStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st, !h.st.CheckedAt.IsZero()
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
					ID       string `json:"id"`
					Name     string `json:"name"`
					Keys     int    `json:"keys_active"`
					Models   int    `json:"models"`
					InFlight int    `json:"in_flight"`
					Queued   int    `json:"queued"`
				} `json:"providers"`
			}
			if err := decodeJSON(resp.Body, &out); err != nil {
				return fmt.Errorf("decode routing status: %w", err)
			}
			fmt.Printf("cooling keys: %d\nbreaker keys: %d\n", out.CoolingKeys, out.BreakerKeys)
			for _, p := range out.Providers {
				fmt.Printf("provider %s (%s): %d active keys, %d models, in-flight %d, queued %d\n",
					p.ID, p.Name, p.Keys, p.Models, p.InFlight, p.Queued)
			}
			return nil
		},
	}
}
