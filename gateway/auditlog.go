package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// The audit log of administrative actions (package audit makes it
// tamper-evident). Every state-changing admin request is recorded — who and
// with which roles, what, on which target, the state before and after, the
// outcome, the client address and a request id — whether it succeeded or
// was refused. Events the gateway raises itself (scheduled rotation, SCIM
// reconciliation, SSO logins) go to the same log.

const maxAuditBody = 64 << 10

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// adminRequestID is the client's X-Request-Id when it is a plain token,
// else a fresh one; it is echoed in the response.
func adminRequestID(w http.ResponseWriter, r *http.Request) string {
	id := r.Header.Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		var b [12]byte
		_, _ = rand.Read(b[:])
		id = hex.EncodeToString(b[:])
	}
	w.Header().Set("X-Request-Id", id)
	return id
}

// statusRecorder keeps the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func rolesJSON(p *principal) string {
	type out struct {
		Master   bool            `json:"master,omitempty"`
		Bindings []store.Binding `json:"bindings"`
	}
	bs := p.bindings
	if bs == nil {
		bs = []store.Binding{}
	}
	b, _ := json.Marshal(out{p.master, bs})
	return string(b)
}

// snapshot is the state an admin route acts on, as JSON ("null" when the
// route has no single target or it does not exist).
func (s *Server) snapshot(ctx context.Context, pattern string, r *http.Request) string {
	var v any
	var err error
	pv := r.PathValue
	switch {
	case strings.HasPrefix(pattern, "/admin/keys/{id}"):
		v, err = s.st.GetKey(ctx, pv("id"))
	case pattern == "/admin/teams/{name}":
		v, err = s.st.GetTeam(ctx, pv("name"))
	case pattern == "/admin/teams/{team}/models":
		var all map[string][]string
		if all, err = s.st.TeamModels(ctx); err == nil {
			v = all[pv("team")]
		}
	case strings.HasPrefix(pattern, "/admin/teams/{team}/projects"):
		v, err = s.st.ListProjects(ctx, pv("team"))
	case pattern == "/admin/users/{name}":
		v, err = s.st.GetUser(ctx, pv("name"))
	case pattern == "/admin/users/{name}/roles":
		v, err = s.st.Bindings(ctx, pv("name"))
	case strings.HasPrefix(pattern, "/admin/users/{name}/tokens"):
		v, err = s.st.ListUserTokens(ctx, pv("name"))
	case pattern == "/admin/orgs/{name}":
		var os []store.Org
		if os, err = s.st.ListOrgs(ctx); err == nil {
			i := slices.IndexFunc(os, func(o store.Org) bool { return o.Name == pv("name") })
			if i >= 0 {
				v = os[i]
			}
		}
	case strings.HasPrefix(pattern, "/admin/budgets"):
		v, err = s.st.Budgets(ctx)
	case pattern == "/admin/allowlists":
		v, err = s.st.Allowlists(ctx)
	}
	if err != nil || v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// auditTarget names what a request acts on: the path, which carries the
// ids, and the query (budgets name their scope there).
func auditTarget(r *http.Request) string {
	t := r.URL.Path
	if q := r.URL.RawQuery; q != "" {
		t += "?" + q
	}
	return t
}

// audited wraps a state-changing admin handler.
func (s *Server) audited(pattern string, f adminHandler) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		id := adminRequestID(w, r)
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			f(w, r, p)
			return
		}
		// The body is read once here (bounded as readJSON bounds it) and
		// given back to the handler.
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxAuditBody))
		r.Body = io.NopCloser(bytes.NewReader(body))
		before := s.snapshot(r.Context(), pattern, r)
		rec := &statusRecorder{ResponseWriter: w}
		f(rec, r, p)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		req := json.RawMessage("null")
		if json.Valid(body) {
			req = body
		}
		after, _ := json.Marshal(struct {
			Request json.RawMessage `json:"request"`
			State   json.RawMessage `json:"state"`
		}{req, json.RawMessage(s.snapshot(r.Context(), pattern, r))})
		addr, _ := clientAddrOf(r)
		ip := ""
		if addr.IsValid() {
			ip = addr.String()
		}
		s.appendAudit(r.Context(), store.AuditEntry{Kind: "admin", Actor: p.name, Roles: rolesJSON(p),
			Action: r.Method + " " + pattern, Target: auditTarget(r), Before: before, After: string(after),
			Status: rec.status, SourceIP: ip, RequestID: id})
	}
}

// appendAudit writes an entry; a failure is logged and counted, never
// silent.
func (s *Server) appendAudit(ctx context.Context, e store.AuditEntry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.auditLog.Append(ctx, e); err != nil {
		s.met.auditFailures.add(1, e.Kind)
		s.log.Error("audit: append failed", "err", err, "action", e.Action, "actor", e.Actor, "target", e.Target)
	}
}

// audit records an event the gateway raises itself: args are key/value
// pairs, "by" naming the actor; the rest goes to After.
func (s *Server) audit(event string, args ...any) {
	s.log.Info("audit: "+event, append([]any{"audit", true, "event", event}, args...)...)
	kind := "system"
	switch {
	case strings.HasPrefix(event, "scim."):
		kind = "scim"
	case strings.HasPrefix(event, "sso."):
		kind = "auth"
	}
	fields := map[string]any{}
	actor := "gateway"
	for i := 0; i+1 < len(args); i += 2 {
		k, _ := args[i].(string)
		if k == "by" {
			if a, ok := args[i+1].(string); ok {
				actor = a
			}
			continue
		}
		fields[k] = args[i+1]
	}
	after, _ := json.Marshal(fields)
	tgt, _ := fields["key"].(string)
	if tgt == "" {
		tgt, _ = fields["user"].(string)
	}
	s.appendAudit(context.Background(), store.AuditEntry{Kind: kind, Actor: actor, Roles: "[]", Action: event, Target: tgt,
		Before: "null", After: string(after), Status: 0})
}

// checkpointLoop signs new entries every checkpoint_interval.
func (s *Server) checkpointLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Audit.CheckpointInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last checkpoint on the way out, so a clean shutdown leaves
			// nothing unsigned.
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if seq, err := s.auditLog.Checkpoint(cctx); err != nil {
				s.log.Error("audit: final checkpoint failed", "err", err)
			} else if seq > 0 {
				s.anchorUpTo(cctx)
			}
			cancel()
			return
		case <-t.C:
			if seq, err := s.auditLog.Checkpoint(ctx); err != nil {
				s.met.auditFailures.add(1, "checkpoint")
				s.log.Error("audit: checkpoint failed", "err", err)
			} else if seq > 0 {
				s.anchorUpTo(ctx)
			}
			s.checkAnchorLag(ctx)
		}
	}
}
