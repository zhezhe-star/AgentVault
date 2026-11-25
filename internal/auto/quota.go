package auto

// Quota awareness (M6-3, design §6): a primary poll of GLM's hidden quota
// endpoint, a local-file fallback (~/.config/llm-schedule/glm-quota.json,
// unit=6 = weekly credits), and a DeepSeek availability probe. All external
// calls go through injectable base URLs — production endpoints are only
// ever touched by the running proxy with the owner-approved poll enabled;
// tests use httptest servers exclusively. Keys flow from the SecretStore
// in-process and are never logged or persisted here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

// DefaultQuotaInterval is the poll cadence for the GLM hidden endpoint.
const DefaultQuotaInterval = 5 * time.Minute

// DefaultGLMQuotaURL is GLM's internal quota endpoint (no official
// contract: any shape change degrades to the file fallback, design R1).
const DefaultGLMQuotaURL = "https://open.bigmodel.cn/api/monitor/usage/quota/limit"

// DefaultDeepSeekBalanceURL is DeepSeek's balance endpoint.
const DefaultDeepSeekBalanceURL = "https://api.deepseek.com/user/balance"

// DefaultQuotaFile is the local fallback snapshot written by the owner's
// quota-check script (unit=6 is the weekly credit window).
const DefaultQuotaFile = ".config/llm-schedule/glm-quota.json"

// QuotaSnapshot is one refreshed plan usage reading.
type QuotaSnapshot struct {
	UsedPct       float64 // 0–100
	NextReset     time.Time
	Source        string // "api" | "file"
	WindowMinutes int    // 0 when unknown (unit meaning is provider-specific)
}

// GLMQuotaClient polls the hidden quota endpoint. The key is injected by
// the caller (fetched from the SecretStore in-process); it must never be
// logged. baseURL is overridable for httptest.
type GLMQuotaClient struct {
	BaseURL string
	Key     func() (string, error)
	HTTP    *http.Client
}

// quotaAPIResponse tolerates both flat and {"data":{...}} wrappers and
// percentage expressed 0–1 or 0–100.
type quotaAPIResponse struct {
	Percentage    json.Number `json:"percentage"`
	NextResetTime int64       `json:"next_reset_time"` // unix ms (observed script format)
	Data          *struct {
		Percentage    json.Number `json:"percentage"`
		NextResetTime int64       `json:"next_reset_time"`
	} `json:"data"`
}

// Fetch reads one snapshot from the endpoint.
func (c *GLMQuotaClient) Fetch(ctx context.Context) (QuotaSnapshot, error) {
	key := ""
	if c.Key != nil {
		k, err := c.Key()
		if err != nil {
			return QuotaSnapshot{}, fmt.Errorf("auto: quota key: %w", err)
		}
		key = k
	}
	url := c.BaseURL
	if url == "" {
		url = DefaultGLMQuotaURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota request: %w", err)
	}
	// Plan key as the raw credential header; nothing else is attached.
	req.Header.Set("Authorization", key)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota status %d", resp.StatusCode)
	}
	var raw quotaAPIResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota decode: %w", err)
	}
	pctStr, reset := raw.Percentage, raw.NextResetTime
	if raw.Data != nil {
		pctStr, reset = raw.Data.Percentage, raw.Data.NextResetTime
	}
	pct, err := pctStr.Float64()
	if err != nil {
		return QuotaSnapshot{}, fmt.Errorf("auto: quota percentage: %w", err)
	}
	// Tolerate 0–1 fractions.
	if pct > 0 && pct <= 1 {
		pct *= 100
	}
	snap := QuotaSnapshot{UsedPct: pct, Source: "api"}
	if reset > 0 {
		snap.NextReset = time.UnixMilli(reset)
	}
	return snap, nil
}

// quotaFileEntry mirrors one entry of the fallback snapshot file.
type quotaFileEntry struct {
	Type          string  `json:"type"`
	Unit          int     `json:"unit"`
	UsagePct      float64 `json:"usagePct"`
	NextResetTime int64   `json:"nextResetTime"` // unix ms
}

type quotaFileDoc struct {
	Limits []quotaFileEntry `json:"limits"`
}

// LoadQuotaFile reads the local fallback snapshot (unit=6 weekly; unit=5
// 5h window). Missing file or entries yield an error so callers know the
// fallback produced nothing.
func LoadQuotaFile(path string) ([]QuotaSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auto: quota file: %w", err)
	}
	var doc quotaFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("auto: quota file decode: %w", err)
	}
	var out []QuotaSnapshot
	for _, e := range doc.Limits {
		if e.Unit != 5 && e.Unit != 6 {
			continue
		}
		snap := QuotaSnapshot{
			UsedPct: e.UsagePct, Source: "file",
			WindowMinutes: map[int]int{5: 300, 6: 7 * 24 * 60}[e.Unit],
		}
		if e.NextResetTime > 0 {
			snap.NextReset = time.UnixMilli(e.NextResetTime)
		}
		out = append(out, snap)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("auto: quota file has no usable limits")
	}
	return out, nil
}

// QuotaPoller refreshes the plans table from the API, degrading to the
// file fallback on failure. Backoff doubles on each consecutive failure,
// capped at 8x the interval; every successful poll resets it.
type QuotaPoller struct {
	Client   *GLMQuotaClient
	Fallback string // absolute path to glm-quota.json
	Interval time.Duration
	// Apply persists a snapshot (writes plans rows; never a secret).
	Apply func(QuotaSnapshot) error
	// Now is injectable for tests.
	Now func() time.Time
}

// Run blocks polling until ctx is cancelled; one immediate read first.
// It never returns an error: quota awareness degrades, it does not fail
// (design R1). Returns the last error seen, for observability only.
func (p *QuotaPoller) Run(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultQuotaInterval
	}
	backoff := 1.0
	lastErr := error(nil)
	read := func() {
		if p.Client != nil {
			if snap, err := p.Client.Fetch(ctx); err == nil {
				if err := p.apply(snap); err != nil {
					lastErr = err
				} else {
					backoff, lastErr = 1, nil
				}
				return
			} else {
				lastErr = err
			}
		}
		// Primary unavailable (or no client): file fallback.
		for _, snap := range p.fallback() {
			if err := p.apply(snap); err != nil {
				lastErr = err
			} else {
				backoff, lastErr = 1, nil
			}
		}
	}
	read()
	for {
		wait := time.Duration(float64(interval) * backoff)
		backoff = math.Min(backoff*2, 8)
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(wait):
			read()
		}
	}
}

func (p *QuotaPoller) fallback() []QuotaSnapshot {
	path := p.Fallback
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = home + "/" + DefaultQuotaFile
	}
	snaps, err := LoadQuotaFile(path)
	if err != nil {
		return nil
	}
	return snaps
}

func (p *QuotaPoller) apply(snap QuotaSnapshot) error {
	if p.Apply == nil {
		return nil
	}
	return p.Apply(snap)
}

// DeepSeekStatus is one availability probe result.
type DeepSeekStatus struct {
	Available bool
	CheckedAt time.Time
}

// DeepSeekClient probes /user/balance for the is_available hard switch
// (design §6). baseURL overridable for httptest.
type DeepSeekClient struct {
	BaseURL string
	Key     func() (string, error)
	HTTP    *http.Client
}

// IsAvailable returns the balance endpoint's is_available flag.
func (c *DeepSeekClient) IsAvailable(ctx context.Context) (DeepSeekStatus, error) {
	key := ""
	if c.Key != nil {
		k, err := c.Key()
		if err != nil {
			return DeepSeekStatus{}, fmt.Errorf("auto: deepseek key: %w", err)
		}
		key = k
	}
	url := c.BaseURL
	if url == "" {
		url = DefaultDeepSeekBalanceURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DeepSeekStatus{}, fmt.Errorf("auto: deepseek request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return DeepSeekStatus{}, fmt.Errorf("auto: deepseek fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DeepSeekStatus{}, fmt.Errorf("auto: deepseek read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return DeepSeekStatus{}, fmt.Errorf("auto: deepseek status %d", resp.StatusCode)
	}
	var doc struct {
		IsAvailable bool `json:"is_available"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return DeepSeekStatus{}, fmt.Errorf("auto: deepseek decode: %w", err)
	}
	return DeepSeekStatus{Available: doc.IsAvailable, CheckedAt: time.Now()}, nil
}
