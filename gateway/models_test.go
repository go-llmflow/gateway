package gateway

import (
	"strings"
	"testing"
)

func TestTeamManagedModels(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	tok := map[string]string{
		"tom": e.adminToken("tom", `{"role":"team_admin","team":"etabA"}`),
		"mia": e.adminToken("mia", `{"role":"member","team":"etabA"}`),
	}
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	e.check(tok, []call{
		{"mia", "PUT", "/admin/teams/etabA/models", `{"models":["llama"]}`, 403},
		{"tom", "PUT", "/admin/teams/etabB/models", `{"models":["llama"]}`, 403},
		{"tom", "PUT", "/admin/teams/etabA/models", `{"models":["nope"]}`, 400},
		{"tom", "PUT", "/admin/teams/etabA/models", `{"models":["llama"]}`, 200},
		{"mia", "GET", "/admin/teams/etabA/models", "", 200},
		{"tom", "GET", "/admin/teams/etabB/models", "", 403},
		{"master", "PUT", "/admin/teams/nobody/models", `{"models":[]}`, 404},
	})
	// The hidden model answers like an unknown one, and is not listed.
	batch := strings.Replace(chat, `"llama"`, `"llama-batch"`, 1)
	if resp := e.do("POST", "/v1/chat/completions", keyA, batch); readAll(t, resp) == "" || resp.StatusCode != 404 {
		t.Fatalf("hidden model: %d", resp.StatusCode)
	}
	if resp := e.do("POST", "/v1/chat/completions", keyA, chat); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("allowed model: %d", resp.StatusCode)
	}
	if out := readAll(t, e.do("GET", "/v1/models", keyA, "")); strings.Contains(out, "llama-batch") || !strings.Contains(out, `"id":"llama"`) {
		t.Fatalf("models of etabA: %s", out)
	}
	// Other teams are unaffected.
	if resp := e.do("POST", "/v1/chat/completions", keyB, batch); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("other team: %d", resp.StatusCode)
	}
	if out := readAll(t, e.do("GET", "/admin/teams/etabA/models", tok["mia"], "")); !strings.Contains(out, `"effective":["llama"]`) {
		t.Fatalf("effective: %s", out)
	}
	// An empty list exposes everything again.
	e.check(tok, []call{{"tom", "PUT", "/admin/teams/etabA/models", `{"models":[]}`, 200}})
	if resp := e.do("POST", "/v1/chat/completions", keyA, batch); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("after reset: %d", resp.StatusCode)
	}
}
