package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bmc is a fake Redfish service over TLS.
type bmc struct {
	mu       sync.Mutex
	routes   map[string]string // path -> JSON
	status   map[string]int
	huge     map[string]bool
	logins   int
	logouts  int
	basic    int
	tokens   int
	user     string
	password string
	srv      *httptest.Server
	caFile   string
}

func newBMC(t *testing.T, routes map[string]string) *bmc {
	t.Helper()
	b := &bmc{routes: routes, status: map[string]int{}, huge: map[string]bool{}, user: "admin", password: "s3cret"}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	b.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(b.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *bmc) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method == http.MethodPost && r.URL.Path == "/redfish/v1/SessionService/Sessions" {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["UserName"] != b.user || in["Password"] != b.password {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		b.logins++
		w.Header().Set("X-Auth-Token", "tok-123")
		w.Header().Set("Location", "https://bmc.example/redfish/v1/SessionService/Sessions/9")
		w.WriteHeader(http.StatusCreated)
		return
	}
	if r.Method == http.MethodDelete && r.URL.Path == "/redfish/v1/SessionService/Sessions/9" {
		b.logouts++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if tok := r.Header.Get("X-Auth-Token"); tok != "" {
		if tok != "tok-123" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		b.tokens++
	} else if u, p, ok := r.BasicAuth(); !ok || u != b.user || p != b.password {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	} else {
		b.basic++
	}
	if code := b.status[r.URL.Path]; code != 0 {
		if code/100 == 3 {
			http.Redirect(w, r, "https://elsewhere.invalid/x", code)
			return
		}
		http.Error(w, "x", code)
		return
	}
	if b.huge[r.URL.Path] {
		_, _ = w.Write(bytes.Repeat([]byte(" "), MaxBody+10))
		return
	}
	doc, ok := b.routes[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(doc))
}

func (b *bmc) client(t *testing.T, mod func(*Config)) *Client {
	t.Helper()
	cfg := Config{BaseURL: b.srv.URL, Credentials: Static{User: "admin", Password: "s3cret"}, CAFile: b.caFile, Timeout: 5 * time.Second}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func routes(chassis string, extra map[string]string) map[string]string {
	r := map[string]string{
		"/redfish/v1/":          `{"RedfishVersion":"1.17.0","Chassis":{"@odata.id":"/redfish/v1/Chassis"},"SessionService":{"@odata.id":"/redfish/v1/SessionService"}}`,
		"/redfish/v1/Chassis":   `{"Members":[{"@odata.id":"/redfish/v1/Chassis/1"},{"@odata.id":"/redfish/v1/Chassis/2"}]}`,
		"/redfish/v1/Chassis/1": chassis,
		"/redfish/v1/Chassis/2": `{"Id":"2"}`,
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func f(v float64) *float64 { return &v }

func TestReadsTheEnergyCounterByDiscovery(t *testing.T) {
	b := newBMC(t, routes(`{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/Chassis/1/EnvironmentMetrics"}}`,
		map[string]string{"/redfish/v1/Chassis/1/EnvironmentMetrics": `{"EnergykWh":{"Reading":1234.5},"PowerWatts":{"Reading":410}}`}))
	c := b.client(t, nil)
	r, err := c.Read(context.Background())
	if err != nil || r.EnergyKWh == nil || *r.EnergyKWh != 1234.5 || r.Chassis != "/redfish/v1/Chassis/1/EnvironmentMetrics" {
		t.Fatalf("%+v %v", r, err)
	}
	// Discovery is done once: the second read goes straight to the resource.
	before := b.basic
	if _, err := c.Read(context.Background()); err != nil || b.basic != before+1 {
		t.Fatalf("second read: %v, %d requests", err, b.basic-before)
	}
}

func TestFallsBackToPower(t *testing.T) {
	// No energy counter, a PowerWatts.
	b := newBMC(t, routes(`{"EnvironmentMetrics":{"@odata.id":"/em"},"Power":{"@odata.id":"/redfish/v1/Chassis/1/Power"}}`,
		map[string]string{"/em": `{"PowerWatts":{"Reading":395.5}}`}))
	b.routes["/em"] = `{"PowerWatts":{"Reading":395.5}}`
	delete(b.routes, "/em")
	b.routes["/redfish/v1/em"] = `{"PowerWatts":{"Reading":395.5}}`
	b.routes["/redfish/v1/Chassis/1"] = `{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/em"}}`
	r, err := b.client(t, nil).Read(context.Background())
	if err != nil || r.EnergyKWh != nil || r.PowerWatts == nil || *r.PowerWatts != 395.5 {
		t.Fatalf("%+v %v", r, err)
	}
	// Only the older Power resource: PowerControl[].PowerConsumedWatts, summed.
	b2 := newBMC(t, routes(`{"Power":{"@odata.id":"/redfish/v1/Chassis/1/Power"}}`,
		map[string]string{"/redfish/v1/Chassis/1/Power": `{"PowerControl":[{"PowerConsumedWatts":200},{"PowerConsumedWatts":150.5},{"PowerConsumedWatts":null}]}`}))
	r, err = b2.client(t, nil).Read(context.Background())
	if err != nil || r.PowerWatts == nil || *r.PowerWatts != 350.5 {
		t.Fatalf("%+v %v", r, err)
	}
	// Nothing at all.
	b3 := newBMC(t, routes(`{"Id":"1"}`, nil))
	if _, err := b3.client(t, nil).Read(context.Background()); !errors.Is(err, ErrNoReading) {
		t.Fatalf("no resource: %v", err)
	}
}

func TestChassisChoiceAndDirectPath(t *testing.T) {
	b := newBMC(t, routes(`{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/Chassis/1/EnvironmentMetrics"}}`, map[string]string{
		"/redfish/v1/Chassis/1/EnvironmentMetrics": `{"EnergykWh":{"Reading":1}}`,
		"/redfish/v1/Chassis/2":                    `{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/Chassis/2/EnvironmentMetrics"}}`,
		"/redfish/v1/Chassis/2/EnvironmentMetrics": `{"EnergykWh":{"Reading":2}}`,
	}))
	r, err := b.client(t, func(c *Config) { c.Chassis = "2" }).Read(context.Background())
	if err != nil || *r.EnergyKWh != 2 {
		t.Fatalf("chassis 2: %+v %v", r, err)
	}
	// A direct path skips discovery: the service root is never read.
	b.mu.Lock()
	delete(b.routes, "/redfish/v1/")
	b.mu.Unlock()
	r, err = b.client(t, func(c *Config) { c.MetricsPath = "/redfish/v1/Chassis/1/EnvironmentMetrics" }).Read(context.Background())
	if err != nil || *r.EnergyKWh != 1 {
		t.Fatalf("direct: %+v %v", r, err)
	}
	if _, err := New(Config{BaseURL: b.srv.URL, MetricsPath: "/etc/passwd", Credentials: Static{User: "a", Password: "b"}}); err == nil {
		t.Fatal("a metrics path outside /redfish/ was accepted")
	}
}

func TestSessionAuthentication(t *testing.T) {
	b := newBMC(t, routes(`{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/Chassis/1/EnvironmentMetrics"}}`,
		map[string]string{"/redfish/v1/Chassis/1/EnvironmentMetrics": `{"EnergykWh":{"Reading":7}}`}))
	c := b.client(t, func(c *Config) { c.Session = true })
	if r, err := c.Read(context.Background()); err != nil || *r.EnergyKWh != 7 {
		t.Fatalf("%+v %v", r, err)
	}
	if b.logins != 1 || b.basic != 0 || b.tokens == 0 {
		t.Fatalf("logins %d, basic %d, token requests %d", b.logins, b.basic, b.tokens)
	}
	if err := c.Close(context.Background()); err != nil || b.logouts != 1 {
		t.Fatalf("logout: %v %d", err, b.logouts)
	}
	// A wrong password: ErrAuth, no session.
	bad := b.client(t, func(c *Config) { c.Session = true; c.Credentials = Static{User: "admin", Password: "nope"} })
	if _, err := bad.Read(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestTypedErrorsAndLimits(t *testing.T) {
	good := `{"EnvironmentMetrics":{"@odata.id":"/redfish/v1/Chassis/1/EnvironmentMetrics"}}`
	em := map[string]string{"/redfish/v1/Chassis/1/EnvironmentMetrics": `{"EnergykWh":{"Reading":7}}`}
	b := newBMC(t, routes(good, em))
	read := func(c *Client) error { _, err := c.Read(context.Background()); return err }

	if err := read(b.client(t, func(c *Config) { c.Credentials = Static{User: "admin", Password: "wrong"} })); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong password: %v", err)
	}
	b.status["/redfish/v1/Chassis"] = http.StatusNotFound
	if err := read(b.client(t, nil)); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	b.status["/redfish/v1/Chassis"] = http.StatusInternalServerError
	var se *StatusError
	if err := read(b.client(t, nil)); !errors.As(err, &se) || se.Code != 500 {
		t.Errorf("500: %v", err)
	}
	// A redirect is not followed: the answer is the 302 itself.
	b.status["/redfish/v1/Chassis"] = http.StatusFound
	if err := read(b.client(t, nil)); !errors.As(err, &se) || se.Code != 302 {
		t.Errorf("redirect: %v", err)
	}
	delete(b.status, "/redfish/v1/Chassis")
	b.huge["/redfish/v1/Chassis"] = true
	if err := read(b.client(t, nil)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: %v", err)
	}
	delete(b.huge, "/redfish/v1/Chassis")
	b.mu.Lock()
	b.routes["/redfish/v1/Chassis"] = `not json`
	b.mu.Unlock()
	if err := read(b.client(t, nil)); !errors.Is(err, ErrMalformed) {
		t.Errorf("malformed: %v", err)
	}
	// http is refused at construction.
	if _, err := New(Config{BaseURL: "http://bmc", Credentials: Static{}}); !errors.Is(err, ErrInsecure) {
		t.Errorf("http: %v", err)
	}
	if _, err := New(Config{BaseURL: "https://u:p@bmc", Credentials: Static{}}); !errors.Is(err, ErrInsecure) {
		t.Errorf("credentials in the URL: %v", err)
	}
	if _, err := New(Config{BaseURL: "https://bmc"}); err == nil {
		t.Error("no credentials")
	}
}

// A certificate that is not trusted fails, and the error never carries the
// password.
func TestUntrustedCertificateAndNoCredentialInErrors(t *testing.T) {
	b := newBMC(t, routes(`{}`, nil))
	c, err := New(Config{BaseURL: b.srv.URL, Credentials: Static{User: "admin", Password: "s3cret"}, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Read(context.Background())
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("untrusted certificate: %v", err)
	}
	if _, err := New(Config{BaseURL: b.srv.URL, Credentials: Static{}, CAFile: filepath.Join(t.TempDir(), "missing.pem")}); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a certificate"), 0o600)
	if _, err := New(Config{BaseURL: b.srv.URL, Credentials: Static{}, CAFile: bad}); err == nil {
		t.Fatal("a CA file without a certificate was accepted")
	}
	if got := scrub(errors.New("boom s3cret boom"), "s3cret"); strings.Contains(got.Error(), "s3cret") {
		t.Fatal("scrub left the secret")
	}
}

func TestParseEnvironmentMetrics(t *testing.T) {
	for _, tc := range []struct {
		doc        string
		kwh, watts *float64
		wantErr    bool
	}{
		{`{"EnergykWh":{"Reading":12.5},"PowerWatts":{"Reading":300}}`, f(12.5), f(300), false},
		{`{"EnergyJoules":{"Reading":7200000}}`, f(2), nil, false},                     // 2 kWh
		{`{"EnergykWh":{"Reading":1},"EnergyJoules":{"Reading":9}}`, f(1), nil, false}, // kWh wins
		{`{"EnergykWh":{"Reading":null}}`, nil, nil, false},
		{`{"EnergykWh":{"Reading":-1},"PowerWatts":{"Reading":-5}}`, nil, nil, false},
		{`{"EnergykWh":{"Reading":1e300}}`, nil, nil, false},
		{`{"EnergykWh":{}}`, nil, nil, false},
		{`{}`, nil, nil, false},
		{`[1,2]`, nil, nil, true},
		{``, nil, nil, true},
		{`{"EnergykWh":"x"}`, nil, nil, true},
	} {
		m, err := ParseEnvironmentMetrics([]byte(tc.doc))
		if (err != nil) != tc.wantErr || err != nil && !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", tc.doc, err)
			continue
		}
		eq := func(a, b *float64) bool {
			return a == nil && b == nil || a != nil && b != nil && math.Abs(*a-*b) < 1e-9
		}
		if !eq(m.EnergyKWh, tc.kwh) || !eq(m.PowerWatts, tc.watts) {
			t.Errorf("%s: got %v / %v", tc.doc, m.EnergyKWh, m.PowerWatts)
		}
	}
}

func FuzzParseEnvironmentMetrics(f *testing.F) {
	for _, s := range []string{`{"EnergykWh":{"Reading":1}}`, `{"EnergyJoules":{"Reading":3.6e6}}`, `{"PowerWatts":{"Reading":-1}}`, `null`, `{"EnergykWh":{"Reading":1e999}}`, `[]`, ``} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		m, err := ParseEnvironmentMetrics(doc)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("an untyped error: %v", err)
			}
			return
		}
		for _, v := range []*float64{m.EnergyKWh, m.PowerWatts} {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0) {
				t.Fatalf("an unusable reading escaped: %v from %q", *v, doc)
			}
		}
	})
}

func FuzzParsePowerConsumed(f *testing.F) {
	for _, s := range []string{`{"PowerControl":[{"PowerConsumedWatts":100}]}`, `{"PowerControl":[]}`, `{"PowerControl":[{"PowerConsumedWatts":-1}]}`, `{}`, `x`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, doc []byte) {
		w, err := ParsePowerConsumed(doc)
		if err != nil {
			if !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrNoReading) {
				t.Fatalf("an untyped error: %v", err)
			}
			return
		}
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			t.Fatalf("an unusable power escaped: %v from %q", w, doc)
		}
	})
}
