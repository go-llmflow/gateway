package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func newLog(t *testing.T) (*Log, *store.Memory, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	return New(st, priv, func() time.Time { now = now.Add(time.Second); return now }), st, pub
}

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(context.Background(), store.AuditEntry{Kind: "admin", Actor: "alice", Roles: `[]`,
			Action: "PUT /admin/budgets", Target: fmt.Sprintf("team/etab%d", i), Before: `null`, After: `{"max_cost":1}`,
			Status: 200, SourceIP: "192.0.2.1", RequestID: fmt.Sprintf("r%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
}

func verify(t *testing.T, st Store, pub ed25519.PublicKey) Report {
	t.Helper()
	r, err := Verify(context.Background(), st, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wantProblem(t *testing.T, r Report, substr string) {
	t.Helper()
	for _, p := range r.Problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("no problem %q in %v", substr, r.Problems)
}

func TestChainAndCheckpoints(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	if r := verify(t, st, pub); !r.OK() || r.Entries != 0 {
		t.Fatalf("empty log: %+v", r)
	}
	appendN(t, l, 5)
	if seq, err := l.Checkpoint(ctx); err != nil || seq != 5 {
		t.Fatalf("checkpoint: %d %v", seq, err)
	}
	if seq, _ := l.Checkpoint(ctx); seq != 0 {
		t.Fatalf("a second checkpoint with nothing new: %d", seq)
	}
	appendN(t, l, 3)
	r := verify(t, st, pub)
	if !r.OK() || r.Entries != 8 || r.First != 1 || r.Last != 8 || r.Checkpoints != 1 || r.Unsigned != 3 {
		t.Fatalf("%+v", r)
	}
	es, _ := st.AuditRange(ctx, 1, 10)
	if es[0].Seq != 1 || !isZero(es[0].PrevHash) || !equal(es[1].PrevHash, es[0].Hash) {
		t.Fatal("not chained")
	}
}

// Every way of altering the stored log is caught.
func TestTamperingIsCaught(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		tamper func(st *store.Memory)
		want   string
	}{
		"one field changed": {func(st *store.Memory) {
			st.TamperAudit(3, func(e *store.AuditEntry) { e.After = `{"max_cost":1000}` })
		}, "entry 3: content does not match"},
		"one field changed and rehashed": {func(st *store.Memory) {
			st.TamperAudit(3, func(e *store.AuditEntry) { e.Actor = "mallory"; e.Hash = Hash(e) })
		}, "entry 4: does not chain from entry 3"},
		"the whole tail rehashed": {func(st *store.Memory) {
			// The attacker rewrites entry 3 and recomputes every hash after
			// it: the chain holds, the signed checkpoint does not.
			var prev []byte
			for seq := int64(3); seq <= 8; seq++ {
				st.TamperAudit(seq, func(e *store.AuditEntry) {
					if seq == 3 {
						e.Actor = "mallory"
					} else {
						e.PrevHash = prev
					}
					e.Hash = Hash(e)
					prev = e.Hash
				})
			}
		}, "checkpoint 5: signs a hash the entry does not have"},
		"an entry deleted":          {func(st *store.Memory) { st.DeleteAudit(4) }, "entries 4 to 4 are missing"},
		"the first entries deleted": {func(st *store.Memory) { st.DeleteAudit(1); st.DeleteAudit(2) }, "no valid checkpoint at 2 anchors it"},
		"the tail deleted past a checkpoint": {func(st *store.Memory) {
			for seq := int64(4); seq <= 8; seq++ {
				st.DeleteAudit(seq)
			}
		}, "checkpoint 5: signs entries that are no longer in the log"},
	} {
		t.Run(name, func(t *testing.T) {
			l, st, pub := newLog(t)
			appendN(t, l, 5)
			if _, err := l.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			appendN(t, l, 3)
			tc.tamper(st)
			r := verify(t, st, pub)
			if r.OK() {
				t.Fatalf("tampering not caught: %+v", r)
			}
			wantProblem(t, r, tc.want)
		})
	}
}

func TestCheckpointSignatures(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	appendN(t, l, 2)
	l.Checkpoint(ctx)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	r, _ := Verify(ctx, st, []ed25519.PublicKey{other})
	wantProblem(t, r, "signed by unknown key")
	// A checkpoint forged without the key.
	_, forger, _ := ed25519.GenerateKey(rand.Reader)
	appendN(t, l, 1)
	es, _ := st.AuditRange(ctx, 3, 1)
	tm := time.Now()
	st.AuditAddCheckpoint(ctx, store.AuditCheckpoint{Seq: 3, Hash: es[0].Hash, Time: tm, KeyID: KeyID(pub),
		Signature: ed25519.Sign(forger, CheckpointMessage(3, es[0].Hash, tm))})
	wantProblem(t, verify(t, st, pub), "checkpoint 3: bad signature")
}

// Pruning cuts whole segments at a checkpoint, and what remains verifies.
func TestPrune(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	appendN(t, l, 4)
	l.Checkpoint(ctx)
	appendN(t, l, 4)
	l.Checkpoint(ctx)
	appendN(t, l, 2)
	if _, err := l.Prune(ctx, 3); err == nil {
		t.Fatal("pruned to an entry that is not a checkpoint")
	}
	n, err := l.Prune(ctx, 4)
	if err != nil || n != 4 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if r := verify(t, st, pub); !r.OK() || r.First != 5 || r.Entries != 6 {
		t.Fatalf("after prune: %+v", r)
	}
	if _, err := l.Prune(ctx, 8); err != nil {
		t.Fatal(err)
	}
	// Prune everything signed, then keep appending: the new entries chain
	// from the checkpoint.
	if _, err := l.Prune(ctx, 8); err != nil {
		t.Fatal(err)
	}
	st.DeleteAudit(9)
	st.DeleteAudit(10)
	if r := verify(t, st, pub); !r.OK() || r.Entries != 0 {
		t.Fatalf("empty after prune: %+v", r)
	}
	appendN(t, l, 1)
	es, _ := st.AuditRange(ctx, 1, 10)
	if len(es) != 1 || es[0].Seq != 9 {
		t.Fatalf("after an empty log: %+v", es)
	}
	if r := verify(t, st, pub); !r.OK() {
		t.Fatalf("%+v", r)
	}
}

// Retention prunes whole segments up to a checkpoint, only expired ones,
// never past the limit, and never a log that does not verify.
func TestPruneExpired(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	keys := []ed25519.PublicKey{pub}
	appendN(t, l, 3)  // admin, times t+1s..t+3s
	l.Checkpoint(ctx) // at 3
	appendN(t, l, 3)
	l.Checkpoint(ctx) // at 6
	if _, err := l.Append(ctx, store.AuditEntry{Kind: "auth", Roles: "[]", Before: "null", After: "{}"}); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 1)
	l.Checkpoint(ctx) // at 8
	base := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	admin := map[string]time.Duration{"admin": day}

	// Nothing has expired yet.
	if seq, n, err := l.PruneExpired(ctx, keys, base.Add(time.Hour), admin, 1<<62); err != nil || n != 0 || seq != 0 {
		t.Fatalf("early: %d %d %v", seq, n, err)
	}
	// Admin entries expired, but the auth entry (seq 7) has no retention:
	// pruning stops at the checkpoint before it.
	seq, n, err := l.PruneExpired(ctx, keys, base.Add(2*day), admin, 1<<62)
	if err != nil || seq != 6 || n != 6 {
		t.Fatalf("pruned through %d (%d entries), %v", seq, n, err)
	}
	if r := verify(t, st, pub); !r.OK() || r.First != 7 {
		t.Fatalf("after prune: %+v", r)
	}
	// The limit (what the anchor export holds) bounds it.
	l2, st2, pub2 := newLog(t)
	appendN(t, l2, 2)
	l2.Checkpoint(ctx)
	appendN(t, l2, 2)
	l2.Checkpoint(ctx)
	if seq, _, _ := l2.PruneExpired(ctx, []ed25519.PublicKey{pub2}, base.Add(2*day), admin, 2); seq != 2 {
		t.Fatalf("limit: pruned through %d", seq)
	}
	// A tampered log is not pruned.
	st2.TamperAudit(3, func(e *store.AuditEntry) { e.Actor = "mallory" })
	if _, _, err := l2.PruneExpired(ctx, []ed25519.PublicKey{pub2}, base.Add(2*day), admin, 1<<62); err == nil {
		t.Fatal("a tampered log was pruned")
	}
}

func TestKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, s := range []string{base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(priv), fmt.Sprintf("%x", priv.Seed())} {
		k, err := ParsePrivateKey(s + "\n")
		if err != nil || !k.Public().(ed25519.PublicKey).Equal(pub) {
			t.Fatalf("%q: %v", s, err)
		}
	}
	bad := append([]byte{}, priv...)
	bad[40] ^= 1
	for _, s := range []string{"", "!!", base64.StdEncoding.EncodeToString([]byte("short")), base64.StdEncoding.EncodeToString(bad)} {
		if _, err := ParsePrivateKey(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if k, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub)); err != nil || !k.Equal(pub) {
		t.Fatal(err)
	}
	if _, err := ParsePublicKey("abc"); err == nil {
		t.Fatal("short public key accepted")
	}
	if l := New(store.NewMemory(), nil, nil); func() error { _, err := l.Checkpoint(context.Background()); return err }() == nil {
		t.Fatal("checkpoint without a key")
	}
}
