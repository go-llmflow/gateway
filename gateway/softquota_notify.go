package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Soft-quota notifications (#96) go through the soft-budget alert outbox
// (hastate.go): at least once, one row per (key, channel), the key being the
// idempotency key a webhook receiver de-duplicates on, delivered by any
// replica. The row is queued in the same transaction as the threshold's mark
// (store.MarkWarning), so a replica killed in between leaves both or neither.
//
// Channels: the alerts block's webhook (one POST per threshold, naming the
// recipients), and, with an smtp relay, one mail per recipient: the person
// (when their user name is an address, as an SSO or SCIM user's is) and, with
// notify = ["team_admin"], the administrators of their teams.

// quotaPayload is the webhook body and what a mail is made of.
type quotaPayload struct {
	Type       string    `json:"type"` // "soft_quota"
	User       string    `json:"user"`
	Window     string    `json:"window"`
	Threshold  int       `json:"threshold"`
	UsedPct    float64   `json:"used_pct"`
	Instance   time.Time `json:"window_instance,omitzero"`
	ResetsAt   time.Time `json:"resets_at"`
	Message    string    `json:"message"`
	Lang       string    `json:"lang"`
	Recipients []string  `json:"recipients,omitempty"`
	// To is the recipient of one mail row.
	To string `json:"to,omitempty"`
}

// quotaKey names one threshold of one window instance: the idempotency key.
func quotaKey(p quotaPayload) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{"soft_quota", p.User, p.Window, strconv.Itoa(p.Threshold), p.Instance.UTC().Format(time.RFC3339)}, "|")))
	return hex.EncodeToString(sum[:16])
}

// address returns name as a mail address when it is exactly one.
func address(name string) (string, bool) {
	if len(name) > 254 || !strings.Contains(name, "@") {
		return "", false
	}
	a, err := mail.ParseAddress(name)
	if err != nil || a.Address != name || a.Name != "" {
		return "", false
	}
	return name, true
}

// quotaRecipients: who is told, from the configuration's notify list.
func (s *Server) quotaRecipients(ctx context.Context, user string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		if a, ok := address(n); ok && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, n := range s.cfg.SoftQuotas.Notify {
		switch n {
		case config.NotifyUser:
			add(user)
		case config.NotifyTeamAdmin:
			binds, err := s.st.Bindings(ctx, user)
			if err != nil {
				continue
			}
			teams := map[string]bool{}
			for _, b := range binds {
				if b.Team != "" {
					teams[b.Team] = true
				}
			}
			all, err := s.st.Bindings(ctx, "")
			if err != nil {
				continue
			}
			for _, b := range all {
				if b.Role == store.RoleTeamAdmin && teams[b.Team] {
					add(b.User)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Server) quotaPayloadFor(ctx context.Context, user string, w winUse, th int) quotaPayload {
	lang := s.langOf(nil)
	inst := w.Start
	if w.Rolling {
		inst = store.BucketOf(s.now())
	}
	p := quotaPayload{Type: "soft_quota", User: user, Window: w.Window, Threshold: th, UsedPct: round2(w.Pct), Instance: inst.UTC(),
		ResetsAt: w.Reset.UTC(), Lang: lang, Message: s.warnText(w.Window, w.Pct, w.Reset, lang)}
	if s.alerts != nil {
		p.Recipients = s.quotaRecipients(ctx, user)
	}
	return p
}

// quotaOutbox is the rows of a notification: the webhook, and a mail per
// recipient when a relay is configured. A webhook-less, relay-less gateway
// queues nothing (the audit event and the metric remain).
func (s *Server) quotaOutbox(p quotaPayload) []store.WarnOutbox {
	var out []store.WarnOutbox
	key := quotaKey(p)
	if s.cfg.Alerts.WebhookURL != "" {
		if b, err := json.Marshal(p); err == nil {
			out = append(out, store.WarnOutbox{Key: key, Channel: "webhook", Payload: b})
		}
	}
	if s.cfg.Alerts.SMTP != nil {
		for _, to := range p.Recipients {
			m := p
			m.To, m.Recipients = to, nil
			if b, err := json.Marshal(m); err == nil {
				sum := sha256.Sum256([]byte(to))
				out = append(out, store.WarnOutbox{Key: key + "/" + hex.EncodeToString(sum[:4]), Channel: "smtp", Payload: b})
			}
		}
	}
	return out
}

// quotaWebhook posts a soft-quota notification with its idempotency key.
func (a *alerter) quotaWebhook(ctx context.Context, body []byte, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
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

// quotaMail sends one person's mail.
func (a *alerter) quotaMail(p quotaPayload) error {
	s := a.cfg.SMTP
	to, ok := address(p.To)
	if !ok {
		return permanent{fmt.Errorf("not a mail address")}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", headerSafe(s.From), headerSafe(to))
	fmt.Fprintf(&b, "Subject: %s\r\n", headerSafe("[llmflow] "+p.Message))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "%s\r\n\r\n%s: %s\r\n", p.Message, p.Window, headerSafe(p.User))
	var auth smtp.Auth
	if s.Username != "" {
		host, _, _ := net.SplitHostPort(s.Server)
		auth = smtp.PlainAuth("", s.Username, a.smtpPassword, host)
	}
	return a.sendMail(s.Server, auth, s.From, []string{to}, []byte(b.String()))
}

// sendQuota delivers a notification directly (no shared outbox), each
// channel retried independently.
func (a *alerter) sendQuota(ctx context.Context, p quotaPayload) {
	key := quotaKey(p)
	if a.cfg.WebhookURL != "" {
		body, _ := json.Marshal(p)
		if err := a.retry(ctx, "webhook", func() error { return a.quotaWebhook(ctx, body, key) }); err != nil {
			a.log("soft-quota webhook failed, given up", "user", p.User, "err", err)
		}
	}
	if a.cfg.SMTP != nil {
		for _, to := range p.Recipients {
			m := p
			m.To, m.Recipients = to, nil
			if err := a.retry(ctx, "smtp", func() error { return a.quotaMail(m) }); err != nil {
				a.log("soft-quota mail failed, given up", "user", p.User, "err", err)
			}
		}
	}
}

// deliverQuota delivers a claimed outbox row of a soft-quota notification;
// ok is false when the row is not one.
func (s *Server) deliverQuota(ctx context.Context, a store.OutboxAlert) (handled bool, err error) {
	var p quotaPayload
	if json.Unmarshal(a.Payload, &p) != nil || p.Type != "soft_quota" {
		return false, nil
	}
	switch a.Channel {
	case "webhook":
		// The key of the row is the base key; a mail row's has a suffix.
		return true, s.alerts.quotaWebhook(ctx, a.Payload, a.Key)
	case "smtp":
		return true, s.alerts.quotaMail(p)
	}
	return true, permanent{fmt.Errorf("unknown channel")}
}
