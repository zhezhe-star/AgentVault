package auto

import "testing"

// TestEntrypointRecognition covers the three tier entrypoint aliases: they
// must be recognized (and no ordinary alias mistaken for one).
func TestEntrypointRecognition(t *testing.T) {
	for _, name := range []string{"auto", "auto-deep", "auto-bg"} {
		if !IsEntrypoint(name) {
			t.Fatalf("%s should be an entrypoint", name)
		}
	}
	for _, name := range []string{"glm-5.4", "auto-deep-pro", "Auto", "", "autobg", "haiku"} {
		if IsEntrypoint(name) {
			t.Fatalf("%s must not be an entrypoint", name)
		}
	}
}

// TestClassifySignalPriority walks the v1 signal priority: thinking >
// alias semantics > tools structure > size > fail-safe routine.
func TestClassifySignalPriority(t *testing.T) {
	cases := []struct {
		name     string
		req      Request
		wantTier Tier
		wantRule string
	}{
		{"thinking wins over everything", Request{Thinking: true, AliasHint: "haiku", MaxTokens: 16}, TierDeep, "thinking-on"},
		{"haiku alias", Request{AliasHint: "claude-3-5-haiku"}, TierBackground, "alias-haiku"},
		{"no tools -> routine fail-safe", Request{}, TierRoutine, "no-tools"},
		{"small max_tokens alone stays routine", Request{MaxTokens: 512}, TierRoutine, "small-max-tokens"},
		{"tools present -> tools-present routine", Request{Tools: 3}, TierRoutine, "tools-present"},
		{"utterly empty", Request{}, TierRoutine, "no-tools"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, rule := Classify(tc.req, DefaultRules)
			if tier != tc.wantTier || rule != tc.wantRule {
				t.Fatalf("got (%s,%s), want (%s,%s)", tier, rule, tc.wantTier, tc.wantRule)
			}
		})
	}
}

// TestDeepNeverDowngrades pins the blacklist by construction: with
// thinking on, no other signal can pull the tier below deep.
func TestDeepNeverDowngrades(t *testing.T) {
	req := Request{Thinking: true, AliasHint: "haiku", Tools: 0, MaxTokens: 100}
	tier, _ := Classify(req, DefaultRules)
	if tier != TierDeep {
		t.Fatalf("deep downgraded to %s", tier)
	}
}

// TestDecideTargets checks the initial in-plan target table: deep ->
// glm-5.4, routine/background -> glm-5.3-flash, and the long-context deep
// override.
func TestDecideTargets(t *testing.T) {
	targets := DefaultTargets()
	long := Request{ContextChars: LongContextTokens * 4 * 2} // far over threshold

	d := Decide(EntrypointAuto, Request{Thinking: true}, nil, targets)
	if d.Tier != TierDeep || d.TargetModel != "glm-5.4" {
		t.Fatalf("auto+thinking: %+v", d)
	}
	d = Decide(EntrypointAuto, Request{}, nil, targets)
	if d.Tier != TierRoutine || d.TargetModel != "glm-5.3-flash" {
		t.Fatalf("auto fallback: %+v", d)
	}
	d = Decide(EntrypointDeep, Request{}, nil, targets)
	if !d.PinnedTier || d.Tier != TierDeep || d.TargetModel != "glm-5.4" || d.MatchedRule != "pinned-deep" {
		t.Fatalf("pinned deep: %+v", d)
	}
	d = Decide(EntrypointBG, Request{Thinking: true}, nil, targets)
	if !d.PinnedTier || d.Tier != TierBackground || d.TargetModel != "glm-5.3-flash" {
		t.Fatalf("pinned bg ignores thinking: %+v", d)
	}
	d = Decide(EntrypointDeep, long, nil, targets)
	if d.MatchedRule != "pinned-deep/long-context" || d.TargetModel != targets.DeepLongContext {
		t.Fatalf("deep long-context: %+v", d)
	}
}

// TestDecideOverflowNeverInM6A: the decision layer must not propose
// overflow in M6-A (owner-gated path arrives with M6-6).
func TestDecideOverflowNeverInM6A(t *testing.T) {
	for _, ep := range EntryPoints {
		if d := Decide(ep, Request{Thinking: true}, nil, DefaultTargets()); d.NeedsOverflow {
			t.Fatalf("%s proposed overflow: %+v", ep, d)
		}
	}
}

// TestExtractFeatures reads the wire signals out of raw bodies: Anthropic
// thinking, OpenAI reasoning_effort / reasoning object, tools count,
// max_tokens and rough context size.
func TestExtractFeatures(t *testing.T) {
	anthropic := []byte(`{"model":"auto","thinking":{"type":"enabled"},"max_tokens":4096,
		"tools":[{"name":"bash"}],"system":"ignore me","messages":[{"role":"user","content":"hi"}]}`)
	r := ExtractFeatures(anthropic, "auto")
	if !r.Thinking || r.Tools != 1 || r.MaxTokens != 4096 || r.ContextChars == 0 {
		t.Fatalf("anthropic features: %+v", r)
	}

	openaiThinking := []byte(`{"model":"auto","reasoning_effort":"high","messages":[]}`)
	if !ExtractFeatures(openaiThinking, "auto").Thinking {
		t.Fatal("reasoning_effort=high must signal deep")
	}
	openaiNone := []byte(`{"model":"auto","reasoning_effort":"none","messages":[]}`)
	if ExtractFeatures(openaiNone, "auto").Thinking {
		t.Fatal("reasoning_effort=none must not signal deep")
	}
	reasoningObj := []byte(`{"model":"auto","reasoning":{"effort":"medium"},"messages":[]}`)
	if !ExtractFeatures(reasoningObj, "auto").Thinking {
		t.Fatal("reasoning.effort must signal deep")
	}
	plain := []byte(`{"model":"auto","messages":[{"role":"user","content":"x"}]}`)
	if ExtractFeatures(plain, "auto").Thinking {
		t.Fatal("plain body must not signal deep")
	}
}
