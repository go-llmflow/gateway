package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/export"
	"github.com/go-llmflow/gateway/store"
)

// flakySink is a file export that refuses writes while down is set.
type flakySink struct {
	export.Sink
	down atomic.Bool
}

func (f *flakySink) Put(ctx context.Context, key string, body []byte, o export.PutOptions) error {
	if f.down.Load() {
		return errors.New("anchor store unreachable")
	}
	return f.Sink.Put(ctx, key, body, o)
}

// TestAnchorSyncClosesTheTail: with anchor_sync an administrative entry has its
// checkpoint on the anchor before the next thing happens (no checkpoint interval
// to wait for), and with "refuse" a store that does not take it stops
// administrative writes until it does again.
func TestAnchorSyncClosesTheTail(t *testing.T) {
	ctx := context.Background()
	up := newFakeVLLM(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := testConfig(up.URL())
	cfg.Audit = config.Audit{CheckpointInterval: time.Hour, AnchorMaxLag: time.Minute, AnchorSync: "refuse", AnchorSyncTimeout: 300 * time.Millisecond}
	logs := &syncBuffer{}
	st := store.NewMemory()
	secrets := Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, AuditKey: base64.StdEncoding.EncodeToString(priv.Seed())}
	srv, err := New(cfg, st, secrets, WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	inner, err := export.Open(ctx, config.Export{Name: "anchor", Kind: "file", Directory: t.TempDir(), QueueSize: 16}, export.Credentials{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := &flakySink{Sink: inner}
	if err := srv.startAnchor(ctx, export.NewQueue(sink, 16)); err != nil {
		t.Fatal(err)
	}
	e := newEnvFor(t, srv, st, cfg)

	budget := func(n int) int {
		resp := e.do("PUT", "/admin/budgets", master, `{"kind":"team","name":"etabA","period":"day","max_cost":`+string(rune('0'+n))+`}`)
		readAll(t, resp)
		return resp.StatusCode
	}

	// A healthy anchor: the checkpoint is there as soon as the write returns.
	if c := budget(1); c != 200 {
		t.Fatalf("first write: %d", c)
	}
	a, err := NewestAnchor(ctx, sink)
	if err != nil || a.Seq != 1 {
		t.Fatalf("anchor right after the write: %+v %v", a, err)
	}
	keys := []ed25519.PublicKey{pub}
	tl := TailLockOf(cfg.Audit, time.Now().Add(time.Hour))
	if len(tl.Kinds) == 0 {
		t.Fatal("no tail lock with anchor_sync")
	}
	if r, _ := audit.VerifyAnchoredLocked(ctx, st, keys, a, tl); !r.OK() {
		t.Fatalf("verify with the lock: %v", r.Problems)
	}

	// The anchor store goes down: the write that finds out is answered (it was
	// done), an alert is raised, and the next administrative write is refused.
	sink.down.Store(true)
	if c := budget(2); c != 200 {
		t.Fatalf("the write that found the anchor down: %d", c)
	}
	if !srv.anchor.broken.Load() || !strings.Contains(logs.String(), "audit_anchor_sync_failed") {
		t.Fatalf("not refusing, or no alert:\n%s", logs.String())
	}
	resp := e.do("PUT", "/admin/budgets", master, `{"kind":"team","name":"etabA","period":"day","max_cost":3}`)
	if body := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(body, "audit_anchor_unreachable") {
		t.Fatalf("a write while the anchor is down: %d %s", resp.StatusCode, body)
	}
	// Reads are not refused.
	if resp := e.do("GET", "/admin/budgets", master, ""); resp.StatusCode != 200 {
		t.Fatalf("a read while the anchor is down: %d", resp.StatusCode)
	}

	// It comes back: the queue's retry writes what was missed, and the gateway
	// takes writes again.
	sink.down.Store(false)
	deadline := time.Now().Add(20 * time.Second)
	for srv.anchor.broken.Load() {
		if time.Now().After(deadline) {
			t.Fatal("still refusing 20 s after the anchor came back")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if c := budget(4); c != 200 {
		t.Fatalf("a write after the anchor recovered: %d", c)
	}
	if a, _ := NewestAnchor(ctx, sink); a.Seq < 3 {
		t.Fatalf("anchor at %d after recovery", a.Seq)
	}
}
