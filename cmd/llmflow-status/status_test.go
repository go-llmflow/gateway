package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A secret the tests plant everywhere a leak could happen.
const secret = "sk-llmflow-SECRET-0123456789abcdef"

var fixedNow = time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)

func testEnv(vars map[string]string) env {
	return env{
		getenv: func(k string) string { return vars[k] },
		now:    func() time.Time { return fixedNow },
		loc:    time.UTC,
		cache:  func() (string, error) { return "", os.ErrNotExist },
	}
}

// gateway is a fake /v1/me/usage that echoes the credential it was shown in
// every string field it can (a leaky gateway), and counts its calls.
type gateway struct {
	*httptest.Server
	calls atomic.Int64
	seen  atomic.Value // the Authorization header of the last call
	body  string
}

func newGateway(t *testing.T, body string) *gateway {
	g := &gateway{body: body}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls.Add(1)
		g.seen.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/me/usage" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"bad key ` + r.Header.Get("Authorization") + `"}`))
			return
		}
		_, _ = w.Write([]byte(g.body))
	}))
	t.Cleanup(g.Close)
	return g
}

const usageJSON = `{"user":"` + secret + `","profile":{"id":"p","label":"` + secret + `"},"warn_at":[75,95],
 "session":{"used_pct":85.4,"resets_at":"2026-10-29T15:45:00Z"},
 "week":{"used_pct":40,"resets_at":"2026-11-02T00:00:00Z"},"warning":"` + secret + `"}`

func runCmd(t *testing.T, e env, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := rootCmd(e)
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	code = execute(cmd)
	return out.String(), errb.String(), code
}

func vars(g *gateway) map[string]string {
	return map[string]string{"ANTHROPIC_BASE_URL": g.URL, "ANTHROPIC_AUTH_TOKEN": secret, "LANG": "en_US.UTF-8"}
}

func TestGoldenOutputs(t *testing.T) {
	g := newGateway(t, usageJSON)
	cache := t.TempDir()
	stdin := `{"model":{"display_name":"Opus"},"session_id":"x"}`
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"english, no colour": {[]string{"--no-color"}, nil, "session 85% · 15:45 │ week 40% · Mon 00:00"},
		"french by flag":     {[]string{"--no-color", "--lang", "fr"}, nil, "session 85% · 15:45 │ semaine 40% · lun. 00:00"},
		"french by locale":   {[]string{"--no-color"}, map[string]string{"LANG": "fr_FR.UTF-8"}, "session 85% · 15:45 │ semaine 40% · lun. 00:00"},
		"NO_COLOR":           {nil, map[string]string{"NO_COLOR": "1"}, "session 85% · 15:45 │ week 40% · Mon 00:00"},
		// 85 % is past 75 but not 95: yellow; 40 %: green.
		"coloured": {nil, nil, yellow + "session 85% · 15:45" + reset + " │ " + green + "week 40% · Mon 00:00" + reset},
	} {
		v := vars(g)
		for k, x := range tc.env {
			v[k] = x
		}
		out, errs, code := runCmd(t, testEnv(v), stdin, append([]string{"--cache-dir", cache, "--ttl", "0"}, tc.args...)...)
		if code != 0 || errs != "" || strings.TrimRight(out, "\n") != tc.want {
			t.Errorf("%s: %q (stderr %q, exit %d), want %q", name, out, errs, code, tc.want)
		}
	}
	// Past the last threshold: red.
	if c := colour([]int{75, 95}, 96); c != red {
		t.Error("red past 95")
	}
	if c := colour([]int{50}, 60); c != red {
		t.Error("a single threshold passed is red")
	}
	if c := colour([]int{}, 99); c != "" {
		t.Error("warnings off: no colour")
	}
	if c := colour([]int{50, 75, 95}, 80); c != yellow {
		t.Error("between: yellow")
	}
}

// The contract with the gateway: testdata/me_usage.contract.json holds the
// fields this command reads, and the gateway's TestMeUsageContract checks
// that its real /v1/me/usage carries each of them with the same JSON type.
// Here the same file must render, so a fake cannot drift from the server.
func TestContractFixtureRenders(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "me_usage.contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := newGateway(t, string(body))
	out, errs, code := runCmd(t, testEnv(vars(g)), "{}", "--cache-dir", t.TempDir(), "--ttl", "0", "--no-color")
	if want := "session 85% · 15:45 │ week 40% · Mon 00:00"; code != 0 || errs != "" || strings.TrimRight(out, "\n") != want {
		t.Fatalf("%q (stderr %q, exit %d), want %q", out, errs, code, want)
	}
}

func TestNothingWithoutWindows(t *testing.T) {
	g := newGateway(t, `{"user":"alice","profile":null,"warn_at":[75,95],"session":null,"week":null}`)
	out, errs, code := runCmd(t, testEnv(vars(g)), "{}", "--cache-dir", t.TempDir())
	if out != "" || errs != "" || code != 0 {
		t.Fatalf("%q %q %d", out, errs, code)
	}
}

// Any failure prints nothing, exits 0, and is quick: no credential, a wrong
// one, an unreachable gateway, a slow one, garbage, an oversize answer, a
// redirect, a URL that is not acceptable.
func TestFailuresPrintNothingQuickly(t *testing.T) {
	good := newGateway(t, usageJSON)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	defer slow.Close()
	garbage := newGateway(t, `{"session": [`)
	huge := newGateway(t, `{"session":null,"pad":"`+strings.Repeat("a", 2*maxResponse)+`"}`)
	var elsewhereAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhereAuth.Store(r.Header.Get("Authorization")) }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/steal", 302) }))
	defer redirect.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"no credential":        {map[string]string{"ANTHROPIC_BASE_URL": good.URL}, nil},
		"wrong credential":     {map[string]string{"ANTHROPIC_BASE_URL": good.URL, "ANTHROPIC_AUTH_TOKEN": "sk-wrong"}, nil},
		"no URL":               {map[string]string{"ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"unreachable":          {map[string]string{"ANTHROPIC_BASE_URL": deadURL, "ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"slow":                 {map[string]string{"ANTHROPIC_BASE_URL": slow.URL, "ANTHROPIC_AUTH_TOKEN": secret}, []string{"--timeout", "200ms"}},
		"garbage":              {vars(garbage), nil},
		"oversize":             {vars(huge), nil},
		"redirect":             {map[string]string{"ANTHROPIC_BASE_URL": redirect.URL, "ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"http off the box":     {map[string]string{"ANTHROPIC_BASE_URL": "http://gateway.example.org", "ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"user info":            {map[string]string{"ANTHROPIC_BASE_URL": "https://u:p@gateway.example.org", "ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"unreadable URL":       {map[string]string{"ANTHROPIC_BASE_URL": "://", "ANTHROPIC_AUTH_TOKEN": secret}, nil},
		"credential file gone": {map[string]string{"ANTHROPIC_BASE_URL": good.URL}, []string{"--token-file", "/nonexistent/token"}},
	} {
		t0 := time.Now()
		out, errs, code := runCmd(t, testEnv(tc.env), "not even json", append([]string{"--cache-dir", t.TempDir()}, tc.args...)...)
		if out != "" || errs != "" || code != 0 {
			t.Errorf("%s: stdout %q stderr %q exit %d", name, out, errs, code)
		}
		if d := time.Since(t0); d > 2500*time.Millisecond {
			t.Errorf("%s took %v", name, d)
		}
		if strings.Contains(out+errs, secret) {
			t.Errorf("%s: the credential was printed", name)
		}
	}
	if v := elsewhereAuth.Load(); v != nil {
		t.Fatalf("the redirect was followed, and carried %q", v)
	}
	// --verbose says why, and still never the credential.
	out, errs, _ := runCmd(t, testEnv(map[string]string{"ANTHROPIC_BASE_URL": good.URL, "ANTHROPIC_AUTH_TOKEN": "sk-wrong"}), "{}", "--verbose", "--cache-dir", t.TempDir())
	if out != "" || !strings.Contains(errs, "401") || strings.Contains(errs, "sk-wrong") {
		t.Fatalf("verbose: %q %q", out, errs)
	}
}

// The credential is never in what the program prints, in any mode, nor in
// the cache (the planted test): the fake gateway echoes it into the user, the
// profile label, the warning and its error bodies, and the program is run
// every way it can be.
func TestNoCredentialInOutputOrCache(t *testing.T) {
	g := newGateway(t, usageJSON)
	cache := t.TempDir()
	for _, args := range [][]string{{}, {"--no-color"}, {"--lang", "fr"}, {"--verbose"}, {"--ttl", "0"}} {
		out, errs, _ := runCmd(t, testEnv(vars(g)), `{"session_id":"`+secret+`"}`, append([]string{"--cache-dir", cache}, args...)...)
		if out == "" {
			t.Fatalf("%v: nothing printed", args)
		}
		if leaks(secret, out, errs) {
			t.Fatalf("%v: the credential is in the output: %q %q", args, out, errs)
		}
	}
	// A wrong credential makes the gateway echo it in an error body.
	_, errs, _ := runCmd(t, testEnv(map[string]string{"ANTHROPIC_BASE_URL": g.URL, "ANTHROPIC_AUTH_TOKEN": "sk-other-SECRET"}), "{}", "--verbose", "--cache-dir", cache)
	if leaks("sk-other-SECRET", errs) {
		t.Fatalf("stderr: %q", errs)
	}
	// Nothing in the cache directory holds it, neither in a name nor in a body.
	n := 0
	err := filepath.Walk(cache, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		n++
		if leaks(secret, filepath.Base(p)) {
			t.Errorf("the credential is in a file name: %s", p)
		}
		if !info.IsDir() {
			b, _ := os.ReadFile(p)
			if leaks(secret, string(b)) || leaks("llmflow-SECRET", string(b)) {
				t.Errorf("the credential is in %s: %s", p, b)
			}
			var c cached
			if json.Unmarshal(b, &c) != nil {
				t.Errorf("cache file unreadable: %s", b)
			}
		}
		return nil
	})
	if err != nil || n < 2 {
		t.Fatalf("cache directory walk: %d entries, %v", n, err)
	}
	// The planted positive: a leaky renderer is caught by the same check, so
	// the checks above can see a leak.
	if !leaks(secret, leakyRender(secret)) {
		t.Fatal("the leak check cannot see a leak")
	}
	if leaks(secret, "session 85% · 15:45") {
		t.Fatal("the leak check sees a leak in a clean line")
	}
}

func leaks(secret string, outs ...string) bool {
	for _, o := range outs {
		if strings.Contains(o, secret) || strings.Contains(o, strings.TrimPrefix(secret, "sk-")) {
			return true
		}
	}
	return false
}

// leakyRender is what a careless version would do: print the user field.
func leakyRender(cred string) string { return "session 85% · " + cred }

// The cache: reused within --ttl, refreshed after it, per gateway and
// credential, the file 0600 in a 0700 directory.
func TestCache(t *testing.T) {
	g := newGateway(t, usageJSON)
	cache := filepath.Join(t.TempDir(), "sub", "llmflow-status")
	now := fixedNow
	e := testEnv(vars(g))
	e.now = func() time.Time { return now }
	run := func(extra ...string) string {
		out, _, _ := runCmd(t, e, "{}", append([]string{"--cache-dir", cache, "--no-color"}, extra...)...)
		return out
	}
	first := run()
	if first == "" || g.calls.Load() != 1 {
		t.Fatalf("%q after %d calls", first, g.calls.Load())
	}
	now = fixedNow.Add(20 * time.Second)
	if run() != first || g.calls.Load() != 1 {
		t.Fatalf("within the ttl the gateway was called again (%d)", g.calls.Load())
	}
	now = fixedNow.Add(31 * time.Second)
	if run() != first || g.calls.Load() != 2 {
		t.Fatalf("after the ttl it was not (%d)", g.calls.Load())
	}
	now = fixedNow.Add(-time.Hour) // a clock that went back: not fresh
	run()
	if g.calls.Load() != 3 {
		t.Fatalf("an entry from the future was reused (%d)", g.calls.Load())
	}
	now = fixedNow
	// Another credential does not read this one's answer.
	e2 := testEnv(map[string]string{"ANTHROPIC_BASE_URL": g.URL, "ANTHROPIC_AUTH_TOKEN": "sk-other"})
	e2.now = e.now
	runCmd(t, e2, "{}", "--cache-dir", cache)
	if g.calls.Load() != 4 {
		t.Fatalf("a cache shared between credentials (%d calls)", g.calls.Load())
	}
	// Permissions (Unix).
	if runtime.GOOS != "windows" {
		di, err := os.Stat(cache)
		if err != nil || di.Mode().Perm() != 0o700 {
			t.Fatalf("cache dir %v %v", di, err)
		}
		ents, _ := os.ReadDir(cache)
		files := 0
		for _, ent := range ents {
			fi, _ := ent.Info()
			if fi.Mode().Perm() != 0o600 {
				t.Errorf("%s is %v", ent.Name(), fi.Mode().Perm())
			}
			if strings.HasSuffix(ent.Name(), ".tmp") {
				t.Errorf("a temporary file was left: %s", ent.Name())
			}
			files++
		}
		if files != 1 { // the other credential was refused: nothing of it is cached
			t.Fatalf("%d cache files, want 1", files)
		}
	}
	// A corrupt or huge cache is a miss, not a failure.
	for _, ent := range func() []os.DirEntry { d, _ := os.ReadDir(cache); return d }() {
		_ = os.WriteFile(filepath.Join(cache, ent.Name()), []byte("{{{"), 0o600)
	}
	if run() != first {
		t.Fatal("a corrupt cache broke the line")
	}
	// An unwritable cache location costs requests, not the line.
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, nil, 0o600)
	out, _, _ := runCmd(t, e, "{}", "--cache-dir", filepath.Join(blocker, "x"), "--no-color")
	if out != first {
		t.Fatalf("unwritable cache: %q", out)
	}
}

func TestCredentialFromFileAndOrder(t *testing.T) {
	g := newGateway(t, usageJSON)
	f := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(f, []byte(secret+"\n"), 0o600)
	v := map[string]string{"ANTHROPIC_BASE_URL": g.URL}
	if out, _, _ := runCmd(t, testEnv(v), "{}", "--token-file", f, "--cache-dir", t.TempDir()); out == "" {
		t.Fatal("token file not used")
	}
	// ANTHROPIC_AUTH_TOKEN before ANTHROPIC_API_KEY, as Claude Code sends them.
	v["ANTHROPIC_AUTH_TOKEN"], v["ANTHROPIC_API_KEY"] = secret, "sk-ignored"
	if out, _, _ := runCmd(t, testEnv(v), "{}", "--cache-dir", t.TempDir()); out == "" || g.seen.Load() != "Bearer "+secret {
		t.Fatalf("%q %v", out, g.seen.Load())
	}
	v["ANTHROPIC_AUTH_TOKEN"] = ""
	v["ANTHROPIC_API_KEY"] = secret
	if out, _, _ := runCmd(t, testEnv(v), "{}", "--cache-dir", t.TempDir()); out == "" {
		t.Fatal("ANTHROPIC_API_KEY not used")
	}
}

func TestBadFlagIsAUsageError(t *testing.T) {
	_, errs, code := runCmd(t, testEnv(nil), "{}", "--nope")
	if code != 2 || !strings.Contains(errs, "unknown flag") {
		t.Fatalf("%d %q", code, errs)
	}
}

// FuzzRender: whatever the gateway answers, rendering never panics, never
// prints the credential it was not given, and is one line.
func FuzzRender(f *testing.F) {
	f.Add(usageJSON)
	f.Add(`{}`)
	f.Add(`{"warn_at":[],"session":{"used_pct":-5,"resets_at":"0001-01-01T00:00:00Z"}}`)
	f.Add(`{"week":{"used_pct":1e308,"resets_at":"2026-01-01T00:00:00Z"}}`)
	f.Fuzz(func(t *testing.T, body string) {
		var u usage
		if json.Unmarshal([]byte(body), &u) != nil {
			return
		}
		out := render(u, options{}, testEnv(nil))
		if strings.ContainsAny(out, "\r\n") {
			t.Fatalf("not one line: %q", out)
		}
	})
}
