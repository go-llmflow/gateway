package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// attributionEnv: etabA (lab "physics") has projects p1, p3 and a disabled
// px; etabB (another organisation) has p2. Keys: a service key of etabA, a
// member's key (mia, member of etabA), a key pinned to p1.
type attributionEnv struct {
	*env
	service, mine, pinned string
}

func newAttributionEnv(t *testing.T) *attributionEnv {
	t.Helper()
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	for i := range cfg.Orgs {
		if cfg.Orgs[i].Name == "etabA" {
			cfg.Orgs[i].Labs = map[string]string{"etabA": "physics"}
		}
	}
	e := newEnv(t, cfg)
	e.check(nil, []call{
		{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p1"}`, 201},
		{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p3"}`, 201},
		{"master", "POST", "/admin/teams/etabA/projects", `{"name":"px"}`, 201},
		{"master", "DELETE", "/admin/teams/etabA/projects/px", "", 204},
		{"master", "POST", "/admin/teams/etabB/projects", `{"name":"p2"}`, 201},
	})
	a := &attributionEnv{env: e}
	_, a.service = e.newKey("etabA")
	mia := e.adminToken("mia", `{"role":"member","team":"etabA"}`)
	resp := e.do("POST", "/admin/keys", mia, `{"team":"etabA","name":"mine"}`)
	var k struct{ Key string }
	if err := json.Unmarshal([]byte(readAll(t, resp)), &k); err != nil || resp.StatusCode != 201 {
		t.Fatalf("member key: %d %v", resp.StatusCode, err)
	}
	a.mine = k.Key
	a.pinned = e.createKey(`{"team":"etabA","project":"p1"}`)
	return a
}

func (a *attributionEnv) chatAs(key string, hdr ...string) int {
	return a.chatWith(key, chat, hdr...)
}

func TestProjectAttribution(t *testing.T) {
	a := newAttributionEnv(t)
	t.Run("header names a project of the key's team", func(t *testing.T) {
		expect(t, "p1", a.chatAs(a.service, "X-Llmflow-Project", "p1"), 200)
		r := waitRecord(t, a.st, func(r store.Record) bool { return r.Project == "p1" })
		if r.Team != "etabA" || r.Lab != "physics" || r.User != "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("a member's key records the user", func(t *testing.T) {
		expect(t, "mia p3", a.chatAs(a.mine, "X-Llmflow-Project", "p3"), 200)
		r := waitRecord(t, a.st, func(r store.Record) bool { return r.Project == "p3" })
		if r.User != "mia" {
			t.Fatalf("user %q", r.User)
		}
	})
	for _, c := range []struct{ name, key, value string }{
		{"another organisation's project", a.service, "p2"},
		{"team/project spelling", a.service, "etabB/p2"},
		{"a disabled project", a.service, "px"},
		{"an unknown project", a.service, "nope"},
		{"not a name", a.service, "../p1"},
		{"a pinned key, another project", a.pinned, "p3"},
	} {
		t.Run("refused: "+c.name, func(t *testing.T) {
			resp := a.do("POST", "/v1/chat/completions", c.key, chat, "X-Llmflow-Project", c.value)
			out := readAll(t, resp)
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(out, "project not allowed for this key") {
				t.Fatalf("%d %s", resp.StatusCode, out)
			}
		})
	}
	t.Run("two headers", func(t *testing.T) {
		req, _ := http.NewRequest("POST", a.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
		req.Header.Set("Authorization", "Bearer "+a.service)
		req.Header.Add("X-Llmflow-Project", "p1")
		req.Header.Add("X-Llmflow-Project", "p3")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%d", resp.StatusCode)
		}
	})
	t.Run("pinned key, no header", func(t *testing.T) {
		expect(t, "pinned", a.chatAs(a.pinned), 200)
	})
	t.Run("a member whose role was removed", func(t *testing.T) {
		a.check(nil, []call{{"master", "DELETE", "/admin/users/mia/roles?role=member&team=etabA", "", 204}})
		expect(t, "mia after removal", a.chatAs(a.mine, "X-Llmflow-Project", "p1"), 403)
	})
}

func TestDefaultProjectAndHeaderName(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Attribution.ProjectHeader = "X-Cost-Center"
	cfg.Attribution.DefaultProject = "general"
	e := newEnv(t, cfg)
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabA/projects", `{"name":"general"}`, 201},
		{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p1"}`, 201}})
	_, key := e.newKey("etabA")
	_, keyB := e.newKey("etabB") // etabB has no "general": no project
	expect(t, "default", e.chatWith(key, chat), 200)
	waitRecord(t, e.st, func(r store.Record) bool { return r.Team == "etabA" && r.Project == "general" })
	expect(t, "configured header", e.chatWith(key, chat, "X-Cost-Center", "p1"), 200)
	waitRecord(t, e.st, func(r store.Record) bool { return r.Project == "p1" })
	expect(t, "etabB without a general project", e.chatWith(keyB, chat), 200)
	waitRecord(t, e.st, func(r store.Record) bool { return r.Team == "etabB" && r.Project == "" })
}

// FuzzProjectHeader: whatever the header holds, a key of etabA is charged
// to nothing but one of etabA's enabled projects (or none); never to etabB's.
func FuzzProjectHeader(f *testing.F) {
	for _, s := range []string{"p1", "p2", "etabB/p2", "px", "", "P1", "p1 ", "p1\x00", "..", "p1,p2", "%70%31"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		a := fuzzAttribution(t)
		req, _ := http.NewRequest("POST", "http://gw/v1/chat/completions", nil)
		req.Header["X-Llmflow-Project"] = []string{value}
		got, err := a.srv.resolveProject(req, a.key)
		if err != nil {
			return
		}
		if got != "" && got != "p1" && got != "p3" {
			t.Fatalf("header %q charged project %q", value, got)
		}
	})
}

type fuzzState struct {
	srv *Server
	key store.Key
}

var fuzzOnce *fuzzState

func fuzzAttribution(t *testing.T) *fuzzState {
	if fuzzOnce != nil {
		return fuzzOnce
	}
	st := store.NewMemory()
	ctx := context.Background()
	for _, o := range []string{"etabA", "etabB"} {
		_ = st.CreateOrg(ctx, o)
		_ = st.CreateTeam(ctx, o, o)
	}
	_ = st.CreateProject(ctx, "etabA", "p1")
	_ = st.CreateProject(ctx, "etabA", "p3")
	_ = st.CreateProject(ctx, "etabA", "px")
	_ = st.DisableProject(ctx, "etabA", "px")
	_ = st.CreateProject(ctx, "etabB", "p2")
	cfg := testConfig("http://127.0.0.1:1")
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.budget.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	fuzzOnce = &fuzzState{srv: srv, key: store.Key{ID: "k1", Team: "etabA", Org: "etabA"}}
	return fuzzOnce
}
