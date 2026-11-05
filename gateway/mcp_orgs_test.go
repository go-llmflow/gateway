package gateway

import "testing"

// Regression test for the security review of 2026-11-05 (v0.27.0), finding 3:
// the gateway calls a tool server with its own token, so an administrator of any
// organisation could reach every tool server. A tool server now names the
// organisations whose teams may use it; "*" is every tool server of the
// team's organisation, no more.
func TestMCPToolServerIsScopedToItsOrgs(t *testing.T) {
	f := newFakeTools(t)
	e := newMCPEnv(t, f.srv.URL)
	e.srv.mcpServers["docs"].cfg.Orgs = []string{"etabA"} // etabB has no right to it
	ob := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")

	// Naming the server is refused; "*" is accepted but opens nothing of etabA's.
	if c := e.setTools(ob, "etabB", `{"tools":["docs.*"]}`); c != 400 {
		t.Errorf("an org admin named a tool server of another organisation: %d", c)
	}
	if c := e.setTools(ob, "etabB", `{"tools":["*"]}`); c != 200 {
		t.Fatalf("*: %d", c)
	}
	if _, m := e.rpc(keyB, "tools/list", nil); len(toolNames(t, m)) != 0 {
		t.Errorf("EXPLOIT: etabB lists a tool server it has no right to: %v", toolNames(t, m))
	}
	if _, m := e.rpc(keyB, "tools/call", map[string]any{"name": "docs.echo", "arguments": map[string]any{}}); errCode(m) == 0 {
		t.Errorf("EXPLOIT: etabB called a tool server it has no right to: %v", m)
	}
	if f.calls.Load() != 0 {
		t.Errorf("the tool server was called %d times on etabB's behalf", f.calls.Load())
	}
	// etabA, which may, is served.
	if c := e.setTools(master, "etabA", `{"tools":["*"]}`); c != 200 {
		t.Fatalf("etabA: %d", c)
	}
	if _, m := e.rpc(keyA, "tools/list", nil); len(toolNames(t, m)) == 0 {
		t.Errorf("etabA lists no tool: %v", m)
	}
	// The admin's view of the team lists only the servers its organisation may use.
	if resp := e.do("GET", "/admin/teams/etabB/tools", ob, ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Errorf("get: %d", resp.StatusCode)
	}
}
