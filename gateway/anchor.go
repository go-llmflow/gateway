package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/export"
	"github.com/go-llmflow/gateway/store"
)

// Anchoring the audit log outside the database (review of #29: entries and
// checkpoints in one database can be truncated or rolled back together,
// and Verify cannot tell). Every checkpoint, and the entries it covers, is
// written to the export the configuration names — on S3 under Object Lock
// retention when the bucket has it — and `audit verify --anchor` checks the
// database against the newest one. The age of the newest exported
// checkpoint is a metric, and an alert past audit.anchor_max_lag.

const (
	anchorCheckpoints = "audit/checkpoints/"
	anchorEntries     = "audit/entries/"
)

// CheckpointKey is the object name of the checkpoint at seq (zero-padded,
// so the lexicographic listing is in seq order).
func CheckpointKey(seq int64) string { return fmt.Sprintf("%s%020d.json", anchorCheckpoints, seq) }

func entriesKey(from, to int64) string {
	return fmt.Sprintf("%s%020d-%020d.jsonl.gz", anchorEntries, from, to)
}

// anchorState tracks what was handed to and written by the export.
type anchorState struct {
	q        *export.Queue
	mu       sync.Mutex
	enqueued int64               // newest checkpoint seq queued
	times    map[int64]time.Time // queued checkpoint seq -> its time
	written  atomic.Int64        // unix micro of the newest written checkpoint's time
	seq      atomic.Int64        // seq of the newest written checkpoint
	lagged   atomic.Bool         // an alert is out for this lag episode
}

// entryJSON is an exported entry: everything the hash covers, plus the
// hashes, so an auditor can recompute the chain from the export alone.
type entryJSON struct {
	V         int    `json:"v"`
	Seq       int64  `json:"seq"`
	TimeMicro int64  `json:"time_us"`
	Kind      string `json:"kind"`
	Actor     string `json:"actor"`
	Roles     string `json:"roles"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Before    string `json:"before"`
	After     string `json:"after"`
	Status    int    `json:"status"`
	SourceIP  string `json:"source_ip"`
	RequestID string `json:"request_id"`
	PrevHash  string `json:"prev_hash"`
	Hash      string `json:"hash"`
}

// startAnchor opens the anchor export's queue and catches up with the
// checkpoints the database has and the export does not.
func (s *Server) startAnchor(ctx context.Context, q *export.Queue) error {
	a := &anchorState{q: q, times: map[int64]time.Time{}}
	keys, err := q.Sink().List(ctx, anchorCheckpoints)
	if err != nil {
		return fmt.Errorf("gateway: audit anchor %s: listing checkpoints: %w", q.Sink().Name(), err)
	}
	if n := len(keys); n > 0 {
		b, err := q.Sink().Get(ctx, keys[n-1])
		if err != nil {
			return fmt.Errorf("gateway: audit anchor %s: %w", q.Sink().Name(), err)
		}
		cp, err := audit.ParseCheckpoint(b)
		if err != nil {
			return fmt.Errorf("gateway: audit anchor %s: %s: %w", q.Sink().Name(), keys[n-1], err)
		}
		a.enqueued = cp.Seq
		a.written.Store(cp.Time.UnixMicro())
		a.seq.Store(cp.Seq)
	}
	q.OnWritten = func(key string) {
		seq, ok := strings.CutPrefix(key, anchorCheckpoints)
		if !ok {
			return
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(seq, ".json"), 10, 64)
		if err != nil {
			return
		}
		a.mu.Lock()
		t := a.times[n]
		delete(a.times, n)
		a.mu.Unlock()
		if us := t.UnixMicro(); us > a.written.Load() {
			a.written.Store(us)
		}
		if n > a.seq.Load() {
			a.seq.Store(n)
		}
	}
	s.anchor = a
	s.anchorUpTo(ctx)
	return nil
}

// anchorUpTo exports every checkpoint newer than the last one queued, each
// after the entries it covers.
func (s *Server) anchorUpTo(ctx context.Context) {
	a := s.anchor
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cps, err := s.st.AuditCheckpoints(ctx)
	if err != nil {
		s.log.Error("audit anchor: reading checkpoints", "err", err)
		return
	}
	for _, cp := range cps {
		if cp.Seq <= a.enqueued {
			continue
		}
		from := a.enqueued + 1
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		enc := json.NewEncoder(zw)
		for next := from; next <= cp.Seq; {
			es, err := s.st.AuditRange(ctx, next, 1000)
			if err != nil {
				s.log.Error("audit anchor: reading entries", "err", err)
				return
			}
			if len(es) == 0 {
				break // pruned: the checkpoint still anchors
			}
			for _, e := range es {
				if e.Seq > cp.Seq {
					break
				}
				_ = enc.Encode(entryJSON{e.V, e.Seq, e.Time.UnixMicro(), e.Kind, e.Actor, e.Roles, e.Action, e.Target,
					e.Before, e.After, e.Status, e.SourceIP, e.RequestID, hex.EncodeToString(e.PrevHash), hex.EncodeToString(e.Hash)})
			}
			next = es[len(es)-1].Seq + 1
		}
		_ = zw.Close()
		opts := export.PutOptions{ContentType: "application/x-ndjson", ContentEncoding: "gzip", Retain: true}
		if !a.q.Enqueue(entriesKey(from, cp.Seq), buf.Bytes(), opts) {
			return // dropped, counted and alerted; retried at the next checkpoint
		}
		a.times[cp.Seq] = cp.Time
		if !a.q.Enqueue(CheckpointKey(cp.Seq), audit.MarshalCheckpoint(cp), export.PutOptions{ContentType: "application/json", Retain: true}) {
			delete(a.times, cp.Seq)
			return
		}
		a.enqueued = cp.Seq
	}
}

// anchorAge is how old the newest exported checkpoint is (0: none yet).
func (s *Server) anchorAge() time.Duration {
	if s.anchor == nil {
		return 0
	}
	us := s.anchor.written.Load()
	if us == 0 {
		return 0
	}
	return s.now().Sub(time.UnixMicro(us))
}

// checkAnchorLag alerts once per episode when the newest exported
// checkpoint is older than anchor_max_lag.
func (s *Server) checkAnchorLag(ctx context.Context) {
	if s.anchor == nil {
		return
	}
	cps, err := s.st.AuditCheckpoints(ctx)
	if err != nil || len(cps) == 0 {
		return
	}
	newest := cps[len(cps)-1]
	// The oldest database checkpoint not yet exported, and how long it has
	// waited.
	var pending *store.AuditCheckpoint
	for i := range cps {
		if cps[i].Seq > s.anchor.seq.Load() {
			pending = &cps[i]
			break
		}
	}
	if pending != nil && s.now().Sub(pending.Time) > s.cfg.Audit.AnchorMaxLag {
		lag := s.now().Sub(pending.Time)
		if s.anchor.lagged.CompareAndSwap(false, true) {
			s.met.auditFailures.add(1, "anchor_lag")
			s.raise(ctx, "audit_anchor_lag", map[string]any{"export": s.anchor.q.Sink().Name(), "lag_seconds": int64(lag.Seconds()),
				"newest_checkpoint": newest.Seq})
		}
		return
	}
	s.anchor.lagged.Store(false)
}

// raise logs an operational alert and sends it to the alert channels.
func (s *Server) raise(ctx context.Context, typ string, fields map[string]any) {
	args := []any{"alert", typ}
	for k, v := range fields {
		args = append(args, k, v)
	}
	s.log.Error("alert: "+typ, args...)
	if s.alerts != nil {
		al := s.alerts
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			al.event(s.alertCtx, typ, fields)
		}()
	}
}

// exportQueues opens the configured exports (credentials resolved by the
// caller) and wires their drop and failure alerts.
func (s *Server) openExports(ctx context.Context) error {
	s.exports = map[string]*export.Queue{}
	for _, e := range s.cfg.Exports {
		sink, err := export.Open(ctx, e, s.secrets.ExportCredentials[e.Name], s.now)
		if err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
		q := export.NewQueue(sink, e.QueueSize)
		name := e.Name
		q.OnDrop = func(key string) {
			s.raise(context.Background(), "export_dropped", map[string]any{"export": name, "key": key})
		}
		q.OnFail = func(key string, err error) {
			s.raise(context.Background(), "export_failed", map[string]any{"export": name, "key": key, "error": err.Error()})
		}
		s.exports[name] = q
	}
	if a := s.cfg.Audit.Anchor; a != "" {
		return s.startAnchor(ctx, s.exports[a])
	}
	return nil
}

// NewestAnchor reads the newest checkpoint an export holds.
func NewestAnchor(ctx context.Context, sink export.Sink) (store.AuditCheckpoint, error) {
	keys, err := sink.List(ctx, anchorCheckpoints)
	if err != nil {
		return store.AuditCheckpoint{}, err
	}
	if len(keys) == 0 {
		return store.AuditCheckpoint{}, fmt.Errorf("export %s holds no audit checkpoint", sink.Name())
	}
	b, err := sink.Get(ctx, keys[len(keys)-1])
	if err != nil {
		return store.AuditCheckpoint{}, err
	}
	return audit.ParseCheckpoint(b)
}

// writeExportMetrics writes the export counters and the anchor's age.
func (s *Server) writeExportMetrics(w io.Writer) {
	if len(s.exports) > 0 {
		names := make([]string, 0, len(s.exports))
		for n := range s.exports {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, m := range []struct {
			name, help string
			get        func(*export.Queue) int64
		}{
			{"llmflow_export_written_total", "Objects written to an export.", func(q *export.Queue) int64 { return q.Written.Load() }},
			{"llmflow_export_dropped_total", "Objects dropped because an export's queue was full.", func(q *export.Queue) int64 { return q.Dropped.Load() }},
			{"llmflow_export_failed_total", "Objects an export still could not write after every retry.", func(q *export.Queue) int64 { return q.Failed.Load() }},
		} {
			writeHeader(w, m.name, m.help, "counter")
			for _, n := range names {
				writeSample(w, m.name, []string{"export"}, []string{n}, float64(m.get(s.exports[n])))
			}
		}
	}
	if s.anchor != nil {
		writeHeader(w, "llmflow_audit_anchor_age_seconds", "Age of the newest audit checkpoint written to the anchor export.", "gauge")
		writeSample(w, "llmflow_audit_anchor_age_seconds", nil, nil, s.anchorAge().Seconds())
	}
}

// AuditEntryJSON decodes one exported entry back (for verifying an export
// offline and for tests).
func AuditEntryJSON(b []byte) (store.AuditEntry, error) {
	var j entryJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return store.AuditEntry{}, err
	}
	ph, err := hex.DecodeString(j.PrevHash)
	if err != nil {
		return store.AuditEntry{}, err
	}
	h, err := hex.DecodeString(j.Hash)
	if err != nil {
		return store.AuditEntry{}, err
	}
	return store.AuditEntry{V: j.V, Seq: j.Seq, Time: time.UnixMicro(j.TimeMicro).UTC(), Kind: j.Kind, Actor: j.Actor, Roles: j.Roles,
		Action: j.Action, Target: j.Target, Before: j.Before, After: j.After, Status: j.Status, SourceIP: j.SourceIP,
		RequestID: j.RequestID, PrevHash: ph, Hash: h}, nil
}
