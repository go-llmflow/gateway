package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// profileConfig is the test configuration with workload profiles: a
// workday of 1 GPU-hour, so days and GPU-hours read alike. A request of
// max_tokens 20 holds 0.208 and costs 0.164.
func profileConfig(url string) *config.Config {
	cfg := testConfig(url)
	cfg.Profiles = config.Profiles{
		Workday: map[string]float64{"": 1},
		List: []config.Profile{
			{ID: "p", Label: config.DaysLabel(0.4), Days: 0.4, SessionShare: 1, OverflowCap: 0.3},
			{ID: "s", Label: "short sessions", Days: 5, SessionShare: 0.25},
			{ID: "big", Label: config.DaysLabel(7), Days: 7, SessionShare: 1},
		},
		SessionWindow: 5 * time.Hour,
	}
	return cfg
}

const chat20 = `{"model":"llama","max_tokens":20,"messages":[]}`

func (e *env) ownKey(tok string) string {
	e.t.Helper()
	resp := e.do("POST", "/admin/keys", tok, `{"team":"etabA","name":"k"}`)
	var k struct{ Key string }
	if err := json.Unmarshal([]byte(readAll(e.t, resp)), &k); err != nil || resp.StatusCode != 201 {
		e.t.Fatalf("key: %d %v", resp.StatusCode, err)
	}
	return k.Key
}

// A person's week, in days: refused beyond it in workdays, with when it
// resets; allowed beyond it on the overflow cap once their administrator
// allows it; the records say which profile and whether overflow.
func TestProfileWeekAndOverflow(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	ta := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	e.check(map[string]string{"ta": ta}, []call{{"ta", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	key := e.ownKey(alice)
	monday := store.Week.ResetAt(time.Now()).Format(time.RFC3339)
	for i := 0; i < 2; i++ { // 0.164 + 0.164 fit 0.4; a third (W 0.208) does not
		resp := e.do("POST", "/v1/chat/completions", key, chat20)
		if out := readAll(t, resp); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %s", i+1, resp.StatusCode, out)
		}
	}
	e.waitRecords(2)
	e.srv.escrow.forgetRoom()
	resp := e.do("POST", "/v1/chat/completions", key, chat20)
	out := readAll(t, resp)
	if resp.StatusCode != 429 || !strings.Contains(out, "weekly allowance of 0.4 day: 0.07 day left, and this request may cost up to 0.21 day") ||
		!strings.Contains(out, "resets Monday") ||
		resp.Header.Get(budgetResetHeader) != monday {
		t.Fatalf("beyond the week: %d %q %s", resp.StatusCode, resp.Header.Get(budgetResetHeader), out)
	}
	// It names the caller's own allowance only: no other scope, no budget kind.
	for _, leak := range []string{"etabA", "profile_week", "alice"} {
		if strings.Contains(out, leak) {
			t.Fatalf("the refusal names %q: %s", leak, out)
		}
	}
	// Allowed to overflow: on the monthly cap (0.3 holds one request).
	e.check(map[string]string{"ta": ta}, []call{{"ta", "PUT", "/admin/users/alice/profile", `{"profile":"p","overflow":true}`, 200}})
	e.srv.escrow.forgetRoom()
	if code := e.chatWith(key, chat20); code != 200 {
		t.Fatalf("overflow allowed: %d", code)
	}
	e.waitRecords(3)
	e.srv.escrow.forgetRoom()
	resp = e.do("POST", "/v1/chat/completions", key, chat20)
	out = readAll(t, resp)
	// Both refuse: the earlier of the week's and the month's resets.
	first := store.Week.ResetAt(time.Now())
	if m := store.Month.ResetAt(time.Now()); m.Before(first) {
		first = m
	}
	if resp.StatusCode != 429 || !strings.Contains(out, "monthly overflow allowance of 0.3 day") || !strings.Contains(out, "weekly allowance") ||
		resp.Header.Get(budgetResetHeader) != first.Format(time.RFC3339) {
		t.Fatalf("beyond the overflow cap: %d %q %s", resp.StatusCode, resp.Header.Get(budgetResetHeader), out)
	}
	recs := e.st.Records()
	over := 0
	for _, r := range recs {
		if r.User == "alice" && r.Profile != "p" {
			t.Fatalf("record without its profile: %+v", r)
		}
		if r.Overflow {
			over++
		}
	}
	if over != 1 {
		t.Fatalf("%d overflow records, want 1", over)
	}
	// The week never exceeded its 0.4, nor the overflow its 0.3.
	week, _ := e.st.Ledger(context.Background(), store.BudgetKey{Kind: store.ScopeProfileWeek, Name: "alice"}, store.Week.Start(time.Now()))
	ov, _ := e.st.Ledger(context.Background(), store.BudgetKey{Kind: store.ScopeProfileOverflow, Name: "alice"}, store.Month.Start(time.Now()))
	if week.Spent != 328_000_000 || ov.Spent != 164_000_000 {
		t.Fatalf("week %+v overflow %+v", week, ov)
	}
	// Reports by profile.
	day := store.Day.Start(time.Now())
	resp = e.do("GET", "/admin/spend?group_by=profile&format=csv&from="+day.Format("2006-01-02")+"&to="+day.AddDate(0, 0, 1).Format("2006-01-02"), master, "")
	if out := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(out, "profile") || !strings.Contains(out, "p,") {
		t.Fatalf("report by profile: %d %s", resp.StatusCode, out)
	}
	if !e.audited("PUT /admin/users/{name}/profile", "/admin/users/alice/profile") {
		t.Fatal("assignment not audited")
	}
}

// The session window: refused within the week when the rolling window is
// full, with when it has room again.
func TestProfileSession(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	bob := e.adminToken("bob", `{"role":"member","team":"etabA"}`)
	e.check(nil, []call{{"master", "PUT", "/admin/users/bob/profile", `{"profile":"s"}`, 200}})
	key := e.ownKey(bob)
	if code := e.chatWith(key, chat20); code != 200 { // 0.164 of 0.25
		t.Fatalf("first: %d", code)
	}
	e.waitRecords(1)
	e.srv.escrow.forgetRoom()
	resp := e.do("POST", "/v1/chat/completions", key, chat20)
	out := readAll(t, resp)
	reset, err := time.Parse(time.RFC3339, resp.Header.Get(budgetResetHeader))
	if resp.StatusCode != 429 || !strings.Contains(out, "session allowance of 0.25 day per 5h: 0.09 day left") || !strings.Contains(out, "room again from") || err != nil ||
		reset.Before(time.Now().Add(5*time.Hour)) || reset.After(time.Now().Add(5*time.Hour+10*time.Minute)) {
		t.Fatalf("session full: %d %q %s", resp.StatusCode, resp.Header.Get(budgetResetHeader), out)
	}
}

// default_profile: a person without an assignment gets it at their first
// request, which waits for the new budgets and goes through.
func TestProfileDefaultAtFirstRequest(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := profileConfig(up.URL())
	cfg.Profiles.Default = "p"
	e := newEnv(t, cfg)
	carol := e.adminToken("carol", `{"role":"member","team":"etabA"}`)
	key := e.ownKey(carol)
	if code := e.chatWith(key, chat20); code != 200 {
		t.Fatalf("first request under the default profile: %d", code)
	}
	if !e.srv.budget.has(store.BudgetKey{Kind: store.ScopeProfileWeek, Name: "carol"}) {
		t.Fatal("no weekly budget created")
	}
	// A service key (no person) has no profile.
	svc := e.createKey(`{"team":"etabA"}`)
	if code := e.chatWith(svc, chat20); code != 200 {
		t.Fatalf("service key: %d", code)
	}
	// Removing an assignment falls back to the default.
	e.check(nil, []call{
		{"master", "PUT", "/admin/users/carol/profile", `{"profile":"big"}`, 200},
		{"master", "DELETE", "/admin/users/carol/profile", ``, 200},
	})
	if b := e.srv.profileBudgets()["carol"][store.BudgetKey{Kind: store.ScopeProfileWeek, Name: "carol"}]; b.MaxCost != 0.4 {
		t.Fatalf("back to the default: %+v", b)
	}
}

// A person's standing carries the shares used and the weekly window's
// identity, for response headers and threshold notices.
func TestProfilePersonShares(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	e.check(nil, []call{{"master", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	if code := e.chatWith(e.ownKey(alice), chat20); code != 200 {
		t.Fatalf("request: %d", code)
	}
	e.waitRecords(1)
	pe := e.srv.personOf("alice")
	if len(pe.Units) != 1 {
		t.Fatalf("%+v", pe)
	}
	u := pe.Units[0]
	// 0.164 of 0.4 day; 0.164 of the session's 1 day.
	if u.WeekPct < 40 || u.WeekPct > 42 || u.SessionPct < 16 || u.SessionPct > 17 || !u.WeekStart.Equal(store.Week.Start(time.Now())) {
		t.Fatalf("shares: %+v", u)
	}
}

// Without a workday in the request's unit, a priced request under a
// profile is refused: nothing could hold its cost against the profile.
func TestProfileUncalibratedUnitRefused(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := profileConfig(up.URL())
	cfg.Profiles.Workday = map[string]float64{}
	cfg.Profiles.Default = "p"
	e := newEnv(t, cfg)
	dan := e.adminToken("dan", `{"role":"member","team":"etabA"}`)
	resp := e.do("POST", "/v1/chat/completions", e.ownKey(dan), chat20)
	if out := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(out, "no workday value for gpu_hours") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
}

// The authorisation matrix of assignments: no cross-organisation
// assignment, a person with a binding elsewhere or none is the proxy
// admins'.
func TestProfileAuthzMatrix(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	toks := map[string]string{
		"proxy": e.adminToken("px", `{"role":"proxy_admin"}`),
		"oa":    e.adminToken("oa", `{"role":"org_admin","org":"etabA"}`),
		"ob":    e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`),
		"ta":    e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`),
		"viewA": e.adminToken("va", `{"role":"viewer","org":"etabA"}`),
		"alice": e.adminToken("alice", `{"role":"member","team":"etabA"}`),
	}
	e.adminToken("both", `{"role":"member","team":"etabA"}`, `{"role":"member","team":"etabB"}`)
	e.adminToken("loner")
	e.adminToken("oadm", `{"role":"org_admin","org":"etabA"}`)
	e.adminToken("twoorg", `{"role":"viewer","org":"etabA"}`, `{"role":"viewer","org":"etabB"}`)
	e.adminToken("mixed", `{"role":"member","team":"etabA"}`, `{"role":"org_admin","org":"etabB"}`)
	body := `{"profile":"p"}`
	put := func(who, user string, want int) call {
		return call{who, "PUT", "/admin/users/" + user + "/profile", body, want}
	}
	get := func(who, user string, want int) call {
		return call{who, "GET", "/admin/users/" + user + "/profile", "", want}
	}
	e.check(toks, []call{
		// alice: in etabA only.
		put("proxy", "alice", 200), put("oa", "alice", 200), put("ta", "alice", 200),
		put("ob", "alice", 403), put("viewA", "alice", 403), put("alice", "alice", 403),
		// both: in etabA and etabB: neither organisation decides alone.
		put("oa", "both", 403), put("ta", "both", 403), put("ob", "both", 403), put("proxy", "both", 200),
		// no binding, or a JWT's user: proxy admins only.
		put("oa", "loner", 403), put("proxy", "loner", 200),
		put("oa", "jwt-0123abcd", 403), put("proxy", "jwt-0123abcd", 200),
		// an organisation-level binding is authority, not membership (#99):
		// a person who only has org_admin or viewer roles is the proxy
		// admins', whoever granted them.
		put("ta", "oadm", 403), put("oa", "oadm", 403), put("proxy", "oadm", 200),
		// two organisation-level bindings: nobody but the proxy admins.
		put("oa", "twoorg", 403), put("ob", "twoorg", 403), put("proxy", "twoorg", 200),
		get("oa", "twoorg", 403), get("ob", "twoorg", 403),
		// a team member who also holds an org role elsewhere stays etabA's.
		put("oa", "mixed", 200), put("ob", "mixed", 403),
		// reading
		get("alice", "alice", 200), get("viewA", "alice", 200), get("ob", "alice", 403), get("alice", "both", 403),
		// bad input
		{"proxy", "PUT", "/admin/users/alice/profile", `{"profile":"nope"}`, 400},
		{"proxy", "PUT", "/admin/users/alice/profile", `{"profile":"s","overflow":true}`, 400},
		{"proxy", "PUT", "/admin/users/alice/profile", `{"overflow":true}`, 400},
		{"proxy", "PUT", "/admin/users/alice/profile", `{"profile":"p","extra":1}`, 400},
		{"proxy", "PUT", "/admin/users/a%20b/profile", body, 400},
		// the profile budgets are not the budgets API's
		{"proxy", "PUT", "/admin/budgets", `{"kind":"profile_week","name":"alice","period":"week","max_cost":9}`, 404},
		// calibration: proxy roles only
		{"oa", "GET", "/admin/profiles/calibrate", "", 403},
		{"oa", "POST", "/admin/profiles/calibrate", `{"unit":"gpu_hours","amount":2}`, 403},
		{"proxy", "POST", "/admin/profiles/calibrate", `{"unit":"gpu_hours","amount":-2}`, 400},
		{"proxy", "POST", "/admin/profiles/calibrate", `{"unit":"EUR","amount":2}`, 400},
	})
	// Listing: an organisation admin sees their organisation's people.
	out := readAll(t, e.do("GET", "/admin/profiles/people", toks["ob"], ""))
	if strings.Contains(out, `"user":"alice"`) || !strings.Contains(out, `"user":"both"`) || !strings.Contains(out, `"can_assign":false`) {
		t.Fatalf("ob's listing: %s", out)
	}
}

// #99: an organisation admin cannot claim a person by granting them an
// organisation-level role first: assign, read and delete stay refused after
// every grant they are allowed to make.
func TestProfileGrantThenActRefused(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	toks := map[string]string{
		"oa": e.adminToken("oa", `{"role":"org_admin","org":"etabA"}`),
	}
	e.adminToken("loner")
	e.check(toks, []call{
		{"oa", "PUT", "/admin/users/loner/profile", `{"profile":"p"}`, 403},
		// #104: a binding-less user is a proxy admin's to place: every grant
		// of the org admin is refused, so there is nothing to act through.
		{"oa", "PUT", "/admin/users/loner/roles", `{"role":"viewer","org":"etabA"}`, 403},
		{"oa", "PUT", "/admin/users/loner/roles", `{"role":"org_admin","org":"etabA"}`, 403},
		{"oa", "PUT", "/admin/users/loner/roles", `{"role":"member","team":"etabA"}`, 403},
		{"oa", "PUT", "/admin/users/loner/profile", `{"profile":"p"}`, 403},
		{"oa", "GET", "/admin/users/loner/profile", "", 403},
		{"oa", "DELETE", "/admin/users/loner/profile", "", 403},
		{"oa", "PUT", "/admin/budgets", `{"kind":"user","name":"loner","period":"week","max_cost":9}`, 403},
	})
}

// #104: provenance. Another organisation's user is not pullable either; a
// user provisioned into the organisation is administered normally; a binding
// an org admin granted is no provisioning, and one with unknown provenance
// (stored before v0.18.2) is legitimate.
func TestProfileProvenance(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	toks := map[string]string{
		"oa": e.adminToken("oa", `{"role":"org_admin","org":"etabA"}`),
		"ob": e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`),
	}
	e.adminToken("bmem", `{"role":"member","team":"etabB"}`)
	e.adminToken("amem", `{"role":"member","team":"etabA"}`)
	e.adminToken("orphan")
	e.adminToken("legacy")
	ctx := context.Background()
	// An org admin's grant recorded as such, and a pre-provenance one.
	if err := e.st.Grant(ctx, store.Binding{User: "orphan", Role: store.RoleMember, Team: "etabA", Provenance: store.ProvOrgAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Grant(ctx, store.Binding{User: "legacy", Role: store.RoleMember, Team: "etabA"}); err != nil {
		t.Fatal(err)
	}
	body := `{"profile":"p"}`
	e.check(toks, []call{
		// another organisation's user
		{"oa", "PUT", "/admin/users/bmem/roles", `{"role":"member","team":"etabA"}`, 403},
		{"oa", "PUT", "/admin/users/bmem/profile", body, 403},
		{"ob", "PUT", "/admin/users/bmem/profile", body, 200},
		// a user provisioned into the organisation: an org admin may add roles
		{"oa", "PUT", "/admin/users/amem/roles", `{"role":"viewer","org":"etabA"}`, 200},
		{"oa", "PUT", "/admin/users/amem/profile", body, 200},
		// a role an org admin granted is not provisioning
		{"oa", "PUT", "/admin/users/orphan/profile", body, 403},
		{"oa", "GET", "/admin/users/orphan/profile", "", 403},
		{"oa", "PUT", "/admin/users/orphan/roles", `{"role":"viewer","team":"etabA"}`, 403},
		{"master", "PUT", "/admin/users/orphan/profile", body, 200},
		// unknown provenance is legitimate
		{"oa", "PUT", "/admin/users/legacy/profile", body, 200},
		// ... and a proxy admin places anybody
		{"master", "PUT", "/admin/users/orphan/roles", `{"role":"viewer","team":"etabA"}`, 200},
	})
	got := map[string]store.Provenance{}
	bs, err := e.st.Bindings(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		if b.User == "amem" && b.Role == store.RoleViewer {
			got["org_admin_grant"] = b.Provenance
		}
		if b.User == "orphan" && b.Role == store.RoleViewer {
			got["proxy_grant"] = b.Provenance
		}
	}
	if got["org_admin_grant"] != store.ProvOrgAdmin || got["proxy_grant"] != store.ProvProxyAdmin {
		t.Fatalf("recorded provenance: %v", got)
	}
}

// Calibration from a synthetic history: the proposal is the p75 of the
// active person-days; accepting it moves every profile budget.
func TestProfileCalibration(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	e.check(nil, []call{{"master", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	today := store.Day.Start(time.Now())
	var recs []store.Record
	// 8 person-days costing 1..8 (two people, four days), split over two
	// records each, and a day of zero cost that is not an active day.
	n := 0
	for d := 1; d <= 4; d++ {
		for _, u := range []string{"u1", "u2"} {
			n++
			at := today.AddDate(0, 0, -d).Add(10 * time.Hour)
			c := float64(n)
			recs = append(recs, store.Record{ID: fmt.Sprintf("c%d-a", n), Time: at, User: u, Team: "etabA", Cost: c / 2},
				store.Record{ID: fmt.Sprintf("c%d-b", n), Time: at.Add(time.Hour), User: u, Team: "etabA", Cost: c / 2})
		}
	}
	recs = append(recs, store.Record{ID: "zero", Time: today.AddDate(0, 0, -1), User: "u3", Team: "etabA"})
	if err := e.st.InsertRecords(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
	resp := e.do("GET", "/admin/profiles/calibrate?weeks=1", master, "")
	var cs []calibration
	if err := json.Unmarshal([]byte(readAll(t, resp)), &cs); err != nil || len(cs) == 0 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
	c := cs[0]
	if c.Unit != "gpu_hours" || c.Days != 8 || c.P50 != 4 || c.P75 != 6 || c.P90 != 8 || c.Proposal != 6 || c.Current == nil || *c.Current != 1 {
		t.Fatalf("calibration %+v", c)
	}
	e.check(nil, []call{{"master", "POST", "/admin/profiles/calibrate", `{"unit":"gpu_hours","amount":6}`, 200}})
	if b := e.srv.profileBudgets()["alice"][store.BudgetKey{Kind: store.ScopeProfileWeek, Name: "alice"}]; b.MaxCost < 2.4-1e-9 || b.MaxCost > 2.4+1e-9 {
		t.Fatalf("after the calibration: %v", b.MaxCost)
	}
	out := readAll(t, e.do("GET", "/admin/profiles", master, ""))
	if !strings.Contains(out, `"source":"calibration"`) || !strings.Contains(out, `"set_by":"master"`) {
		t.Fatalf("%s", out)
	}
}

// The capacity check: share × C × measured cost per slot-hour × 168 h in
// workdays, or "not measured", never a guess.
func TestProfileCapacity(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := profileConfig(up.URL())
	cfg.Profiles.Default = "p"
	e := newEnv(t, cfg)
	e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	e.adminToken("bob", `{"role":"member","team":"etabA"}`)
	e.check(nil, []call{{"master", "PUT", "/admin/users/bob/profile", `{"profile":"big"}`, 200}})
	get := func() []orgCapacity {
		var out []orgCapacity
		resp := e.do("GET", "/admin/profiles/capacity", master, "")
		if err := json.Unmarshal([]byte(readAll(t, resp)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, c := range get() {
		if c.Org == "etabA" && (c.Capacity != nil || !strings.Contains(c.Note, "not measured") || c.AssignedDays != 7.4 || c.People != 2) {
			t.Fatalf("unmeasured: %+v", c)
		}
	}
	// 2 GPU-hours over 1 busy hour: 2 per slot-hour; etabA's share 0.75 of
	// C = 4: 0.75 × 4 × 2 × 168 = 1008 workdays a week.
	if err := e.st.InsertRecords(context.Background(), []store.Record{{ID: "load", Time: time.Now().Add(-time.Hour), Pool: "b300", Team: "etabA",
		Cost: 2, LatencyMS: 3_600_000}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range get() {
		if c.Org == "etabA" && (c.Capacity == nil || *c.Capacity != 1008 || c.Ratio == nil || *c.Ratio != 0.01) {
			t.Fatalf("measured: %+v", c)
		}
	}
}

// Conversion tables: a profile into budgets, days into words.
func TestProfileConversion(t *testing.T) {
	pr := newProfiler(config.Profiles{Workday: map[string]float64{"": 0.8, "USD": 13},
		List:    []config.Profile{{ID: "2d", Days: 2, SessionShare: 0.5, OverflowCap: 3}, {ID: "q", Days: 0.25, SessionShare: 0.25}},
		Default: "q", SessionWindow: 5 * time.Hour})
	pr.assign["ann"] = store.Assignment{User: "ann", Profile: "2d", Overflow: true}
	pr.assign["ben"] = store.Assignment{User: "ben", Profile: "2d"}
	type want struct {
		kind store.ScopeKind
		unit string
		per  store.Period
		max  float64
	}
	for user, ws := range map[string][]want{
		"ann": {{store.ScopeProfileOverflow, "", store.Month, 2.4}, {store.ScopeProfileSession, "", "rolling:5h", 0.4}, {store.ScopeProfileWeek, "", store.Week, 1.6},
			{store.ScopeProfileOverflow, "USD", store.Month, 39}, {store.ScopeProfileSession, "USD", "rolling:5h", 6.5}, {store.ScopeProfileWeek, "USD", store.Week, 26}},
		"ben": {{store.ScopeProfileSession, "", "rolling:5h", 0.4}, {store.ScopeProfileWeek, "", store.Week, 1.6},
			{store.ScopeProfileSession, "USD", "rolling:5h", 6.5}, {store.ScopeProfileWeek, "USD", store.Week, 26}},
		"cat": {{store.ScopeProfileSession, "", "rolling:5h", 0.2}, {store.ScopeProfileWeek, "", store.Week, 0.2},
			{store.ScopeProfileSession, "USD", "rolling:5h", 3.25}, {store.ScopeProfileWeek, "USD", store.Week, 3.25}},
	} {
		got := map[store.BudgetKey]store.Budget{}
		for _, b := range pr.desired(user) {
			got[b.BudgetKey] = b
		}
		if len(got) != len(ws) {
			t.Fatalf("%s: %+v", user, got)
		}
		for _, w := range ws {
			g, ok := got[store.BudgetKey{Kind: w.kind, Name: user, Unit: w.unit}]
			if !ok || g.Period != w.per || g.MaxCost < w.max-1e-9 || g.MaxCost > w.max+1e-9 {
				t.Errorf("%s: %v %v %v, want %+v", user, ok, g.Period, g.MaxCost, w)
			}
		}
	}
	if pr.desired("") != nil {
		t.Fatal("no person, no profile")
	}
	for d, w := range map[float64]string{0.25: "0.25 day", 1: "1 day", 1.333: "1.33 days", 2: "2 days", 0: "0 days"} {
		if got := fmtDays(d); got != w {
			t.Errorf("fmtDays(%v) = %q, want %q", d, got, w)
		}
	}
	if fmtWindow(5*time.Hour) != "5h" || fmtWindow(90*time.Minute) != "90m" {
		t.Fatal("fmtWindow")
	}
	for _, c := range []struct {
		xs   []float64
		p    float64
		want float64
	}{{[]float64{1, 2, 3, 4}, 0.75, 3}, {[]float64{1, 2, 3, 4, 5, 6, 7, 8}, 0.75, 6}, {[]float64{5}, 0.75, 5}, {nil, 0.5, 0}} {
		if got := percentile(c.xs, c.p); got != c.want {
			t.Errorf("percentile(%v, %v) = %v, want %v", c.xs, c.p, got, c.want)
		}
	}
}

// The profiles pages render, carry the CSRF token, and assign.
func TestProfilesUI(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, profileConfig(up.URL()))
	e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	e.check(nil, []call{{"master", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	// The UI needs an SSO session; the pages' handlers are called directly
	// with the master principal, as the UI does after its session check.
	p := &principal{name: "master", master: true}
	for _, path := range []string{"/ui/profiles", "/ui/profiles/capacity", "/ui/profiles/calibrate"} {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		switch path {
		case "/ui/profiles":
			e.srv.uiProfiles(rec, r, p, "tok")
		case "/ui/profiles/capacity":
			e.srv.uiProfileCapacity(rec, r, p, "tok")
		default:
			e.srv.uiCalibrate(rec, r, p, "tok")
		}
		body := rec.Body.String()
		if rec.Code != http.StatusOK || !strings.Contains(body, "</html>") {
			t.Fatalf("%s: %d %s", path, rec.Code, body)
		}
		if path == "/ui/profiles" && (!strings.Contains(body, "0 / 0.4 day") || !strings.Contains(body, `name="csrf" value="tok"`) || !strings.Contains(body, "<meter")) {
			t.Fatalf("profiles page: %s", body)
		}
	}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/ui/profiles", strings.NewReader("user=alice&profile=big&csrf=tok"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = r.ParseForm()
	e.srv.uiAssignProfile(rec, r, p, "tok")
	if !strings.Contains(rec.Body.String(), "alice: profile big") {
		t.Fatalf("assign: %s", rec.Body.String())
	}
	if a, ok := e.srv.prof.assignment("alice"); !ok || a.Profile != "big" {
		t.Fatalf("%+v", a)
	}
}

// FuzzProfileForms posts arbitrary forms to the assignment and calibration
// handlers: no 5xx, no script tag, nothing stored that is not a configured
// profile of a valid name, and a workday only ever a positive finite number.
func FuzzProfileForms(f *testing.F) {
	up := newFakeVLLM(f)
	e := newEnv(&testing.T{}, profileConfig(up.URL()))
	f.Cleanup(func() { e.gw.Close(); e.srv.Close(context.Background()) })
	f.Add(true, "user=alice&profile=big&overflow=on")
	f.Add(true, "user=%3Cscript%3E&profile=p")
	f.Add(true, "user=a&profile=%3Cscript%3Ealert(1)%3C/script%3E")
	f.Add(true, "user=jwt-0123&profile=")
	f.Add(false, "unit=gpu_hours&amount=NaN")
	f.Add(false, "unit=gpu_hours&amount=Inf")
	f.Add(false, "unit=gpu_hours&amount=1e999")
	f.Add(false, "unit=gpu_hours&amount=-1")
	f.Add(false, "unit=%3Cscript%3E&amount=2")
	f.Add(false, "unit=gpu_hours&amount=0x10p2")
	p := &principal{name: "master", master: true}
	f.Fuzz(func(t *testing.T, assign bool, form string) {
		if len(form) > 2000 {
			return
		}
		vals, err := url.ParseQuery(form)
		if err != nil {
			return
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/ui/profiles", strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			return
		}
		if assign {
			e.srv.uiAssignProfile(rec, r, p, "tok")
		} else {
			e.srv.uiAcceptCalibration(rec, r, p, "tok")
		}
		body := rec.Body.String()
		if rec.Code >= 500 {
			t.Fatalf("%q: %d %s", form, rec.Code, body)
		}
		if strings.Contains(body, "<script") {
			t.Fatalf("%q: a script tag in the page", form)
		}
		if a, ok := e.srv.prof.assignment(vals.Get("user")); ok && a.Profile != "" {
			if _, known := e.srv.cfg.Profiles.Get(a.Profile); !known || !validPerson(a.User) {
				t.Fatalf("stored %+v", a)
			}
		}
		for u, w := range e.srv.prof.workdays() {
			if !(w.Amount > 0) || math.IsInf(w.Amount, 0) || math.IsNaN(w.Amount) {
				t.Fatalf("workday %q = %v", u, w.Amount)
			}
		}
	})
}

// FuzzProfileAPI: whatever the body and the person, an assignment either
// fails as a client error or stores a configured profile for a valid name.
func FuzzProfileAPI(f *testing.F) {
	up := newFakeVLLM(f)
	e := newEnv(&testing.T{}, profileConfig(up.URL()))
	f.Cleanup(func() { e.gw.Close(); e.srv.Close(context.Background()) })
	f.Add("alice", `{"profile":"p"}`)
	f.Add("jwt-x", `{"profile":"p","overflow":true}`)
	f.Add("a/b", `{"profile":"../p"}`)
	f.Add("x", `{"profile":"p","overflow":"yes"}`)
	f.Add("é", `{"profile":""}`)
	f.Fuzz(func(t *testing.T, user, body string) {
		if len(user) > 300 || len(body) > 1000 {
			return
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("PUT", "/admin/users/x/profile", strings.NewReader(body))
		r.SetPathValue("name", user)
		e.srv.setUserProfile(rec, r, &principal{name: "master", master: true})
		if rec.Code >= 500 {
			t.Fatalf("%q %q: %d %s", user, body, rec.Code, rec.Body.String())
		}
		if rec.Code == 200 {
			a, ok := e.srv.prof.assignment(user)
			if !validPerson(user) || ok && !store.ValidProfileID(a.Profile) {
				t.Fatalf("stored %q %+v", user, a)
			}
			if _, known := e.srv.cfg.Profiles.Get(a.Profile); ok && !known {
				t.Fatalf("unknown profile stored: %+v", a)
			}
		}
	})
}
