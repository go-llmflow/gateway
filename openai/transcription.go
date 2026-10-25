package openai

import (
	"encoding/json"
	"fmt"
)

// Transcriptions, as vLLM v0.32.0 serves them
// (vllm/entrypoints/speech_to_text/transcription/protocol.py, opened
// 2026-10-25): a multipart form with file, model, language, prompt,
// response_format (json, text, verbose_json, diarized_json), temperature,
// stream, stream_include_usage, timestamp_granularities[] and
// max_completion_tokens. A JSON answer carries usage {type: "duration",
// seconds}; a stream is chat-like chunks (choices[].delta) with token usage
// (UsageInfo) when stream_include_usage is set.

// TranscriptionFields are the form fields passed to vLLM; any other field is
// dropped (priority, request_id, a client cache_salt…).
var TranscriptionFields = []string{"model", "language", "prompt", "response_format", "temperature", "stream",
	"stream_include_usage", "timestamp_granularities[]", "max_completion_tokens"}

var transcriptionFormats = map[string]bool{"json": true, "text": true, "verbose_json": true, "diarized_json": true}

// rewriteTranscription rewrites the JSON view of a transcription form (the
// gateway builds it from the form fields; every value is a string, as in
// the form, except timestamp_granularities[] which may be a list).
func rewriteTranscription(body []byte, resolve func(string) (string, error)) ([]byte, Request, error) {
	var req Request
	m, err := decodeObject(body)
	if err != nil {
		return nil, req, err
	}
	if err := json.Unmarshal(m["model"], &req.Model); err != nil || req.Model == "" {
		return nil, req, ErrNoModel
	}
	upstream, err := resolve(req.Model)
	if err != nil {
		return nil, req, err
	}
	out := map[string]json.RawMessage{}
	for _, f := range TranscriptionFields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	for k := range m {
		if _, ok := out[k]; !ok {
			req.Stripped = append(req.Stripped, k)
		}
	}
	out["model"], _ = json.Marshal(upstream)
	str := func(k string) (string, bool) {
		var s string
		raw, ok := out[k]
		if !ok {
			return "", false
		}
		if json.Unmarshal(raw, &s) != nil {
			return "", true
		}
		return s, true
	}
	if f, ok := str("response_format"); ok {
		if !transcriptionFormats[f] {
			return nil, req, fmt.Errorf(`"response_format" must be json, text, verbose_json or diarized_json`)
		}
		if f == "text" {
			// Plain text carries no usage: vLLM is asked for JSON, and the
			// gateway answers its text.
			req.TextOut = true
			out["response_format"] = json.RawMessage(`"json"`)
		}
	}
	if s, ok := str("stream"); ok {
		switch s {
		case "true", "True", "1":
			req.Stream = true
		case "false", "False", "0", "":
		default:
			return nil, req, fmt.Errorf(`"stream" must be true or false`)
		}
	}
	if s, ok := str("max_completion_tokens"); ok && s != "" {
		var n int64
		if _, err := fmt.Sscan(s, &n); err != nil || n < 0 {
			return nil, req, fmt.Errorf(`"max_completion_tokens" must be a non-negative integer`)
		}
		req.MaxTokens = n
	}
	if req.Stream {
		if req.TextOut {
			return nil, req, fmt.Errorf(`a streamed transcription is not plain text: use response_format json`)
		}
		if s, ok := str("stream_include_usage"); ok && (s == "true" || s == "True" || s == "1") {
			req.ClientWantsUsage = true
		}
		out["stream_include_usage"] = json.RawMessage(`"true"`)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, req, fmt.Errorf("re-encoding the request: %w", err)
	}
	return b, req, nil
}

// parseTranscriptionUsage reads the usage of a JSON transcription:
// {type: "duration", seconds} (or token counts, should a model report
// them), or verbose_json's top-level duration.
func parseTranscriptionUsage(body []byte) (Usage, bool) {
	var env struct {
		Duration *float64 `json:"duration"`
		Usage    *struct {
			Type             string  `json:"type"`
			Seconds          float64 `json:"seconds"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			InputTokens      int64   `json:"input_tokens"`
			OutputTokens     int64   `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return Usage{}, false
	}
	var u Usage
	switch {
	case env.Usage != nil && env.Usage.Type == "duration":
		u.AudioSeconds = env.Usage.Seconds
	case env.Usage != nil:
		u.PromptTokens = env.Usage.PromptTokens + env.Usage.InputTokens
		u.CompletionTokens = env.Usage.CompletionTokens + env.Usage.OutputTokens
	case env.Duration != nil:
		u.AudioSeconds = *env.Duration
	default:
		return Usage{}, false
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u, u.Valid()
}

// TranscriptText returns the text of a JSON transcription.
func TranscriptText(body []byte) (string, bool) {
	var r struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(body, &r) != nil || r.Text == nil {
		return "", false
	}
	return *r.Text, true
}
