package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// alerter sends soft-budget alerts to a webhook and/or by e-mail through an
// SMTP relay (net/smtp SendMail: STARTTLS when the server offers it;
// PlainAuth "will only send the credentials if the connection is using TLS
// or is connected to localhost").
type alerter struct {
	cfg          config.Alerts
	webhookToken string
	smtpPassword string
	client       *http.Client
	sendMail     func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
	log          func(msg string, args ...any)
	// Each channel is tried up to attempts times, waiting backoff(i)
	// before attempt i+1; sleep waits (tests replace it).
	attempts int
	backoff  func(i int) time.Duration
	sleep    func(ctx context.Context, d time.Duration) error
	// failed and retried count, by channel, the alerts given up on and the
	// retries made.
	failed, retried *counterVec
}

func newAlerter(cfg config.Alerts, webhookToken, smtpPassword string, log func(string, ...any)) *alerter {
	return &alerter{
		cfg: cfg, webhookToken: webhookToken, smtpPassword: smtpPassword, log: log,
		client: &http.Client{
			Timeout:       10 * time.Second,
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		sendMail: smtp.SendMail,
		attempts: 6,
		backoff:  exponentialBackoff(2*time.Second, 2*time.Minute),
		sleep:    sleepCtx,
		failed:   newCounterVec("llmflow_alert_failures_total", "Soft-budget alerts given up on after every retry, by channel.", "channel"),
		retried:  newCounterVec("llmflow_alert_retries_total", "Soft-budget alert retries, by channel.", "channel"),
	}
}

// exponentialBackoff doubles from base up to limit, with up to 25% jitter
// so that several gateways do not retry in step.
func exponentialBackoff(base, limit time.Duration) func(int) time.Duration {
	return func(i int) time.Duration {
		d := base << min(i, 20)
		if d > limit || d <= 0 {
			d = limit
		}
		return d + time.Duration(rand.Int64N(int64(d)/4+1))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// permanent marks an error that a retry cannot fix (a 4xx answer other
// than 408 and 429).
type permanent struct{ error }

// retry runs try until it succeeds, fails permanently, attempts run out or
// ctx ends. It reports the last error.
func (a *alerter) retry(ctx context.Context, channel string, try func() error) error {
	var err error
	for i := 0; i < a.attempts; i++ {
		if i > 0 {
			a.retried.add(1, channel)
			if serr := a.sleep(ctx, a.backoff(i-1)); serr != nil {
				break
			}
		}
		if err = try(); err == nil {
			return nil
		}
		var p permanent
		if errors.As(err, &p) {
			break
		}
	}
	a.failed.add(1, channel)
	return err
}

// event sends an operational alert (an export dropping objects, the audit
// anchor lagging) to the configured channels, with the same retries.
func (a *alerter) event(ctx context.Context, typ string, fields map[string]any) {
	payload := map[string]any{"type": typ}
	for k, v := range fields {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if a.cfg.WebhookURL != "" {
		err := a.retry(ctx, "webhook", func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.WebhookURL, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if a.webhookToken != "" {
				req.Header.Set("Authorization", "Bearer "+a.webhookToken)
			}
			resp, err := a.client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			if resp.StatusCode >= 300 {
				return fmt.Errorf("webhook answered %d", resp.StatusCode)
			}
			return nil
		})
		if err != nil {
			a.log("alert webhook failed, given up", "type", typ, "err", err)
		}
	}
	if s := a.cfg.SMTP; s != nil {
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\n", headerSafe(s.From), headerSafe(strings.Join(s.To, ", ")), headerSafe("[llmflow] "+typ))
		b.WriteString("MIME-Version: 1.0\r\nContent-Type: application/json; charset=utf-8\r\n\r\n")
		b.Write(body)
		b.WriteString("\r\n")
		var auth smtp.Auth
		if s.Username != "" {
			host, _, _ := net.SplitHostPort(s.Server)
			auth = smtp.PlainAuth("", s.Username, a.smtpPassword, host)
		}
		if err := a.retry(ctx, "smtp", func() error { return a.sendMail(s.Server, auth, s.From, s.To, []byte(b.String())) }); err != nil {
			a.log("alert mail failed, given up", "type", typ, "err", err)
		}
	}
}

// idempotencyKey names one alert (budget and period) for a receiver that
// de-duplicates: every retry of the webhook carries the same one.
func idempotencyKey(p alertPayload) string {
	sum := sha256.Sum256([]byte(p.Budget.String() + "|" + p.PeriodStart.UTC().Format(time.RFC3339)))
	return hex.EncodeToString(sum[:16])
}

// alertPayload is the webhook body.
type alertPayload struct {
	Type        string          `json:"type"`
	Budget      store.BudgetKey `json:"budget"`
	Period      store.Period    `json:"period"`
	PeriodStart time.Time       `json:"period_start"`
	SoftLimit   float64         `json:"soft_limit"`
	MaxCost     float64         `json:"max_cost"`
	Spent       float64         `json:"spent"`
}

func (a *alerter) send(ctx context.Context, al softAlert) {
	p := alertPayload{Type: "soft_budget", Budget: al.b.BudgetKey, Period: al.b.Period, PeriodStart: al.start,
		SoftLimit: al.b.SoftLimit, MaxCost: al.b.MaxCost, Spent: al.spent}
	// The channels are retried independently: a mail that went out is not
	// sent again because the webhook is down.
	var wg sync.WaitGroup
	if a.cfg.WebhookURL != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.retry(ctx, "webhook", func() error { return a.webhook(ctx, p) }); err != nil {
				a.log("soft-budget webhook failed, given up", "budget", al.b.BudgetKey.String(), "err", err)
			}
		}()
	}
	if a.cfg.SMTP != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.retry(ctx, "smtp", func() error { return a.mail(p) }); err != nil {
				a.log("soft-budget mail failed, given up", "budget", al.b.BudgetKey.String(), "err", err)
			}
		}()
	}
	wg.Wait()
}

func (a *alerter) webhook(ctx context.Context, p alertPayload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey(p))
	if a.webhookToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.webhookToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		err := fmt.Errorf("webhook answered %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
			return permanent{err}
		}
		return err
	}
	return nil
}

// headerSafe removes what could end or fold a header line.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (a *alerter) mail(p alertPayload) error {
	s := a.cfg.SMTP
	subject := headerSafe(fmt.Sprintf("[llmflow] soft budget reached: %s %s", p.Budget.Kind, p.Budget.Name))
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", headerSafe(s.From))
	fmt.Fprintf(&b, "To: %s\r\n", headerSafe(strings.Join(s.To, ", ")))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "Budget: %s %s", headerSafe(string(p.Budget.Kind)), headerSafe(p.Budget.Name))
	if p.Budget.Model != "" {
		fmt.Fprintf(&b, " (model %s)", headerSafe(p.Budget.Model))
	}
	fmt.Fprintf(&b, "\r\nPeriod: %s from %s\r\nSpent: %.6f GPU-hours\r\nSoft limit: %.6f GPU-hours\r\nMaximum: %.6f GPU-hours\r\n",
		p.Period, p.PeriodStart.Format(time.RFC3339), p.Spent, p.SoftLimit, p.MaxCost)
	var auth smtp.Auth
	if s.Username != "" {
		host, _, _ := net.SplitHostPort(s.Server)
		auth = smtp.PlainAuth("", s.Username, a.smtpPassword, host)
	}
	return a.sendMail(s.Server, auth, s.From, s.To, []byte(b.String()))
}
