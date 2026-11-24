package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// openAIUsage mirrors the OpenAI usage block (also present in the final
// SSE chunk of streamed chats).
type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// anthropicUsage mirrors the Anthropic usage block (top level in message
// responses, nested in message/message_start events when streaming).
type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// usageEnvelope probes a payload for a "usage" block. Both protocols name
// the key "usage" with different fields, so it is decoded as raw JSON and
// tried against each shape. Anthropic also nests usage under "message"
// (message_start / non-streaming message bodies).
type usageEnvelope struct {
	Usage   json.RawMessage `json:"usage"`
	Message *struct {
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
}

// ExtractTokens pulls cumulative (tokens_in, tokens_out) out of a captured
// upstream response. It handles both plain JSON bodies and SSE streams by
// scanning every JSON payload it can find (each SSE line after "data: "),
// keeping the last non-zero values so the final chunk wins.
func ExtractTokens(body []byte) (in, out int64) {
	for _, payload := range jsonPayloads(body) {
		var env usageEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			continue
		}
		for _, raw := range []json.RawMessage{env.Usage, nestedUsage(env)} {
			if len(raw) == 0 {
				continue
			}
			var o openAIUsage
			if err := json.Unmarshal(raw, &o); err == nil && o.PromptTokens > 0 {
				in, out = o.PromptTokens, o.CompletionTokens
				continue
			}
			var a anthropicUsage
			if err := json.Unmarshal(raw, &a); err == nil && a.InputTokens > 0 {
				in, out = a.InputTokens, a.OutputTokens
			}
		}
	}
	return in, out
}

func nestedUsage(env usageEnvelope) json.RawMessage {
	if env.Message != nil {
		return env.Message.Usage
	}
	return nil
}

// jsonPayloads yields candidate JSON documents: the whole body if it looks
// like one object, otherwise each SSE "data:" line's payload.
func jsonPayloads(body []byte) [][]byte {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("{")) {
		return [][]byte{trimmed}
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		out = append(out, []byte(payload))
	}
	return out
}
