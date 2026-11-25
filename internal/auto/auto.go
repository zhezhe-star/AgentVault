// Package auto implements the auto-mode decision layer (design
// docs/design/auto-mode.md §3/§4): clients request one of the tier
// entrypoint aliases (auto / auto-deep / auto-bg) and Decide maps request
// features onto a concrete target model within the GLM plan.
//
// Scope note (M6-A): quota awareness, overflow confirmation and cache
// affinity are later blocks (M6-3/M6-6/M6-4); Decide only classifies the
// tier and picks the in-plan target. Explicit alias requests never reach
// this package — the proxy forwards them unchanged.
package auto

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Entrypoint aliases clients may request. These are NOT registered model
// rows: the proxy recognizes them in forward, decides, and rewrites the
// request to the target alias before routing.
const (
	EntrypointAuto = "auto"      // classify the tier per the rules
	EntrypointDeep = "auto-deep" // pinned deep tier (skip classification)
	EntrypointBG   = "auto-bg"   // pinned background tier (skip classification)
)

// EntryPoints lists the auto tier entrypoint aliases (display order for
// /v1/models).
var EntryPoints = []string{EntrypointAuto, EntrypointDeep, EntrypointBG}

// IsEntrypoint reports whether the requested model name is an auto tier
// entrypoint.
func IsEntrypoint(model string) bool {
	switch model {
	case EntrypointAuto, EntrypointDeep, EntrypointBG:
		return true
	}
	return false
}

// Tier is the task class a request is classified into.
type Tier string

const (
	TierBackground Tier = "background"
	TierRoutine    Tier = "routine"
	TierDeep       Tier = "deep"
)

// LongContextTokens is the context-size threshold above which deep and
// routine requests are routed to the long-context target glm-5.4[1m]
// (the 1M-context model variant; glm-5.3-flash rejects contexts beyond
// roughly 200k tokens with "Prompt is too long", so oversize sessions
// must land on 5.4[1m]). Configurable per Targets.
const LongContextTokens = 200_000

// LongContextTarget is the model alias serving 1M-token contexts.
const LongContextTarget = "glm-5.4[1m]"

// Request carries the signals the rules read (design §4). Signals are
// extracted from the request body by ClassifyFeatures; the caller fills
// AliasHint only when the client actually named a recognizable alias
// (impossible for the entrypoint names themselves today, kept for rule
// completeness and tests).
type Request struct {
	Thinking     bool   // thinking/reasoning enabled (strongest signal)
	AliasHint    string // original model name, for alias-semantic rules
	Tools        int    // number of tools attached (0 = plain text)
	MaxTokens    int    // requested output cap (weak signal)
	ContextChars int    // rough input size in characters (weak signal)
}

// Rule is one row of the classification table: ordered, first match wins.
// The table drives tier classification only; deep is never downgraded
// because rules are evaluated in priority order and classification stops
// at the first match (blacklist by construction, design §4).
type Rule struct {
	Name  string
	Tier  Tier
	Note  string
	Match func(Request) bool
}

// DefaultRules is the v1 classification table (design §4 priority order):
// 1. thinking/reasoning > 2. alias semantics (haiku-like) > 3. tools
// structure > 4. size signals > fallback. System-prompt features and
// streaming are deliberately not consulted. Fail-safe: everything that
// does not clearly match a stronger signal lands on routine.
var DefaultRules = []Rule{
	{
		Name: "thinking-on", Tier: TierDeep,
		Note:  "explicit thinking/reasoning parameter = deep",
		Match: func(r Request) bool { return r.Thinking },
	},
	{
		Name: "alias-haiku", Tier: TierBackground,
		Note:  "haiku-class alias semantics = background",
		Match: func(r Request) bool { return strings.Contains(strings.ToLower(r.AliasHint), "haiku") },
	},
	{
		Name: "tools-present", Tier: TierRoutine,
		Note:  "tool-carrying request = routine (agentic work, not background)",
		Match: func(r Request) bool { return r.Tools > 0 },
	},
	{
		Name: "small-max-tokens", Tier: TierRoutine,
		Note:  "small output cap leans background-light but stays routine in v1",
		Match: func(r Request) bool { return r.MaxTokens > 0 && r.MaxTokens <= 1024 },
	},
	{
		Name: "no-tools", Tier: TierRoutine,
		Note:  "plain-text request without tools = routine (fail-safe, not background)",
		Match: func(r Request) bool { return r.Tools == 0 },
	},
	{
		Name: "default", Tier: TierRoutine,
		Note:  "unclassified: fail-safe routine (design §4)",
		Match: func(Request) bool { return true },
	},
}

// Classify runs the rules table in order and returns the first match.
func Classify(r Request, rules []Rule) (Tier, string) {
	for _, rule := range rules {
		if rule.Match(r) {
			return rule.Tier, rule.Name
		}
	}
	// An empty table must still fail safe to routine.
	return TierRoutine, "default"
}

// Targets maps a tier onto its in-plan target model alias (design §2/§12
// initial table, data-driven so it can be retuned without code changes).
type Targets struct {
	Deep              string
	Routine           string
	Background        string
	LongContextTarget string // glm-5.4[1m]: deep AND routine oversize sessions
	LongContextTokens int
}

// DefaultTargets is the initial target table for the GLM coding plan.
func DefaultTargets() Targets {
	return Targets{
		Deep:              "glm-5.4",
		Routine:           "glm-5.3-flash",
		Background:        "glm-5.3-flash",
		LongContextTarget: LongContextTarget,
		LongContextTokens: LongContextTokens,
	}
}

// Target returns the model alias for a tier under the table, applying the
// long-context override for deep and routine requests (glm-5.3-flash
// rejects ~200k+ token contexts, background stays put).
func (t Targets) Target(tier Tier, r Request) string {
	if tier != TierBackground && t.LongContextTokens > 0 && t.LongContextTarget != "" &&
		contextTokens(r) >= t.LongContextTokens {
		return t.LongContextTarget
	}
	switch tier {
	case TierDeep:
		return t.Deep
	case TierBackground:
		return t.Background
	default:
		return t.Routine
	}
}

// contextTokens estimates the input token count from the character count
// (rough ~4 chars/token for mixed CJK/code; precision is not required —
// only the long-context threshold reads it).
func contextTokens(r Request) int { return r.ContextChars / 4 }

// Decision is the outcome of one auto decision, recorded into usage for
// auditability (design §11).
type Decision struct {
	Entrypoint    string // which entrypoint alias the client requested
	Tier          Tier
	TargetModel   string
	MatchedRule   string
	Reason        string
	PinnedTier    bool // entrypoint pinned the tier; classification skipped
	NeedsOverflow bool // always false in M6-A (overflow is M6-6, owner-gated)
}

// Decide maps an entrypoint request onto a target model. Pinned
// entrypoints skip classification but keep the same target/record path
// (design §3a-1). The rules table may be nil to use DefaultRules.
func Decide(entrypoint string, req Request, rules []Rule, targets Targets) Decision {
	if rules == nil {
		rules = DefaultRules
	}
	d := Decision{Entrypoint: entrypoint, TargetModel: targets.Routine}
	switch entrypoint {
	case EntrypointDeep:
		d.Tier, d.PinnedTier = TierDeep, true
		d.MatchedRule = "pinned-deep"
		d.Reason = "entrypoint auto-deep pins the deep tier"
	case EntrypointBG:
		d.Tier, d.PinnedTier = TierBackground, true
		d.MatchedRule = "pinned-bg"
		d.Reason = "entrypoint auto-bg pins the background tier"
	default:
		tier, rule := Classify(req, rules)
		d.Tier, d.MatchedRule = tier, rule
		d.Reason = "classified by rule " + rule
	}
	d.TargetModel = targets.Target(d.Tier, req)
	if d.Tier != TierBackground && targets.LongContextTokens > 0 && contextTokens(req) >= targets.LongContextTokens {
		d.MatchedRule += "/long-context"
		d.Reason += "; oversized context routed to " + targets.LongContextTarget
	}
	return d
}

// Quota bands for the GLM plan usage percentage (design §5 step 4).
// M6-A scope: the band only annotates decision_reason — target selection
// is untouched; overflow handling is the owner-gated M6-C path.
const (
	BandHealthy  = "quota-healthy"  // used < 70%
	BandElevated = "quota-elevated" // 70–95%
	BandCritical = "quota-critical" // > 95%
)

// QuotaBand maps a used-percentage (0–100) onto its band, "-1" (unknown)
// to "".
func QuotaBand(usedPct float64) string {
	if usedPct < 0 {
		return ""
	}
	switch {
	case usedPct > 95:
		return BandCritical
	case usedPct >= 70:
		return BandElevated
	default:
		return BandHealthy
	}
}

// AnnotateQuota appends the quota band to a decision's reason (no target
// change — GLM in-plan selection stays; overflow needs owner confirmation
// via M6-C). usedPct < 0 means "unknown", leaving the decision untouched.
func (d *Decision) AnnotateQuota(usedPct float64) {
	band := QuotaBand(usedPct)
	if band == "" {
		return
	}
	d.Reason += "; " + band + fmt.Sprintf(" (%.0f%% used)", usedPct)
}

// bodyProbe mirrors the request-body fields the signal extractor reads
// (both wire flavors). Everything else is ignored.
type bodyProbe struct {
	Thinking        json.RawMessage   `json:"thinking"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Reasoning       json.RawMessage   `json:"reasoning"`
	Tools           []json.RawMessage `json:"tools"`
	MaxTokens       int               `json:"max_tokens"`
	Messages        []json.RawMessage `json:"messages"`
}

// ExtractFeatures pulls the classification signals from a raw request
// body (OpenAI chat-completions or Anthropic messages flavor). Thinking is
// detected as: Anthropic {"thinking":{"type":"enabled"}} / truthy flag,
// OpenAI reasoning_effort not in {none, minimal, ""}, or a truthy
// {"reasoning": ...} object. It never reads the system prompt (design §4).
func ExtractFeatures(body []byte, aliasHint string) Request {
	var p bodyProbe
	_ = json.Unmarshal(body, &p)
	r := Request{
		AliasHint: aliasHint,
		Tools:     len(p.Tools),
		MaxTokens: p.MaxTokens,
	}
	r.Thinking = thinkingEnabled(p)
	for _, m := range p.Messages {
		r.ContextChars += len(m)
	}
	return r
}

func thinkingEnabled(p bodyProbe) bool {
	if len(p.Thinking) > 0 {
		var v any
		if err := json.Unmarshal(p.Thinking, &v); err == nil {
			switch t := v.(type) {
			case bool:
				if t {
					return true
				}
			case map[string]any:
				if typ, _ := t["type"].(string); typ == "enabled" {
					return true
				}
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(p.ReasoningEffort)) {
	case "", "none", "minimal":
	default:
		return true
	}
	if len(p.Reasoning) > 0 {
		var v map[string]any
		if err := json.Unmarshal(p.Reasoning, &v); err == nil && len(v) > 0 {
			if ef, _ := v["effort"].(string); ef != "" && ef != "none" {
				return true
			}
		}
	}
	return false
}
