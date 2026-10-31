package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The contract with cmd/llmflow-status: its testdata/me_usage.contract.json
// holds the fields that command reads, and the command's own test renders
// that file. Here the REAL /v1/me/usage of a person with a profile must carry
// every field of the file, with the same JSON type (and a time where the file
// has one), so the command's fake gateway cannot drift from this server.
func TestMeUsageContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "cmd", "llmflow-status", "testdata", "me_usage.contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	up := newFakeVLLM(t)
	cfg := softConfig(up.URL())
	cfg.SoftQuotas.WarnAt = []int{75, 95}
	e := newEnv(t, cfg)
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	ta := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	e.check(map[string]string{"ta": ta}, []call{{"ta", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	ka := e.ownKey(alice)
	readAll(t, e.do("POST", "/v1/chat/completions", ka, chat20))
	e.waitRecords(1)
	st, _, _, body := e.meUsage(ka)
	if st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	var got any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if err := sameShape("", want, got); err != nil {
		t.Fatalf("/v1/me/usage breaks the llmflow-status contract: %v\nreal: %s", err, body)
	}
}

// The control: a response that renamed a field or changed its type fails.
func TestSameShapeSeesADrift(t *testing.T) {
	want := map[string]any{"week": map[string]any{"used_pct": 40.0, "resets_at": "2026-11-02T00:00:00Z"}}
	for name, got := range map[string]any{
		"renamed":   map[string]any{"week": map[string]any{"used_percent": 40.0, "resets_at": "2026-11-02T00:00:00Z"}},
		"retyped":   map[string]any{"week": map[string]any{"used_pct": "40", "resets_at": "2026-11-02T00:00:00Z"}},
		"not time":  map[string]any{"week": map[string]any{"used_pct": 40.0, "resets_at": "Monday"}},
		"no window": map[string]any{},
	} {
		if sameShape("", want, got) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := sameShape("", want, map[string]any{"week": map[string]any{"used_pct": 3.0, "resets_at": "2026-11-09T00:00:00Z", "extra": 1.0}}); err != nil {
		t.Errorf("same shape with an extra field: %v", err)
	}
}

// sameShape says whether got has every field of want, recursively, with the
// same JSON type; a string of want that is an RFC 3339 time must be one in got.
func sameShape(path string, want, got any) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %T", path, got)
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s.%s: missing", path, k)
			}
			if err := sameShape(path+"."+k, wv, gv); err != nil {
				return err
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array, got %T", path, got)
		}
		if len(w) > 0 {
			for i, gv := range g {
				if err := sameShape(fmt.Sprintf("%s[%d]", path, i), w[0], gv); err != nil {
					return err
				}
			}
		}
	case string:
		g, ok := got.(string)
		if !ok {
			return fmt.Errorf("%s: want a string, got %T", path, got)
		}
		if _, err := time.Parse(time.RFC3339, w); err == nil {
			if _, err := time.Parse(time.RFC3339, g); err != nil {
				return fmt.Errorf("%s: want an RFC 3339 time, got %q", path, g)
			}
		}
	case float64:
		if _, ok := got.(float64); !ok {
			return fmt.Errorf("%s: want a number, got %T", path, got)
		}
	case bool:
		if _, ok := got.(bool); !ok {
			return fmt.Errorf("%s: want a boolean, got %T", path, got)
		}
	}
	return nil
}
