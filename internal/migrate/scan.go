// Package migrate implements the plaintext key scanner and import/rewrite
// flow for M4: find plaintext credentials in local dotfiles, classify them
// by consumption style (proxy-attach / run-inject / compliant), and later
// rewrite the files after vault import with .bak backups.
package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Class is how a plaintext key is consumed, hence how it migrates.
type Class string

const (
	// ClassProxyAttach: the client can point base_url at the local proxy
	// (claude settings, litellm config). Rewrite = swap base_url + placeholder.
	ClassProxyAttach Class = "proxy-attach"
	// ClassRunInject: the tool needs its own raw key in env (MCP servers,
	// direct SDK CLIs). Rewrite = comment out the line; inject via agentvault run.
	ClassRunInject Class = "run-inject"
	// ClassCompliant: leave alone (already a placeholder, or explicitly exempt).
	ClassCompliant Class = "compliant"
)

// Finding is one plaintext key occurrence. MaskedValue never contains the
// full secret by construction.
type Finding struct {
	Path        string `json:"path"`         // absolute file path
	Line        int    `json:"line"`         // 1-based
	Var         string `json:"var"`          // env var / field name
	Kind        string `json:"kind"`         // shell-export | json-env | yaml-key | toml-env
	MaskedValue string `json:"masked_value"` // abcd…wxyz + length
	Class       Class  `json:"class"`
	Provider    string `json:"provider"` // best-guess provider id for import
}

// Scanner scans a HOME-like directory for plaintext keys.
type Scanner struct {
	home string
	// classify overrides the built-in var→class defaults, keyed "VAR" or
	// "path-suffix:VAR" (more specific wins).
	classify map[string]Class
}

// NewScanner builds a scanner over the given home directory.
func NewScanner(home string) *Scanner {
	return &Scanner{home: home, classify: map[string]Class{}}
}

// SetClassification registers a class override for "VAR" or "file:VAR".
func (s *Scanner) SetClassification(key string, c Class) { s.classify[key] = c }

// targets are the known plaintext drop points, relative to HOME. Order is
// also report order.
var targets = []struct {
	rel    string
	parser func(path string) ([]Finding, error)
}{
	{".zshrc", scanShell},
	{".zshenv", scanShell},
	{".bashrc", scanShell},
	{".claude/settings.json", scanJSONEnv},
	{".config/litellm/config.yaml", scanYAML},
	{".codex/config.toml", scanTOML},
}

// Scan runs all parsers over existing targets. Missing files are skipped;
// unreadable files surface as an error on that path's findings list.
func (s *Scanner) Scan() ([]Finding, error) {
	var out []Finding
	for _, t := range targets {
		path := filepath.Join(s.home, t.rel)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found, err := t.parser(path)
		if err != nil {
			return nil, fmt.Errorf("migrate: scan %s: %w", path, err)
		}
		for i := range found {
			found[i].Class = s.classifyVar(t.rel, found[i].Var, found[i].Provider)
		}
		out = append(out, found...)
	}
	return out, nil
}

// classifyVar applies overrides then built-in defaults. Defaults: well-known
// proxy-capable client configs are proxy-attach; shell exports and MCP env
// blocks are run-inject (one tool, one dedicated key).
func (s *Scanner) classifyVar(rel, varName, provider string) Class {
	if c, ok := s.classify[rel+":"+varName]; ok {
		return c
	}
	if c, ok := s.classify[varName]; ok {
		return c
	}
	switch rel {
	case ".claude/settings.json", ".config/litellm/config.yaml":
		return ClassProxyAttach
	default:
		return ClassRunInject
	}
}

// Mask shortens a secret for display: first 4 + … + last 4 with length.
// Values shorter than 12 chars are fully hidden.
func Mask(v string) string {
	if len(v) < 12 {
		return fmt.Sprintf("***(%d chars)", len(v))
	}
	return fmt.Sprintf("%s…%s(%d chars)", v[:4], v[len(v)-4:], len(v))
}

// placeholder reports values that are not real keys (empty, proxy
// placeholders, obvious examples).
func placeholder(v string) bool {
	switch {
	case v == "":
		return true
	case strings.Contains(v, "agentvault-local-placeholder"):
		return true
	case strings.HasPrefix(v, "${"), strings.HasPrefix(v, "$"): // env var references, not plaintext
		return true
	case strings.EqualFold(v, "sk-xxx"), strings.EqualFold(v, "your-api-key"):
		return true
	}
	return false
}

// guessProvider maps var/field names to provider ids for import hints.
func guessProvider(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "ark") || strings.Contains(n, "volc"):
		return "ark"
	case strings.Contains(n, "glm"), strings.Contains(n, "bigmodel"), strings.Contains(n, "zhipu"), strings.Contains(n, "anthropic"):
		return "bigmodel"
	case strings.Contains(n, "deepseek"):
		return "deepseek"
	case strings.Contains(n, "z_ai"), strings.Contains(n, "zai"):
		return "zai"
	}
	return ""
}

var shellLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(?:"([^"]*)"|'([^']*)'|([^#\s]*))`)

// scanShell finds export VAR=... lines whose values look like keys:
// KEY/TOKEN/API_KEY/TOKEN suffixed vars with longish opaque values.
func scanShell(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for i, line := range strings.Split(string(data), "\n") {
		m := shellLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		val := m[2] + m[3] + m[4]
		if !isCredentialVar(name) || placeholder(val) {
			continue
		}
		out = append(out, Finding{
			Path: path, Line: i + 1, Var: name, Kind: "shell-export",
			MaskedValue: Mask(val), Provider: guessProvider(name),
		})
	}
	return out, nil
}

// isCredentialVar narrows shell scanning to credential-looking names so
// ordinary exports (PATH, EDITOR) never match.
func isCredentialVar(name string) bool {
	n := strings.ToUpper(name)
	return strings.HasSuffix(n, "API_KEY") || strings.HasSuffix(n, "_TOKEN") ||
		strings.HasSuffix(n, "APIKEY") || strings.HasSuffix(n, "SECRET_KEY")
}

// scanJSONEnv scans a JSON object's "env" block (claude settings.json shape).
func scanJSONEnv(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	var out []Finding
	for name, val := range doc.Env {
		if !isCredentialVar(name) || placeholder(val) {
			continue
		}
		out = append(out, Finding{
			Path: path, Line: 0, Var: name, Kind: "json-env",
			MaskedValue: Mask(val), Provider: guessProvider(name),
		})
	}
	return out, nil
}

// yamlKeyLine matches api_key: value / api-key: value style YAML scalars,
// including list items (litellm model_list entries).
var yamlKeyLine = regexp.MustCompile(`^\s*(?:-\s*)?(api_key|apikey|token)\s*:\s*["']?([^"'\s]+)["']?\s*$`)

func scanYAML(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for i, line := range strings.Split(string(data), "\n") {
		m := yamlKeyLine.FindStringSubmatch(line)
		if m == nil || placeholder(m[2]) || strings.Contains(m[2], "${") {
			continue
		}
		out = append(out, Finding{
			Path: path, Line: i + 1, Var: m[1], Kind: "yaml-key",
			MaskedValue: Mask(m[2]),
		})
	}
	return out, nil
}

// tomlEnvLine matches key = "value" inside TOML env blocks (codex MCP shape).
var tomlEnvLine = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([^"]*)"\s*$`)
var tomlSection = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

func scanTOML(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Finding
	section := ""
	for i, line := range strings.Split(string(data), "\n") {
		if m := tomlSection.FindStringSubmatch(line); m != nil {
			section = m[1]
			continue
		}
		m := tomlEnvLine.FindStringSubmatch(line)
		if m == nil || !isCredentialVar(m[1]) || placeholder(m[2]) {
			continue
		}
		out = append(out, Finding{
			Path: path, Line: i + 1, Var: m[1], Kind: "toml-env",
			MaskedValue: Mask(m[2]), Provider: guessProvider(section + " " + m[1]),
		})
	}
	return out, nil
}
