package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// The policy response limit holds for a stream: it ends between events with a
// final error event, what was delivered stays under the bound, the settlement
// is an estimate, and the refusal is audited.
func TestStreamResponseLimit(t *testing.T) {
	const deltas, limit = 100, 1024
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < deltas; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%03d\"}}]}\n\n", i); err != nil {
				return
			}
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100,\"total_tokens\":110}}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()
	cfg := testConfig(up.URL)
	cfg.ModelGroups[0].MaxModelLen = 4096
	cfg.Policies = []config.Policy{{Name: "short", MaxResponseBytes: limit}}
	e := newEnv(t, cfg)
	short := e.createKey(`{"team":"etabA","policy":"short"}`)
	plain := e.createKey(`{"team":"etabB"}`)
	body := `{"model":"llama","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// A key without the policy gets the whole stream.
	resp := e.do("POST", "/v1/chat/completions", plain, body)
	if full := readAll(t, resp); !strings.Contains(full, "[DONE]") || strings.Contains(full, "response_limit") {
		t.Fatalf("unlimited stream cut: %d bytes", len(full))
	}

	resp = e.do("POST", "/v1/chat/completions", short, body)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(got)
	i := strings.Index(out, "data: {\"error\"")
	if resp.StatusCode != 200 || i < 0 || !strings.Contains(out[i:], `"code":"response_limit"`) || strings.Contains(out, "[DONE]") {
		t.Fatalf("no clean stop: %d %q", resp.StatusCode, out)
	}
	if i > limit || !strings.HasSuffix(out[:i], "\n\n") {
		t.Fatalf("delivered %d bytes before the stop, limit %d, ends %q", i, limit, out[max(0, i-10):i])
	}
	waitIdle(t, e)
	e.srv.Close(context.Background())
	recs := e.st.Records()
	var rec *struct{ n, tok int64 }
	for _, r := range recs {
		if r.Estimated && r.Stream {
			rec = &struct{ n, tok int64 }{r.BytesOut, r.CompletionTokens}
		}
	}
	if rec == nil || rec.n > limit+300 || rec.tok <= 0 || rec.tok >= deltas {
		t.Fatalf("settlement %+v of %+v", rec, recs)
	}
	ents, _ := e.st.AuditRange(context.Background(), 1, 1000)
	found := false
	for _, a := range ents {
		found = found || (a.Action == "response.limit" && strings.Contains(a.After, `"limit":1024`))
	}
	if !found {
		t.Fatal("the refusal was not audited")
	}
}

// CopySSELimit never writes past its bound and stops between events.
func TestCopySSELimitBoundary(t *testing.T) {
	ev := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"
	src := strings.Repeat(ev, 5)
	for limit := int64(1); limit < int64(len(src)); limit += 7 {
		var sb strings.Builder
		res, err := openai.CopySSELimit(openai.Chat, &sb, nil, strings.NewReader(src), false, limit)
		if err != openai.ErrStreamLimit || int64(sb.Len()) > limit || res.Bytes != int64(sb.Len()) || sb.Len()%len(ev) != 0 || res.Deltas != int64(sb.Len()/len(ev)) {
			t.Fatalf("limit %d: err %v wrote %d (%d) deltas %d", limit, err, sb.Len(), res.Bytes, res.Deltas)
		}
	}
	var sb strings.Builder
	if _, err := openai.CopySSELimit(openai.Chat, &sb, nil, strings.NewReader(src), false, int64(len(src))); err != nil || sb.Len() != len(src) {
		t.Fatalf("exact bound: %v %d", err, sb.Len())
	}
}
