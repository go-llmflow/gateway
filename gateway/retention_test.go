package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestAuditRetention(t *testing.T) {
	ctx := context.Background()
	up := newFakeVLLM(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, anchored := range []bool{false, true} {
		cfg := testConfig(up.URL())
		cfg.Audit = config.Audit{CheckpointInterval: time.Hour, PruneInterval: time.Hour,
			Retention: map[string]time.Duration{"admin": 24 * time.Hour, "system": 24 * time.Hour}}
		if anchored {
			cfg.Exports = []config.Export{{Name: "local", Kind: "file", Directory: t.TempDir(), QueueSize: 16}}
			cfg.Audit.Anchor, cfg.Audit.AnchorMaxLag = "local", time.Hour
		}
		var mu sync.Mutex
		now := time.Now()
		clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		st := store.NewMemory()
		srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey,
			AuditKey: base64.StdEncoding.EncodeToString(priv.Seed())}, WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		e := newEnvFor(t, srv, st, cfg)
		e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":1}`)
		e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":2}`)
		srv.auditLog.Checkpoint(ctx) // at 2
		if anchored {
			// Not exported yet: nothing may be pruned.
			mu.Lock()
			now = now.Add(48 * time.Hour)
			mu.Unlock()
			srv.pruneOnce(ctx)
			if es, _ := st.AuditRange(ctx, 1, 10); len(es) == 0 || es[0].Seq != 1 {
				t.Fatalf("anchored: pruned before the export held it: %+v", es)
			}
			srv.anchorUpTo(ctx)
			srv.exports["local"].Close(ctx)
		} else {
			mu.Lock()
			now = now.Add(48 * time.Hour)
			mu.Unlock()
		}
		srv.pruneOnce(ctx)
		es, _ := st.AuditRange(ctx, 1, 10)
		if len(es) != 1 || es[0].Seq != 3 || es[0].Action != "audit.pruned" {
			t.Fatalf("anchored=%v: after prune %+v", anchored, es)
		}
		if r, _ := audit.Verify(ctx, st, []ed25519.PublicKey{pub}); !r.OK() || r.First != 3 {
			t.Fatalf("anchored=%v: %+v", anchored, r)
		}
	}
}
