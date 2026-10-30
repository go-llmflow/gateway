package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/export"
)

// Scheduled delivery of the decision report (#97, #109). On and after the
// configured day of the month (UTC) the report of the previous calendar month
// is delivered:
//
//   - to the export named by decision_report.destination, as
//     decision/YYYY-MM/decision-YYYY-MM.pdf and .xlsx (write-once under Object
//     Lock where the export has it);
//   - by mail to decision_report.mail_to, through the alerts' SMTP relay (TLS
//     required, smtpsend.go): the PDF as an attachment, the XLSX too when both
//     are small enough, else a line saying where the export holds them.
//
// The sink is the state. An object that exists is delivered; the mail's
// marker object decision/YYYY-MM/mailed.json says the mail went out, so a
// restart, or a second replica, finds both and does nothing. The queue
// uploads asynchronously: a key enqueued and not yet seen in the sink is
// remembered (decisionState.pending) so that the next tick does not typeset the
// report again while an upload retries, and the audit entry "deliver" is
// written when the object is SEEN in the sink, not when it is queued. Every
// delivery and every mail is audited (actor "decision-report", the period, the
// format and the size, never the content).

// decisionTick is how often the schedule is looked at.
var decisionTick = 15 * time.Minute

// decisionPendingTTL is how long a queued upload is trusted to be on its way
// before the report is built again.
const decisionPendingTTL = 2 * time.Hour

// mailAttachmentLimit is the most a report mail carries: relays refuse large
// messages, and base64 grows them by a third.
const mailAttachmentLimit = 8 << 20

type pendingObject struct {
	at     time.Time
	size   int
	format string
}

// decisionState is what a replica remembers between ticks.
type decisionState struct {
	mu      sync.Mutex
	pending map[string]pendingObject
	mailed  map[string]bool
}

// decisionPeriod is the month a report is due for at now: the previous
// calendar month, from the configured day on; ok is false before it.
func decisionPeriod(now time.Time, day int) (from, to time.Time, ok bool) {
	now = now.UTC()
	if now.Day() < day {
		return time.Time{}, time.Time{}, false
	}
	to = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return to.AddDate(0, -1, 0), to, true
}

func decisionKey(from time.Time, format string) string {
	m := from.UTC().Format("2006-01")
	return "decision/" + m + "/decision-" + m + "." + format
}

func decisionMailKey(from time.Time) string {
	return "decision/" + from.UTC().Format("2006-01") + "/mailed.json"
}

func (s *Server) startDecision(ctx context.Context) {
	d := s.cfg.DecisionReport
	if d == nil {
		return
	}
	q := s.exports[d.Destination]
	if q == nil {
		s.log.Error("decision report: the destination export is not open", "destination", d.Destination)
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		t := time.NewTicker(decisionTick)
		defer t.Stop()
		for {
			s.decisionRound(ctx, q)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// decisionRound delivers what is due and not yet delivered.
func (s *Server) decisionRound(ctx context.Context, q *export.Queue) {
	d := s.cfg.DecisionReport
	from, to, ok := decisionPeriod(s.now(), d.Day)
	if !ok {
		return
	}
	period := from.Format("2006-01")
	prefix := "decision/" + period + "/"
	have, err := q.Sink().List(ctx, prefix)
	if err != nil {
		s.log.Error("decision report: listing the export failed", "err", err)
		return
	}
	st := &s.decision
	st.mu.Lock()
	if st.pending == nil {
		st.pending, st.mailed = map[string]pendingObject{}, map[string]bool{}
	}
	// What the sink now holds is delivered: audit it, once, here.
	for key, p := range st.pending {
		if slices.Contains(have, key) {
			delete(st.pending, key)
			s.auditDecision(ctx, "decision-report", "deliver to "+d.Destination, p.format, from, to, p.size, http.StatusOK)
		} else if s.now().Sub(p.at) > decisionPendingTTL {
			delete(st.pending, key) // never arrived: build it again
		}
	}
	var missing []string
	for _, f := range d.Formats {
		key := decisionKey(from, f)
		if _, inFlight := st.pending[key]; !slices.Contains(have, key) && !inFlight {
			missing = append(missing, f)
		}
	}
	needMail := len(d.MailTo) > 0 && s.alerts != nil && !st.mailed[period] && !slices.Contains(have, decisionMailKey(from))
	st.mu.Unlock()
	if len(missing) == 0 && !needMail {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/admin/decision", nil)
	if err != nil {
		return
	}
	data, err := s.buildDecision(req, &principal{name: "decision-report", master: true}, from, to)
	if err != nil {
		s.log.Error("decision report: building it failed", "period", period, "err", err)
		return
	}
	built := map[string][]byte{}
	for _, f := range d.Formats {
		if !slices.Contains(missing, f) && !needMail {
			continue
		}
		body, err := renderDecision(f, data)
		if err != nil {
			s.log.Error("decision report: writing it failed", "format", f, "err", err)
			continue
		}
		built[f] = body
	}
	for _, f := range missing {
		body, ok := built[f]
		if !ok {
			continue
		}
		key := decisionKey(from, f)
		ctype := decisionPDFType
		if f == DecisionXLSX {
			ctype = decisionXLSXType
		}
		if !q.Enqueue(key, body, export.PutOptions{ContentType: ctype, Retain: true}) {
			s.log.Error("decision report: the export queue is full; will retry", "key", key)
			continue
		}
		st.mu.Lock()
		st.pending[key] = pendingObject{at: s.now(), size: len(body), format: f}
		st.mu.Unlock()
	}
	if needMail {
		s.decisionMail(ctx, q, from, to, built)
	}
}

// decisionMail sends the report to decision_report.mail_to and leaves the
// marker object.
func (s *Server) decisionMail(ctx context.Context, q *export.Queue, from, to time.Time, built map[string][]byte) {
	d := s.cfg.DecisionReport
	smtpCfg := s.cfg.Alerts.SMTP
	if smtpCfg == nil {
		return
	}
	period := from.Format("2006-01")
	var atts []mailAttachment
	total := 0
	for _, f := range []string{DecisionPDF, DecisionXLSX} { // the PDF first: it is the one that must fit
		body, ok := built[f]
		if !ok || total+len(body) > mailAttachmentLimit {
			continue
		}
		total += len(body)
		name := "decision-" + period + "." + f
		ctype := decisionPDFType
		if f == DecisionXLSX {
			ctype = decisionXLSXType
		}
		atts = append(atts, mailAttachment{Name: name, Type: ctype, Data: body})
	}
	text := fmt.Sprintf("The decision report of %s is attached.\r\n\r\n", period)
	if len(atts) < len(built) || len(atts) == 0 {
		text += fmt.Sprintf("Some files are too large to send by mail: they are in the export %q under decision/%s/.\r\n", d.Destination, period)
	}
	text += fmt.Sprintf("The files are also stored in the export %q (decision/%s/), write-once where the export has Object Lock.\r\n", d.Destination, period)
	msg := buildReportMail(smtpCfg.From, d.MailTo, "[llmflow] decision report "+period, text, atts)
	err := s.alerts.retry(ctx, "smtp", func() error {
		return s.alerts.sendMail(smtpCfg.Server, nil, smtpCfg.From, d.MailTo, msg)
	})
	if err != nil {
		s.log.Error("decision report: the mail failed; will retry at the next tick", "period", period, "err", err)
		return
	}
	s.auditDecision(ctx, "decision-report", fmt.Sprintf("mail to %d recipients", len(d.MailTo)), "mail", from, to, len(msg), http.StatusOK)
	marker, _ := json.Marshal(map[string]any{"mailed_at": s.now().UTC(), "recipients": len(d.MailTo), "attachments": len(atts)})
	q.Enqueue(decisionMailKey(from), marker, export.PutOptions{ContentType: "application/json", Retain: true})
	s.decision.mu.Lock()
	s.decision.mailed[period] = true
	s.decision.mu.Unlock()
}
