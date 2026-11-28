// Package console serves the embedded control-plane web UI: a three-tier
// view of providers -> keys -> models with cumulative token usage. All data
// comes from the real stores; there is no mock path by design.
package console

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/zhezhe-star/AgentVault/internal/provider"
	"github.com/zhezhe-star/AgentVault/internal/usage"
)

//go:embed static
var staticFiles embed.FS

// Overview is the JSON payload behind the three-tier view.
type Overview struct {
	Providers []ProviderView `json:"providers"`
}

// ProviderView is one provider with its keys.
type ProviderView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	BaseURL string    `json:"base_url"`
	Keys    []KeyView `json:"keys"`
}

// KeyView is one key slot with its model mappings. SecretID is the safe
// reference string; the key material is never included.
type KeyView struct {
	ID     string      `json:"id"`
	Status string      `json:"status"`
	Ref    string      `json:"ref"`
	Models []ModelView `json:"models"`
}

// ModelView is one model mapping with cumulative token usage.
type ModelView struct {
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	// TokensIn/TokensOut stay 0 until the usage store lands in M3; the
	// console renders real zeros, never placeholders that look like data.
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

// Handler returns the console HTTP handler: the embedded UI at / and the
// overview API at /console/api/overview. The usage store feeds the model
// tier's cumulative token counts (real data only; zero when no traffic).
func Handler(store *provider.Store, usageStore *usage.Store, keyRef func(providerID, keyID string) string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/console/api/overview", func(w http.ResponseWriter, _ *http.Request) {
		out := Overview{}
		providers, err := store.ListProviders()
		if err != nil {
			http.Error(w, "list providers", http.StatusInternalServerError)
			return
		}
		for _, p := range providers {
			pv := ProviderView{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL}
			keys, err := store.ListKeys(p.ID)
			if err != nil {
				http.Error(w, "list keys", http.StatusInternalServerError)
				return
			}
			models, err := store.ListModels(p.ID)
			if err != nil {
				http.Error(w, "list models", http.StatusInternalServerError)
				return
			}
			for _, k := range keys {
				kv := KeyView{ID: k.ID, Status: k.Status, Ref: keyRef(p.ID, k.ID)}
				for _, m := range models {
					if m.KeyID != k.ID {
						continue
					}
					mv := ModelView{Name: m.Name, Upstream: m.Upstream}
					if usageStore != nil {
						mv.TokensIn, mv.TokensOut, _ = usageStore.SumTokens(m.Name)
					}
					kv.Models = append(kv.Models, mv)
				}
				pv.Keys = append(pv.Keys, kv)
			}
			out.Providers = append(out.Providers, pv)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	// Owner decision channel (design §7): the console is the third face.
	// GET lists the candidates awaiting a decision; POST completes one
	// ({"decision":"approve"|"deny","id":N} or "id":"all").
	mux.HandleFunc("/console/api/pending", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			list, err := store.ListPendingDecisions(provider.PendingOpen)
			if err != nil {
				http.Error(w, "list pending", http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": list})
		case http.MethodPost:
			var req struct {
				Decision string `json:"decision"`
				ID       any    `json:"id"` // number or "all"
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request body", http.StatusBadRequest)
				return
			}
			status := map[string]string{"approve": provider.PendingApproved, "deny": provider.PendingDenied}[req.Decision]
			if status == "" {
				http.Error(w, `decision must be "approve" or "deny"`, http.StatusBadRequest)
				return
			}
			if s, ok := req.ID.(string); ok && s == "all" {
				n, err := store.DecideAllPending(status)
				if err != nil {
					http.Error(w, "decide all", http.StatusInternalServerError)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"decided": n, "decision": status})
				return
			}
			f, ok := req.ID.(float64) // json numbers decode as float64 into any
			if !ok {
				http.Error(w, `id must be a number or "all"`, http.StatusBadRequest)
				return
			}
			if err := store.DecidePending(int64(f), status); err != nil {
				http.Error(w, "decide", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"decided": 1, "decision": status})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("console: embedded static fs: " + err.Error())
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}
