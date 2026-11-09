package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestModelCatalogue(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), "")
	cfg.Region = "eu"
	cfg.ModelGroups[0].Upstreams[0].Region = "eu"
	e := newEnv(t, cfg)

	resp := e.do("GET", "/admin/models", master, "")
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var list []catalogueEntry
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]catalogueEntry{}
	for _, c := range list {
		byID[c.ID] = c
	}
	l, g := byID["llama"], byID["llama-guard"]
	if l.UpstreamModel != "meta/llama" || l.Pool != "b300" || l.Class != "interactive" || l.InputPerMTok != 1000 || l.OutputPerMTok != 5000 ||
		l.Unit != "gpu_hours" || l.Upstreams != 1 || len(l.Regions) != 1 || l.Regions[0] != "eu" || l.RegionPolicy != "prefer" {
		t.Fatalf("llama: %+v", l)
	}
	if g.Class != "guard" {
		t.Fatalf("a proxy admin sees the guard model: %+v", g)
	}
	// Health is live: an upstream cooling down is not counted as up.
	e.srv.groups["llama"].members()[0].cooldown.Store(time.Now().Add(time.Hour).UnixNano())
	if c := e.srv.catalogue(&principal{master: true}); c[0].ID != "llama" || c[0].Healthy != 0 || c[0].Upstreams != 1 {
		t.Fatalf("health in the catalogue: %+v", c[0])
	}

	// Somebody with no proxy role does not see the gateway's own guard model.
	for _, c := range e.srv.catalogue(&principal{name: "lab-admin"}) {
		if c.Class == "guard" {
			t.Fatalf("a guard model is listed for a non-proxy principal: %+v", c)
		}
	}
	// Without a token: refused.
	if resp := e.do("GET", "/admin/models", "", ""); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
}

func TestUIModelsPage(t *testing.T) {
	e := newSSOEnv(t, "")
	sess, _ := e.login(e.claims("alice", "llm-admins"))
	if c, o, _ := e.uiGet("/ui/orgs", sess); c != 200 || !strings.Contains(o, "<h2>Teams</h2>") || !strings.Contains(o, ">etabA<") {
		t.Fatalf("orgs page: %d %s", c, o)
	}
	c, page, _ := e.uiGet("/ui/models", sess)
	if c != 200 || !strings.Contains(page, ">llama<") || !strings.Contains(page, "<title>Models") || !strings.Contains(page, `href="/ui/models"`) {
		t.Fatalf("%d %s", c, page)
	}
}
