package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/secret"
)

// Placeholder replaces plaintext in rewritten files. Clients either talk to
// the proxy (which injects the real key) or read it via agentvault run.
const Placeholder = "agentvault-local-placeholder"

// ImportItem is one planned rewrite: where, what classification, and which
// keychain ref receives the secret.
type ImportItem struct {
	Finding Finding
	Ref     string // keychain ref, e.g. "ark/ark_api_key"
}

// ImportPlan is the full set of rewrites behind one confirmation.
type ImportPlan struct {
	Items []ImportItem
}

// BuildPlan turns findings into a plan, skipping compliant entries (never
// touched) and assigning keychain refs provider/keyID.
func BuildPlan(found []Finding) ImportPlan {
	return BuildPlanFiltered(found, nil)
}

// BuildPlanFiltered additionally restricts the plan to the given absolute
// paths (nil/empty = all files) — the per-file approval gate.
func BuildPlanFiltered(found []Finding, only []string) ImportPlan {
	allowed := map[string]bool{}
	for _, p := range only {
		allowed[p] = true
	}
	plan := ImportPlan{}
	seen := map[string]int{}
	for _, f := range found {
		if f.Class == ClassCompliant {
			continue
		}
		if len(allowed) > 0 && !allowed[f.Path] {
			continue
		}
		provider := f.Provider
		if provider == "" {
			provider = "misc"
		}
		keyID := sanitizeKeyID(f.Var)
		// Same var multiple times (same or different files): disambiguate so
		// every secret lands in its own keychain entry — file base first,
		// then an occurrence counter (each ref must be unique).
		base := sanitizeKeyID(strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path)))
		seen[provider+"/"+keyID]++
		n := seen[provider+"/"+keyID]
		if n > 1 {
			keyID = keyID + "_" + base
			seen[provider+"/"+keyID+"_"+base]++
			if m := seen[provider+"/"+keyID+"_"+base]; m > 1 {
				keyID = fmt.Sprintf("%s_%d", keyID, m)
			}
		}
		plan.Items = append(plan.Items, ImportItem{
			Finding: f,
			Ref:     provider + "/" + keyID,
		})
	}
	return plan
}

func sanitizeKeyID(v string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(v, "-", "_"), " ", "_"))
}

// Preview renders the plan; masked values only.
func (p ImportPlan) Preview(w io.Writer) {
	for _, it := range p.Items {
		fmt.Fprintf(w, "%s:%d %s [%s] -> keychain ref:%s\n",
			it.Finding.Path, it.Finding.Line, it.Finding.Var, it.Finding.Class, it.Ref)
	}
	fmt.Fprintf(w, "\n%d item(s) to import; .bak backups will be written\n", len(p.Items))
}

// plaintext extracts the full secret for a finding by re-reading its file —
// findings only carry masks, so this runs at apply time, not plan time.
func (it ImportItem) plaintext() (string, error) {
	data, err := os.ReadFile(it.Finding.Path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	if it.Finding.Kind == "json-env" {
		return jsonEnvValue(data, it.Finding.Var)
	}
	if it.Finding.Line <= 0 || it.Finding.Line > len(lines) {
		return "", fmt.Errorf("migrate: line %d out of range in %s", it.Finding.Line, it.Finding.Path)
	}
	line := lines[it.Finding.Line-1]
	switch it.Finding.Kind {
	case "shell-export":
		if m := shellLine.FindStringSubmatch(line); m != nil {
			return m[2] + m[3] + m[4], nil
		}
	case "yaml-key":
		if m := yamlKeyLine.FindStringSubmatch(line); m != nil {
			return m[2], nil
		}
	case "toml-env":
		if m := tomlEnvLine.FindStringSubmatch(line); m != nil {
			return m[2], nil
		}
	}
	return "", fmt.Errorf("migrate: cannot re-extract %s at %s:%d", it.Finding.Var, it.Finding.Path, it.Finding.Line)
}

// jsonEnvValue pulls one var's value from a JSON "env" block.
func jsonEnvValue(data []byte, varName string) (string, error) {
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", err
	}
	v, ok := doc.Env[varName]
	if !ok {
		return "", fmt.Errorf("migrate: var %s not in json env", varName)
	}
	return v, nil
}

// ImportReport summarizes what Apply did.
type ImportReport struct {
	Imported []string // keychain refs
	Files    []string // rewritten files (with .bak siblings)
}

// Apply executes a confirmed plan: store each secret in the vault backend,
// back up and rewrite each affected file. Compliant findings are absent from
// the plan by construction. Files are rewritten only after ALL secrets of
// that file were stored, so a partial failure never loses a key.
// Options tunes Apply's rewrites. Zero value rewrites values only.
type Options struct {
	// ProxyAddr (e.g. "127.0.0.1:8787") makes proxy-attach rewrites also
	// point the client's base_url at the local proxy: litellm api_base gets
	// set/added, JSON env *_BASE_URL siblings get rewritten to http://addr.
	ProxyAddr string
}

// Apply is ApplyWithOptions with default options.
func Apply(store secret.Store, service string, plan ImportPlan) (ImportReport, error) {
	return ApplyWithOptions(store, service, plan, Options{})
}

// ApplyWithOptions executes a confirmed plan: store each secret in the vault
// backend, back up and rewrite each affected file. Compliant findings are
// absent from the plan by construction. Files are rewritten only after ALL
// secrets of that file were stored, so a partial failure never loses a key.
func ApplyWithOptions(store secret.Store, service string, plan ImportPlan, opts Options) (ImportReport, error) {
	var rep ImportReport
	// Phase 1: extract and store all secrets first.
	secrets := map[string]string{} // ref -> plaintext
	for _, it := range plan.Items {
		plain, err := it.plaintext()
		if err != nil {
			return rep, err
		}
		if placeholder(plain) {
			continue // nothing real to store; rewrite still neutralizes the line
		}
		if err := store.Set(service, it.Ref, plain); err != nil {
			return rep, fmt.Errorf("migrate: store %s: %w", it.Ref, err)
		}
		secrets[it.Ref] = plain
		rep.Imported = append(rep.Imported, it.Ref)
	}

	// Phase 2: rewrite files, one .bak per file, only now that storage succeeded.
	seenFile := map[string]string{} // path -> .bak path
	for _, it := range plan.Items {
		bak, done := seenFile[it.Finding.Path]
		if !done {
			bak = it.Finding.Path + ".bak-" + time.Now().Format("20060102-150405")
			data, err := os.ReadFile(it.Finding.Path)
			if err != nil {
				return rep, err
			}
			if err := os.WriteFile(bak, data, 0o600); err != nil {
				return rep, fmt.Errorf("migrate: write backup %s: %w", bak, err)
			}
			seenFile[it.Finding.Path] = bak
		}
		if err := rewriteLine(it, opts); err != nil {
			return rep, err
		}
	}
	for path := range seenFile {
		rep.Files = append(rep.Files, path)
	}
	return rep, nil
}

// rewriteLine replaces one occurrence in place: run-inject shell lines become
// a comment naming the keychain ref and the agentvault run command; JSON/YAML/
// TOML values become the placeholder string (structural validity preserved).
func rewriteLine(it ImportItem, opts Options) error {
	data, err := os.ReadFile(it.Finding.Path)
	if err != nil {
		return err
	}
	if it.Finding.Kind == "json-env" {
		// JSON env vars have no line anchor; rewrite by exact key match in
		// the whole document (env var names are unique within the block).
		re := regexp.MustCompile(`("` + regexp.QuoteMeta(it.Finding.Var) + `"\s*:\s*")[^"]*(")`)
		out := re.ReplaceAll(data, []byte("${1}"+Placeholder+"${2}"))
		// proxy-attach: point the sibling *_BASE_URL at the proxy.
		if opts.ProxyAddr != "" && it.Finding.Class == ClassProxyAttach {
			if sibling, ok := baseURLOf(it.Finding.Var); ok {
				re := regexp.MustCompile(`("` + regexp.QuoteMeta(sibling) + `"\s*:\s*")[^"]*(")`)
				out = re.ReplaceAll(out, []byte("${1}http://"+opts.ProxyAddr+"${2}"))
			}
		}
		return os.WriteFile(it.Finding.Path, out, 0o600)
	}
	lines := strings.Split(string(data), "\n")
	i := it.Finding.Line - 1
	switch it.Finding.Kind {
	case "shell-export":
		// Strip the plaintext entirely: comment names the ref and restore path.
		lines[i] = fmt.Sprintf("# agentvault: %s migrated to keychain ref:%s; restore via: agentvault run --env %s=ref:%s -- <cmd>",
			it.Finding.Var, it.Ref, it.Finding.Var, it.Ref)
	case "yaml-key":
		lines[i] = replaceYAMLValue(lines[i], Placeholder)
		if opts.ProxyAddr != "" && it.Finding.Class == ClassProxyAttach {
			lines = ensureYAMLBase(lines, i, "http://"+opts.ProxyAddr+"/v1")
		}
	case "toml-env":
		lines[i] = replaceTOMLValue(lines[i], Placeholder)
	default:
		return fmt.Errorf("migrate: unsupported kind %s", it.Finding.Kind)
	}
	return os.WriteFile(it.Finding.Path, []byte(strings.Join(lines, "\n")), 0o600)
}

// baseURLOf maps a credential var to its client's base_url var: the
// ANTHROPIC_AUTH_TOKEN -> ANTHROPIC_BASE_URL convention.
func baseURLOf(varName string) (string, bool) {
	if strings.HasSuffix(varName, "_TOKEN") {
		return strings.TrimSuffix(varName, "_TOKEN") + "_BASE_URL", true
	}
	return "", false
}

// leading returns the indentation of a line.
func leading(l string) string {
	return l[:len(l)-len(strings.TrimLeft(l, " "))]
}

// ensureYAMLBase sets or inserts an api_base line for the api_key at idx,
// keeping the same block (replacing an existing api_base nearby, else
// inserting right after the api_key line with matching indent).
func ensureYAMLBase(lines []string, idx int, base string) []string {
	indent := leading(lines[idx]) + "  "
	// Search forward within the block, then backward (litellm puts api_base
	// before api_key).
	for end := idx + 1; end < len(lines); end++ {
		l := lines[end]
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, " ") {
			break // dedented: left the block
		}
		if strings.Contains(l, "api_base:") {
			lines[end] = leading(l) + "api_base: " + base
			return lines
		}
	}
	for start := idx - 1; start >= 0; start-- {
		l := lines[start]
		if strings.Contains(l, "api_base:") {
			lines[start] = leading(l) + "api_base: " + base
			return lines
		}
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, " ") {
			break // dedented above the block
		}
	}
	return append(lines[:idx+1], append([]string{indent + "api_base: " + base}, lines[idx+1:]...)...)
}

// replaceJSONValue swaps the value of "VAR": "old" in a single JSON line.
func replaceJSONValue(line, varName, newV string) string {
	re := regexp.MustCompile(`("` + regexp.QuoteMeta(varName) + `"\s*:\s*")[^"]*(")`)
	return re.ReplaceAllString(line, `${1}`+newV+`${2}`)
}

// replaceYAMLValue swaps the scalar after "api_key:".
func replaceYAMLValue(line, newV string) string {
	re := regexp.MustCompile(`((?:-\s*)?(api_key|apikey|token)\s*:\s*)["']?[^"'\s]+["']?`)
	return re.ReplaceAllString(line, `${1}"`+newV+`"`)
}

// replaceTOMLValue swaps the quoted value of a TOML key line.
func replaceTOMLValue(line, newV string) string {
	re := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*\s*=\s*)"[^"]*"`)
	return re.ReplaceAllString(line, `${1}"`+newV+`"`)
}
