package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/secret"
	"github.com/go-llmflow/gateway/store"
)

// fakeWriter records delivered secrets and can be made to fail.
type fakeWriter struct {
	mu     sync.Mutex
	got    map[string]string
	fail   bool
	writes int
}

func (f *fakeWriter) Write(_ context.Context, ref secret.Ref, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.fail {
		return errors.New("vault answered 403 to the write")
	}
	f.got[ref.String()] = v
	return nil
}

func (f *fakeWriter) value(ref string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[ref]
}

const target = "vault:secret/llmflow/keys/etabA/ci#key"

func TestScheduledRotation(t *testing.T) {
	up := newFakeVLLM(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var e *env
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		// Jumping the clock makes the budget state stale (fail-closed).
		if err := e.srv.budget.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig(up.URL())
	cfg.KeyRotation = &config.KeyRotation{VaultPrefix: "secret/llmflow/keys", CheckInterval: time.Hour}
	w := &fakeWriter{got: map[string]string{}}
	e = newEnv(t, cfg, WithClock(clock), WithSecretWriter(w))
	old := e.createKey(`{"team":"etabA"}`)
	var ks []struct{ ID string }
	_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", master, ""))), &ks)
	id := ks[0].ID
	path := "/admin/keys/" + id + "/rotation"

	for body, why := range map[string]string{
		`{"every":"720h","grace_period":"24h","target":"vault:secret/llmflow/keys/etabB/ci#key"}`: "another team's path",
		`{"every":"720h","grace_period":"24h","target":"vault:secret/elsewhere/etabA/ci#key"}`:    "outside the prefix",
		`{"every":"720h","grace_period":"24h","target":"/etc/llmflow/key"}`:                       "a file",
		`{"every":"720h","grace_period":"24h","target":"` + target + `?version=2"}`:               "a version",
		`{"every":"720h","target":"` + target + `"}`:                                              "no grace period",
		`{"every":"10m","grace_period":"1m","target":"` + target + `"}`:                           "too frequent",
		`{"every":"2h","grace_period":"3h","target":"` + target + `"}`:                            "grace longer than every",
		`{"every":"forever"}`: "not a duration",
	} {
		expect(t, why, e.do("PUT", path, master, body).StatusCode, 400)
	}
	tok := e.adminToken("tom", `{"role":"member","team":"etabA"}`)
	expect(t, "member schedules a team key", e.do("PUT", path, tok, `{"every":"720h","grace_period":"24h","target":"`+target+`"}`).StatusCode, 403)
	expect(t, "schedule", e.do("PUT", path, master, `{"every":"720h","grace_period":"24h","target":"`+target+`"}`).StatusCode, 200)

	ctx := context.Background()
	if n := e.srv.rotateDue(ctx); n != 0 {
		t.Fatalf("rotated %d keys before they were due", n)
	}
	advance(721 * time.Hour)

	// Delivery fails: the rotation is undone, the old secret keeps working
	// and the key stays due.
	w.fail = true
	if n := e.srv.rotateDue(ctx); n != 0 || w.writes != 1 {
		t.Fatalf("failed delivery: rotated %d, writes %d", n, w.writes)
	}
	advance(25 * time.Hour)
	expect(t, "old key after a failed delivery", e.chatWith(old, chat), 200)

	w.fail = false
	if n := e.srv.rotateDue(ctx); n != 1 {
		t.Fatalf("rotated %d, want 1", n)
	}
	fresh := w.value(target)
	if fresh == "" || fresh == old {
		t.Fatalf("delivered %q", fresh)
	}
	expect(t, "delivered key", e.chatWith(fresh, chat), 200)
	expect(t, "old key in the grace period", e.chatWith(old, chat), 200)
	advance(24*time.Hour + time.Second)
	expect(t, "old key after the grace period", e.chatWith(old, chat), 401)
	if n := e.srv.rotateDue(ctx); n != 0 {
		t.Fatalf("rotated %d keys right after a rotation", n)
	}

	// Two replicas on one store: a due key is rotated once.
	srv2, err := New(cfg, e.st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey}, WithClock(clock), WithSecretWriter(w))
	if err != nil {
		t.Fatal(err)
	}
	advance(721 * time.Hour)
	before := w.writes
	var wg sync.WaitGroup
	var n1, n2 int
	wg.Add(2)
	go func() { defer wg.Done(); n1 = e.srv.rotateDue(ctx) }()
	go func() { defer wg.Done(); n2 = srv2.rotateDue(ctx) }()
	wg.Wait()
	if n1+n2 != 1 || w.writes-before != 1 {
		t.Fatalf("two replicas rotated %d+%d times with %d writes, want once", n1, n2, w.writes-before)
	}
	expect(t, "key delivered by either replica", e.chatWith(w.value(target), chat), 200)

	// Clearing the schedule.
	expect(t, "clear", e.do("PUT", path, master, `{"every":"0s"}`).StatusCode, 200)
	advance(10000 * time.Hour)
	if n := e.srv.rotateDue(ctx); n != 0 {
		t.Fatalf("rotated %d keys without a schedule", n)
	}
	k, _ := e.st.GetKey(ctx, id)
	if k.Rotation != (store.Rotation{}) {
		t.Fatalf("schedule after clearing: %+v", k.Rotation)
	}
}

// Without a key_rotation block, schedules are refused.
func TestScheduledRotationNotConfigured(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.createKey(`{"team":"etabA"}`)
	var ks []struct{ ID string }
	_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", master, ""))), &ks)
	expect(t, "schedule", e.do("PUT", "/admin/keys/"+ks[0].ID+"/rotation", master, `{"every":"720h","grace_period":"24h","target":"`+target+`"}`).StatusCode, 400)
}
