package gateway

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-llmflow/gateway/export"
)

// Scheduled delivery of the decision report (#97). On and after the
// configured day of the month (UTC) the report of the previous calendar month
// is written once to the export named by decision_report.destination, as
// decision/YYYY-MM/decision-YYYY-MM.pdf and .xlsx. The gateway sends no email:
// the object is the delivery (write-once where the export has Object Lock),
// and a mail gateway or a bucket notification takes it from there. The
// existence of the object is the state: a restart, or a second replica, finds
// it and does nothing; a second writer's duplicate is refused by the sink. The
// report is generated as the platform, with the reading rights of the proxy
// roles, and every delivery is audited (actor "decision-report", the period,
// the format and the size, never the content).

// decisionTick is how often the schedule is looked at.
var decisionTick = 15 * time.Minute

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
	prefix := "decision/" + from.Format("2006-01") + "/"
	have, err := q.Sink().List(ctx, prefix)
	if err != nil {
		s.log.Error("decision report: listing the export failed", "err", err)
		return
	}
	var missing []string
	for _, f := range d.Formats {
		if !slices.Contains(have, decisionKey(from, f)) {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/admin/decision", nil)
	if err != nil {
		return
	}
	data, err := s.buildDecision(req, &principal{name: "decision-report", master: true}, from, to)
	if err != nil {
		s.log.Error("decision report: building it failed", "period", from.Format("2006-01"), "err", err)
		return
	}
	for _, f := range missing {
		var out bytes.Buffer
		ctype := "application/pdf"
		if f == DecisionXLSX {
			ctype = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
			err = writeDecisionXLSX(&out, data)
		} else {
			err = writeDecisionPDF(&out, data)
		}
		if err != nil {
			s.log.Error("decision report: writing it failed", "format", f, "err", err)
			continue
		}
		key := decisionKey(from, f)
		if !q.Enqueue(key, out.Bytes(), export.PutOptions{ContentType: ctype, Retain: true}) {
			s.log.Error("decision report: the export queue is full; will retry", "key", key)
			continue
		}
		s.auditDecision(ctx, "decision-report", fmt.Sprintf("deliver to %s", d.Destination), f, from, to, out.Len(), http.StatusOK)
	}
}
