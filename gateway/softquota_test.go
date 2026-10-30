package gateway

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
	"github.com/jackc/pgx/v5"
)

// softConfig is profileConfig with a threshold low enough for two requests
// (0.164 each against 0.4 of a workday: 41 % and 82 % of the week).
func softConfig(url string) *config.Config {
	cfg := profileConfig(url)
	cfg.SoftQuotas = config.SoftQuotas{WarnAt: []int{30}, Language: "en", Timezone: "UTC", Notify: []string{config.NotifyUser}}
	return cfg
}

type meUsageOut struct {
	User    string `json:"user"`
	Profile *struct {
		ID string `json:"id"`
	} `json:"profile"`
	WarnAt  []int `json:"warn_at"`
	Session *struct {
		UsedPct  float64   `json:"used_pct"`
		ResetsAt time.Time `json:"resets_at"`
		Level    int       `json:"warn_level"`
	} `json:"session"`
	Week *struct {
		UsedPct  float64   `json:"used_pct"`
		ResetsAt time.Time `json:"resets_at"`
		Level    int       `json:"warn_level"`
	} `json:"week"`
	Warning string `json:"warning"`
	Budgets []struct {
		WarnAt []int `json:"warn_at"`
	} `json:"budgets"`
}

func (e *env) meUsage(key string, hdr ...string) (int, http.Header, meUsageOut, string) {
	e.t.Helper()
	resp := e.do("GET", "/v1/me/usage", key, "", hdr...)
	body := readAll(e.t, resp)
	var out meUsageOut
	if resp.StatusCode == 200 {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			e.t.Fatalf("%v: %s", err, body)
		}
	}
	return resp.StatusCode, resp.Header, out, body
}

// A person's session and week on every successful response, a warning once a
// threshold is passed (in the request's language), the same numbers at
// /v1/me/usage; they name the caller's own windows only.
func TestSoftQuotaHeadersAndUsage(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, softConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	bob := e.adminToken("bob", `{"role":"member","team":"etabA"}`)
	ta := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	e.check(map[string]string{"ta": ta}, []call{
		{"ta", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200},
		{"ta", "PUT", "/admin/users/bob/profile", `{"profile":"p"}`, 200},
	})
	ka, kb := e.ownKey(alice), e.ownKey(bob)
	week := store.Week.ResetAt(time.Now()).UTC().Format(time.RFC3339)

	resp := e.do("POST", "/v1/chat/completions", ka, chat20)
	readAll(t, resp)
	h := resp.Header
	if resp.StatusCode != 200 || h.Get(hdrUsageSession) != "0" || h.Get(hdrUsageWeek) != "0" || h.Get(hdrUsageWeekReset) != week || h.Get(hdrUsageWarning) != "" {
		t.Fatalf("first request: %d %v", resp.StatusCode, h)
	}
	if _, err := time.Parse(time.RFC3339, h.Get(hdrUsageSessionReset)); err != nil {
		t.Fatalf("session reset %q", h.Get(hdrUsageSessionReset))
	}
	e.waitRecords(1)
	// Past the threshold (41 % >= 30 %): the warning, in English and in French.
	resp = e.do("POST", "/v1/chat/completions", ka, chat20)
	readAll(t, resp)
	pct, _ := strconv.Atoi(resp.Header.Get(hdrUsageWeek))
	if resp.StatusCode != 200 || pct < 40 || pct > 42 ||
		!strings.HasPrefix(resp.Header.Get(hdrUsageWarning), "You've used ") ||
		!strings.HasSuffix(resp.Header.Get(hdrUsageWarning), " of your weekly allowance · resets Mon 00:00") {
		t.Fatalf("second request: %d %v", resp.StatusCode, resp.Header)
	}
	// The French text and the language choice (a third request would not fit
	// the week: the wording is checked on the function, the refusal's French
	// end to end in TestSoftQuotaRefusalWording).
	mon := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if got := e.srv.warnText(windowWeek, 85.7, mon, "fr"); got != "Vous avez utilisé 85 % de votre quota hebdomadaire · réinitialisation lun. 00:00" {
		t.Fatalf("%q", got)
	}
	if got := e.srv.warnText(windowSession, 85, time.Date(2026, 10, 29, 15, 45, 0, 0, time.UTC), "fr"); got != "Vous avez utilisé 85 % de votre quota de session · réinitialisation à 15:45" {
		t.Fatalf("%q", got)
	}
	if got := e.srv.warnText(windowSession, 85, time.Date(2026, 10, 29, 15, 45, 0, 0, time.UTC), "en"); got != "You've used 85% of your session allowance · resets 15:45" {
		t.Fatalf("%q", got)
	}
	for in, want := range map[string]string{"fr-FR,fr;q=0.9,en;q=0.5": "fr", "en-GB": "en", "de, fr;q=0.5": "fr", "de": "en", "": "en"} {
		r := httptest.NewRequest("GET", "/", nil)
		if in != "" {
			r.Header.Set("Accept-Language", in)
		}
		if got := e.srv.langOf(r); got != want {
			t.Fatalf("Accept-Language %q: %s, want %s", in, got, want)
		}
	}

	// /v1/me/usage: the caller's own windows, never another's.
	st, hh, me, body := e.meUsage(ka)
	if st != 200 || me.User != "alice" || me.Profile == nil || me.Profile.ID != "p" || me.Week == nil || me.Session == nil ||
		me.Week.UsedPct < 40 || me.Week.Level != 30 || me.Week.ResetsAt.UTC().Format(time.RFC3339) != week ||
		len(me.WarnAt) != 1 || me.WarnAt[0] != 30 || !strings.Contains(me.Warning, "weekly allowance") {
		t.Fatalf("alice: %d %s", st, body)
	}
	if cc := hh.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	st, _, bobOut, body := e.meUsage(kb)
	if st != 200 || bobOut.User != "bob" || bobOut.Week == nil || bobOut.Week.UsedPct != 0 || bobOut.Warning != "" || strings.Contains(body, "alice") {
		t.Fatalf("bob sees %d %s", st, body)
	}
	// What the response headers of bob's request say is bob's: nothing of alice's.
	resp = e.do("POST", "/v1/chat/completions", kb, chat20)
	readAll(t, resp)
	if resp.Header.Get(hdrUsageWeek) != "0" || resp.Header.Get(hdrUsageWarning) != "" {
		t.Fatalf("bob's headers carry alice's usage: %v", resp.Header)
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			if strings.Contains(v, "alice") {
				t.Fatalf("header %s names another person: %q", k, v)
			}
		}
	}
	// A key without an owner has no person: no windows, still no cache.
	_, svc := e.newKey("etabA")
	st, hh, o, body := e.meUsage(svc)
	if st != 200 || o.User != "" || o.Week != nil || o.Session != nil || hh.Get("Cache-Control") != "no-store" {
		t.Fatalf("service key: %d %s", st, body)
	}
	// Refusals are no-store too, and a wrong key learns nothing.
	st, hh, _, body = e.meUsage("sk-wrong")
	if st != 401 || hh.Get("Cache-Control") != "no-store" || strings.Contains(body, "alice") {
		t.Fatalf("wrong key: %d %v %s", st, hh, body)
	}
	if st, _, _, _ := e.meUsage(""); st != 401 {
		t.Fatalf("no key: %d", st)
	}
}

// The usage endpoint is rate limited per key.
func TestSoftQuotaUsageRateLimited(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, softConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	ka := e.ownKey(alice)
	var limited int
	for i := 0; i < meUsageRPM+5; i++ {
		st, hh, _, _ := e.meUsage(ka)
		if st == http.StatusTooManyRequests {
			limited++
			if hh.Get("Retry-After") == "" {
				t.Fatal("no Retry-After")
			}
		}
	}
	if limited != 5 {
		t.Fatalf("%d requests refused, want 5 above %d a minute", limited, meUsageRPM)
	}
	// Another key is not limited by this one's.
	kb := e.ownKey(e.adminToken("bob", `{"role":"member","team":"etabA"}`))
	if st, _, _, _ := e.meUsage(kb); st != 200 {
		t.Fatalf("another key limited: %d", st)
	}
}

// The hard limit in Anthropic's words, in the request's language, with
// retry-after in integer seconds and x-should-retry: false.
func TestSoftQuotaRefusalWording(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, softConfig(up.URL()))
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	ta := e.adminToken("ta", `{"role":"team_admin","team":"etabA"}`)
	e.check(map[string]string{"ta": ta}, []call{{"ta", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200}})
	ka := e.ownKey(alice)
	for i := 0; i < 2; i++ {
		readAll(t, e.do("POST", "/v1/chat/completions", ka, chat20))
	}
	e.waitRecords(2)
	e.srv.escrow.forgetRoom()
	resp := e.do("POST", "/v1/chat/completions", ka, chat20, "Accept-Language", "fr")
	out := readAll(t, resp)
	if resp.StatusCode != 429 || !strings.Contains(out, "Vous avez atteint votre quota hebdomadaire · réinitialisation lun. 00:00") {
		t.Fatalf("French refusal: %d %s", resp.StatusCode, out)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 || resp.Header.Get("X-Should-Retry") != "false" || resp.Header.Get(budgetResetHeader) == "" {
		t.Fatalf("headers of the refusal: %v", resp.Header)
	}
	resp = e.do("POST", "/v1/chat/completions", ka, chat20)
	if out := readAll(t, resp); !strings.Contains(out, "You've hit your weekly allowance · resets Mon 00:00") {
		t.Fatalf("English refusal: %s", out)
	}
}

// The chain gateway -> org -> profile -> budget with runtime overrides: what
// /v1/me/usage reports as in force follows each change, which is audited.
func TestSoftQuotaThresholdChainAtRuntime(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := softConfig(up.URL())
	cfg.SoftQuotas.WarnAt = nil // omitted: Anthropic's 75 and 95
	e := newEnv(t, cfg)
	e.check(nil, []call{{"master", "POST", "/admin/orgs", `{"name":"orgC"}`, 201}})
	alice := e.adminToken("alice", `{"role":"member","team":"etabA"}`)
	oadmin := e.adminToken("oadmin", `{"role":"org_admin","org":"etabA"}`)
	oadminC := e.adminToken("oadminC", `{"role":"org_admin","org":"orgC"}`)
	e.check(map[string]string{"oadmin": oadmin}, []call{
		{"oadmin", "PUT", "/admin/users/alice/profile", `{"profile":"p"}`, 200},
	})
	ka := e.ownKey(alice)
	warn := func() []int {
		t.Helper()
		_, _, me, body := e.meUsage(ka)
		if me.Profile == nil {
			t.Fatalf("no profile: %s", body)
		}
		return me.WarnAt
	}
	same := func(got []int, want ...int) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := warn(); !same(got, 75, 95) {
		t.Fatalf("omitted: %v", got)
	}
	e.check(map[string]string{"oadmin": oadmin, "oadminC": oadminC}, []call{
		{"master", "PUT", "/admin/soft-quotas/gateway", `{"warn_at":[70]}`, 200},
	})
	if got := warn(); !same(got, 70) {
		t.Fatalf("gateway: %v", got)
	}
	e.check(map[string]string{"oadmin": oadmin, "oadminC": oadminC}, []call{
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/etabA", `{"warn_at":[80,95]}`, 200},
		{"oadminC", "PUT", "/admin/soft-quotas/orgs/etabA", `{"warn_at":[1]}`, 403}, // another organisation's admin
		{"oadmin", "PUT", "/admin/soft-quotas/gateway", `{"warn_at":[1]}`, 403},     // not the gateway's
		{"oadmin", "PUT", "/admin/soft-quotas/profiles/p", `{"warn_at":[1]}`, 403},
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/etabA", `{"warn_at":[95,80]}`, 400}, // not increasing
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/etabA", `{"warn_at":[0]}`, 400},
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/etabA", `{"warn_at":[1,2,3,4,5,6]}`, 400},
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/etabA", `{}`, 400}, // warn_at is required
		{"oadmin", "PUT", "/admin/soft-quotas/orgs/nowhere", `{"warn_at":[50]}`, 403},
		{"master", "PUT", "/admin/soft-quotas/orgs/nowhere", `{"warn_at":[50]}`, 404},
	})
	if got := warn(); !same(got, 80, 95) {
		t.Fatalf("org: %v", got)
	}
	e.check(nil, []call{{"master", "PUT", "/admin/soft-quotas/profiles/p", `{"warn_at":[50,75,95]}`, 200}})
	if got := warn(); !same(got, 50, 75, 95) {
		t.Fatalf("profile: %v", got)
	}
	e.check(nil, []call{{"master", "PUT", "/admin/soft-quotas/profiles/p", `{"warn_at":[]}`, 200}})
	if got := warn(); len(got) != 0 {
		t.Fatalf("off at the profile: %v", got)
	}
	e.check(nil, []call{{"master", "DELETE", "/admin/soft-quotas/profiles/p", "", 204}})
	if got := warn(); !same(got, 80, 95) {
		t.Fatalf("profile override removed, the org's applies again: %v", got)
	}
	// Every change is in the audit log.
	resp := e.do("GET", "/admin/audit", master, "")
	if out := readAll(t, resp); !strings.Contains(out, "soft_quota.levels") {
		t.Fatalf("no audit event: %s", out)
	}
	// A user budget's own list wins over everything.
	e.check(nil, []call{
		{"master", "PUT", "/admin/budgets", `{"kind":"user","name":"alice","period":"day","max_cost":100,"warn_at":[10]}`, 200},
		{"master", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":100,"warn_at":[10]}`, 400},
		{"master", "PUT", "/admin/budgets", `{"kind":"user","name":"bob","period":"day","max_cost":100,"warn_at":[75,50]}`, 400},
	})
	_, _, me, body := e.meUsage(ka)
	if len(me.Budgets) != 1 || !same(me.Budgets[0].WarnAt, 10) || len(me.WarnAt) != 2 {
		t.Fatalf("a budget's own thresholds: %s", body)
	}
}

// A threshold fires once per window instance; a rolling window fires again
// only after its usage fell below the threshold minus the hysteresis; a
// calendar window fires again in the next instance.
func TestSoftQuotaFiresOncePerInstance(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, softConfig(up.URL()))
	var mu sync.Mutex
	var fired []string
	e.srv.warn.fired = func(user, window string, th int) {
		mu.Lock()
		fired = append(fired, user+"/"+window+"/"+strconv.Itoa(th))
		mu.Unlock()
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(fired) }
	ctx := context.Background()
	levels := []int{75, 95}
	sess := func(pct float64) winUse {
		return winUse{Window: windowSession, Pct: pct, Rolling: true, Reset: time.Now().Add(time.Hour)}
	}
	step := func(pct float64, want int, what string) {
		t.Helper()
		e.srv.warnWindow(ctx, "alice", sess(pct), levels)
		if got := count(); got != want {
			t.Fatalf("%s: %d notifications, want %d (%v)", what, got, want, fired)
		}
	}
	step(10, 0, "below")
	step(80, 1, "passes 75")
	step(81, 1, "stays above")
	step(72, 1, "falls but not below 70: not re-armed")
	step(80, 1, "back above: still the same episode")
	step(60, 1, "below 70: re-armed")
	step(80, 2, "passes 75 again")
	step(96, 3, "passes 95 (75 already fired, only the highest crossed is told)")
	step(96, 3, "stays")
	// Both thresholds crossed at once: one notification, for the highest.
	e.srv.warnWindow(ctx, "bob", sess(99), levels)
	if got := count(); got != 4 || fired[3] != "bob/session/95" {
		t.Fatalf("two thresholds at once: %v", fired)
	}
	// A calendar window: once per instance.
	w1 := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	week := func(start time.Time, pct float64) winUse {
		return winUse{Window: windowWeek, Pct: pct, Start: start, Reset: start.AddDate(0, 0, 7)}
	}
	e.srv.warnWindow(ctx, "carol", week(w1, 80), levels)
	e.srv.warnWindow(ctx, "carol", week(w1, 90), levels)
	n := count()
	e.srv.warnWindow(ctx, "carol", week(w1, 30), levels) // usage falls: a calendar window does not re-arm
	e.srv.warnWindow(ctx, "carol", week(w1, 80), levels)
	if count() != n {
		t.Fatalf("a calendar window fired twice in one instance: %v", fired)
	}
	e.srv.warnWindow(ctx, "carol", week(w1.AddDate(0, 0, 7), 80), levels)
	if count() != n+1 {
		t.Fatalf("next week did not fire: %v", fired)
	}
	// Warnings off: nothing, whatever the usage.
	e.srv.warnWindow(ctx, "dave", week(w1, 100), []int{})
	if count() != n+1 {
		t.Fatalf("[] still fired: %v", fired)
	}
}

// Webhook (with its idempotency key) and one mail per recipient: the person,
// and their team admin when notify says so.
func TestSoftQuotaNotifies(t *testing.T) {
	var mu sync.Mutex
	var hooks []map[string]any
	var keys []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		hooks, keys = append(hooks, m), append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
	}))
	defer hook.Close()
	up := newFakeVLLM(t)
	cfg := softConfig(up.URL())
	cfg.Alerts.WebhookURL = hook.URL
	cfg.Alerts.SMTP = &config.SMTP{Server: "relay.example:25", From: "llmflow@uni.fr"}
	cfg.SoftQuotas.Notify = []string{config.NotifyUser, config.NotifyTeamAdmin}
	cfg.SoftQuotas.Language = config.LangFR
	e := newEnv(t, cfg)
	e.adminToken("alice@uni.fr", `{"role":"member","team":"etabA"}`)
	e.adminToken("boss@uni.fr", `{"role":"team_admin","team":"etabA"}`)
	e.adminToken("plainname", `{"role":"member","team":"etabA"}`)
	type mailed struct{ to, msg string }
	var mails []mailed
	e.srv.alerts.sendMail = func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		mu.Lock()
		mails = append(mails, mailed{strings.Join(to, ","), string(msg)})
		mu.Unlock()
		return nil
	}
	w := winUse{Window: windowWeek, Pct: 82, Start: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Reset: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	e.srv.warnWindow(context.Background(), "alice@uni.fr", w, []int{75})
	e.srv.warnWindow(context.Background(), "plainname", w, []int{75})
	eventually(t, 5*time.Second, "webhook and mails", func() bool { mu.Lock(); defer mu.Unlock(); return len(hooks) == 2 && len(mails) == 3 })
	mu.Lock()
	defer mu.Unlock()
	if keys[0] == "" || keys[0] == keys[1] {
		t.Fatalf("idempotency keys %q", keys)
	}
	var h map[string]any
	for _, x := range hooks {
		if x["user"] == "alice@uni.fr" {
			h = x
		}
	}
	if h["type"] != "soft_quota" || h["user"] != "alice@uni.fr" || h["window"] != "week" || h["threshold"] != float64(75) ||
		!strings.Contains(h["message"].(string), "Vous avez utilisé 82 % de votre quota hebdomadaire") {
		t.Fatalf("webhook body %v", h)
	}
	got := map[string]bool{}
	for _, m := range mails {
		got[m.to] = true
		if !strings.Contains(m.msg, "Vous avez utilisé 82 %") || strings.Contains(m.msg, "\r\nBcc:") {
			t.Fatalf("mail to %s: %q", m.to, m.msg)
		}
	}
	// alice@uni.fr and her team admin (twice for alice: she is the person and, for
	// plainname, only the admin is an address).
	if !got["alice@uni.fr"] || !got["boss@uni.fr"] || len(mails) != 3 {
		t.Fatalf("mails %v", mails)
	}
}

// The three-replica property on a real PostgreSQL: whatever the replicas do
// at once, and however often one is killed (its memory of what it settled
// lost), a threshold fires exactly once per window instance and once per
// episode of a rolling window, and the webhook receives each once.
//
// Planted control: the same run with the store's decision skipped (noDedup:
// every replica that sees a crossing fires it) must fire more than once, or
// the test could not see a duplicate.
func TestSoftQuotaOnceAcrossReplicasPG(t *testing.T) {
	const weeks, cycles = 4, 3
	run := func(t *testing.T, control bool) (fired int, hooks map[string]int) {
		var hmu sync.Mutex
		hooks = map[string]int{}
		hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hmu.Lock()
			hooks[r.Header.Get("Idempotency-Key")]++
			hmu.Unlock()
		}))
		defer hook.Close()
		up := newFakeVLLM(t)
		cfg := softConfig(up.URL())
		cfg.Alerts.WebhookURL = hook.URL
		direct, _ := freshDB(t, "")
		reps := []*env{haEnv(t, cfg, direct, "a", WithWarnEvery(time.Hour)), haEnv(t, cfg, direct, "b", WithWarnEvery(time.Hour)), haEnv(t, cfg, direct, "c", WithWarnEvery(time.Hour))}
		var n atomic.Int64
		for _, r := range reps {
			r.srv.warn.noDedup = control
			r.srv.warn.fired = func(string, string, int) { n.Add(1) }
		}
		ctx := context.Background()
		kill := func(r *env) { // a killed replica comes back knowing nothing of what it settled
			r.srv.warn.mu.Lock()
			r.srv.warn.local, r.srv.warn.checked = map[string]time.Time{}, map[string]bool{}
			r.srv.warn.mu.Unlock()
		}
		burst := func(w winUse, pct float64, levels []int) {
			w.Pct = pct
			var wg sync.WaitGroup
			for _, r := range reps {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < 5; i++ {
						r.srv.warnWindow(ctx, "alice", w, levels)
						if rand.IntN(3) == 0 { // the package-level generator is safe for concurrent use
							kill(r)
						}
					}
				}()
			}
			wg.Wait()
		}
		start := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
		for k := 0; k < weeks; k++ { // one fire per ISO week
			w := winUse{Window: windowWeek, Start: start.AddDate(0, 0, 7*k), Reset: start.AddDate(0, 0, 7*k+7)}
			burst(w, 80, []int{75, 95})
			burst(w, 50, []int{75, 95}) // usage falls: a calendar window does not re-arm
			burst(w, 80, []int{75, 95})
		}
		sess := winUse{Window: windowSession, Rolling: true, Reset: time.Now().Add(time.Hour)}
		for k := 0; k < cycles; k++ { // one fire per episode
			burst(sess, 80, []int{75})
			burst(sess, 72, []int{75}) // above the hysteresis bound (70): same episode
			burst(sess, 80, []int{75})
			burst(sess, 40, []int{75}) // below it: armed again
		}
		time.Sleep(100 * time.Millisecond)
		// Let the outbox deliver (every replica runs its loop).
		eventually(t, 15*time.Second, "outbox drained", func() bool {
			var left int
			c, err := pgx.Connect(ctx, direct)
			if err != nil {
				return false
			}
			defer c.Close(ctx)
			if err := c.QueryRow(ctx, `SELECT count(*) FROM llmflow_alert_outbox WHERE state = 'pending'`).Scan(&left); err != nil {
				return false
			}
			return left == 0
		})
		return int(n.Load()), hooks
	}
	want := weeks + cycles
	fired, hooks := run(t, false)
	if fired != want {
		t.Fatalf("fired %d times over %d week instances and %d episodes, want %d", fired, weeks, cycles, want)
	}
	// One delivery per distinct window instance (the rolling episodes of one
	// 5-minute bucket share their idempotency key: the receiver de-duplicates).
	for k, c := range hooks {
		if c != 1 {
			t.Fatalf("webhook key %s delivered %d times", k, c)
		}
	}
	if len(hooks) < weeks || len(hooks) > want {
		t.Fatalf("%d webhook deliveries (%v), want %d to %d", len(hooks), hooks, weeks, want)
	}
	if cfired, _ := run(t, true); cfired <= want {
		t.Fatalf("control: %d fires without the store's decision, want more than %d: the test cannot see a duplicate", cfired, want)
	}
}
