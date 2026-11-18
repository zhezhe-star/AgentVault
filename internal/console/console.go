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
// overview API at /console/api/overview.
func Handler(store *provider.Store, keyRef func(providerID, keyID string) string) http.Handler {
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
					if m.KeyID == k.ID {
						kv.Models = append(kv.Models, ModelView{Name: m.Name, Upstream: m.Upstream})
					}
				}
				pv.Keys = append(pv.Keys, kv)
			}
			out.Providers = append(out.Providers, pv)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})

	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic("console: embedded static fs: " + err.Error())
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}
