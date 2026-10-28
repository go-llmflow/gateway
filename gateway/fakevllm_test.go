package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeVLLM records what the gateway sends and answers like vLLM's OpenAI
// server: a JSON completion with usage, or an SSE stream whose last chunk
// carries usage only when stream_options.include_usage is true.
type fakeVLLM struct {
	t      testing.TB
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	status int           // answer with this status when non-zero
	gate   chan struct{} // when set, completions wait on it
	hits   int
	// reply, when set, is the answer's content: joined for a JSON answer,
	// one delta per element for a stream (default "hi"; t0, t1, t2).
	reply []string
	// classify answers /classify (a guard classifier) when set.
	classify func(inputs []string) string
}

func (f *fakeVLLM) setReply(parts ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = parts
}

type recorded struct {
	Header http.Header
	Body   map[string]any
	Path   string
}

func newFakeVLLM(t testing.TB) *fakeVLLM {
	f := &fakeVLLM{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVLLM) URL() string { return f.srv.URL }

func (f *fakeVLLM) last() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		f.t.Fatal("upstream received nothing")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeVLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *fakeVLLM) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path == "/v1/models" {
		// vLLM's ModelList (max_model_len read at health check, #87).
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"meta/llama","object":"model","max_model_len":128}]}`)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if r.URL.Path == "/v1/audio/transcriptions" {
		body = parseFakeForm(r.Header.Get("Content-Type"), raw)
	}
	f.mu.Lock()
	f.hits++
	f.reqs = append(f.reqs, recorded{Header: r.Header.Clone(), Body: body, Path: r.URL.Path})
	status, gate, reply, classify := f.status, f.gate, f.reply, f.classify
	f.mu.Unlock()
	if r.URL.Path == "/classify" && classify != nil && status == 0 {
		var in struct{ Input []string }
		_ = json.Unmarshal(raw, &in)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, classify(in.Input))
		return
	}
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		http.Error(w, `{"error":"boom"}`, status)
		return
	}
	// Like vLLM, never more output than the request's bound (#87).
	comp := outputTokens(body)
	usage := fmt.Sprintf(`{"prompt_tokens":100,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":40}}`, comp, 100+comp)
	if r.URL.Path == "/v1/responses" {
		f.serveResponses(w, body, reply)
		return
	}
	if r.URL.Path == "/v1/messages" {
		f.serveMessages(w, body, reply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/audio/transcriptions":
		text := "hello world"
		if reply != nil {
			text = strings.Join(reply, "")
		}
		tb, _ := json.Marshal(text)
		if body["stream"] == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, d := range strings.SplitAfter(text, " ") {
				db, _ := json.Marshal(d)
				fmt.Fprintf(w, "data: {\"object\":\"transcription.chunk\",\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", db)
			}
			if body["stream_include_usage"] == "true" {
				fmt.Fprint(w, "data: {\"object\":\"transcription.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":5,\"total_tokens\":55}}\n\n")
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, `{"text":%s,"segments":[{"text":%s}],"usage":{"type":"duration","seconds":90}}`, tb, tb)
		return
	case "/v1/rerank", "/v2/rerank":
		docs, _ := body["documents"].([]any)
		var results []string
		for i, d := range docs {
			s, _ := json.Marshal(d)
			results = append(results, fmt.Sprintf(`{"index":%d,"relevance_score":%g,"document":{"text":%s,"extra":"x"}}`, i, 1/float64(i+1), s))
		}
		fmt.Fprintf(w, `{"id":"rerank-1","model":"meta/llama","usage":{"prompt_tokens":30,"total_tokens":30},"results":[%s]}`, strings.Join(results, ","))
		return
	case "/tokenize":
		fmt.Fprint(w, `{"count":3,"max_model_len":4096,"tokens":[1,2,3]}`)
		return
	case "/detokenize":
		fmt.Fprint(w, `{"prompt":"abc"}`)
		return
	case "/v1/messages/count_tokens":
		// vLLM v0.32.0 AnthropicCountTokensResponse.
		fmt.Fprint(w, `{"input_tokens":17,"context_management":{"original_input_tokens":17}}`)
		return
	}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		deltas := reply
		if deltas == nil {
			deltas = []string{"t0", "t1", "t2"}
		}
		for _, d := range deltas {
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s}}],\"usage\":null}\n\n", b)
			fl.Flush()
		}
		if reply != nil {
			fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n")
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] == true {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[],\"usage\":%s}\n\n", usage)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	content := "hi"
	if reply != nil {
		content = strings.Join(reply, "")
	}
	cb, _ := json.Marshal(content)
	fmt.Fprintf(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":%s}}],"usage":%s}`, cb, usage)
}

// outputTokens is the fake's output: 20 tokens, or fewer when the request's
// bound (max_completion_tokens, max_tokens, max_output_tokens) is smaller.
func outputTokens(body map[string]any) int {
	out := 20
	for _, k := range []string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
		if v, ok := body[k].(float64); ok {
			out = min(out, int(v))
			break
		}
	}
	return out
}

// parseFakeForm reads a multipart form into its fields as strings, with
// file_name, file_type and file_bytes for the file.
func parseFakeForm(ctype string, raw []byte) map[string]any {
	out := map[string]any{}
	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return out
	}
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return out
		}
		v, _ := io.ReadAll(p)
		if p.FormName() == "file" {
			out["file_name"], out["file_type"], out["file_bytes"] = p.FileName(), p.Header.Get("Content-Type"), len(v)
			continue
		}
		out[p.FormName()] = string(v)
	}
}

// serveMessages answers like vLLM's Anthropic Messages API: input_tokens
// excludes the cached prompt (60 + 40 cached = 100).
func (f *fakeVLLM) serveMessages(w http.ResponseWriter, body map[string]any, reply []string) {
	if reply == nil {
		reply = []string{"hi"}
	}
	text, _ := json.Marshal(strings.Join(reply, ""))
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":60,\"cache_read_input_tokens\":40,\"output_tokens\":0}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		for _, d := range reply {
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", b)
			fl.Flush()
		}
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":60,\"cache_read_input_tokens\":40,\"output_tokens\":20}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"meta/llama","stop_reason":"end_turn","stop_sequence":null,`+
		`"content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":%s}],"usage":{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":20}}`, text)
}

// serveResponses answers like vLLM's Responses API: a response object with
// output items and Responses usage, or its event stream ending with
// response.completed.
func (f *fakeVLLM) serveResponses(w http.ResponseWriter, body map[string]any, reply []string) {
	if reply == nil {
		reply = []string{"hi"}
	}
	usage := `{"input_tokens":100,"input_tokens_details":{"cached_tokens":40},"output_tokens":20,"total_tokens":120}`
	text, _ := json.Marshal(strings.Join(reply, ""))
	resp := fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1,"model":"meta/llama","status":"completed","output":[`+
		`{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"thinking"}]},`+
		`{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":%s,"annotations":[]}]}],"usage":%s}`, text, usage)
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"usage\":null}}\n\n")
		for _, d := range reply {
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", b)
			fl.Flush()
		}
		fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", resp)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, resp)
}
