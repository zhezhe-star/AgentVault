package usage

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestInsertQueryRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.Insert(Record{
		ProviderID: "bigmodel", KeyID: "primary", Model: "glm-5.3-flash",
		Protocol: "anthropic", Status: 200, TokensIn: 20, TokensOut: 64, LatencyMS: 350,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.Query(10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Model != "glm-5.3-flash" || got[0].TokensIn != 20 || got[0].TokensOut != 64 || got[0].Status != 200 {
		t.Fatalf("unexpected record: %+v", got)
	}
	in, out, err := s.SumTokens("glm-5.3-flash")
	if err != nil || in != 20 || out != 64 {
		t.Fatalf("sum: in=%d out=%d err=%v", in, out, err)
	}
}

func TestConcurrentInserts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Insert(Record{ProviderID: "p", KeyID: "k", Model: "m",
				Protocol: "openai", Status: 200, TokensIn: 1, TokensOut: 1}); err != nil {
				t.Errorf("insert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := s.Query(n)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != n {
		t.Fatalf("records = %d, want %d", len(got), n)
	}
	if in, _, _ := s.SumTokens("m"); in != n {
		t.Fatalf("sum tokens = %d, want %d", in, n)
	}
}

func TestExtractTokensOpenAI(t *testing.T) {
	body := []byte(`{"id":"x","usage":{"prompt_tokens":11,"completion_tokens":7}}`)
	in, out := ExtractTokens(body)
	if in != 11 || out != 7 {
		t.Fatalf("openai json: in=%d out=%d", in, out)
	}
	// SSE: final chunk carries usage; earlier chunks must not win.
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n")
	in, out = ExtractTokens(sse)
	if in != 30 || out != 5 {
		t.Fatalf("openai sse: in=%d out=%d", in, out)
	}
}

func TestExtractTokensAnthropic(t *testing.T) {
	body := []byte(`{"type":"message","usage":{"input_tokens":20,"output_tokens":64}}`)
	in, out := ExtractTokens(body)
	if in != 20 || out != 64 {
		t.Fatalf("anthropic json: in=%d out=%d", in, out)
	}
	// Streaming: usage arrives in message_start (input) and message_delta
	// (cumulative output); the delta must win.
	sse := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":25,\"output_tokens\":42}}\n\n")
	in, out = ExtractTokens(sse)
	if in != 25 || out != 42 {
		t.Fatalf("anthropic sse: in=%d out=%d", in, out)
	}
}

func TestExtractTokensGarbage(t *testing.T) {
	in, out := ExtractTokens([]byte("not json at all"))
	if in != 0 || out != 0 {
		t.Fatalf("garbage: in=%d out=%d", in, out)
	}
}

func TestDecisionFieldsRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	rec := Record{
		ProviderID: "p", KeyID: "k", Model: "glm-5.4", Protocol: "anthropic", Status: 200,
		TaskTier: "deep", MatchedRule: "pinned-deep", DecisionReason: "entrypoint auto-deep pins the deep tier",
		ViaManual: true, CacheRebindCost: 42,
	}
	if err := s.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := s.Query(10)
	if err != nil || len(got) != 1 {
		t.Fatalf("Query: %v %+v", err, got)
	}
	g := got[0]
	if g.TaskTier != "deep" || g.MatchedRule != "pinned-deep" || g.DecisionReason == "" ||
		!g.ViaManual || g.CacheRebindCost != 42 || g.Overflow || g.OffPeak {
		t.Fatalf("decision fields did not roundtrip: %+v", g)
	}
	// A plain record keeps the new columns at zero values.
	if err := s.Insert(Record{ProviderID: "p", Model: "glm-5.3-flash", Protocol: "openai", Status: 200}); err != nil {
		t.Fatalf("Insert plain: %v", err)
	}
	got, _ = s.Query(10)
	plain := got[0]
	if plain.TaskTier != "" || plain.ViaManual {
		t.Fatalf("plain record polluted: %+v", plain)
	}
}
