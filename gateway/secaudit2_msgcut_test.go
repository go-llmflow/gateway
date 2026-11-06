package gateway

// Security audit 2026-11-05 (2), untracked proof. A Messages stream cut by the
// client after the text and before message_delta is billed the output_tokens
// of message_start (0 or 1), not the text_delta events forwarded: the
// messagesUsageTracker (openai/messages.go:140-155) reports HaveUsage from
// message_start, so proxy.go:442 never estimates. The chat-completions
// control is estimated from the forwarded deltas.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSecAudit2MessagesStreamCutBillsNoOutput(t *testing.T) {
	up := newFakeVLLM(t)
	const deltas = 15
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/chat/completions" {
			up.serve(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		if r.URL.Path == "/v1/messages" {
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
			fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			for i := 0; i < deltas; i++ {
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tok \"}}\n\n")
			}
			fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		} else {
			for i := 0; i < deltas; i++ {
				fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok \"}}],\"usage\":null}\n\n")
			}
			fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n")
		}
		fl.Flush()
		<-r.Context().Done() // the final usage would come now; the client is gone
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t, testConfig(srv.URL))
	_, key := e.newKey("etabA")

	cut := func(path, body, stop string) {
		req, _ := http.NewRequest("POST", e.gw.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), stop) {
				break
			}
		}
		resp.Body.Close() // all the text received; leave before the usage
	}
	cut("/v1/messages", `{"model":"llama","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "content_block_stop")
	cut("/v1/chat/completions", `{"model":"llama","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "finish_reason")
	e.waitRecords(2)
	time.Sleep(50 * time.Millisecond)
	for _, r := range e.st.Records() {
		b, _ := json.Marshal(map[string]any{"completion": r.CompletionTokens, "estimated": r.Estimated, "cost": r.Cost})
		t.Logf("record %s", b)
		if r.CompletionTokens < deltas/2 && !r.Estimated {
			t.Errorf("EXPLOIT: a stream cut after %d text deltas is billed %d completion tokens, not estimated (cost %g)", deltas, r.CompletionTokens, r.Cost)
		}
	}
}
