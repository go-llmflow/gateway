package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func testAlert() softAlert {
	return softAlert{b: store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}, Period: store.Day, MaxCost: 10, SoftLimit: 8},
		start: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), spent: 8.5}
}

type hook struct {
	mu    sync.Mutex
	codes []int // answers, in order; the last one repeats
	keys  []string
}

func (h *hook) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.keys = append(h.keys, r.Header.Get("Idempotency-Key"))
		code := h.codes[min(len(h.keys)-1, len(h.codes)-1)]
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func noWait(a *alerter) {
	a.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
}

func TestAlertRetries(t *testing.T) {
	h := &hook{codes: []int{503, 502, 200}}
	srv := h.server(t)
	var mails int
	a := newAlerter(config.Alerts{WebhookURL: srv.URL, SMTP: &config.SMTP{Server: "127.0.0.1:25", From: "a@b.c", To: []string{"d@e.f"}}}, "", "", func(string, ...any) {})
	noWait(a)
	a.sendMail = func(string, smtp.Auth, string, []string, []byte) error { mails++; return nil }
	a.send(context.Background(), testAlert())
	if len(h.keys) != 3 || h.keys[0] == "" || h.keys[0] != h.keys[2] {
		t.Fatalf("webhook calls %v: want 3 with one idempotency key", h.keys)
	}
	// The mail went out once, although the webhook needed retries.
	if mails != 1 {
		t.Fatalf("%d mails", mails)
	}
	var m strings.Builder
	a.retried.write(&m)
	a.failed.write(&m)
	if !strings.Contains(m.String(), `llmflow_alert_retries_total{channel="webhook"} 2`) || strings.Contains(m.String(), `llmflow_alert_failures_total{channel`) {
		t.Fatalf("metrics:\n%s", m.String())
	}

	// A 4xx other than 408/429 is not retried; a 429 is.
	h2 := &hook{codes: []int{400}}
	a2 := newAlerter(config.Alerts{WebhookURL: h2.server(t).URL}, "", "", func(string, ...any) {})
	noWait(a2)
	a2.send(context.Background(), testAlert())
	if len(h2.keys) != 1 {
		t.Fatalf("a 400 was retried: %d calls", len(h2.keys))
	}
	h3 := &hook{codes: []int{429, 200}}
	a3 := newAlerter(config.Alerts{WebhookURL: h3.server(t).URL}, "", "", func(string, ...any) {})
	noWait(a3)
	a3.send(context.Background(), testAlert())
	if len(h3.keys) != 2 {
		t.Fatalf("a 429 was not retried: %d calls", len(h3.keys))
	}

	// A channel that never recovers is given up after the attempts, and
	// counted.
	var given []string
	a4 := newAlerter(config.Alerts{SMTP: &config.SMTP{Server: "127.0.0.1:25", From: "a@b.c", To: []string{"d@e.f"}}}, "", "", func(msg string, _ ...any) { given = append(given, msg) })
	noWait(a4)
	calls := 0
	a4.sendMail = func(string, smtp.Auth, string, []string, []byte) error { calls++; return errors.New("relay down") }
	a4.send(context.Background(), testAlert())
	m.Reset()
	a4.failed.write(&m)
	if calls != a4.attempts || len(given) != 1 || !strings.Contains(m.String(), `llmflow_alert_failures_total{channel="smtp"} 1`) {
		t.Fatalf("%d calls, logged %v, metrics %s", calls, given, m.String())
	}

	// Shutdown stops a retry that is waiting.
	a5 := newAlerter(config.Alerts{SMTP: &config.SMTP{Server: "127.0.0.1:25", From: "a@b.c", To: []string{"d@e.f"}}}, "", "", func(string, ...any) {})
	calls = 0
	a5.sendMail = func(string, smtp.Auth, string, []string, []byte) error { calls++; return errors.New("down") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a5.send(ctx, testAlert()); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled retry kept waiting")
	}
	if calls != 1 {
		t.Fatalf("%d attempts before the cancel", calls)
	}
}

func TestBackoff(t *testing.T) {
	b := exponentialBackoff(2*time.Second, time.Minute)
	for i, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute} {
		if d := b(i); d < want || d > want+want/4 {
			t.Errorf("backoff(%d) = %v, want %v plus up to 25%%", i, d, want)
		}
	}
	if d := b(100); d < time.Minute || d > time.Minute+time.Minute/4 {
		t.Errorf("backoff(100) = %v", d)
	}
}
