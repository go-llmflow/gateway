package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestAuditAnchorExport(t *testing.T) {
	ctx := context.Background()
	up := newFakeVLLM(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	dir := t.TempDir()
	cfg := testConfig(up.URL())
	cfg.Exports = []config.Export{{Name: "local", Kind: "file", Directory: dir, Prefix: "gw/", QueueSize: 16}}
	cfg.Audit = config.Audit{CheckpointInterval: time.Hour, Anchor: "local", AnchorMaxLag: time.Minute}
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	logs := &syncBuffer{}
	st := store.NewMemory()
	secrets := Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, AuditKey: base64.StdEncoding.EncodeToString(priv.Seed())}
	srv, err := New(cfg, st, secrets, WithClock(clock), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e := newEnvFor(t, srv, st, cfg)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":1}`)
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":2}`)
	if _, err := srv.auditLog.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	srv.anchorUpTo(ctx)
	srv.exports["local"].Close(ctx) // wait for the writes

	sink := srv.exports["local"].Sink()
	keys, _ := sink.List(ctx, "audit/")
	if len(keys) != 2 || !strings.HasPrefix(keys[0], "audit/checkpoints/") || !strings.HasPrefix(keys[1], "audit/entries/") {
		t.Fatalf("exported %v", keys)
	}
	// The exported entries are the chain itself: they recompute.
	gz, _ := sink.Get(ctx, keys[1])
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d exported entries", len(lines))
	}
	for _, l := range lines {
		x, err := AuditEntryJSON([]byte(l))
		if err != nil || !bytes.Equal(audit.Hash(&x), x.Hash) {
			t.Fatalf("exported entry does not recompute: %v", err)
		}
	}
	anchor, err := NewestAnchor(ctx, sink)
	if err != nil || anchor.Seq != 2 {
		t.Fatalf("anchor %+v %v", anchor, err)
	}
	keysPub := []ed25519.PublicKey{pub}
	if r, _ := audit.VerifyAnchored(ctx, st, keysPub, anchor); !r.OK() {
		t.Fatalf("%v", r.Problems)
	}
	if r, _ := audit.VerifyAnchored(ctx, store.NewMemory(), keysPub, anchor); r.OK() {
		t.Fatal("an emptied database verified against the anchor")
	}
	var m strings.Builder
	srv.writeExportMetrics(&m)
	if !strings.Contains(m.String(), `llmflow_export_written_total{export="local"} 2`) || !strings.Contains(m.String(), "llmflow_audit_anchor_age_seconds") {
		t.Fatalf("metrics:\n%s", m.String())
	}

	// A new checkpoint that never reaches the export: past the lag, one
	// alert.
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":3}`)
	if _, err := srv.auditLog.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	srv.checkAnchorLag(ctx)
	srv.checkAnchorLag(ctx)
	if n := strings.Count(logs.String(), "audit_anchor_lag"); n < 1 || strings.Count(logs.String(), `"msg":"alert: audit_anchor_lag"`) != 1 {
		t.Fatalf("lag alerts: %d\n%s", n, logs.String())
	}

	// A restart catches up from what the export holds.
	srv2, err := New(cfg, st, secrets, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	srv2.Close(ctx)
	if a, _ := NewestAnchor(ctx, sink); a.Seq != 3 {
		t.Fatalf("after restart the newest anchor is %d", a.Seq)
	}
}

func newEnvFor(t *testing.T, srv *Server, st *store.Memory, cfg *config.Config) *env {
	e := &env{t: t, srv: srv, st: st, cfg: cfg}
	e.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { e.gw.Close(); srv.Close(context.Background()) })
	return e
}
