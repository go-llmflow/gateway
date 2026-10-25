package gateway

import (
	"crypto/ed25519"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/store"
)

// The audit log through the admin API (and the admin UI): browsing, and the
// chain's verification. Both are for proxy administrators and viewers only:
// an entry's before and after can describe any organisation.

// maxAuditPage bounds a page of entries.
const maxAuditPage = 200

// verifying lets one verification run at a time: it reads the whole log.
var verifying sync.Mutex

// listAudit returns entries from seq "from" (default 1), at most "limit".
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyView() {
		forbidden(w)
		return
	}
	q := r.URL.Query()
	from, err := strconv.ParseInt(q.Get("from"), 10, 64)
	if q.Get("from") == "" {
		from, err = 1, nil
	}
	limit, lerr := strconv.Atoi(q.Get("limit"))
	if q.Get("limit") == "" {
		limit, lerr = 50, nil
	}
	if err != nil || lerr != nil || from < 1 || limit < 1 || limit > maxAuditPage {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from >= 1 and 1 <= limit <= 200")
		return
	}
	es, err := s.st.AuditRange(r.Context(), from, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	out := make([]entryJSON, 0, len(es))
	for _, e := range es {
		out = append(out, toEntryJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// auditStatus is the result of a verification.
type auditStatus struct {
	OK          bool     `json:"ok"`
	First       int64    `json:"first"`
	Last        int64    `json:"last"`
	Entries     int      `json:"entries"`
	Checkpoints int      `json:"checkpoints"`
	Unsigned    int      `json:"unsigned"`
	Problems    []string `json:"problems"`
	// Anchored: checked against the newest checkpoint of the anchor export.
	Anchored  bool  `json:"anchored"`
	AnchorSeq int64 `json:"anchor_seq,omitempty"`
	// Signed: checkpoints were checked with the gateway's own key. An
	// independent check uses `llmflow-gateway audit verify` with a public
	// key kept elsewhere.
	Signed bool `json:"signed"`
}

// verifyAudit recomputes the chain (and checks it against the anchor when
// one is configured). One at a time: 429 while another runs.
func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyView() {
		forbidden(w)
		return
	}
	if !verifying.TryLock() {
		apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "a verification is already running")
		return
	}
	defer verifying.Unlock()
	var keys []ed25519.PublicKey
	if s.auditKey != nil {
		keys = append(keys, s.auditKey.Public().(ed25519.PublicKey))
	}
	var (
		rep    audit.Report
		err    error
		status = auditStatus{Signed: len(keys) > 0}
	)
	if s.anchor != nil {
		var a store.AuditCheckpoint
		if a, err = NewestAnchor(r.Context(), s.anchor.q.Sink()); err == nil {
			rep, err = audit.VerifyAnchored(r.Context(), s.st, keys, a)
			status.Anchored, status.AnchorSeq = true, a.Seq
		}
	} else {
		rep, err = audit.Verify(r.Context(), s.st, keys)
	}
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "verification could not run: "+err.Error())
		return
	}
	status.OK, status.First, status.Last, status.Entries = rep.OK(), rep.First, rep.Last, rep.Entries
	status.Checkpoints, status.Unsigned, status.Problems = rep.Checkpoints, rep.Unsigned, rep.Problems
	if status.Problems == nil {
		status.Problems = []string{}
	}
	writeJSON(w, http.StatusOK, status)
}
