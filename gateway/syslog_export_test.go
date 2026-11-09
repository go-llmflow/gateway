package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// TestRequestLogToSyslog sends a team's request log to a syslog destination:
// one RFC 5424 message per record, octet-counted, metadata only.
func TestRequestLogToSyslog(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	frames := make(chan string, 8)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		for {
			l, err := br.ReadString(' ')
			if err != nil {
				return
			}
			n, err := strconv.Atoi(strings.TrimSuffix(l, " "))
			if err != nil || n < 1 {
				return
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(br, b); err != nil {
				return
			}
			frames <- string(b)
		}
	}()

	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Exports = []config.Export{{Name: "siem", Kind: "syslog", Transport: "tcp", Address: ln.Addr().String(), InsecurePlain: true,
		Hostname: "gw-test", Facility: 16, QueueSize: 16}}
	l := logging("meta", false, "etabA")
	l.Destination = "siem"
	cfg.Logging = []config.Logging{l}
	e := newEnv(t, cfg)
	_, a := e.newKey("etabA")
	for i := 0; i < 2; i++ {
		if c := e.chatWith(a, chat); c != 200 {
			t.Fatalf("chat: %d", c)
		}
	}
	e.gw.Close()
	for _, lg := range e.srv.uniqueRequestLogs() {
		lg.flush()
	}
	e.srv.exports["siem"].Close(context.Background())

	for i := 0; i < 2; i++ {
		select {
		case m := <-frames:
			if !strings.HasPrefix(m, "<134>1 ") || !strings.Contains(m, " gw-test llmflow-gateway ") ||
				!strings.Contains(m, ` requests [llmflow@32473 object="requests/meta/`) {
				t.Fatalf("message %d: %q", i, m)
			}
			if strings.Contains(m, "messages") || strings.Contains(m, `"content"`) {
				t.Fatalf("a metadata-only log holds content: %q", m)
			}
			if !strings.Contains(m, `"Team":"etabA"`) {
				t.Fatalf("message %d lacks the team: %q", i, m)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
}
