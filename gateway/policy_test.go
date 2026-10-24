package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

func policyEnv(t *testing.T) *env {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Tags = config.Tags{Header: "X-Llmflow-Tags", Allowed: []string{"eval"}}
	cfg.Policies = []config.Policy{
		{Name: "strict", Teams: []string{"etabA"}, Require: []string{"user", "metadata.project"}, RequireTags: true},
		{Name: "tiny", MaxBodyBytes: 200, MaxResponseBytes: 10},
		{Name: "roomy", MaxBodyBytes: 1 << 20},
	}
	return newEnv(t, cfg)
}

func TestRequiredParams(t *testing.T) {
	e := policyEnv(t)
	key := e.createKey(`{"team":"etabA"}`)
	ok := `{"model":"llama","user":"alice","metadata":{"project":"p1"},"messages":[{"role":"user","content":"hi"}]}`
	tag := []string{"X-Llmflow-Tags", "eval"}
	for body, want := range map[string]string{
		chat: "user, metadata.project, a request tag",
		`{"model":"llama","user":"","metadata":{"project":"p1"},"messages":[]}`:  "user",
		`{"model":"llama","user":"a","metadata":{"project":null},"messages":[]}`: "metadata.project",
		`{"model":"llama","user":"a","metadata":"p1","messages":[]}`:             "metadata.project",
		`{"model":"llama","user":["x"],"metadata":{"project":{}},"messages":[]}`: "metadata.project",
	} {
		h := tag
		if body == chat {
			h = nil
		}
		resp := e.do("POST", "/v1/chat/completions", key, body, h...)
		out := readAll(t, resp)
		var er struct {
			Error struct{ Type, Message string }
		}
		_ = json.Unmarshal([]byte(out), &er)
		if resp.StatusCode != 400 || er.Error.Type != "invalid_request_error" || !strings.HasSuffix(er.Error.Message, "requires: "+want) {
			t.Errorf("%s: %d %s, want 400 requiring %s", body, resp.StatusCode, out, want)
		}
	}
	expect(t, "complete request", e.chatWith(key, ok, tag...), 200)
	// Another team has no policy.
	other := e.createKey(`{"team":"etabB"}`)
	expect(t, "team without policy", e.chatWith(other, chat), 200)
	// A key's own policy wins over its team's.
	roomy := e.createKey(`{"team":"etabA","policy":"roomy"}`)
	expect(t, "key policy instead of the team's", e.chatWith(roomy, chat), 200)
	expect(t, "unknown policy", e.do("POST", "/admin/keys", master, `{"team":"etabA","policy":"nope"}`).StatusCode, 400)
	tok := e.adminToken("mia", `{"role":"member","team":"etabA"}`)
	expect(t, "a member choosing a policy", e.do("POST", "/admin/keys", tok, `{"team":"etabA","policy":"roomy"}`).StatusCode, 403)
}

func TestPolicySizeLimits(t *testing.T) {
	e := policyEnv(t)
	tiny := e.createKey(`{"team":"etabB","policy":"tiny"}`)
	big := `{"model":"llama","messages":[{"role":"user","content":"` + strings.Repeat("x", 300) + `"}]}`
	expect(t, "body over the policy limit", e.chatWith(tiny, big), 413)
	// The global limit is 4096 bytes in the test configuration; a policy
	// may raise it.
	larger := `{"model":"llama","messages":[{"role":"user","content":"` + strings.Repeat("x", 5000) + `"}]}`
	plain := e.createKey(`{"team":"etabB"}`)
	expect(t, "body over the global limit", e.chatWith(plain, larger), 413)
	roomy := e.createKey(`{"team":"etabB","policy":"roomy"}`)
	expect(t, "body under the policy's larger limit", e.chatWith(roomy, larger), 200)
	// The response limit: the fake answer is longer than 10 bytes.
	expect(t, "response over the policy limit", e.chatWith(tiny, chat), 502)
	expect(t, "response under the global limit", e.chatWith(plain, chat), 200)
}

// FuzzRequiredFields: any body and any path list are judged without
// panicking, and a field reported present really is reachable.
func FuzzRequiredFields(f *testing.F) {
	f.Add(`{"user":"a","metadata":{"project":"p"}}`, "user,metadata.project")
	f.Add(`{"a":{"b":{"c":[1]}}}`, "a.b.c,a.b.d,a.x.y")
	f.Add(`[1,2]`, "user")
	f.Add(`{"user":"  "}`, "user")
	f.Add(`{"a":null}`, "a.b")
	f.Fuzz(func(t *testing.T, body, paths string) {
		var require []string
		for _, p := range strings.Split(paths, ",") {
			if config.ValidFieldPath(p) {
				require = append(require, p)
			}
		}
		missing := missingFields([]byte(body), require)
		if len(missing) > len(require) {
			t.Fatalf("more missing than required: %v", missing)
		}
		var doc map[string]any
		if json.Unmarshal([]byte(body), &doc) != nil && len(missing) != len(require) {
			t.Fatalf("a non-object body satisfied %v", require)
		}
	})
}
