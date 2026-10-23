package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestKeyRotation(t *testing.T) {
	up := newFakeVLLM(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	logs := &syncBuffer{}
	e := newEnv(t, testConfig(up.URL()), WithClock(clock), WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	old := e.createKey(`{"team":"etabA"}`)
	var ks []struct{ ID string }
	_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", master, ""))), &ks)
	id := ks[0].ID
	expect(t, "old key before rotation", e.chatWith(old, chat), 200)

	rotate := func(body string) (int, string, string) {
		resp := e.do("POST", "/admin/keys/"+id+"/rotate", master, body)
		out := readAll(t, resp)
		var r struct {
			ID, Key string
			Prev    string `json:"previous_expires_at"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		if resp.StatusCode == 200 && r.ID != id {
			t.Fatalf("rotated key id %q, want %q", r.ID, id)
		}
		return resp.StatusCode, r.Key, r.Prev
	}

	expect(t, "bad grace", func() int { c, _, _ := rotate(`{"grace_period":"forever"}`); return c }(), 400)
	expect(t, "grace too long", func() int { c, _, _ := rotate(`{"grace_period":"1000h"}`); return c }(), 400)

	code, fresh, prev := rotate(`{"grace_period":"1h"}`)
	if code != 200 || !strings.HasPrefix(fresh, "llmf-") || fresh == old || prev == "" {
		t.Fatalf("rotate: %d %q %q", code, fresh, prev)
	}
	expect(t, "new key", e.chatWith(fresh, chat), 200)
	expect(t, "old key inside the grace period", e.chatWith(old, chat), 200)
	mu.Lock()
	now = now.Add(time.Hour + time.Second)
	mu.Unlock()
	expect(t, "old key after the grace period", e.chatWith(old, chat), 401)
	expect(t, "new key after the grace period", e.chatWith(fresh, chat), 200)

	// Without a grace period the replaced secret stops at once.
	code, newer, _ := rotate("")
	expect(t, "rotate without body", code, 200)
	expect(t, "replaced key", e.chatWith(fresh, chat), 401)
	expect(t, "newest key", e.chatWith(newer, chat), 200)

	// Who may rotate: the team's admins and the key's owner, not others.
	tok := e.adminToken("tom", `{"role":"member","team":"etabA"}`)
	expect(t, "member rotates a team key", e.do("POST", "/admin/keys/"+id+"/rotate", tok, "").StatusCode, 403)
	expect(t, "unknown key", e.do("POST", "/admin/keys/k_none/rotate", master, "").StatusCode, 404)
	expect(t, "revoke", e.do("DELETE", "/admin/keys/"+id, master, "").StatusCode, 204)
	expect(t, "rotate a revoked key", e.do("POST", "/admin/keys/"+id+"/rotate", master, "").StatusCode, 404)

	// Audited, and the secret never reaches the log.
	l := logs.String()
	if !strings.Contains(l, `"event":"key.rotated"`) || !strings.Contains(l, `"audit":true`) {
		t.Fatalf("no audit event in the log:\n%s", l)
	}
	for _, s := range []string{old, fresh, newer} {
		if strings.Contains(l, s) {
			t.Fatal("a key secret reached the log")
		}
	}
}
