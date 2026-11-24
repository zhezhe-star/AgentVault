// Responses API bridge: serves POST /v1/responses by translating the
// OpenAI Responses wire format to an upstream chat-completions call (key
// injection, routing and usage recording are shared with the passthrough
// path), then translating the upstream reply — plain JSON or SSE stream —
// back into Responses format.
//
// Scope notes: the bridge keeps no server-side state, so `store` and
// `previous_response_id` are accepted and ignored rather than rejected;
// reasoning parameters map onto Zhipu's `thinking` switch on a best-effort
// basis; tool types other than plain `function` (codex's `namespace`,
// `web_search`, …) have no chat-completions equivalent and are dropped.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zhezhe-star/AgentVault/internal/usage"
)

// responsesRequest is the subset of the Responses request the bridge acts
// on. Everything not listed (store, include, metadata, prompt_cache_key,
// client_metadata, previous_response_id, …) is accepted and ignored.
type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      json.RawMessage `json:"instructions"` // string, or null
	Stream            bool            `json:"stream"`
	Tools             []responsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	Reasoning         json.RawMessage `json:"reasoning"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
}

// responsesTool mirrors the flat Responses tool layout
// ({"type":"function","name":...,"parameters":{...}}).
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// responsesItem is one entry of the `input` array.
type responsesItem struct {
	Type string `json:"type"`
	Role string `json:"role"`
	ID   string `json:"id"`
	// message content: string, null, or an array of {type,text} parts.
	Content json.RawMessage `json:"content"`
	// function_call
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	// function_call_output: string, null, or [{type:"output_text",text}]
	Output json.RawMessage `json:"output"`
}

// ---- Responses -> chat-completions ----

// chatMessage is one chat-completions message. Content is pre-encoded JSON
// (a JSON string) so callers can pass it through untouched.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// chatToolCall is the chat-completions tool_calls entry (message and
// streaming delta shapes both fit; index is only set in deltas).
type chatToolCall struct {
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function *chatToolCallFn `json:"function,omitempty"`
}

type chatToolCallFn struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type chatTool struct {
	Type     string      `json:"type"`
	Function chatToolDef `json:"function"`
}

type chatToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatPayload is the chat-completions request body the bridge sends upstream.
type chatPayload struct {
	Model             string        `json:"model"`
	Messages          []chatMessage `json:"messages"`
	Tools             []chatTool    `json:"tools,omitempty"`
	ToolChoice        any           `json:"tool_choice,omitempty"`
	Stream            bool          `json:"stream"`
	Thinking          *chatThinking `json:"thinking,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
}

// chatThinking maps Zhipu's effort-only thinking switch; unsupported
// upstreams are spared it because it is only set when the request asked
// for reasoning.
type chatThinking struct {
	Effort string `json:"effort"`
}

// buildChatPayload converts a Responses request into a chat-completions
// payload. Roles: instructions become a leading system message and codex's
// `developer` items map to system as well (chat-completions has no
// developer role).
func buildChatPayload(req *responsesRequest) (*chatPayload, error) {
	payload := &chatPayload{Model: req.Model, Stream: req.Stream}
	if len(req.Instructions) > 0 {
		var instr string
		if err := json.Unmarshal(req.Instructions, &instr); err == nil && instr != "" {
			payload.Messages = append(payload.Messages, systemMessage(instr))
		}
	}

	switch {
	case len(req.Input) == 0:
		// tolerated: empty input, nothing to add
	case req.Input[0] == '"':
		var text string
		if err := json.Unmarshal(req.Input, &text); err != nil {
			return nil, fmt.Errorf("decode input string: %w", err)
		}
		payload.Messages = append(payload.Messages, chatMessage{Role: "user", Content: jsonEncodeString(text)})
	default:
		var items []responsesItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("decode input items: %w", err)
		}
		for _, item := range items {
			msg, ok, err := convertInputItem(item)
			if err != nil {
				return nil, err
			}
			if ok {
				payload.Messages = append(payload.Messages, msg)
			}
		}
	}

	for _, t := range req.Tools {
		if t.Type != "function" || t.Name == "" {
			continue // namespace / web_search / custom: no chat equivalent
		}
		payload.Tools = append(payload.Tools, chatTool{
			Type: "function",
			Function: chatToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if choice := convertToolChoice(req.ToolChoice); choice != nil {
		payload.ToolChoice = choice
	}
	payload.ParallelToolCalls = req.ParallelToolCalls
	if th := convertReasoning(req.Reasoning); th != nil {
		payload.Thinking = th
	}
	return payload, nil
}

func systemMessage(text string) chatMessage {
	return chatMessage{Role: "system", Content: jsonEncodeString(text)}
}

func jsonEncodeString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// convertInputItem maps one Responses input item to a chat message. Items
// without a chat-completions equivalent (reasoning, web_search_call, …)
// return ok=false and are skipped.
func convertInputItem(item responsesItem) (chatMessage, bool, error) {
	switch item.Type {
	case "", "message":
		text := flattenText(item.Content)
		return chatMessage{Role: chatRole(item.Role), Content: jsonEncodeString(text)}, true, nil
	case "function_call":
		args := item.Arguments
		if args == "" {
			args = "{}"
		}
		callID := item.CallID
		if callID == "" {
			callID = "call_" + shortHash(item.ID+item.Name+args)
		}
		return chatMessage{
			Role: "assistant",
			ToolCalls: []chatToolCall{{
				ID:       callID,
				Type:     "function",
				Function: &chatToolCallFn{Name: item.Name, Arguments: args},
			}},
		}, true, nil
	case "function_call_output":
		text := flattenText(item.Output)
		callID := item.CallID
		if callID == "" {
			callID = "call_" + shortHash(item.ID)
		}
		return chatMessage{Role: "tool", Content: jsonEncodeString(text), ToolCallID: callID}, true, nil
	default:
		// reasoning / web_search_call / local_shell_call / custom_tool_call …
		// have no chat-completions equivalent; skipping keeps the
		// conversation a faithful subset rather than a 500.
		return chatMessage{}, false, nil
	}
}

// chatRole maps Responses roles to chat-completions roles.
func chatRole(role string) string {
	switch role {
	case "developer":
		return "system"
	case "assistant", "system", "user", "tool":
		return role
	case "":
		return "user"
	default:
		return "user"
	}
}

// flattenText extracts concatenated text from a Responses content field:
// a JSON string, null, or an array of {type,text} parts.
func flattenText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			texts = append(texts, p.Text)
		}
		return strings.Join(texts, "")
	}
	return ""
}

// convertToolChoice maps the Responses tool_choice onto the chat shape:
// a bare string passes through; {"type":"function","name":x} nests the
// name under function. Returns nil when absent or unmappable.
func convertToolChoice(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil && s != "" {
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj.Type != "function" || obj.Name == "" {
		return nil
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": obj.Name}}
}

// convertReasoning best-effort maps reasoning.effort onto Zhipu's thinking
// switch (`{"effort": "low"}`). The `type` field is deliberately not sent:
// always-thinking models reject type=disabled outright, while effort-only
// thinking is accepted across GLM models. Providers that don't know
// `thinking` only see it when the client actually asked for reasoning.
func convertReasoning(raw json.RawMessage) *chatThinking {
	var r struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &r); err != nil {
		return nil
	}
	switch r.Effort {
	case "minimal", "low", "medium", "high":
		return &chatThinking{Effort: r.Effort}
	default:
		return nil
	}
}

// ---- upstream chat-completions response shapes ----

type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type chatRespMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"` // null alongside tool calls
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []chatToolCall `json:"tool_calls"`
}

type chatCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      chatRespMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Role             string         `json:"role"`
			Content          *string        `json:"content"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
			ReasoningContent string         `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// ---- chat-completions -> Responses ----

type outputTextPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type outputMessageItem struct {
	Type    string           `json:"type"`
	ID      string           `json:"id"`
	Status  string           `json:"status"`
	Role    string           `json:"role"`
	Content []outputTextPart `json:"content"`
}

type outputFunctionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type responsesUsageDetails struct {
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type responsesUsage struct {
	InputTokens         int64                 `json:"input_tokens"`
	InputTokensDetails  responsesUsageDetails `json:"input_tokens_details"`
	OutputTokens        int64                 `json:"output_tokens"`
	OutputTokensDetails responsesUsageDetails `json:"output_tokens_details"`
	TotalTokens         int64                 `json:"total_tokens"`
}

func responsesUsageFrom(u *chatUsage) *responsesUsage {
	if u == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0) {
		return nil
	}
	return &responsesUsage{
		InputTokens:        u.PromptTokens,
		InputTokensDetails: responsesUsageDetails{CachedTokens: 0},
		OutputTokens:       u.CompletionTokens,
		OutputTokensDetails: responsesUsageDetails{
			ReasoningTokens: 0,
		},
		TotalTokens: u.TotalTokens,
	}
}

// responsesResponse is the Responses response envelope (used whole for
// non-streaming and inside created/completed events when streaming).
type responsesResponse struct {
	ID                 string          `json:"id"`
	Object             string          `json:"object"`
	CreatedAt          int64           `json:"created_at"`
	Status             string          `json:"status"`
	Model              string          `json:"model"`
	Output             []any           `json:"output"`
	Usage              *responsesUsage `json:"usage"`
	Error              any             `json:"error"`
	IncompleteDetails  any             `json:"incomplete_details"`
	Instructions       any             `json:"instructions"`
	Metadata           any             `json:"metadata"`
	Temperature        any             `json:"temperature"`
	ToolChoice         any             `json:"tool_choice"`
	Tools              []any           `json:"tools"`
	ParallelToolCalls  bool            `json:"parallel_tool_calls"`
	PreviousResponseID any             `json:"previous_response_id"`
	Reasoning          any             `json:"reasoning"`
	Store              bool            `json:"store"`
	Text               map[string]any  `json:"text,omitempty"`
}

func newResponsesEnvelope(req *responsesRequest, model string) *responsesResponse {
	return &responsesResponse{
		ID:        "resp_" + newID(),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "in_progress",
		Model:     model,
		Output:    []any{},
		Store:     false,
	}
}

func newMessageItem(text string) *outputMessageItem {
	return &outputMessageItem{
		Type:   "message",
		ID:     "msg_" + newID(),
		Status: "completed",
		Role:   "assistant",
		Content: []outputTextPart{{
			Type:        "output_text",
			Text:        text,
			Annotations: []any{},
		}},
	}
}

func newFunctionCallItem(callID, name, args string) *outputFunctionCallItem {
	return &outputFunctionCallItem{
		Type:      "function_call",
		ID:        "fc_" + newID(),
		CallID:    callID,
		Name:      name,
		Arguments: args,
		Status:    "completed",
	}
}

// newID makes a random hex id for resp_/msg_/fc_ identifiers.
func newID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing is practically impossible; fall back to time.
		return fmt.Sprintf("%x", sha256.Sum256([]byte(time.Now().String())))[:24]
	}
	return hex.EncodeToString(buf)
}

// shortHash derives a stable short suffix (for synthesized call ids).
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:24]
}

// convertChatCompletion turns a non-streaming upstream reply into a
// completed Responses envelope.
func convertChatCompletion(req *responsesRequest, cc *chatCompletion) *responsesResponse {
	out := newResponsesEnvelope(req, req.Model)
	out.Status = "completed"
	for _, choice := range cc.Choices {
		if text := choice.Message.Content; text != nil && *text != "" {
			out.Output = append(out.Output, newMessageItem(*text))
		}
		for _, tc := range choice.Message.ToolCalls {
			if tc.Function == nil {
				continue
			}
			callID := tc.ID
			if callID == "" {
				callID = "call_" + shortHash(tc.Function.Name+tc.Function.Arguments)
			}
			args := tc.Function.Arguments
			if args == "" {
				args = "{}"
			}
			out.Output = append(out.Output, newFunctionCallItem(callID, tc.Function.Name, args))
		}
	}
	if len(out.Output) == 0 {
		// A response with no output items is legal but confusing; an empty
		// assistant message keeps clients from treating it as malformed.
		out.Output = append(out.Output, newMessageItem(""))
	}
	out.Usage = responsesUsageFrom(cc.Usage)
	return out
}

// ---- HTTP handling ----

// handleResponses serves POST /v1/responses.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body")
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		writeError(w, http.StatusBadRequest, `body must be JSON with a "model" field`)
		return
	}
	payload, err := buildChatPayload(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode upstream request")
		return
	}
	// Same alias discipline as the passthrough path: providers only know
	// their own model ids, so rewrite the converted payload's model field
	// to the registered upstream id (no-op for 1:1 names).
	if mapped := s.router.upstreamName(req.Model); mapped != "" && mapped != req.Model {
		if nb, err := rewriteModelField(payloadBytes, mapped); err == nil {
			payloadBytes = nb
		}
	}
	s.forwardResponses(w, r, &req, payloadBytes)
}

// forwardResponses routes the converted payload with the same
// routing/quota/key-injection loop as the passthrough path, then converts
// the upstream reply back to Responses format.
func (s *Server) forwardResponses(w http.ResponseWriter, r *http.Request, req *responsesRequest, payload []byte) {
	start := time.Now()
	const protocol = "openai"
	attempted := 0
	var lastStatus int
	var lastBody []byte
	var lastRoute candidate
	for {
		route, ok := s.router.pick(req.Model)
		if !ok {
			if attempted == 0 && !s.router.known(req.Model) {
				writeError(w, http.StatusNotFound, "no active key serves model "+req.Model)
			} else if attempted == 0 {
				// Every key is cooling down: 429 so clients retry, and
				// record it so throttling is visible.
				writeCooldown429(w, req.Model, s.router.retryAfter())
				s.recordUsage(usage.Record{
					Model:    req.Model,
					Protocol: protocol, Status: http.StatusTooManyRequests,
					LatencyMS: time.Since(start).Milliseconds(),
				})
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(lastStatus)
				_, _ = w.Write(lastBody)
				s.recordUsage(usage.Record{
					ProviderID: lastRoute.providerID, KeyID: lastRoute.keyID, Model: req.Model,
					Protocol: protocol, Status: lastStatus,
					LatencyMS: time.Since(start).Milliseconds(),
				})
			}
			return
		}
		if qt := s.quotas.overLimit(route); qt != nil {
			writeQuotaError(w, qt)
			s.recordUsage(usage.Record{
				ProviderID: route.providerID, KeyID: route.keyID, Model: req.Model,
				Protocol: protocol, Status: http.StatusTooManyRequests,
				LatencyMS: time.Since(start).Milliseconds(),
			})
			return
		}
		key, err := s.keys.get(route.providerID, route.keyID)
		if isNotFound(err) {
			writeError(w, http.StatusInternalServerError, "key missing from vault")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "fetch key from vault")
			return
		}

		upstreamURL := strings.TrimRight(route.baseURL, "/") + "/chat/completions"
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(payload))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "build upstream request")
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+key)
		if req.Stream {
			upReq.Header.Set("Accept", "text/event-stream")
		}

		resp, err := s.client.Do(upReq)
		if err != nil {
			s.router.fail(route)
			s.keys.invalidate(route.providerID, route.keyID)
			attempted++
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastStatus, lastBody = resp.StatusCode, respBody
			lastRoute = route
			s.failUpstream(route, resp, respBody)
			s.keys.invalidate(route.providerID, route.keyID)
			attempted++
			continue
		}
		s.router.success(route)
		s.deliverResponses(w, r.Context(), req, resp, route, start, protocol)
		return
	}
}

// deliverResponses converts a successful upstream reply (non-2xx statuses
// never reach here: they fail over or were already handled) and records
// usage exactly once.
func (s *Server) deliverResponses(w http.ResponseWriter, ctx context.Context, req *responsesRequest, resp *http.Response, route candidate, start time.Time, protocol string) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 4xx other than 429: relay the upstream error body; no failover.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	if req.Stream {
		tokensIn, tokensOut := streamResponses(w, ctx, req, resp.Body)
		s.recordUsage(usage.Record{
			ProviderID: route.providerID, KeyID: route.keyID, Model: req.Model,
			Protocol: protocol, Status: http.StatusOK,
			TokensIn: tokensIn, TokensOut: tokensOut,
			LatencyMS: time.Since(start).Milliseconds(),
		})
		return
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read upstream response")
		return
	}
	var cc chatCompletion
	if err := json.Unmarshal(raw, &cc); err != nil {
		writeError(w, http.StatusBadGateway, "decode upstream response")
		return
	}
	out := convertChatCompletion(req, &cc)
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
	tokensIn, tokensOut := int64(0), int64(0)
	if u := out.Usage; u != nil {
		tokensIn, tokensOut = u.InputTokens, u.OutputTokens
	}
	s.recordUsage(usage.Record{
		ProviderID: route.providerID, KeyID: route.keyID, Model: req.Model,
		Protocol: protocol, Status: resp.StatusCode,
		TokensIn: tokensIn, TokensOut: tokensOut,
		LatencyMS: time.Since(start).Milliseconds(),
	})
}

// ---- streaming conversion ----

// sseEvent is one emitted Responses SSE event.
type sseEvent struct {
	name string
	data any
}

// streamResponses converts an upstream chat-completions SSE stream into a
// Responses SSE stream and returns the usage seen in the final chunk.
func streamResponses(w http.ResponseWriter, ctx context.Context, req *responsesRequest, body io.Reader) (tokensIn, tokensOut int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	envelope := newResponsesEnvelope(req, req.Model)

	writeEvent := func(ev sseEvent) bool {
		data, err := json.Marshal(ev.data)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, data); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}

	writeEvent(sseEvent{"response.created", map[string]any{"type": "response.created", "response": envelope}})
	writeEvent(sseEvent{"response.in_progress", map[string]any{"type": "response.in_progress", "response": envelope}})

	st := &streamState{envelope: envelope}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return st.tokensIn, st.tokensOut
		default:
		}
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk chatStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		st.handleChunk(&chunk, writeEvent)
	}

	st.finish(writeEvent)
	return st.tokensIn, st.tokensOut
}

// streamState tracks in-flight output items while converting the stream.
type streamState struct {
	envelope *responsesResponse
	// message item currently being streamed (nil until first text delta).
	msgItem  *outputMessageItem
	msgIndex int
	msgText  strings.Builder
	// function calls being streamed, keyed by upstream tool_call index.
	calls     map[int]*streamCall
	nextIndex int // next output_index to hand out
	tokensIn  int64
	tokensOut int64
	finished  bool
}

type streamCall struct {
	item  *outputFunctionCallItem
	index int
	args  strings.Builder
}

func (st *streamState) handleChunk(chunk *chatStreamChunk, emit func(sseEvent) bool) {
	if u := chunk.Usage; u != nil {
		if ru := responsesUsageFrom(u); ru != nil {
			st.envelope.Usage = ru
			st.tokensIn, st.tokensOut = ru.InputTokens, ru.OutputTokens
		}
	}
	for _, choice := range chunk.Choices {
		d := choice.Delta
		if d.Content != nil && *d.Content != "" {
			st.openMessage(emit)
			frag := *d.Content
			st.msgText.WriteString(frag)
			emit(sseEvent{"response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": st.msgItem.ID,
				"output_index": st.msgIndex, "content_index": 0, "delta": frag,
			}})
		}
		for _, tc := range d.ToolCalls {
			if tc.Function == nil {
				continue
			}
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			call := st.calls[idx]
			if call == nil {
				call = st.openCall(idx, tc, emit)
			} else if tc.Function.Name != "" && call.item.Name == "" {
				call.item.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				call.args.WriteString(tc.Function.Arguments)
				emit(sseEvent{"response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "item_id": call.item.ID,
					"output_index": call.index, "delta": tc.Function.Arguments,
				}})
			}
		}
	}
}

// openMessage emits output_item.added for the assistant message.
func (st *streamState) openMessage(emit func(sseEvent) bool) {
	if st.msgItem != nil {
		return
	}
	st.msgItem = &outputMessageItem{
		Type: "message", ID: "msg_" + newID(), Status: "in_progress", Role: "assistant",
		Content: []outputTextPart{},
	}
	st.msgIndex = st.nextIndex
	st.nextIndex++
	emit(sseEvent{"response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": st.msgIndex, "item": st.msgItem,
	}})
}

// openCall emits output_item.added for a function call.
func (st *streamState) openCall(idx int, tc chatToolCall, emit func(sseEvent) bool) *streamCall {
	callID := tc.ID
	if callID == "" {
		callID = "call_" + newID()
	}
	item := &outputFunctionCallItem{
		Type: "function_call", ID: "fc_" + newID(), CallID: callID,
		Name: tc.Function.Name, Arguments: "", Status: "in_progress",
	}
	outIdx := st.nextIndex
	st.nextIndex++
	emit(sseEvent{"response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": outIdx, "item": item,
	}})
	if st.calls == nil {
		st.calls = map[int]*streamCall{}
	}
	call := &streamCall{item: item, index: outIdx}
	st.calls[idx] = call
	return call
}

// finish closes open items and emits response.completed.
func (st *streamState) finish(emit func(sseEvent) bool) {
	if st.finished {
		return
	}
	st.finished = true

	if st.msgItem != nil {
		text := st.msgText.String()
		emit(sseEvent{"response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": st.msgItem.ID,
			"output_index": st.msgIndex, "content_index": 0, "text": text,
		}})
		done := newMessageItem(text)
		done.ID = st.msgItem.ID
		emit(sseEvent{"response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": st.msgIndex, "item": done,
		}})
		st.envelope.Output = append(st.envelope.Output, done)
	}
	// Close calls in output order.
	var ordered []*streamCall
	for _, c := range st.calls {
		ordered = append(ordered, c)
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].index < ordered[i].index {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, c := range ordered {
		item := c.item
		item.Status = "completed"
		if item.Arguments == "" {
			item.Arguments = c.args.String()
		}
		if item.Arguments == "" {
			item.Arguments = "{}"
		}
		emit(sseEvent{"response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": item.ID,
			"output_index": c.index, "arguments": item.Arguments,
		}})
		emit(sseEvent{"response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": c.index, "item": item,
		}})
		st.envelope.Output = append(st.envelope.Output, item)
	}
	if len(st.envelope.Output) == 0 {
		empty := newMessageItem("")
		st.envelope.Output = append(st.envelope.Output, empty)
		emit(sseEvent{"response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0, "item": empty,
		}})
	}
	st.envelope.Status = "completed"
	emit(sseEvent{"response.completed", map[string]any{
		"type": "response.completed", "response": st.envelope,
	}})
}
