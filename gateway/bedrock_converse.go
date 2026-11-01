package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// Amazon Bedrock Converse and ConverseStream (#89, part 3): the models of the
// vendors other than Anthropic (Mistral, Llama, Nova …), reached by clients in
// OpenAI format at /v1/chat/completions. Sources opened 2026-11-01
// (docs.aws.amazon.com/bedrock/latest/APIReference/):
//
//   - API_runtime_Converse.html: POST /model/{modelId}/converse; the body has
//     messages, system, inferenceConfig {maxTokens, stopSequences, temperature,
//     topP}, toolConfig, guardrailConfig, additionalModelRequestFields,
//     additionalModelResponseFieldPaths, outputConfig, performanceConfig,
//     promptVariables, requestMetadata and serviceTier; "If you use a prompt
//     from Prompt management, you can't include … additionalModelRequestFields,
//     inferenceConfig, system, or toolConfig" (the prompt's size is unknown);
//     the answer has output.message {role, content[]}, stopReason (end_turn |
//     tool_use | max_tokens | stop_sequence | guardrail_intervened |
//     content_filtered | malformed_model_output | malformed_tool_use |
//     model_context_window_exceeded) and usage {inputTokens, outputTokens,
//     totalTokens, cacheReadInputTokens, cacheWriteInputTokens,
//     cacheDetails[{inputTokens, ttl}]}; a guardrail's cost is in
//     trace.guardrail….invocationMetrics.usage (policy units);
//   - API_runtime_ConverseStream.html: POST /model/{modelId}/converse-stream;
//     the answer is an event stream whose members are messageStart {role},
//     contentBlockStart, contentBlockDelta {contentBlockIndex, delta},
//     contentBlockStop, messageStop {stopReason}, metadata {usage, metrics, …}
//     and the exceptions internalServerException (500), modelStreamErrorException
//     (424), serviceUnavailableException (503), throttlingException (429),
//     validationException (400);
//   - API_runtime_ContentBlock.html (a union: text, image, document, video,
//     audio, toolUse, toolResult, guardContent, reasoningContent, cachePoint …),
//     API_runtime_ImageBlock.html (format png | jpeg | gif | webp, source
//     bytes) and API_runtime_InferenceConfiguration.html (maxTokens: minimum 1).
//
// The usage of a Converse answer is not Anthropic's: inputTokens EXCLUDES the
// tokens read from and written to a cache ("total input tokens = inputTokens +
// cacheReadInputTokens + cacheWriteInputTokens", prompt-caching.html), and the
// cache writes are split by lifetime in cacheDetails.
//
// What is mapped. OpenAI messages become Converse messages (system and
// developer messages the system; consecutive messages of one role merge, since
// Converse alternates roles), text parts become text blocks and data: URLs of
// png, jpeg, gif and webp images image blocks. Everything else is REFUSED, not
// dropped, because a dropped field changes what the model reads or costs:
//
//   - tools, tool_calls and tool messages (the mapping of tool use is not part
//     of this release; with a context window and maxTokens a tool exchange would
//     be bounded like any other, so this is a limit of the release, not of the
//     bound);
//   - additionalModelRequestFields: it carries vendor-specific parameters
//     (Anthropic betas and thinking budgets, top_k, Nova and Mistral switches)
//     that can widen the context or add sampling steps, and no vendor allow-list
//     of fields verified to be bounded exists here; the OpenAI route has no way
//     to send it either, so it is refused whatever;
//   - the options of part 2 that change a request's cost or hide its size:
//     guardrailConfig, serviceTier, performanceConfig, promptVariables,
//     requestMetadata, outputConfig, and the X-Amzn-Bedrock-* headers;
//   - any other OpenAI field with no Converse equivalent (n above 1, seed,
//     response_format, logprobs, penalties, audio …).

var (
	errBedrockInvokeEndpoint   = errors.New("this model is served by Amazon Bedrock through InvokeModel and is reachable at /v1/messages only (set api = \"converse\" for a model reached at /v1/chat/completions)")
	errBedrockConverseEndpoint = errors.New("this model is served by Amazon Bedrock through Converse and is reachable at /v1/chat/completions only")
)

func (b *bedrockRuntime) converse() bool { return b.cfg.API == config.APIConverse }

// converseAllowed are the OpenAI chat fields the mapping reads.
var converseAllowed = map[string]bool{
	"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "stop": true, "stream": true, "stream_options": true, "n": true, "user": true,
}

// converseOnly are the Converse request fields no client can send either.
var converseOnly = []string{"outputConfig", "additionalModelResponseFieldPaths", "toolConfig", "inferenceConfig", "system"}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// refuseBedrockHeaders refuses the X-Amzn-Bedrock-* headers of a client.
func refuseBedrockHeaders(h http.Header) error {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amzn-bedrock-") {
			return errors.New("X-Amzn-Bedrock-* headers (guardrails, service tier, performance config, trace, request metadata) are not forwarded: they change what a request costs")
		}
	}
	return nil
}

// converseIngress runs on the client's chat request before admission: it
// refuses what cannot be bounded or mapped (see above) and returns the body.
func converseIngress(h http.Header, body []byte) ([]byte, error) {
	if err := refuseBedrockHeaders(h); err != nil {
		return nil, err
	}
	m, err := bedrockObject(body)
	if err != nil {
		return nil, err
	}
	if err := checkBedrockBody(m); err != nil {
		return nil, err
	}
	for _, f := range converseOnly {
		if raw, ok := m[f]; ok && !isNull(raw) {
			return nil, fmt.Errorf("%q is a Converse field, not an OpenAI one, and is not forwarded: it can change what a request costs or reads", f)
		}
	}
	for k, raw := range m {
		if !converseAllowed[k] && !isNull(raw) {
			return nil, fmt.Errorf("the field %q cannot be mapped to Bedrock Converse (supported: model, messages, max_tokens, max_completion_tokens, temperature, top_p, stop, stream, stream_options; tools, n above 1, response_format and the rest are refused rather than dropped)", clipText(printable(k), 40))
		}
	}
	if raw, ok := m["n"]; ok && !isNull(raw) {
		var n int
		if json.Unmarshal(raw, &n) != nil || n != 1 {
			return nil, errors.New(`"n" must be 1 on a Bedrock Converse model`)
		}
	}
	// The mapping is checked now, before anything is held or spent.
	if _, err := converseRequest(body); err != nil {
		return nil, err
	}
	return body, nil
}

type openAIPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type openAIMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
	FunctionCall json.RawMessage `json:"function_call"`
	Name         string          `json:"name"`
}

var imageFormats = map[string]string{"png": "png", "jpeg": "jpeg", "jpg": "jpeg", "gif": "gif", "webp": "webp"}

// maxConverseParts bounds a conversation: more is refused.
const maxConverseParts = 4096

// contentBlocks maps the content of an OpenAI message.
func contentBlocks(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []map[string]any{{"text": s}}, nil
	}
	var parts []openAIPart
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) > maxConverseParts {
		return nil, errors.New("a message's content must be a string or an array of at most 4096 text and image parts")
	}
	var out []map[string]any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, map[string]any{"text": p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				return nil, errors.New("an image_url part needs a url")
			}
			blk, err := imageBlock(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			out = append(out, blk)
		default:
			return nil, fmt.Errorf("a content part of type %q cannot be mapped to Bedrock Converse (text and data: URL images are supported)", clipText(printable(p.Type), 40))
		}
	}
	return out, nil
}

// imageBlock maps a data: URL. A remote URL is refused: the gateway fetches
// nothing for a client.
func imageBlock(url string) (map[string]any, error) {
	rest, ok := strings.CutPrefix(url, "data:image/")
	if !ok {
		return nil, errors.New("an image must be a data: URL (data:image/png;base64,…); the gateway fetches no remote image")
	}
	format, b64, ok := strings.Cut(rest, ";base64,")
	f, known := imageFormats[format]
	if !ok || !known {
		return nil, errors.New("an image must be png, jpeg, gif or webp, base64 encoded")
	}
	if _, err := base64.StdEncoding.DecodeString(b64); err != nil || b64 == "" {
		return nil, errors.New("an image is not valid base64")
	}
	return map[string]any{"image": map[string]any{"format": f, "source": map[string]any{"bytes": b64}}}, nil
}

// converseRequest maps an OpenAI chat body to a Converse body.
func converseRequest(body []byte) ([]byte, error) {
	m, err := bedrockObject(body)
	if err != nil {
		return nil, err
	}
	if err := checkBedrockBody(m); err != nil {
		return nil, err
	}
	for k, raw := range m {
		if !converseAllowed[k] && !isNull(raw) {
			return nil, fmt.Errorf("the field %q cannot be mapped to Bedrock Converse", clipText(printable(k), 40))
		}
	}
	var msgs []openAIMessage
	if raw, ok := m["messages"]; ok {
		if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) > maxConverseParts {
			return nil, errors.New(`"messages" must be an array of at most 4096 messages`)
		}
	}
	var system []map[string]any
	var conv []map[string]any // role, content
	for _, msg := range msgs {
		if len(msg.ToolCalls) > 0 && !isNull(msg.ToolCalls) || len(msg.FunctionCall) > 0 && !isNull(msg.FunctionCall) || msg.ToolCallID != "" {
			return nil, errors.New("tool use cannot be mapped to Bedrock Converse in this release")
		}
		blocks, err := contentBlocks(msg.Content)
		if err != nil {
			return nil, err
		}
		switch msg.Role {
		case "system", "developer":
			for _, b := range blocks {
				if t, ok := b["text"]; ok {
					system = append(system, map[string]any{"text": t})
				} else {
					return nil, errors.New("a system message holds text only")
				}
			}
		case "user", "assistant":
			if len(blocks) == 0 {
				return nil, errors.New("a message has no content")
			}
			if n := len(conv); n > 0 && conv[n-1]["role"] == msg.Role {
				conv[n-1]["content"] = append(conv[n-1]["content"].([]map[string]any), blocks...)
			} else {
				conv = append(conv, map[string]any{"role": msg.Role, "content": blocks})
			}
		default:
			return nil, fmt.Errorf("the message role %q cannot be mapped to Bedrock Converse", clipText(printable(msg.Role), 40))
		}
	}
	if len(conv) == 0 || conv[0]["role"] != "user" {
		return nil, errors.New("a conversation must start with a user message")
	}
	out := map[string]any{"messages": conv}
	if len(system) > 0 {
		out["system"] = system
	}
	inf := map[string]any{}
	// max_completion_tokens wins over max_tokens (as for vLLM).
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := m[k]; ok && !isNull(raw) {
			var n int64
			if json.Unmarshal(raw, &n) != nil || n < 1 {
				return nil, fmt.Errorf("%q must be a positive integer", k)
			}
			inf["maxTokens"] = n
			break
		}
	}
	for k, to := range map[string]string{"temperature": "temperature", "top_p": "topP"} {
		if raw, ok := m[k]; ok && !isNull(raw) {
			var f float64
			if json.Unmarshal(raw, &f) != nil || f < 0 || f > 2 {
				return nil, fmt.Errorf("%q must be a number between 0 and 2", k)
			}
			inf[to] = f
		}
	}
	if raw, ok := m["stop"]; ok && !isNull(raw) {
		var one string
		var many []string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			many = []string{one}
		case json.Unmarshal(raw, &many) == nil && len(many) <= 4:
		default:
			return nil, errors.New(`"stop" must be a string or an array of at most 4 strings`)
		}
		for _, s := range many {
			if s == "" {
				return nil, errors.New(`"stop" holds an empty string`)
			}
		}
		inf["stopSequences"] = many
	}
	if len(inf) > 0 {
		out["inferenceConfig"] = inf
	}
	return json.Marshal(out)
}

// converseUsage is the usage of a Converse answer or of the metadata event.
type converseUsage struct {
	InputTokens  *int64 `json:"inputTokens"`
	OutputTokens *int64 `json:"outputTokens"`
	TotalTokens  int64  `json:"totalTokens"`
	CacheRead    int64  `json:"cacheReadInputTokens"`
	CacheWrite   int64  `json:"cacheWriteInputTokens"`
	CacheDetails []struct {
		InputTokens int64  `json:"inputTokens"`
		TTL         string `json:"ttl"`
	} `json:"cacheDetails"`
}

// usage converts to the gateway's Usage. The prompt is every input token,
// cached or not (inputTokens excludes both cache counts); the cache writes are
// split by lifetime (all 5-minute when the split is absent or does not add
// up). ok is false for a missing or negative count.
func (u converseUsage) usage() (openai.Usage, bool) {
	if u.InputTokens == nil || u.OutputTokens == nil || *u.InputTokens < 0 || *u.OutputTokens < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
		return openai.Usage{}, false
	}
	prompt := *u.InputTokens + u.CacheRead + u.CacheWrite
	out := openai.Usage{PromptTokens: prompt, CompletionTokens: *u.OutputTokens, TotalTokens: prompt + *u.OutputTokens,
		PromptTokensDetails: &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{u.CacheRead}}
	out.CacheWrite5m = u.CacheWrite
	var w5, w1h int64
	for _, d := range u.CacheDetails {
		if d.InputTokens < 0 {
			w5, w1h = -1, -1
			break
		}
		if d.TTL == "1h" {
			w1h += d.InputTokens
		} else {
			w5 += d.InputTokens
		}
	}
	if len(u.CacheDetails) > 0 && w5 >= 0 && w5+w1h == u.CacheWrite {
		out.CacheWrite5m, out.CacheWrite1h = w5, w1h
	}
	return out, out.Valid()
}

// finishReason maps Converse's stopReason to OpenAI's finish_reason.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "guardrail_intervened", "content_filtered":
		return "content_filter"
	case "tool_use":
		return "tool_calls"
	}
	return "stop"
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func chatUsageOf(u openai.Usage) chatUsage {
	c := chatUsage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	c.PromptTokensDetails.CachedTokens = u.Cached()
	return c
}

// translateConverse maps a Converse answer to an OpenAI chat completion and
// returns its usage, read from the Converse fields (an OpenAI usage object has
// no room for the cache writes, which are priced apart). ok is false when the
// answer cannot be read or carries no usage: the request is then charged W.
func translateConverse(data []byte, model, id string, now time.Time) (body []byte, usage openai.Usage, ok bool) {
	var in struct {
		Output struct {
			Message struct {
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string         `json:"stopReason"`
		Usage      *converseUsage `json:"usage"`
	}
	if json.Unmarshal(data, &in) != nil || in.Usage == nil {
		return nil, openai.Usage{}, false
	}
	u, valid := in.Usage.usage()
	if !valid {
		return nil, openai.Usage{}, false
	}
	var text strings.Builder
	for _, raw := range in.Output.Message.Content {
		var blk struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal(raw, &blk) == nil && blk.Text != nil {
			text.WriteString(*blk.Text)
		}
	}
	out, err := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion", "created": now.Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": finishReason(in.StopReason)}},
		"usage": chatUsageOf(u),
	})
	if err != nil {
		return nil, openai.Usage{}, false
	}
	return out, u, true
}

// ---- the stream ----

// converseSSE reads a ConverseStream answer (the shared eventReader: bounded
// frames, both CRCs checked) and presents it as the OpenAI chat chunk stream.
// It is COMPLETE when messageStart, messageStop and the metadata event with
// the usage arrived and the stream ended cleanly; anything else is charged W.
type converseSSE struct {
	*eventReader
	model, id   string
	created     int64
	start, stop bool
	haveUsage   bool
	usage       openai.Usage
}

func newConverseSSE(r io.Reader, model, id string, now time.Time) *converseSSE {
	c := &converseSSE{eventReader: newEventReader(r), model: model, id: "chatcmpl-" + id, created: now.Unix()}
	c.event = c.handle
	c.errEvent = func(why string) string {
		msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": why, "type": "api_error"}})
		// No [DONE]: an OpenAI client reads a stream that ends without it as a
		// failed one, whatever a filter between does with the error object.
		return "data: " + string(msg) + "\n\n"
	}
	c.finished = func() bool { return c.start && c.stop && c.haveUsage }
	c.end = func() { c.out.WriteString("data: [DONE]\n\n") }
	c.endText = "the stream from the provider ended before the usage"
	return c
}

// Complete reports whether the final usage was delivered.
func (c *converseSSE) Complete() bool {
	return c.done && !c.failed && c.start && c.stop && c.haveUsage
}

// Usage returns the usage of the metadata event.
func (c *converseSSE) Usage() (openai.Usage, bool) { return c.usage, c.haveUsage }

func (c *converseSSE) chunk(delta map[string]any, finish any, usage any) {
	choices := []any{}
	if usage == nil {
		choices = append(choices, map[string]any{"index": 0, "delta": delta, "finish_reason": finish})
	}
	m := map[string]any{"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model, "choices": choices}
	if usage != nil {
		m["usage"] = usage
	}
	b, _ := json.Marshal(m)
	c.out.WriteString("data: ")
	c.out.Write(b)
	c.out.WriteString("\n\n")
}

func (c *converseSSE) handle(typ string, payload []byte) {
	switch typ {
	case "messageStart":
		c.start = true
		c.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)
	case "contentBlockDelta":
		var ev struct {
			Delta struct {
				Text *string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			c.fail("an event of the provider's stream could not be read")
			return
		}
		if ev.Delta.Text != nil && *ev.Delta.Text != "" && c.start {
			c.chunk(map[string]any{"content": *ev.Delta.Text}, nil, nil)
		}
	case "messageStop":
		var ev struct {
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			c.fail("an event of the provider's stream could not be read")
			return
		}
		if c.start {
			c.stop = true
			c.chunk(map[string]any{}, finishReason(ev.StopReason), nil)
		}
	case "metadata":
		var ev struct {
			Usage *converseUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &ev) != nil || ev.Usage == nil {
			c.fail("an event of the provider's stream could not be read")
			return
		}
		u, ok := ev.Usage.usage()
		if !ok {
			c.fail("the usage of the provider's stream could not be read")
			return
		}
		if c.start && c.stop {
			c.usage, c.haveUsage = u, true
			c.chunk(nil, nil, chatUsageOf(u))
		}
	}
}
