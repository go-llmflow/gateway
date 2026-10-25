package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/mcp"
	"github.com/go-llmflow/gateway/store"
)

const toolToken = "tool-server-token"

// fakeTools is a modern tool server: it checks the binding's headers and
// the gateway's token, and records what it received.
type fakeTools struct {
	srv   *httptest.Server
	calls atomic.Int32
	mu    sync.Mutex
	last  http.Header
	body  map[string]any
}

func newFakeTools(t testing.TB) *fakeTools {
	f := &fakeTools{}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTools) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+toolToken {
		http.Error(w, "token", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	m, perr := mcp.ParseMessage(b)
	if perr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	meta, _ := m.RequestMeta()
	if !meta.Present || r.Header.Get(mcp.HeaderMethod) != m.Method || r.Header.Get(mcp.HeaderProtocolVersion) != mcp.Version {
		w.WriteHeader(http.StatusBadRequest)
		w.Write(mcp.ErrorResponse(m.ID, mcp.Errorf(mcp.CodeHeaderMismatch, "headers")))
		return
	}
	reply := func(res any) {
		out, _ := mcp.Result(m.ID, res)
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
	switch m.Method {
	case "tools/list":
		reply(map[string]any{"resultType": "complete", "tools": []any{
			map[string]any{"name": "search", "description": "Search", "inputSchema": json.RawMessage(
				`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"},"q":{"type":"string"}}}`)},
			map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "admin_reset", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "ask", "inputSchema": map[string]any{"type": "object"}},
		}})
	case "tools/call":
		f.calls.Add(1)
		var p map[string]any
		raw, _ := json.Marshal(m.Params)
		json.Unmarshal(raw, &p)
		f.mu.Lock()
		f.last, f.body = r.Header.Clone(), p
		f.mu.Unlock()
		if p["name"] == "ask" {
			reply(map[string]any{"resultType": "input_required", "inputRequests": map[string]any{
				"who": map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "Name?"}}},
				"requestState": "st"})
			return
		}
		args, _ := json.Marshal(p["arguments"])
		reply(map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": string(args)}}})
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write(mcp.ErrorResponse(m.ID, mcp.Errorf(mcp.CodeMethodNotFound, "no")))
	}
}

func newMCPEnv(t testing.TB, url string) *env {
	cfg := testConfig("http://127.0.0.1:9") // no model is called here
	cfg.MCPServers = []config.MCPServer{{Name: "docs", URL: url, Protocol: mcp.Version, Timeout: 5 * time.Second, MaxResponseBytes: 1 << 20}}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey,
		MCPTokens: map[string]string{"docs": toolToken}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	return &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
}

// rpc posts a modern request with the binding's headers.
func (e *env) rpc(key, method string, params map[string]any, hdr ...string) (int, map[string]any) {
	e.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{mcp.MetaProtocolVersion: mcp.Version, mcp.MetaClientCapabilities: map[string]any{}}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	h := []string{"Content-Type", "application/json", mcp.HeaderProtocolVersion, mcp.Version, mcp.HeaderMethod, method}
	if n, ok := params["name"].(string); ok {
		h = append(h, mcp.HeaderName, mcp.EncodeHeaderValue(n))
	}
	resp := e.do("POST", "/mcp", key, string(body), append(h, hdr...)...)
	return decodeRPC(e.t, resp)
}

func decodeRPC(t testing.TB, resp *http.Response) (int, map[string]any) {
	t.Helper()
	out := readAll(t, resp)
	var m map[string]any
	if out != "" {
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%d %q: %v", resp.StatusCode, out, err)
		}
	}
	return resp.StatusCode, m
}

func toolNames(t testing.TB, m map[string]any) []string {
	res, _ := m["result"].(map[string]any)
	tools, ok := res["tools"].([]any)
	if !ok {
		t.Fatalf("no tools: %v", m)
	}
	var names []string
	for _, x := range tools {
		names = append(names, x.(map[string]any)["name"].(string))
	}
	return names
}

func errCode(m map[string]any) int {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return int(c)
}

func (e *env) setTools(token, team, body string) int {
	e.t.Helper()
	resp := e.do("PUT", "/admin/teams/"+team+"/tools", token, body)
	readAll(e.t, resp)
	return resp.StatusCode
}

func TestMCPAllowListsPerTeam(t *testing.T) {
	f := newFakeTools(t)
	e := newMCPEnv(t, f.srv.URL)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	// No list: no tool.
	if st, m := e.rpc(keyA, "tools/list", nil); st != 200 || len(toolNames(t, m)) != 0 {
		t.Fatalf("default: %d %v", st, m)
	}
	if st := e.setTools(master, "etabA", `{"tools":["docs.search","docs.echo","docs.ask"]}`); st != 200 {
		t.Fatalf("set: %d", st)
	}
	_, m := e.rpc(keyA, "tools/list", nil)
	if got := strings.Join(toolNames(t, m), ","); got != "docs.search,docs.echo,docs.ask" {
		t.Fatalf("etabA sees %s", got)
	}
	if res := m["result"].(map[string]any); res["resultType"] != "complete" || res["_meta"] == nil {
		t.Fatalf("modern result: %v", res)
	}
	if _, m := e.rpc(keyB, "tools/list", nil); len(toolNames(t, m)) != 0 {
		t.Fatalf("etabB sees %v", m)
	}
	// A tool outside the list answers like an unknown one and is not
	// called; the refusal is audited.
	n := f.calls.Load()
	if _, m := e.rpc(keyA, "tools/call", map[string]any{"name": "docs.admin_reset", "arguments": map[string]any{}}); errCode(m) != mcp.CodeInvalidParams {
		t.Fatalf("denied tool: %v", m)
	}
	if _, m := e.rpc(keyB, "tools/call", map[string]any{"name": "docs.echo", "arguments": map[string]any{}}); errCode(m) != mcp.CodeInvalidParams {
		t.Fatalf("other team: %v", m)
	}
	if f.calls.Load() != n || !e.audited("tool.denied", "docs.admin_reset") {
		t.Fatal("a denied call reached the tool server or was not audited")
	}
	// Only organisation administrators set the list; patterns name
	// configured servers.
	tadm := e.adminToken("tadm", `{"role":"team_admin","team":"etabA"}`)
	oadmB := e.adminToken("oadmB", `{"role":"org_admin","org":"etabB"}`)
	oadmA := e.adminToken("oadmA", `{"role":"org_admin","org":"etabA"}`)
	for _, c := range []struct {
		tok, team, body string
		want            int
	}{
		{tadm, "etabA", `{"tools":["*"]}`, 403},
		{oadmB, "etabA", `{"tools":["*"]}`, 403},
		{oadmA, "etabA", `{"tools":["docs.*"]}`, 200},
		{master, "etabA", `{"tools":["other.*"]}`, 400},
		{master, "etabA", `{"tools":["docs"]}`, 400},
		{master, "etabA", `{"tools":["docs.a b"]}`, 400},
	} {
		if st := e.setTools(c.tok, c.team, c.body); st != c.want {
			t.Errorf("%s: %d, want %d", c.body, st, c.want)
		}
	}
	if resp := e.do("GET", "/admin/teams/etabA/tools", tadm, ""); resp.StatusCode != 200 || !strings.Contains(readAll(t, resp), `"docs.*"`) {
		t.Fatal("a team admin cannot read the list")
	}
	if _, m := e.rpc(keyA, "tools/list", nil); len(toolNames(t, m)) != 4 {
		t.Fatalf("docs.*: %v", m)
	}
}

// audited reports whether the log holds an entry with this action and
// target.
func (e *env) audited(action, target string) bool {
	es, err := e.st.AuditRange(context.Background(), 1, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, x := range es {
		if x.Action == action && x.Target == target {
			return true
		}
	}
	return false
}

func TestMCPCallAuditedAndForwarded(t *testing.T) {
	f := newFakeTools(t)
	e := newMCPEnv(t, f.srv.URL)
	keyID, key := e.newKey("etabA")
	e.setTools(master, "etabA", `{"tools":["docs.*"]}`)
	st, m := e.rpc(key, "tools/call", map[string]any{"name": "docs.search", "arguments": map[string]any{"region": "Zürich", "q": "secret-query"}},
		"traceparent", "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01")
	if st != 200 || m["error"] != nil {
		t.Fatalf("%d %v", st, m)
	}
	f.mu.Lock()
	last, body := f.last, f.body
	f.mu.Unlock()
	// The server sees its own tool name, the gateway's token (not the
	// client's key), and the x-mcp-header parameter mirrored.
	if body["name"] != "search" || last.Get(mcp.HeaderName) != "search" || last.Get("Mcp-Param-Region") != mcp.EncodeHeaderValue("Zürich") ||
		strings.Contains(last.Get("Authorization"), key) {
		t.Fatalf("forwarded %v %v", body, last)
	}
	es, _ := e.st.AuditRange(context.Background(), 1, 1000)
	var call, result *store.AuditEntry
	for i := range es {
		switch es[i].Action {
		case "tool.call":
			call = &es[i]
		case "tool.result":
			result = &es[i]
		}
	}
	if call == nil || result == nil || call.Seq > result.Seq || call.Kind != "tool" || call.Actor != "key:"+keyID || call.Target != "docs.search" {
		t.Fatalf("audit: %+v %+v", call, result)
	}
	// The arguments are hashed, never stored.
	if strings.Contains(call.After, "secret-query") || !strings.Contains(call.After, "arguments_sha256") || !strings.Contains(result.After, `"status":"ok"`) {
		t.Fatalf("audit content: %s / %s", call.After, result.After)
	}
	// input_required goes back to a modern client as is.
	if _, m := e.rpc(key, "tools/call", map[string]any{"name": "docs.ask", "arguments": map[string]any{}}); m["result"].(map[string]any)["resultType"] != "input_required" {
		t.Fatalf("input_required: %v", m)
	}
	// The audit log cannot be written: the call is not made.
	n := f.calls.Load()
	e.st.SetFail(errors.New("store down"))
	st, m = e.rpc(key, "tools/call", map[string]any{"name": "docs.echo", "arguments": map[string]any{}})
	e.st.SetFail(nil)
	if st != http.StatusServiceUnavailable || f.calls.Load() != n {
		t.Fatalf("audit down: %d %v, %d calls", st, m, f.calls.Load()-n)
	}
}

func TestMCPBinding(t *testing.T) {
	f := newFakeTools(t)
	e := newMCPEnv(t, f.srv.URL)
	_, key := e.newKey("etabA")
	e.setTools(master, "etabA", `{"tools":["*"]}`)
	call := func(body string, hdr ...string) (int, map[string]any) {
		return decodeRPC(t, e.do("POST", "/mcp", key, body, append([]string{"Content-Type", "application/json"}, hdr...)...))
	}
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	hv := []string{mcp.HeaderProtocolVersion, mcp.Version, mcp.HeaderMethod, "tools/call"}
	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"docs.echo","arguments":{},` + meta + `}}`
	for name, c := range map[string]struct {
		body string
		hdr  []string
		st   int
		code int
	}{
		"name header missing":   {callBody, hv, 400, mcp.CodeHeaderMismatch},
		"name header differs":   {callBody, append(hv, mcp.HeaderName, "docs.search"), 400, mcp.CodeHeaderMismatch},
		"method header":         {callBody, []string{mcp.HeaderProtocolVersion, mcp.Version, mcp.HeaderMethod, "tools/list", mcp.HeaderName, "docs.echo"}, 400, mcp.CodeHeaderMismatch},
		"version header":        {callBody, []string{mcp.HeaderProtocolVersion, "2025-11-25", mcp.HeaderMethod, "tools/call", mcp.HeaderName, "docs.echo"}, 400, mcp.CodeHeaderMismatch},
		"unsupported version":   {strings.Replace(callBody, "2026-07-28", "2099-01-01", 1), []string{mcp.HeaderProtocolVersion, "2099-01-01", mcp.HeaderMethod, "tools/call", mcp.HeaderName, "docs.echo"}, 400, mcp.CodeUnsupportedVersion},
		"no capabilities":       {strings.Replace(callBody, `,"io.modelcontextprotocol/clientCapabilities":{}`, "", 1), append(hv, mcp.HeaderName, "docs.echo"), 400, mcp.CodeInvalidParams},
		"modern header no meta": {`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, []string{mcp.HeaderProtocolVersion, mcp.Version}, 400, mcp.CodeInvalidParams},
		"unknown method":        {`{"jsonrpc":"2.0","id":1,"method":"resources/list","params":{` + meta + `}}`, []string{mcp.HeaderProtocolVersion, mcp.Version, mcp.HeaderMethod, "resources/list"}, 404, mcp.CodeMethodNotFound},
		"batch":                 {`[` + callBody + `]`, nil, 400, mcp.CodeInvalidRequest},
		"base64 name":           {callBody, append(hv, mcp.HeaderName, "=?base64?ZG9jcy5lY2hv?="), 200, 0},
	} {
		st, m := call(c.body, c.hdr...)
		if st != c.st || errCode(m) != c.code {
			t.Errorf("%s: %d %v, want %d %d", name, st, m, c.st, c.code)
		}
	}
	// No GET stream, no session to delete, no browser origin, no key.
	for _, method := range []string{"GET", "DELETE"} {
		if resp := e.do(method, "/mcp", key, ""); resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d", method, resp.StatusCode)
		}
	}
	if st, _ := call(callBody, append(hv, mcp.HeaderName, "docs.echo", "Origin", "https://evil.example")...); st != 403 {
		t.Errorf("Origin: %d", st)
	}
	if resp := e.do("POST", "/mcp", "", callBody); resp.StatusCode != 401 {
		t.Errorf("no key: %d", resp.StatusCode)
	}
	// A notification is accepted without a body.
	if resp := e.do("POST", "/mcp", key, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != 202 || readAll(t, resp) != "" {
		t.Errorf("notification: %d", resp.StatusCode)
	}
	// server/discover lists the versions served.
	st, m := e.rpc(key, "server/discover", nil)
	if res, _ := m["result"].(map[string]any); st != 200 || !strings.Contains(strings.Join(anyStrings(res["supportedVersions"]), ","), "2026-07-28,2025-11-25") {
		t.Errorf("discover: %d %v", st, m)
	}
}

func anyStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func TestMCPLegacyClient(t *testing.T) {
	f := newFakeTools(t)
	e := newMCPEnv(t, f.srv.URL)
	_, key := e.newKey("etabA")
	e.setTools(master, "etabA", `{"tools":["docs.echo","docs.ask"]}`)
	legacy := func(body string, hdr ...string) (int, map[string]any) {
		return decodeRPC(t, e.do("POST", "/mcp", key, body, append([]string{"Content-Type", "application/json"}, hdr...)...))
	}
	st, m := legacy(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"old","version":"1"}}}`)
	res, _ := m["result"].(map[string]any)
	if st != 200 || res["protocolVersion"] != "2025-06-18" || res["resultType"] != nil {
		t.Fatalf("initialize: %d %v", st, m)
	}
	st, m = legacy(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, mcp.HeaderProtocolVersion, "2025-06-18")
	if st != 200 || strings.Join(toolNames(t, m), ",") != "docs.echo,docs.ask" || m["result"].(map[string]any)["resultType"] != nil {
		t.Fatalf("list: %d %v", st, m)
	}
	st, m = legacy(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"docs.echo","arguments":{"a":1}}}`, mcp.HeaderProtocolVersion, "2025-06-18")
	if st != 200 || m["error"] != nil || m["result"].(map[string]any)["resultType"] != nil {
		t.Fatalf("call: %d %v", st, m)
	}
	// A legacy client cannot answer an input request: it gets an error.
	if _, m := legacy(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"docs.ask","arguments":{}}}`); errCode(m) != mcp.CodeInternal {
		t.Fatalf("input_required to a legacy client: %v", m)
	}
	// An unknown method is not a 404 (a legacy client reads it as an
	// expired session).
	if st, m := legacy(`{"jsonrpc":"2.0","id":4,"method":"prompts/list"}`); st != 200 || errCode(m) != mcp.CodeMethodNotFound {
		t.Fatalf("unknown method: %d %v", st, m)
	}
	if st, _ := legacy(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`, mcp.HeaderProtocolVersion, "1999-01-01"); st != 400 {
		t.Fatalf("unknown legacy version: %d", st)
	}
}

func TestMCPToolServerRedirectNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	e := newMCPEnv(t, redir.URL)
	_, key := e.newKey("etabA")
	e.setTools(master, "etabA", `{"tools":["*"]}`)
	if st, m := e.rpc(key, "tools/list", nil); st != 200 || len(toolNames(t, m)) != 0 || elsewhere.Load() != 0 {
		t.Fatalf("%d %v, %d requests elsewhere", st, m, elsewhere.Load())
	}
	if _, m := e.rpc(key, "tools/call", map[string]any{"name": "docs.x", "arguments": map[string]any{}}); errCode(m) != mcp.CodeInternal || elsewhere.Load() != 0 {
		t.Fatalf("%v", m)
	}
}

// FuzzMCPEndpoint posts any body with the modern headers taken from it:
// the gateway never fails with a 5xx other than 503, always answers JSON,
// and the tool server only ever sees tools/list and tools/call of tools it
// listed, under the gateway's token.
func FuzzMCPEndpoint(f *testing.F) {
	tools := newFakeTools(f)
	e := newMCPEnv(f, tools.srv.URL)
	_, key := e.newKey("etabA")
	e.setTools(master, "etabA", `{"tools":["docs.search","docs.echo"]}`)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	f.Add(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"docs.search","arguments":{"region":"é"},`+meta+`}}`, "docs.search")
	f.Add(`{"jsonrpc":"2.0","id":"x","method":"tools/list","params":{`+meta+`}}`, "")
	f.Add(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"docs.admin_reset","arguments":{}}}`, "docs.admin_reset")
	f.Add(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"http://169.254.169.254/x","arguments":{}}}`, "http://169.254.169.254/x")
	f.Add(`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`, "")
	f.Fuzz(func(t *testing.T, body, name string) {
		e.t = t
		var probe struct {
			Method string `json:"method"`
			Params struct {
				Meta map[string]any `json:"_meta"`
			} `json:"params"`
		}
		_ = json.Unmarshal([]byte(body), &probe)
		hdr := []string{"Content-Type", "application/json", mcp.HeaderMethod, mcp.EncodeHeaderValue(probe.Method)}
		if v, ok := probe.Params.Meta[mcp.MetaProtocolVersion].(string); ok {
			hdr = append(hdr, mcp.HeaderProtocolVersion, mcp.EncodeHeaderValue(v))
		}
		if name != "" {
			hdr = append(hdr, mcp.HeaderName, mcp.EncodeHeaderValue(name))
		}
		resp := e.do("POST", "/mcp", key, body, hdr...)
		out := readAll(t, resp)
		if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%d %s", resp.StatusCode, out)
		}
		if resp.StatusCode != http.StatusAccepted && !json.Valid([]byte(out)) {
			t.Fatalf("%d: not JSON: %q", resp.StatusCode, out)
		}
		tools.mu.Lock()
		last := tools.body
		tools.mu.Unlock()
		if last != nil && last["name"] != "search" && last["name"] != "echo" {
			t.Fatalf("the tool server was asked for %v", last["name"])
		}
	})
}
