package gateway

// Security audit 2026-11-05 (2), findings 1 and 2, the other ingresses and the
// non-streaming cut: a frame that carries usage before the final one is not
// the final usage, and a request whose answer the client never read is charged
// the worst case, not the prompt estimate.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamCutRecords runs one stream that the upstream cuts short of its final
// usage (the client leaves after the text), and returns the stored records.
func streamCutRecords(t *testing.T, path, body, stop string, frames func(w io.Writer)) (completion int64, estimated bool) {
	t.Helper()
	up := newFakeVLLM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			up.serve(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		frames(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	e := newEnv(t, testConfig(srv.URL))
	_, key := e.newKey("etabA")
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
	resp.Body.Close()
	e.waitRecords(1)
	time.Sleep(50 * time.Millisecond)
	r := e.st.Records()[0]
	return r.CompletionTokens, r.Estimated
}

// Messages: the usage of message_start is a floor, never the answer.
func TestSecAudit2MessagesStartUsageIsNotFinal(t *testing.T) {
	const deltas = 15
	for _, kind := range []string{"text_delta", "thinking_delta", "input_json_delta"} {
		got, est := streamCutRecords(t, "/v1/messages",
			`{"model":"llama","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "content_block_stop",
			func(w io.Writer) {
				fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
				for i := 0; i < deltas; i++ {
					fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":%q,\"x\":\"tok \"}}\n\n", kind)
				}
				fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			})
		if got < deltas || !est {
			t.Errorf("%s: a cut after %d deltas is billed %d completion tokens, estimated=%v", kind, deltas, got, est)
		}
	}
}

// Messages: a message_start that reports MORE than the deltas forwarded is
// kept (the max of the two), and a complete stream is not estimated.
func TestSecAudit2MessagesReportedFloorAndCompleteStream(t *testing.T) {
	got, est := streamCutRecords(t, "/v1/messages",
		`{"model":"llama","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "content_block_stop",
		func(w io.Writer) {
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":9}}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n")
			fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		})
	if got != 9 || !est {
		t.Errorf("floor: %d estimated=%v, want 9 true", got, est)
	}
	got, est = streamCutRecords(t, "/v1/messages",
		`{"model":"llama","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "message_stop",
		func(w io.Writer) {
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
			fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a\"}}\n\n")
			fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		})
	if got != 7 || est {
		t.Errorf("complete: %d estimated=%v, want 7 false", got, est)
	}
}

// Responses: an event before response.completed that carries a usage object
// (in_progress) is not the final usage.
func TestSecAudit2ResponsesEarlyUsageIsNotFinal(t *testing.T) {
	const deltas = 15
	got, est := streamCutRecords(t, "/v1/responses",
		`{"model":"llama","input":"hi","max_output_tokens":20,"stream":true}`, "response.output_text.done",
		func(w io.Writer) {
			fmt.Fprint(w, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n")
			for i := 0; i < deltas; i++ {
				fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"tok \"}\n\n")
			}
			fmt.Fprint(w, "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\"}\n\n")
		})
	if got < deltas || !est {
		t.Errorf("responses: a cut after %d deltas is billed %d, estimated=%v", deltas, got, est)
	}
}

// Chat with continuous_usage_stats: every chunk carries the running usage, so
// a cut is billed what vLLM reported so far, which is the truth.
func TestSecAudit2ChatContinuousUsageIsTheRunningCount(t *testing.T) {
	const deltas = 15
	got, _ := streamCutRecords(t, "/v1/chat/completions",
		`{"model":"llama","max_tokens":20,"stream":true,"stream_options":{"continuous_usage_stats":true},"messages":[{"role":"user","content":"hi"}]}`, "finish_reason",
		func(w io.Writer) {
			for i := 1; i <= deltas; i++ {
				fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok \"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", i, 10+i)
			}
			fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":15,\"total_tokens\":25}}\n\n")
		})
	if got < deltas {
		t.Errorf("chat continuous: %d completion tokens after %d deltas", got, deltas)
	}
}

// A non-streaming request whose client leaves before the answer arrives was
// computed by the upstream: it is charged the worst case under a hard budget
// (not the prompt estimate and zero), and max_tokens of output otherwise.
func TestSecAudit2NonStreamingCutIsChargedTheWorstCase(t *testing.T) {
	for _, budget := range []bool{true, false} {
		up := newFakeVLLM(t)
		got := make(chan struct{}, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				up.serve(w, r)
				return
			}
			_, _ = io.ReadAll(r.Body)
			got <- struct{}{}
			<-r.Context().Done() // the answer would come now; the client is gone
		}))
		e := newEnv(t, testConfig(srv.URL))
		key := e.createKey(`{"team":"etabA"}`)
		if budget {
			e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":10}`)
		}
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", e.gw.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"llama","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		done := make(chan struct{})
		go func() {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
			close(done)
		}()
		<-got
		cancel()
		<-done
		e.waitRecords(1)
		time.Sleep(50 * time.Millisecond)
		r := e.st.Records()[0]
		t.Logf("budget=%v record: completion=%d estimated=%v cost=%g status=%d", budget, r.CompletionTokens, r.Estimated, r.Cost, r.Status)
		if budget {
			// W(20) = 0.128 + 0.004*20 (hardbudgets_test.go).
			if r.Cost < 0.2079 || !r.Estimated {
				t.Errorf("budget: cost %g estimated=%v, want the worst case 0.208", r.Cost, r.Estimated)
			}
		} else if r.CompletionTokens < 20 || !r.Estimated {
			t.Errorf("no budget: %d completion tokens estimated=%v, want max_tokens (20)", r.CompletionTokens, r.Estimated)
		}
		srv.Close()
	}
}
