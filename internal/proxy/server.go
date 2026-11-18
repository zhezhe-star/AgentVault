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
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// UpstreamError carries an upstream status so the handler can relay it.
type UpstreamError struct {
	Status int
	Body   []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("proxy: upstream status %d", e.Status)
}

// Server is the proxy HTTP server.
type Server struct {
	store  *provider.Store
	secret secret.Store
	svc    string
	client *http.Client
	mux    *http.ServeMux
}

// New builds a Server over the given stores.
func New(store *provider.Store, sec secret.Store, service string) *Server {
	s := &Server{
		store:  store,
		secret: sec,
		svc:    service,
		client: &http.Client{Timeout: 10 * time.Minute},
		mux:    http.NewServeMux(),
	}
	s.mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, fmt.Sprintf(`{"error":{"message":"proxy panic: %v"}}`, rec), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// chatRequest is the subset of the OpenAI chat request the proxy needs.
type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
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

	// Resolve the model to a provider and one of its active keys.
	route, err := s.resolveRoute(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Fetch the key in-process from the secret store.
	key, err := s.secret.Get(s.svc, route.providerID+"/"+route.keyID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "fetch key from vault")
		return
	}

	upstreamURL := strings.TrimRight(route.baseURL, "/") + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "build upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+key)

	resp, err := s.client.Do(upReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
		return
	}

	// Copy upstream response headers and stream the body through (SSE
	// transparently included: no buffering beyond header flush).
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_ = flushCopy(w, resp.Body)
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

// resolvedRoute is a chosen upstream for a request.
type resolvedRoute struct {
	providerID string
	keyID      string
	baseURL    string
}

// resolveRoute maps a requested model name to a provider and its first
// active key. Routing policy (rotation, cooldown) lands in M2-3 and will
// replace this selection.
func (s *Server) resolveRoute(model string) (resolvedRoute, error) {
	models, err := s.store.ListAllModels()
	if err != nil {
		return resolvedRoute{}, fmt.Errorf("list models: %w", err)
	}
	for _, m := range models {
		if m.Name != model {
			continue
		}
		keys, err := s.store.ListKeys(m.ProviderID)
		if err != nil {
			return resolvedRoute{}, fmt.Errorf("list keys: %w", err)
		}
		for _, k := range keys {
			if k.Status != provider.KeyActive {
				continue
			}
			p, err := s.store.GetProvider(m.ProviderID)
			if err != nil {
				continue
			}
			return resolvedRoute{providerID: m.ProviderID, keyID: k.ID, baseURL: p.BaseURL}, nil
		}
	}
	return resolvedRoute{}, errors.New("no active key serves model " + model)
}

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
