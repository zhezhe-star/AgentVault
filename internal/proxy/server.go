// Package proxy serves the local OpenAI-compatible proxy: it accepts
// chat-completion requests on 127.0.0.1, resolves the model to a provider
// key, injects the key from the secret store in-process, and forwards the
// call upstream.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/app"
	"github.com/zhezhe-star/AgentVault/internal/auto"
	"github.com/zhezhe-star/AgentVault/internal/console"
	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// UpstreamError carries an upstream status so the handler can relay it.
type UpstreamError struct {
	Status int
	Body   []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("proxy: upstream status %d", e.Status)
}

// DefaultQueueTimeout is how long a request may wait in the FIFO
// concurrency queue before it is answered with 429 + Retry-After.
const DefaultQueueTimeout = 120 * time.Second

// Server is the proxy HTTP server.
type Server struct {
	store  *provider.Store
	secret secret.Store
	svc    string
	keys   *keyCache
	router *router
	client *http.Client
	mux    *http.ServeMux
	logger Logger
	// usageSink receives one record per forwarded request; nil disables
	// recording (tests that don't care).
	usageSink  func(usage.Record)
	quotas     *quotaChecker
	usageStore *usage.Store
	// queueTimeout bounds the FIFO wait when every provider of a model is
	// at its concurrency ceiling.
	queueTimeout time.Duration
	// quotaUsedPct reports the most-binding plan usage percentage (0-100,
	// -1 unknown) for the auto decision reason; wired by the serve command
	// when quota polling is enabled (nil = no band annotation).
	quotaUsedPct func() float64
	// deepSeekStatus reports the cached DeepSeek is_available probe (ok
	// false = never probed, e.g. quota polling disabled).
	deepSeekStatus func() (auto.DeepSeekStatus, bool)
	// affinity is the in-memory session → (provider, key, model) binding
	// table for cache affinity (design §8); nil = disabled (tests).
	affinity *sessionAffinity
	// events is the owner-decision notification face (design §7); nil or
	// empty path disables writing.
	events *auto.EventLog
	// deepSeekProviderID names the overflow provider row consulted for
	// availability before filing an overflow candidate (design §7); ""
	// disables filing.
	deepSeekProviderID string
	// glmPeak / dsPeak are the off-peak schedules (design §9). glmPeak
	// drives request marking and the concurrency relaxation; dsPeak is
	// kept for the DeepSeek side of the schedule (used by the console /
	// future deferred-task queue). nil windows = off-peak disabled.
	glmPeak, dsPeak auto.PeakWindows
	// offPeakBoost computes the effective gate limit for a provider during
	// its off-peak window (design §9 闲时放宽); nil = no relaxation. It
	// returns the possibly-raised limit (0 keeps "unlimited").
	offPeakBoost func(providerID string, limit int) int
	// autoStats counts per-tier auto requests and overflow candidates for
	// the /admin/routing auto panel (design §11).
	statMu             sync.Mutex
	tierRequests       map[auto.Tier]int64
	overflowCandidates int64
}

// SetEventLog installs the owner-decision event file (design §7; nil
// disables notification writes).
func (s *Server) SetEventLog(l *auto.EventLog) { s.events = l }

// SetDeepSeekProvider names the overflow provider id whose vault row and
// availability gate overflow-candidate filing (design §7).
func (s *Server) SetDeepSeekProvider(id string) { s.deepSeekProviderID = id }

// SetOffPeakWindows installs the off-peak schedules (design §9). nil
// windows disable off-peak marking and relaxation entirely.
func (s *Server) SetOffPeakWindows(glm, ds auto.PeakWindows) {
	s.glmPeak, s.dsPeak = glm, ds
}

// SetOffPeakBoost installs the concurrency-relaxation hook (design §9 闲
// 时放宽); the router consults it per pick. nil = no relaxation.
func (s *Server) SetOffPeakBoost(fn func(providerID string, limit int) int) {
	s.offPeakBoost = fn
	s.router.setOffPeakBoost(fn)
}

// glmOffPeak reports whether now is inside the GLM off-peak window
// (nil windows = never off-peak, the standard-tier fail-safe).
// GLMOffPeak is the exported flavor for serve wiring (the off-peak
// concurrency-boost closure reads it per pick).
func (s *Server) GLMOffPeak() bool { return s.glmOffPeak() }

func (s *Server) glmOffPeak() bool {
	if s.glmPeak.Windows == nil && s.glmPeak.Loc == nil {
		return false
	}
	return s.glmPeak.IsOffPeak(time.Now())
}

// deepSeekUsable reports whether the overflow target (design §7) could
// actually serve today: a vault provider row with an active key, and (when
// a probe result exists) a positive is_available. Unavailability degrades
// overflow filing to a decision-reason note — never a pending row.
func (s *Server) deepSeekUsable() bool {
	if s.deepSeekProviderID == "" {
		return false
	}
	if _, err := s.store.GetProvider(s.deepSeekProviderID); err != nil {
		return false
	}
	keys, err := s.store.ListKeys(s.deepSeekProviderID)
	if err != nil {
		return false
	}
	active := false
	for _, k := range keys {
		if k.Status == provider.KeyActive {
			active = true
		}
	}
	if !active {
		return false
	}
	if s.deepSeekStatus != nil {
		if st, ok := s.deepSeekStatus(); ok && !st.Available {
			return false
		}
	}
	return true
}

// resolveOverflow implements design §7 / §5a: when the soft quota band
// makes this tier an overflow candidate (70–95% background, >95%
// background+routine, deep never) AND DeepSeek is usable, file one
// pending_decision (deduped per open candidate class) and notify via the
// event file. The request itself NEVER overflows (e239): an unresolved
// pending just means the request rides GLM's normal path. DeepSeek being
// unusable degrades the candidate to a reason note. Every failure here
// degrades — filing must never take a request down.
func (s *Server) resolveOverflow(d *auto.Decision, features auto.Request, entrypoint string) {
	if d == nil || !s.glmOverflowCandidate(d) {
		return
	}
	target := auto.DefaultOverflowTarget()
	if !s.deepSeekUsable() {
		d.Reason += "; overflow candidate suppressed: deepseek unavailable"
		return
	}
	open, err := s.store.HasOpenPending(string(d.Tier), target.Provider, target.Model)
	if err != nil || open {
		if err == nil {
			d.Reason += "; overflow candidate open (awaiting owner)"
		}
		return
	}
	estTokens := int64(features.ContextChars / 4)
	estCost := float64(estTokens) / 1e6 * deepSeekInputPricePerM
	id, err := s.store.CreatePendingDecision(provider.PendingDecision{
		Provider: target.Provider, Model: target.Model,
		TaskTier: string(d.Tier), MatchedRule: d.MatchedRule,
		Reason: fmt.Sprintf("quota band %s on entrypoint %s; owner approval required before any DeepSeek switch (design §5a)",
			quotaBandOf(s.quotaUsedPct), entrypoint),
		EstTokens: estTokens, EstCost: estCost,
	})
	if err != nil {
		d.Reason += "; overflow candidate filing failed (request continues on GLM)"
		return
	}
	s.statMu.Lock()
	s.overflowCandidates++
	s.statMu.Unlock()
	d.NeedsOverflow = true
	d.MatchedRule += "/overflow-candidate"
	d.Reason += fmt.Sprintf("; overflow candidate filed (pending #%d, awaiting owner)", id)
	_ = s.events.Append(auto.Event{
		Type: auto.EventPendingCreated, ID: id, Tier: string(d.Tier),
		Provider: target.Provider, Model: target.Model,
		EstTokens: estTokens, EstCost: estCost,
	})
}

// glmOverflowCandidate applies the §5 step-4 soft bands. Requires an
// installed quota source: unknown quota never files candidates.
func (s *Server) glmOverflowCandidate(d *auto.Decision) bool {
	if s.quotaUsedPct == nil {
		return false
	}
	return auto.OverflowEligible(d.Tier, auto.QuotaBand(s.quotaUsedPct()))
}

// quotaBandOf converts a quota-source func into its band name ("" unknown).
func quotaBandOf(fn func() float64) string {
	if fn == nil {
		return ""
	}
	return auto.QuotaBand(fn())
}

// deepSeekInputPricePerM is the rough per-million-input-token figure used
// for the pending decision's display-only cost estimate (design §7 预估成
// 本). Deliberately conservative and documented as an estimate — never
// billing data.
const deepSeekInputPricePerM = 0.27

// SetQuotaSource installs the plan-usage reader consulted by the auto
// decision layer (design §5 step 4: the band only annotates the reason —
// GLM in-plan target selection is unchanged in M6-A).
func (s *Server) SetQuotaSource(fn func() float64) { s.quotaUsedPct = fn }

// SetDeepSeekStatus installs the cached availability probe reporter.
func (s *Server) SetDeepSeekStatus(fn func() (auto.DeepSeekStatus, bool)) {
	s.deepSeekStatus = fn
}

// planUsedPct reads the plans table and returns the highest used
// percentage across rows (most-binding quota first). Errors degrade to
// -1: quota awareness never blocks a request.
func (s *Server) planUsedPct() float64 {
	plans, err := s.store.ListPlans()
	if err != nil {
		return -1
	}
	worst := -1.0
	for _, p := range plans {
		if p.Limit <= 0 {
			continue
		}
		pct := (p.Limit - p.Remaining) / p.Limit * 100
		if pct > worst {
			worst = pct
		}
	}
	return worst
}

// PlanUsedPct reports the most-binding plan usage percentage (0-100) from
// the plans table, -1 when unknown; serve installs it as the quota source.
func (s *Server) PlanUsedPct() float64 { return s.planUsedPct() }

// SetAffinityTTL overrides the session-binding TTL (serve flag; tests).
func (s *Server) SetAffinityTTL(d time.Duration) {
	if s.affinity != nil {
		s.affinity.mu.Lock()
		s.affinity.ttl = d
		s.affinity.mu.Unlock()
	}
}

// SetQueueTimeout overrides the FIFO queue wait budget (serve flag).
func (s *Server) SetQueueTimeout(d time.Duration) { s.queueTimeout = d }

// SetUsageSink installs the per-call usage recorder.
func (s *Server) SetUsageSink(fn func(usage.Record)) { s.usageSink = fn }

// EnableUsage attaches the usage store so the console shows cumulative
// real token counts per model.
func (s *Server) EnableUsage(u *usage.Store) { s.usageStore = u }

// consoleHandler builds the console handler against current state.
func (s *Server) consoleHandler() http.Handler {
	return console.Handler(s.store, s.usageStore, app.KeyRef)
}

// EnableQuotas turns on rolling-window token enforcement backed by the
// given stores (provider store for quota config, usage store for counts).
func (s *Server) EnableQuotas(providers *provider.Store, usageStore *usage.Store) {
	s.quotas = &quotaChecker{providers: providers, usage: usageStore}
}

// recordUsage is a no-op-safe wrapper around the sink.
func (s *Server) recordUsage(r usage.Record) {
	if s.usageSink != nil {
		s.usageSink(r)
	}
}

// autoDecisionKey carries the per-request auto decision through the
// forwarding loop so every usage record — including the 429 paths — is
// stamped with the same audit fields (design §11).
type autoDecisionKey struct{}

// resolveAuto rewrites an auto-tier entrypoint request to its target alias
// and returns the enriched request plus the decision (nil for explicit
// aliases, which pass through untouched). boundModel is a healthy session
// affinity binding ("" when none) fed into the decision's sticky logic.
// The rules table is always the defaults in M6-A: quota awareness and
// owner-confirmed overflow arrive with M6-3/M6-6, so Decide only picks the
// in-plan GLM target.
func resolveAuto(r *http.Request, body []byte, model, boundModel string) (*http.Request, *auto.Decision, auto.Request) {
	if !auto.IsEntrypoint(model) {
		// Explicit alias = the escape hatch (design §3a): forward as-is,
		// tag the usage record via_manual.
		return r.WithContext(context.WithValue(r.Context(), autoDecisionKey{}, &auto.Decision{MatchedRule: "explicit-alias"})), nil, auto.Request{}
	}
	features := auto.ExtractFeatures(body, model)
	features.BoundModel = boundModel
	d := auto.Decide(model, features, nil, auto.DefaultTargets())
	return r.WithContext(context.WithValue(r.Context(), autoDecisionKey{}, &d)), &d, features
}

// autoRoute carries the per-request routing context shared by the
// passthrough and Responses loops: the session id, an affinity provider
// preference, the auto decision (nil for explicit aliases), the extracted
// features (for the overflow candidate's cost estimate) and whether the
// §10-3 re-decision path already ran.
type autoRoute struct {
	session        string
	bound          affinityBinding
	preferProvider string
	decision       *auto.Decision
	features       auto.Request
	redecided      bool
}

// planAuto computes the session affinity lookup for a request (before
// resolveAuto, so the healthy binding feeds the sticky decision).
func (s *Server) planAuto(r *http.Request) *autoRoute {
	ar := &autoRoute{}
	if s.affinity == nil {
		return ar
	}
	ar.session = SessionID(r.Header)
	if ar.session == "" {
		return ar
	}
	if b, ok := s.affinity.lookup(ar.session); ok {
		ar.bound = b
		ar.preferProvider = b.providerID
	}
	return ar
}

// resolve applies the decision to the request: off-peak marking, quota
// annotation, overflow-candidate filing, target model rewrite, auto-panel
// counters. Returns the new body (or the original when the decision is
// nil / the rewrite is a no-op).
func (ar *autoRoute) resolve(s *Server, r *http.Request, body []byte, model string) (*http.Request, string, []byte) {
	r, decision, features := resolveAuto(r, body, model, ar.bound.model)
	ar.decision = decision
	ar.features = features
	if decision != nil {
		if auto.IsEntrypoint(model) {
			s.statMu.Lock()
			s.tierRequests[decision.Tier]++
			s.statMu.Unlock()
			// Off-peak marking (design §9): GLM windows only — v1 traffic
			// stays in-plan on GLM, so the DeepSeek schedule is recorded
			// but not consulted here.
			if s.glmOffPeak() {
				decision.OffPeak = true
				decision.Reason += "; off-peak window"
			}
			s.annotateQuota(decision)
			s.resolveOverflow(decision, features, model)
		}
		model = decision.TargetModel
		if nb, err := rewriteModelField(body, model); err == nil {
			body = nb
		}
	}
	return r, model, body
}

// stampHeaders sets the observability response headers (design §11):
// x-agentvault-route (actual provider/model), x-agentvault-decision (tier
// + rule abbreviation) and x-agentvault-off-peak. Auto decisions carry all
// three; explicit aliases only the route header. Must run before
// WriteHeader.
func (ar *autoRoute) stampHeaders(w http.ResponseWriter, route candidate, model string) {
	w.Header().Set("x-agentvault-route", route.providerID+"/"+model)
	if ar.decision == nil {
		return
	}
	w.Header().Set("x-agentvault-decision", string(ar.decision.Tier)+":"+ar.decision.MatchedRule)
	if auto.IsEntrypoint(ar.decision.Entrypoint) {
		w.Header().Set("x-agentvault-off-peak", strconv.FormatBool(ar.decision.OffPeak))
	}
}

// onNoCandidate handles the auto re-decision path (design §10 item 3):
// the target alias just failed to pick with every serving key cooling
// down. Deep never re-routes; routine may fall back between its in-plan
// aliases. Returns the new body when the caller should retry the acquire
// loop, ok=false to fall through to the normal 404/429 answers (which
// keeps known()'s semantics honest for whatever we still cannot serve).
func (ar *autoRoute) onNoCandidate(body []byte, model string) ([]byte, string, bool) {
	if ar.decision == nil || ar.redecided {
		return nil, model, false
	}
	ar.redecided = true
	nd, ok := auto.ReDecide(*ar.decision, auto.DefaultTargets(), model)
	if !ok {
		return nil, model, false
	}
	*ar.decision = nd
	if nb, err := rewriteModelField(body, nd.TargetModel); err != nil {
		return nil, model, false
	} else {
		body = nb
	}
	return body, nd.TargetModel, true
}

// onBind records the session's sticky binding after a successful upstream
// attempt (auto decisions only; explicit aliases keep pure passthrough).
func (ar *autoRoute) onBind(s *Server, route candidate, model string) {
	if ar.decision == nil || s.affinity == nil || ar.session == "" {
		return
	}
	s.affinity.bind(ar.session, route.providerID, route.keyID, model)
}

// annotateQuota stamps the quota band onto a decision (called after
// resolveAuto when a quota source is installed).
func (s *Server) annotateQuota(d *auto.Decision) {
	if d == nil || s.quotaUsedPct == nil {
		return
	}
	d.AnnotateQuota(s.quotaUsedPct())
}

// recordUsageCtx stamps the decision fields carried in the request context
// and records the result.
func (s *Server) recordUsageCtx(ctx context.Context, r usage.Record) {
	if d, ok := ctx.Value(autoDecisionKey{}).(*auto.Decision); ok && d != nil {
		r.TaskTier = string(d.Tier)
		r.MatchedRule = d.MatchedRule
		r.DecisionReason = d.Reason
		r.CacheRebindCost = d.CacheRebind
		r.OffPeak = d.OffPeak
		if d.MatchedRule == "explicit-alias" {
			r.ViaManual = true
		}
	}
	s.recordUsage(r)
}

// New builds a Server over the given stores.
func New(store *provider.Store, sec secret.Store, service string) *Server {
	s := &Server{
		store:        store,
		secret:       sec,
		svc:          service,
		keys:         newKeyCache(sec, service),
		router:       newRouter(store, DefaultCooldown),
		client:       &http.Client{Timeout: 10 * time.Minute},
		mux:          http.NewServeMux(),
		logger:       func(_ RequestLog) {},
		queueTimeout: DefaultQueueTimeout,
	}
	s.affinity = newSessionAffinity(s.router, DefaultAffinityTTL)
	s.tierRequests = map[auto.Tier]int64{}
	s.mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("/v1/responses", s.handleResponses)
	s.mux.HandleFunc("/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s.mux.HandleFunc("/admin/routing", s.handleRoutingStatus)
	// The console handler is mounted at root: its own mux matches
	// /console/api/overview exactly and serves the embedded UI everywhere
	// else. Mounted through a delegating closure so EnableUsage (called
	// after New) is visible to later requests.
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.consoleHandler().ServeHTTP(w, r)
	})
	return s
}

// ListenAndServe binds loopback only and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if !isLoopbackHost(addr) {
		return fmt.Errorf("proxy: refusing to bind non-loopback address %q", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("proxy: listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.recoverMiddleware(s.mux)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RequestLog is what gets logged per request: metadata only. Headers,
// bodies and keys are structurally excluded by construction — there is no
// redaction to get wrong.
type RequestLog struct {
	Method string
	Path   string
	Status int
	Model  string
}

// Logger receives per-request metadata.
type Logger func(RequestLog)

// SetLogger installs a request-metadata logger.
func (s *Server) SetLogger(l Logger) { s.logger = l }

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				http.Error(rec, fmt.Sprintf(`{"error":{"message":"proxy panic: %v"}}`, p), http.StatusInternalServerError)
			}
			s.logger(RequestLog{Method: r.Method, Path: r.URL.Path, Status: rec.status})
			_ = start
		}()
		next.ServeHTTP(rec, r)
	})
}

// statusWriter captures the status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying flusher so SSE keeps working.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// chatRequest is the subset of the OpenAI chat request the proxy needs.
type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// rewriteModelField replaces the "model" member of a JSON request body,
// preserving every other field (including ones chatRequest doesn't model).
func rewriteModelField(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = raw
	return json.Marshal(obj)
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, wireOpenAI)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, wireAnthropic)
}

// wire describes how a protocol flavor reaches its upstream: the path to
// append to the candidate's base URL and how the key is presented.
type wire struct {
	upstreamPath string
	setAuth      func(req *http.Request, key string)
}

var wireOpenAI = wire{
	upstreamPath: "/chat/completions",
	setAuth: func(req *http.Request, key string) {
		req.Header.Set("Authorization", "Bearer "+key)
	},
}

// wireAnthropic forwards Anthropic Messages requests untouched: only the
// credential header is swapped; anthropic-version and the body pass as-is.
var wireAnthropic = wire{
	upstreamPath: "/v1/messages",
	setAuth: func(req *http.Request, key string) {
		req.Header.Set("x-api-key", key)
	},
}

// forward is the protocol-agnostic core shared by /v1/chat/completions and
// /v1/messages: peek the model, route, inject the key per wire, and stream
// the response back with in-request failover. Every forwarded request ends
// in exactly one usage record.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, wireFlavor wire) {
	start := time.Now()
	protocol := "openai"
	if wireFlavor.upstreamPath == wireAnthropic.upstreamPath {
		protocol = "anthropic"
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeError(w, http.StatusBadRequest, `body must be JSON with a "model" field`)
		return
	}

	// Auto tier entrypoints (auto / auto-deep / auto-bg) are not model
	// rows: resolve the target alias up front and run the rest of the
	// normal chain (upstream rewrite, acquire, failover) on the resolved
	// name. Explicit aliases keep the passthrough contract untouched.
	ar := s.planAuto(r)
	r, req.Model, body = ar.resolve(s, r, body, req.Model)

	// Try candidates in order: on 429/5xx cool the key down and fail over
	// to the next one within the same request. The alias→upstream id
	// rewrite happens per picked candidate (design §10 item 2), not
	// pre-pick: one alias may be registered under several providers with
	// different upstream ids.
	attempted := 0
	var lastStatus int
	var lastBody []byte
	var lastRoute candidate
	for {
		route, release, st := s.router.acquirePreferred(r.Context(), req.Model, s.queueTimeout, ar.preferProvider)
		switch st {
		case acquireNoCandidate:
			if nb, nm, retry := ar.onNoCandidate(body, req.Model); retry {
				body, req.Model = nb, nm
				continue
			}
			if attempted == 0 && !s.router.known(req.Model) {
				writeError(w, http.StatusNotFound, "no active key serves model "+req.Model)
			} else if attempted == 0 {
				// Every key is cooling down: this is "temporarily
				// unavailable", not "model missing" — 429 so clients
				// retry, and record it so throttling is visible.
				writeCooldown429(w, req.Model, s.router.retryAfter())
				s.recordUsageCtx(r.Context(), usage.Record{
					Model:    req.Model,
					Protocol: protocol, Status: http.StatusTooManyRequests,
					LatencyMS: time.Since(start).Milliseconds(),
				})
			} else {
				// All remaining candidates exhausted; relay the last
				// upstream failure so clients can react (429 semantics).
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(lastStatus)
				_, _ = w.Write(lastBody)
				s.recordUsageCtx(r.Context(), usage.Record{
					ProviderID: lastRoute.providerID, KeyID: lastRoute.keyID, Model: req.Model,
					Protocol: protocol, Status: lastStatus,
					LatencyMS: time.Since(start).Milliseconds(),
				})
			}
			return
		case acquireQueuedTimeout:
			// Every candidate provider sat at its concurrency ceiling for
			// the whole queue budget: 429 so clients retry later.
			writeQueued429(w, req.Model, s.queueTimeout)
			s.recordUsageCtx(r.Context(), usage.Record{
				Model:    req.Model,
				Protocol: protocol, Status: http.StatusTooManyRequests,
				LatencyMS: time.Since(start).Milliseconds(),
			})
			return
		case acquireCancelled:
			// The client went away while queued; nothing to answer.
			return
		}

		// attempt returns true to continue the failover loop (failures
		// only); any terminal answer means the request is done. The slot
		// is released on every exit — critically only after the response
		// has been fully streamed back, so streaming bodies still count
		// toward the provider's ceiling.
		attempted++
		if func() bool {
			defer release()

			// Rolling-window quota check: over-limit requests are rejected
			// with 429 before touching the upstream (and recorded so the
			// throttling is observable).
			if qt := s.quotas.overLimit(route); qt != nil {
				writeQuotaError(w, qt)
				s.recordUsageCtx(r.Context(), usage.Record{
					ProviderID: route.providerID, KeyID: route.keyID, Model: req.Model,
					Protocol: protocol, Status: http.StatusTooManyRequests,
					LatencyMS: time.Since(start).Milliseconds(),
				})
				return false
			}

			// Fetch the key in-process (TTL-cached; never persisted or logged).
			key, err := s.keys.get(route.providerID, route.keyID)
			if isNotFound(err) {
				writeError(w, http.StatusInternalServerError, "key missing from vault")
				return false
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, "fetch key from vault")
				return false
			}

			base := route.baseURL
			if wireFlavor.upstreamPath == wireAnthropic.upstreamPath {
				base = route.anthropicBaseURL
				if base == "" {
					writeError(w, http.StatusNotFound, "provider does not serve the Anthropic protocol")
					return false
				}
			}
			// Per-candidate alias rewrite (design §10 item 2): the picked
			// provider's own registered upstream id, never a first match.
			upBody := body
			if route.upstream != "" && route.upstream != req.Model {
				nb, err := rewriteModelField(body, route.upstream)
				if err == nil {
					upBody = nb
				}
			}
			upstreamURL := strings.TrimRight(base, "/") + wireFlavor.upstreamPath
			upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(upBody))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "build upstream request")
				return false
			}
			upReq.Header.Set("Content-Type", "application/json")
			wireFlavor.setAuth(upReq, key)
			// Protocol headers pass through; the client's credential headers are
			// deliberately NOT forwarded (the vault key replaces them).
			for _, h := range []string{"anthropic-version", "anthropic-beta", "accept"} {
				if v := r.Header.Get(h); v != "" {
					upReq.Header.Set(h, v)
				}
			}

			resp, err := s.client.Do(upReq)
			if err != nil {
				s.router.fail(route)
				s.keys.invalidate(route.providerID, route.keyID)
				return true
			}

			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				// Drain and stash the failure, cool the key, try the next one.
				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				lastStatus, lastBody = resp.StatusCode, respBody
				lastRoute = route
				s.failUpstream(route, resp, respBody)
				s.keys.invalidate(route.providerID, route.keyID)
				return true
			}
			s.router.success(route)
			ar.onBind(s, route, req.Model)
			ar.stampHeaders(w, route, req.Model)

			// Copy upstream response headers and stream the body through (SSE
			// transparently included: no buffering beyond header flush). A
			// bounded tee captures the bytes so usage can be extracted after
			// the stream ends.
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			var captured bytes.Buffer
			_ = flushCopy(w, io.TeeReader(resp.Body, &captured))
			resp.Body.Close()
			tokensIn, tokensOut := usage.ExtractTokens(captured.Bytes())
			s.recordUsageCtx(r.Context(), usage.Record{
				ProviderID: route.providerID, KeyID: route.keyID, Model: req.Model,
				Protocol: protocol, Status: resp.StatusCode,
				TokensIn: tokensIn, TokensOut: tokensOut,
				LatencyMS: time.Since(start).Milliseconds(),
			})
			return false
		}() {
			// The attempt failed over to another candidate.
			continue
		}
		return
	}
}

// flushCopy copies src to w, flushing after each write so SSE chunks arrive
// without proxy-added buffering.
func flushCopy(w http.ResponseWriter, src io.Reader) error {
	buf := make([]byte, 32*1024)
	flusher, _ := w.(http.Flusher)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// (routing now lives in router.go: round-robin + cooldown + failover.)

// modelsResponse is the OpenAI /v1/models shape.
type modelsResponse struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Owned  string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	models, err := s.store.ListAllModels()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list models")
		return
	}
	out := modelsResponse{Object: "list"}
	seen := map[string]bool{}
	for _, m := range models {
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		out.Data = append(out.Data, modelEntry{ID: m.Name, Object: "model", Owned: m.ProviderID})
	}
	// Auto tier entrypoints are display-only: they are not model rows and
	// never enter the polling pool — forward resolves them per request
	// (design §3a-1).
	for _, ep := range auto.EntryPoints {
		if !seen[ep] {
			seen[ep] = true
			out.Data = append(out.Data, modelEntry{ID: ep, Object: "model", Owned: "agentvault"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleRoutingStatus reports cooldown observation for the status CLI and
// the console (loopback-only by construction of the listener).
func (s *Server) handleRoutingStatus(w http.ResponseWriter, _ *http.Request) {
	providers, _ := s.store.ListProviders()
	type providerStatus struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Keys     int    `json:"keys_active"`
		Models   int    `json:"models"`
		InFlight int    `json:"in_flight"`
		Queued   int    `json:"queued"`
	}
	gstats := s.router.gateStats()
	// Auto panel (design §11): per-tier request counts, overflow candidates
	// filed and the open pending count. Store errors degrade to 0 — the
	// panel is observation, never a request path.
	s.statMu.Lock()
	tiers := map[string]int64{}
	for t, n := range s.tierRequests {
		tiers[string(t)] = n
	}
	candidates := s.overflowCandidates
	s.statMu.Unlock()
	pending, _ := s.store.CountOpenPending()
	out := struct {
		CoolingKeys       int              `json:"cooling_keys"`
		BreakerKeys       int              `json:"breaker_keys"`
		DeepSeekAvailable *bool            `json:"deepseek_available,omitempty"`
		Auto              map[string]any   `json:"auto"`
		Providers         []providerStatus `json:"providers"`
	}{CoolingKeys: s.router.coolingCount(), BreakerKeys: s.router.breakerCount()}
	out.Auto = map[string]any{
		"requests":            tiers,
		"overflow_candidates": candidates,
		"pending":             pending,
	}
	if s.deepSeekStatus != nil {
		if st, ok := s.deepSeekStatus(); ok {
			out.DeepSeekAvailable = &st.Available
		}
	}
	for _, p := range providers {
		keys, _ := s.store.ListKeys(p.ID)
		active := 0
		for _, k := range keys {
			if k.Status == provider.KeyActive {
				active++
			}
		}
		models, _ := s.store.ListModels(p.ID)
		gs := gstats[p.ID]
		out.Providers = append(out.Providers, providerStatus{
			ID: p.ID, Name: p.Name, Keys: active, Models: len(models),
			InFlight: gs.InFlight, Queued: gs.Queued,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": msg, "type": "agentvault_error"},
	})
}

// quotaMarkers are body substrings that indicate a 429 is about quota /
// balance exhaustion rather than momentary concurrency throttling. Quota
// failures do not clear up in seconds, so they take the long cooldown.
var quotaMarkers = []string{
	"quota", "insufficient", "balance", "arrears", "欠费", "余额", "额度",
}

// rateLimitCooldown classifies an upstream 429 and returns the cooldown to
// apply. It honors the upstream's own Retry-After (delta-seconds or
// HTTP-date) when present; otherwise a body that looks quota-related gets
// the default (long) cooldown — signaled by returning 0 — and everything
// else is treated as short-lived concurrency throttling.
func rateLimitCooldown(resp *http.Response, body []byte) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
		if t, err := http.ParseTime(ra); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	lower := strings.ToLower(string(body))
	for _, m := range quotaMarkers {
		if strings.Contains(lower, m) {
			return 0
		}
	}
	return DefaultShortCooldown
}

// failUpstream cools the route down according to the upstream failure:
// 429s are classified by rateLimitCooldown, everything else (5xx, network
// errors) takes the default long cooldown.
func (s *Server) failUpstream(route candidate, resp *http.Response, body []byte) {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if d := rateLimitCooldown(resp, body); d > 0 {
			s.router.fail(route, withCooldown(d))
			return
		}
	}
	s.router.fail(route)
}

// writeCooldown429 answers a request whose every candidate key is cooling
// down: 429 (not 404 — the model exists, it is momentarily unavailable)
// with a Retry-After naming the soonest expected recovery.
func writeCooldown429(w http.ResponseWriter, model string, retryAfter time.Duration) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	}
	writeError(w, http.StatusTooManyRequests,
		"all keys serving model "+model+" are cooling down (rate limited upstream); retry later")
}

// writeQueued429 answers a request that sat out the full queue budget with
// every candidate provider at its concurrency ceiling: the same 429 +
// Retry-After semantics as the cooldown path, but the message names the
// queue so operators can tell throttle-cooling from concurrency queuing
// apart in client logs.
func writeQueued429(w http.ResponseWriter, model string, queuedFor time.Duration) {
	retry := time.Duration(int64(math.Ceil(queuedFor.Seconds()))) * time.Second
	if retry < time.Second {
		retry = time.Second
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
	writeError(w, http.StatusTooManyRequests,
		"queued too long: all providers serving model "+model+" are at their concurrency ceiling; retry later")
}

// writeQuotaError emits a structured 429 naming the quota that tripped.
func writeQuotaError(w http.ResponseWriter, qt *provider.Quota) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": "quota exceeded",
			"type":    "agentvault_quota_exceeded",
			"scope":   qt.Scope,
			"ref_id":  qt.RefID,
		},
	})
}
