package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
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
	}
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
	if a.cfg.WebhookURL != "" {
		if err := a.webhook(ctx, p); err != nil {
			a.log("soft-budget webhook failed", "budget", al.b.BudgetKey.String(), "err", err)
		}
	}
	if a.cfg.SMTP != nil {
		if err := a.mail(p); err != nil {
			a.log("soft-budget mail failed", "budget", al.b.BudgetKey.String(), "err", err)
		}
	}
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
