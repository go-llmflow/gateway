package gateway

import (
	"encoding/json"
	"fmt"
	"io"
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
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
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
	usage := `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":40}}`
	if r.URL.Path == "/v1/responses" {
		f.serveResponses(w, body, reply)
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
