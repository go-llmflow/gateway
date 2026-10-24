package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/store"
)

func auditEntries(t *testing.T, st *store.Memory) []store.AuditEntry {
	t.Helper()
	es, err := st.AuditRange(context.Background(), 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestAdminActionsAreAudited(t *testing.T) {
	up := newFakeVLLM(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cfg := testConfig(up.URL())
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey,
		AuditKey: base64.StdEncoding.EncodeToString(priv.Seed())})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, st: st, cfg: cfg}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { e.gw.Close(); srv.Close(context.Background()) })

	// A budget set, changed; a refused write; reads are not audited.
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":1}`)
	resp := e.do("PUT", "/admin/budgets", master, `{"kind":"team","name":"etabA","period":"day","max_cost":2}`, "X-Request-Id", "req-42")
	readAll(t, resp)
	if resp.Header.Get("X-Request-Id") != "req-42" {
		t.Fatalf("request id not echoed: %q", resp.Header.Get("X-Request-Id"))
	}
	tok := e.adminToken("tom", `{"role":"member","team":"etabA"}`)
	expect(t, "member sets a budget", e.do("PUT", "/admin/budgets", tok, `{"kind":"team","name":"etabA","period":"day","max_cost":99}`).StatusCode, 403)
	readAll(t, e.do("GET", "/admin/budgets", master, ""))
	id := ""
	{
		resp := e.do("POST", "/admin/keys", master, `{"team":"etabA","name":"ci"}`)
		var k struct{ ID, Key string }
		_ = json.Unmarshal([]byte(readAll(t, resp)), &k)
		id = k.ID
		es := auditEntries(t, st)
		if last := es[len(es)-1]; strings.Contains(last.After+last.Before, k.Key) {
			t.Fatal("a key secret reached the audit log")
		}
	}
	expect(t, "revoke", e.do("DELETE", "/admin/keys/"+id, master, "").StatusCode, 204)

	es := auditEntries(t, st)
	var changed, refused, revoked *store.AuditEntry
	for i := range es {
		x := &es[i]
		if x.Action == "GET /admin/budgets" {
			t.Fatal("a read was audited")
		}
		switch {
		case x.RequestID == "req-42":
			changed = x
		case x.Actor == "tom" && x.Action == "PUT /admin/budgets":
			refused = x
		case x.Action == "DELETE /admin/keys/{id}":
			revoked = x
		}
	}
	if changed == nil || refused == nil || revoked == nil {
		t.Fatalf("missing entries: %+v", es)
	}
	if changed.Actor != "master" || !strings.Contains(changed.Roles, `"master":true`) || changed.Status != 200 ||
		!strings.Contains(changed.Before, `"MaxCost":1`) && !strings.Contains(changed.Before, `"max_cost":1`) ||
		!strings.Contains(changed.After, `"max_cost":2`) || changed.SourceIP != "127.0.0.1" || changed.Kind != "admin" {
		t.Fatalf("budget change: %+v", changed)
	}
	if refused.Status != 403 || !strings.Contains(refused.Roles, `"role":"member"`) {
		t.Fatalf("refused write: %+v", refused)
	}
	if !strings.Contains(revoked.Before, `"revoked":false`) || !strings.Contains(revoked.After, `"revoked":true`) || revoked.Target != "/admin/keys/"+id {
		t.Fatalf("revocation before/after: %+v", revoked)
	}

	// Checkpoints sign what was written; a tampered row is caught.
	if _, err := srv.auditLog.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, err := audit.Verify(context.Background(), st, []ed25519.PublicKey{pub})
	if err != nil || !r.OK() || r.Checkpoints != 1 || r.Unsigned != 0 {
		t.Fatalf("verify: %+v %v", r, err)
	}
	st.TamperAudit(changed.Seq, func(x *store.AuditEntry) { x.After = strings.Replace(x.After, `2`, `1`, 1) })
	if r, _ := audit.Verify(context.Background(), st, []ed25519.PublicKey{pub}); r.OK() {
		t.Fatal("a tampered entry was not caught")
	}
}

// Events the gateway raises itself (here a key rotation) are in the log.
func TestSystemEventsAreAudited(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.createKey(`{"team":"etabA"}`)
	var ks []struct{ ID string }
	_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", master, ""))), &ks)
	expect(t, "rotate", e.do("POST", "/admin/keys/"+ks[0].ID+"/rotate", master, "").StatusCode, 200)
	found := false
	for _, x := range auditEntries(t, e.st) {
		if x.Action == "key.rotated" && x.Actor == "master" && x.Target == ks[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("key.rotated not in the audit log")
	}
	if _, err := New(e.cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(e.cfg), CacheSaltKey: testSaltKey, AuditKey: "short"}); err == nil {
		t.Fatal("a bad audit key accepted")
	}
}
