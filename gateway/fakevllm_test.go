package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	status, gate := f.status, f.gate
	f.mu.Unlock()
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
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d\"}}],\"usage\":null}\n\n", i)
			fl.Flush()
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] == true {
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[],\"usage\":%s}\n\n", usage)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":%s}`, usage)
}
