package audit

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// An attacker who can write the database and holds the signing key, but cannot
// write the anchor store, appends entries after the newest anchor and signs
// them: VerifyAnchored (which checks up to the anchor) passes, the tail lock
// does not.
func TestTailLockCatchesHistoryAddedAfterTheAnchor(t *testing.T) {
	ctx := context.Background()
	l, st, pub := newLog(t)
	appendN(t, l, 3)
	l.Checkpoint(ctx)
	cps, _ := st.AuditCheckpoints(ctx)
	anchor := cps[len(cps)-1]
	keys := []ed25519.PublicKey{pub}
	// The log's clock: one second per call, from 2026-10-24 08:00.
	lastTime := func() time.Time {
		es, _ := st.AuditRange(ctx, 1, 1000)
		return es[len(es)-1].Time
	}
	tl := TailLock{Kinds: []string{"admin", "auth", "scim", "system"}, Now: lastTime().Add(time.Hour), Grace: 5 * time.Minute}

	// Nothing after the anchor: fine.
	if r, err := VerifyAnchoredLocked(ctx, st, keys, anchor, tl); err != nil || !r.OK() {
		t.Fatalf("intact: %v %v", err, r.Problems)
	}

	// A guardrail entry after the anchor is the window the lock does not close.
	if _, err := l.Append(ctx, store.AuditEntry{Kind: "guardrail", Actor: "gateway", Roles: "[]", Before: "null", After: "{}"}); err != nil {
		t.Fatal(err)
	}
	tl.Now = lastTime().Add(time.Hour)
	if r, _ := VerifyAnchoredLocked(ctx, st, keys, anchor, tl); !r.OK() {
		t.Fatalf("an unlocked kind after the anchor is tolerated: %v", r.Problems)
	}

	// A locked-kind entry, forged and signed, past the anchor and older than the grace.
	if _, err := l.Append(ctx, store.AuditEntry{Kind: "admin", Actor: "mallory", Roles: "[]", Before: "null", After: "{}", Status: 200}); err != nil {
		t.Fatal(err)
	}
	l.Checkpoint(ctx)
	if r := verify(t, st, pub); !r.OK() {
		t.Fatalf("the forged tail is internally consistent: %v", r.Problems)
	}
	if r, _ := VerifyAnchored(ctx, st, keys, anchor); !r.OK() {
		t.Fatalf("VerifyAnchored alone passes (the documented limit): %v", r.Problems)
	}
	tl.Now = lastTime().Add(time.Hour)
	r, err := VerifyAnchoredLocked(ctx, st, keys, anchor, tl)
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, r, "past the newest anchor")

	// Within the grace period the same entry is merely in flight.
	tl.Now = lastTime().Add(time.Minute)
	if r, _ := VerifyAnchoredLocked(ctx, st, keys, anchor, tl); !r.OK() {
		t.Fatalf("an entry younger than the grace period: %v", r.Problems)
	}

	// An empty lock is VerifyAnchored.
	tl.Kinds = nil
	tl.Now = lastTime().Add(time.Hour)
	if r, _ := VerifyAnchoredLocked(ctx, st, keys, anchor, tl); !r.OK() {
		t.Fatalf("no kinds: %v", r.Problems)
	}
}
