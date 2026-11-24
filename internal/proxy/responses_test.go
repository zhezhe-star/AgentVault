package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// parseEvents splits an SSE body into (event, data) pairs.
func parseEvents(t *testing.T, body string) []ssePair {
	t.Helper()
	var out []ssePair
	sc := bufio.NewScanner(strings.NewReader(body))
	var event, data string
	flush := func() {
		if event != "" || data != "" {
			out = append(out, ssePair{event: event, data: data})
			event, data = "", ""
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			flush()
		}
	}
	flush()
	if len(out) == 0 {
		t.Fatalf("no SSE events parsed from body:\n%s", body)
	}
	return out
}

type ssePair struct {
	event string
	data  string
}

func eventNames(events []ssePair) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.event)
	}
	return names
}

func TestResponsesRequestConversion(t *testing.T) {
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = bufReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cc1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	reqBody := `{
		"model": "test-model",
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"be terse"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_prev","name":"lookup","arguments":"{\"q\":\"x\"}"},
			{"type":"function_call_output","call_id":"call_prev","output":"result-42"}
		],
		"instructions": "system prompt here",
		"tools": [
			{"type":"function","name":"lookup","description":"finds","parameters":{"type":"object","properties":{"q":{"type":"string"}}}},
			{"type":"web_search"},
			{"type":"namespace","name":"ns","tools":[]}
		],
		"tool_choice": {"type":"function","name":"lookup"},
		"parallel_tool_calls": false,
		"reasoning": {"effort":"low","summary":"auto"},
		"store": false,
		"previous_response_id": "resp_missing",
		"include": ["reasoning.encrypted_content"],
		"prompt_cache_key": "session-1"
	}`
	resp, err := http.Post(base+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := bufReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}

	var payload chatPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, gotBody)
	}

	// Roles: instructions -> system, developer -> system, plus user,
	// assistant-with-tool_calls, tool output.
	if len(payload.Messages) != 5 {
		t.Fatalf("messages = %d, want 5: %s", len(payload.Messages), gotBody)
	}
	if payload.Messages[0].Role != "system" || mustJSONString(t, payload.Messages[0].Content) != "system prompt here" {
		t.Fatalf("instructions not mapped to leading system message: %s", gotBody)
	}
	if payload.Messages[1].Role != "system" || mustJSONString(t, payload.Messages[1].Content) != "be terse" {
		t.Fatalf("developer role not mapped to system: %s", gotBody)
	}
	if payload.Messages[2].Role != "user" || mustJSONString(t, payload.Messages[2].Content) != "hello" {
		t.Fatalf("user message wrong: %s", gotBody)
	}
	if len(payload.Messages[3].ToolCalls) != 1 || payload.Messages[3].ToolCalls[0].ID != "call_prev" ||
		payload.Messages[3].ToolCalls[0].Function.Name != "lookup" ||
		payload.Messages[3].ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("function_call input not mapped to tool_calls: %s", gotBody)
	}
	if payload.Messages[4].Role != "tool" || payload.Messages[4].ToolCallID != "call_prev" ||
		mustJSONString(t, payload.Messages[4].Content) != "result-42" {
		t.Fatalf("function_call_output not mapped to tool message: %s", gotBody)
	}

	// Only the function tool survives; namespace and web_search are dropped.
	if len(payload.Tools) != 1 || payload.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tools = %s, want only lookup", gotBody)
	}
	// tool_choice object nesting.
	tc, _ := payload.ToolChoice.(map[string]any)
	if tc == nil || tc["type"] != "function" {
		t.Fatalf("tool_choice = %v, want nested function", payload.ToolChoice)
	}
	// parallel_tool_calls passes through.
	if payload.ParallelToolCalls == nil || *payload.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want false", payload.ParallelToolCalls)
	}
	// reasoning effort low -> thinking effort low (no type field: it is
	// rejected by always-thinking models).
	if payload.Thinking == nil || payload.Thinking.Effort != "low" {
		t.Fatalf("thinking = %v, want effort low", payload.Thinking)
	}
}

func mustJSONString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("content not a JSON string: %v (%s)", err, raw)
	}
	return s
}

func TestResponsesNonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cc2","choices":[{"message":{"role":"assistant","content":"the answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"say it","stream":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var out struct {
		ID     string            `json:"id"`
		Object string            `json:"object"`
		Status string            `json:"status"`
		Model  string            `json:"model"`
		Output []json.RawMessage `json:"output"`
		Usage  *responsesUsage   `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("object/status = %s/%s", out.Object, out.Status)
	}
	if !strings.HasPrefix(out.ID, "resp_") {
		t.Fatalf("id = %s", out.ID)
	}
	if out.Usage == nil || out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 3 || out.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", out.Usage)
	}
	if len(out.Output) != 1 {
		t.Fatalf("output = %d items", len(out.Output))
	}
	var msg outputMessageItem
	if err := json.Unmarshal(out.Output[0], &msg); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Status != "completed" ||
		len(msg.Content) != 1 || msg.Content[0].Type != "output_text" || msg.Content[0].Text != "the answer" {
		t.Fatalf("message item = %+v", msg)
	}
}

func TestResponsesStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		chunks := []string{
			`{"id":"cc3","choices":[{"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{"content":"llo"},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_zzz","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}`,
			`{"id":"cc3","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			w.(http.Flusher).Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %s", ct)
	}
	raw, _ := bufReadAll(resp.Body)
	events := parseEvents(t, string(raw))

	// Sequence must start with created/in_progress and end with completed.
	names := eventNames(events)
	if names[0] != "response.created" || names[1] != "response.in_progress" {
		t.Fatalf("prefix = %v", names)
	}
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("suffix = %v", names)
	}

	// The completed envelope carries usage and both output items.
	var completed struct {
		Response responsesResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-1].data), &completed); err != nil {
		t.Fatalf("decode completed: %v (%s)", err, events[len(events)-1].data)
	}
	if completed.Response.Status != "completed" {
		t.Fatalf("completed status %s", completed.Response.Status)
	}
	if completed.Response.Usage == nil || completed.Response.Usage.InputTokens != 7 ||
		completed.Response.Usage.OutputTokens != 4 {
		t.Fatalf("usage = %+v", completed.Response.Usage)
	}
	if len(completed.Response.Output) != 2 {
		t.Fatalf("output items = %d (%v)", len(completed.Response.Output), names)
	}

	// Text deltas are present with the right fragments.
	var deltas []string
	for _, e := range events {
		if e.event != "response.output_text.delta" {
			continue
		}
		var d struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(e.data), &d); err != nil {
			t.Fatalf("decode delta: %v", err)
		}
		deltas = append(deltas, d.Delta)
	}
	if strings.Join(deltas, "") != "hello world" {
		t.Fatalf("deltas = %v", deltas)
	}

	// A function_call item.done carries call_id, name and accumulated
	// arguments.
	var fcDone outputFunctionCallItem
	for _, e := range events {
		if e.event != "response.output_item.done" {
			continue
		}
		var wrap struct {
			Item json.RawMessage `json:"item"`
		}
		_ = json.Unmarshal([]byte(e.data), &wrap)
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(wrap.Item, &probe)
		if probe.Type == "function_call" {
			if err := json.Unmarshal(wrap.Item, &fcDone); err != nil {
				t.Fatalf("decode fc item: %v", err)
			}
		}
	}
	if fcDone.CallID != "call_zzz" || fcDone.Name != "lookup" || fcDone.Arguments != `{"q":"x"}` {
		t.Fatalf("function_call done = %+v", fcDone)
	}
	// Argument deltas reassembled to the same text.
	var argDeltas strings.Builder
	for _, e := range events {
		if e.event == "response.function_call_arguments.delta" {
			var d struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal([]byte(e.data), &d)
			argDeltas.WriteString(d.Delta)
		}
	}
	if argDeltas.String() != `{"q":"x"}` {
		t.Fatalf("arg deltas = %q", argDeltas.String())
	}
}

func TestResponsesStreamingTextOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"cc4\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi","stream":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := bufReadAll(resp.Body)
	events := parseEvents(t, string(raw))
	names := eventNames(events)

	// Exactly one message item done; completed last.
	var doneItems []json.RawMessage
	for _, e := range events {
		if e.event == "response.output_item.done" {
			var wrap struct {
				Item json.RawMessage `json:"item"`
			}
			_ = json.Unmarshal([]byte(e.data), &wrap)
			doneItems = append(doneItems, wrap.Item)
		}
	}
	if len(doneItems) != 1 {
		t.Fatalf("done items = %d (%v)", len(doneItems), names)
	}
	if names[len(names)-1] != "response.completed" {
		t.Fatalf("last event = %s", names[len(names)-1])
	}
}

func TestResponsesUpstreamToolOnlyCompletion(t *testing.T) {
	// A non-streaming reply that is nothing but tool calls (typical codex
	// first turn) must still produce a valid Responses object.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cc5","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":9,"total_tokens":12}}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"list files","stream":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	var out responsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Output) != 1 {
		t.Fatalf("output = %d items", len(out.Output))
	}
	var fc outputFunctionCallItem
	if err := json.Unmarshal(mustMarshal(out.Output[0]), &fc); err != nil {
		t.Fatalf("decode fc: %v", err)
	}
	if fc.Type != "function_call" || fc.CallID != "call_abc" || fc.Name != "exec_command" || fc.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("function_call = %+v", fc)
	}
	if out.Status != "completed" || out.Usage == nil || out.Usage.OutputTokens != 9 {
		t.Fatalf("status/usage = %s/%+v", out.Status, out.Usage)
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestResponsesUsageRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"cc6","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`)
	}))
	defer upstream.Close()

	store, sec, svc := fixture(t, upstream.URL)
	srv := New(store, sec, svc)
	var records []usage.Record
	srv.SetUsageSink(func(r usage.Record) { records = append(records, r) })
	base := startProxy(t, srv)

	resp, err := http.Post(base+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"test-model","input":"hi","stream":false}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if records[0].TokensIn != 11 || records[0].TokensOut != 2 || records[0].Status != http.StatusOK {
		t.Fatalf("record = %+v", records[0])
	}
}
