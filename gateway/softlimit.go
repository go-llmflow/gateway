package gateway

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// onSoftLimit records each budget that reached its soft limit and sends
// its alert once per period: the store's MarkSoftAlert decides, so several
// gateways and restarts do not send it twice.
func (s *Server) onSoftLimit(alerts []softAlert) {
	for _, al := range alerts {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		first, err := s.st.MarkSoftAlert(ctx, al.b.BudgetKey, al.start)
		cancel()
		if err != nil || !first {
			continue
		}
		s.met.softAlerts.add(1, string(al.b.Kind))
		s.log.Warn("soft budget reached", "budget", al.b.BudgetKey.String(), "spent", al.spent, "soft_limit", al.b.SoftLimit)
		if s.alerts != nil {
			al := al
			s.bg.Add(1)
			go func() {
				defer s.bg.Done()
				// Retries run until they succeed or give up; Close stops them.
				s.alerts.send(s.alertCtx, al)
			}()
		}
	}
}

const maxRequestTags = 8

var errTagNotAllowed = errors.New("request tag not allowed")

// requestTags merges the key's tags with the allow-listed tags of the
// configured request header.
func (s *Server) requestTags(r *http.Request, key store.Key) ([]string, error) {
	tags := append([]string(nil), key.Tags...)
	if h := s.cfg.Tags.Header; h != "" {
		vals := r.Header.Values(h)
		n := 0
		for _, v := range vals {
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				n++
				if n > maxRequestTags || !s.allowedTags[t] {
					return nil, errTagNotAllowed
				}
				tags = append(tags, t)
			}
		}
	}
	if len(tags) == 0 {
		return nil, nil
	}
	sort.Strings(tags)
	out := tags[:1]
	for _, t := range tags[1:] {
		if t != out[len(out)-1] {
			out = append(out, t)
		}
	}
	return out, nil
}
