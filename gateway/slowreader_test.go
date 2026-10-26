package gateway

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Security audit 2026-10-26, finding A2: the server has no WriteTimeout (a
// stream lasts as long as generation does) and max_request_duration only
// cancelled the upstream call. A client that sends a request and never
// reads the answer blocked the handler in its write forever, holding the
// whole buffered answer (up to max_response_bytes, 64 MiB by default), the
// key's in-flight slot, the fair-share grant and the upstream's in-flight
// count. A few such connections exhaust the gateway's memory for every
// tenant. Each response now has a write deadline at the end of
// max_request_duration.
func TestSlowReaderReleasesItsRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			up := newFakeVLLM(t)
			cfg := testConfig(up.URL())
			cfg.Limits.MaxRequestDuration = time.Second
			e := newEnv(t, cfg)
			_, key := e.newKey("etabA", 0, 0, 1) // max_inflight = 1
			// An answer far larger than the socket buffers on both sides.
			if stream {
				parts := make([]string, 4096)
				for i := range parts {
					parts[i] = strings.Repeat("x", 8<<10)
				}
				up.setReply(parts...)
			} else {
				up.setReply(strings.Repeat("x", 48<<20))
			}

			c, err := net.Dial("tcp", e.gw.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.(*net.TCPConn).SetReadBuffer(4 << 10)
			body := fmt.Sprintf(`{"model":"llama","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
			fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: gw\r\nAuthorization: Bearer %s\r\n"+
				"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", key, len(body), body)
			// The client never reads. Once the request is admitted, the key's
			// only in-flight slot is taken.
			deadline := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(deadline) && up.count() == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			if up.count() == 0 {
				t.Fatal("the stalled request never reached the upstream")
			}
			up.setReply()
			// Within max_request_duration (1 s) plus the write grace (5 s)
			// plus slack, the slot is free again: a normal request of the
			// same key is served.
			deadline = time.Now().Add(writeGrace + 10*time.Second)
			for {
				resp := e.do("POST", "/v1/chat/completions", key, chat)
				out := readAll(t, resp)
				if resp.StatusCode == http.StatusOK {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("the stalled reader still holds the key's slot: %d %s", resp.StatusCode, out)
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
	}
}
