package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func TestSpendReports(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	tok := map[string]string{
		"alice": e.adminToken("alice", `{"role":"org_admin","org":"etabA"}`),
		"mia":   e.adminToken("mia", `{"role":"member","team":"etabA"}`),
	}
	// mia's own key, another etabA key, an etabB key.
	resp := e.do("POST", "/admin/keys", tok["mia"], `{"team":"etabA","tags":["eval"]}`)
	var mine struct{ Key string }
	_ = json.Unmarshal([]byte(readAll(t, resp)), &mine)
	other := e.createKey(`{"team":"etabA"}`)
	b := e.createKey(`{"team":"etabB","tags":["eval","prod"]}`)
	batch := strings.Replace(chat, `"llama"`, `"llama-batch"`, 1)
	for _, c := range []struct{ key, body string }{{mine.Key, chat}, {other, chat}, {other, batch}, {b, chat}} {
		expect(t, "request", e.chatWith(c.key, c.body), 200)
	}
	e.srv.Close(context.Background()) // drain the accounting queue

	from := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	report := func(who, query string) (int, string) {
		t.Helper()
		token := master
		if who != "master" {
			token = tok[who]
		}
		resp := e.do("GET", "/admin/spend?from="+from+"&to="+to+query, token, "")
		return resp.StatusCode, readAll(t, resp)
	}
	var out struct {
		GroupBy []string          `json:"group_by"`
		Rows    []store.ReportRow `json:"rows"`
	}
	code, body := report("master", "&group_by=org,team")
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Rows) != 2 ||
		out.Rows[0].Org != "etabA" || out.Rows[0].Requests != 3 || out.Rows[1].Team != "etabB" {
		t.Fatalf("by org,team: %d %s", code, body)
	}
	code, body = report("master", "&group_by=tag")
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Rows) != 3 ||
		out.Rows[0].Tag != "" || out.Rows[0].Requests != 2 || out.Rows[1].Tag != "eval" || out.Rows[1].Requests != 2 {
		t.Fatalf("by tag: %d %s", code, body)
	}
	code, body = report("master", "&group_by=model&format=csv")
	if code != 200 || !strings.HasPrefix(body, "from,to,model,requests,") || !strings.Contains(body, ",llama-batch,1,") {
		t.Fatalf("csv: %d %s", code, body)
	}
	code, body = report("master", "&group_by=team&format=parsable2")
	if code != 200 || !strings.HasPrefix(body, "from|to|team|requests|") || strings.Contains(body, "|\n") {
		t.Fatalf("parsable2: %d %s", code, body)
	}
	// Default grouping, and the v0.1 export still answers.
	code, body = report("master", "")
	if code != 200 || !strings.Contains(body, `"group_by":["org","team"]`) {
		t.Fatalf("default: %d %s", code, body)
	}
	resp = e.do("GET", "/admin/usage.csv?from="+from+"&to="+to, master, "")
	if body := readAll(t, resp); !strings.HasPrefix(body, "from,to,team,pool,requests") {
		t.Fatalf("usage.csv %s", body)
	}

	// Visibility.
	code, body = report("alice", "&group_by=team")
	if code != 200 || !strings.Contains(body, `"team":"etabA"`) || strings.Contains(body, "etabB") {
		t.Fatalf("org admin sees: %s", body)
	}
	code, body = report("mia", "&group_by=team")
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Rows) != 1 || out.Rows[0].Requests != 1 {
		t.Fatalf("member sees only own key: %s", body)
	}

	for _, q := range []string{"&group_by=planet", "&format=xml"} {
		if code, body := report("master", q); code != 400 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	resp = e.do("GET", "/admin/spend?from=x", master, "")
	if readAll(t, resp); resp.StatusCode != 400 {
		t.Fatalf("bad from: %d", resp.StatusCode)
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "-1": "'-1", "@x": "'@x", "llama": "llama", "": ""} {
		if got := csvSafe(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if err := WriteReport(nil, "xml", time.Time{}, time.Time{}, nil, nil); err == nil {
		t.Fatal("bad format accepted")
	}
}
