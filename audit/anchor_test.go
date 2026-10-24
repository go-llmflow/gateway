package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// snapshot copies a memory store's audit state through the Store API.
func copyLog(t *testing.T, from *store.Memory) *store.Memory {
	t.Helper()
	ctx := context.Background()
	to := store.NewMemory()
	es, _ := from.AuditRange(ctx, 1, 1<<20)
	for _, e := range es {
		e := e
		if _, err := to.AuditAppend(ctx, e, func(x *store.AuditEntry) []byte { return e.Hash }); err != nil {
			t.Fatal(err)
		}
	}
	cps, _ := from.AuditCheckpoints(ctx)
	for _, c := range cps {
		if err := to.AuditAddCheckpoint(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	return to
}

// The three things only an external anchor catches, as the review of #29
// listed them: each passes Verify (the database is internally consistent)
// and fails VerifyAnchored.
func TestAnchorCatchesTruncationAndRollback(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	appendN(t, l, 3)
	l.Checkpoint(ctx)
	old := copyLog(t, st) // a backup taken now
	appendN(t, l, 3)
	l.Checkpoint(ctx)
	cps, _ := st.AuditCheckpoints(ctx)
	anchor, err := ParseCheckpoint(MarshalCheckpoint(cps[len(cps)-1])) // exported, then read back
	if err != nil {
		t.Fatal(err)
	}
	keys := []ed25519.PublicKey{pub}
	if r, _ := VerifyAnchored(ctx, st, keys, anchor); !r.OK() {
		t.Fatalf("the intact log: %v", r.Problems)
	}

	// (a) the tail deleted with the checkpoints covering it
	truncated := copyLog(t, old)
	// (b) an older snapshot restored
	restored := old
	// (c) everything deleted
	empty := store.NewMemory()
	for name, s := range map[string]*store.Memory{"tail and its checkpoints deleted": truncated, "older snapshot restored": restored, "everything deleted": empty} {
		if r := verify(t, s, pub); !r.OK() {
			t.Fatalf("%s: Verify alone should pass (documented limit), got %v", name, r.Problems)
		}
		r, err := VerifyAnchored(ctx, s, keys, anchor)
		if err != nil {
			t.Fatal(err)
		}
		if r.OK() {
			t.Errorf("%s: not caught with the anchor", name)
		}
	}

	// A chain rewritten and re-signed with the key (a compromised host):
	// the anchored entry no longer has the anchored hash.
	_, priv2 := pub, l.key
	rewritten := store.NewMemory()
	l2 := New(rewritten, priv2, nil)
	for i := 0; i < 6; i++ {
		l2.Append(ctx, store.AuditEntry{Kind: "admin", Actor: "mallory", Roles: "[]", Before: "null", After: "{}", Status: 200})
	}
	l2.Checkpoint(ctx)
	if r := verify(t, rewritten, pub); !r.OK() {
		t.Fatalf("re-signed log: %v", r.Problems)
	}
	r, _ := VerifyAnchored(ctx, rewritten, keys, anchor)
	wantProblem(t, r, "another hash")

	// A forged anchor is refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged := anchor
	forged.Signature = ed25519.Sign(other, CheckpointMessage(anchor.Seq, anchor.Hash, anchor.Time))
	r, _ = VerifyAnchored(ctx, st, keys, forged)
	wantProblem(t, r, "signature does not verify")

	// Pruned past the anchor: the checkpoint left by the prune carries it.
	if _, err := l.Prune(ctx, anchor.Seq); err != nil {
		t.Fatal(err)
	}
	if r, _ := VerifyAnchored(ctx, st, keys, anchor); !r.OK() {
		t.Fatalf("pruned at the anchor: %v", r.Problems)
	}
}

func TestParseCheckpoint(t *testing.T) {
	c := store.AuditCheckpoint{Seq: 7, Hash: make([]byte, 32), Time: time.Date(2026, 10, 24, 8, 0, 0, 123000, time.UTC), KeyID: "k", Signature: make([]byte, 64)}
	got, err := ParseCheckpoint(MarshalCheckpoint(c))
	if err != nil || got.Seq != 7 || !got.Time.Equal(c.Time) || len(got.Signature) != 64 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, s := range []string{`{`, `{"format":"x"}`, `{"format":"llmflow-audit-checkpoint/v1","seq":1,"hash":"zz"}`,
		`{"format":"llmflow-audit-checkpoint/v1","seq":0,"hash":"` + strings.Repeat("00", 32) + `","signature":"` + strings.Repeat("A", 88) + `"}`,
		`{"format":"llmflow-audit-checkpoint/v1","seq":1,"hash":"` + strings.Repeat("00", 32) + `","signature":"AAAA"}`,
		strings.Repeat(" ", 70<<10)} {
		if _, err := ParseCheckpoint([]byte(s)); err == nil {
			t.Errorf("%.40q accepted", s)
		}
	}
}

// FuzzParseCheckpoint: an anchor file is input from outside.
func FuzzParseCheckpoint(f *testing.F) {
	f.Add(string(MarshalCheckpoint(store.AuditCheckpoint{Seq: 1, Hash: make([]byte, 32), Time: time.Unix(0, 0), KeyID: "k", Signature: make([]byte, 64)})))
	f.Add(`{"format":"llmflow-audit-checkpoint/v1","seq":-1}`)
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseCheckpoint([]byte(s))
		if err == nil && (c.Seq < 1 || len(c.Hash) != 32 || len(c.Signature) != 64) {
			t.Fatalf("%q parsed as %+v", s, c)
		}
	})
}

// Version 2 hashes raw bytes: fields that differ only in invalid UTF-8
// hash differently (version 1 folded both to U+FFFD). Version 1 rows still
// verify.
func TestCanonicalV2(t *testing.T) {
	a := store.AuditEntry{V: 2, Seq: 1, PrevHash: make([]byte, 32), After: "x\xff"}
	b := a
	b.After = "x\xfe"
	if equal(Hash(&a), Hash(&b)) {
		t.Fatal("v2: invalid bytes collide")
	}
	a.V, b.V = 1, 1
	if !equal(Hash(&a), Hash(&b)) {
		t.Fatal("v1 is expected to collide here (that is the defect v2 fixes)")
	}
	// Field boundaries: moving a byte from one field to the next changes
	// the hash.
	c := store.AuditEntry{V: 2, Seq: 1, PrevHash: make([]byte, 32), Actor: "ab", Roles: "c"}
	d := c
	d.Actor, d.Roles = "a", "bc"
	if equal(Hash(&c), Hash(&d)) {
		t.Fatal("v2: field boundaries are ambiguous")
	}
	// A log with a v1 entry followed by v2 entries verifies.
	ctx := context.Background()
	l, st, pub := newLog(t)
	if _, err := st.AuditAppend(ctx, store.AuditEntry{V: 1, Time: time.Now(), Kind: "admin", Roles: "[]", Before: "null", After: "{}"}, Hash); err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 2)
	es, _ := st.AuditRange(ctx, 1, 10)
	if es[0].V != 1 || es[1].V != CurrentVersion {
		t.Fatalf("versions %d %d", es[0].V, es[1].V)
	}
	if r := verify(t, st, pub); !r.OK() {
		t.Fatalf("%v", r.Problems)
	}
}
