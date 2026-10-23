package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

func listKeys(t *testing.T, e *env) []store.Key {
	t.Helper()
	var ks []store.Key
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys", master, ""))), &ks); err != nil {
		t.Fatal(err)
	}
	return ks
}

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func TestClientAddr(t *testing.T) {
	trusted := prefixes("10.0.0.0/8", "fd00::/8")
	for _, tc := range []struct {
		peer, xff, want string
		trusted         []netip.Prefix
	}{
		{"203.0.113.9:1", "1.2.3.4", "203.0.113.9", trusted},                       // untrusted peer: XFF ignored (spoofing)
		{"10.0.0.2:1", "198.51.100.7", "198.51.100.7", trusted},                    // one trusted proxy
		{"10.0.0.2:1", "6.6.6.6, 198.51.100.7, 10.0.0.3", "198.51.100.7", trusted}, // rightmost untrusted wins, spoofed left ignored
		{"10.0.0.2:1", "10.0.0.5, 10.0.0.3", "10.0.0.5", trusted},                  // all trusted: leftmost hop
		{"10.0.0.2:1", "garbage, 10.0.0.3", "10.0.0.3", trusted},                   // unparsable: stop at the last trusted hop
		{"10.0.0.2:1", "", "10.0.0.2", trusted},                                    // no header
		{"10.0.0.2:1", "198.51.100.7", "10.0.0.2", nil},                            // no trusted proxies: header ignored
		{"[fd00::1]:1", "2001:db8::5", "2001:db8::5", trusted},
		{"[::ffff:10.0.0.2]:1", "::ffff:198.51.100.8", "198.51.100.8", trusted},
		{"10.0.0.2:1", strings.Repeat("1.1.1.1,", 70) + "198.51.100.7", "10.0.0.2", trusted}, // absurd chain
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.peer
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		a, ok := ClientAddr(r, tc.trusted)
		if !ok || a.String() != tc.want {
			t.Errorf("peer %s xff %.40q: %v %v, want %s", tc.peer, tc.xff, a, ok, tc.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "not an address"
	if _, ok := ClientAddr(r, trusted); ok {
		t.Fatal("garbage peer accepted")
	}
}

// FuzzClientAddr: whatever the header holds, the answer is the peer or an
// address that appears in the header, and never panics.
func FuzzClientAddr(f *testing.F) {
	f.Add("1.2.3.4, 10.0.0.3")
	f.Add("::1,::ffff:1.2.3.4,,")
	f.Fuzz(func(t *testing.T, xff string) {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "10.0.0.2:1"
		r.Header.Set("X-Forwarded-For", xff)
		a, ok := ClientAddr(r, prefixes("10.0.0.0/8"))
		if !ok {
			t.Fatal("trusted peer refused")
		}
		if a.Zone() != "" {
			t.Fatalf("zoned address %v accepted", a)
		}
		if a.String() == "10.0.0.2" {
			return
		}
		for _, h := range strings.Split(xff, ",") {
			if p, err := netip.ParseAddr(strings.TrimSpace(h)); err == nil && p.Unmap() == a {
				return
			}
		}
		t.Fatalf("address %v not from the header %q", a, xff)
	})
}

func TestRouteRestrictions(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	e := newEnv(t, cfg)
	if resp := e.do("GET", "/healthz", "", ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("healthz from loopback (default): %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/metrics", "", ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("metrics from loopback (default): %d", resp.StatusCode)
	}

	cfg2 := testConfig(up.URL())
	cfg2.Routes.Healthz = prefixes("192.0.2.0/24")
	cfg2.Routes.Metrics = prefixes("192.0.2.0/24")
	cfg2.Routes.Admin = prefixes("192.0.2.0/24")
	cfg2.Routes.Inference = prefixes("192.0.2.0/24")
	e2 := newEnv(t, cfg2)
	for _, p := range []string{"/healthz", "/metrics", "/admin/whoami", "/v1/models"} {
		if resp := e2.do("GET", p, master, ""); readAll(t, resp) == "" || resp.StatusCode != 403 {
			t.Errorf("%s from loopback, not allowed: %d", p, resp.StatusCode)
		}
	}
	// Behind a trusted proxy, the forwarded client address decides.
	cfg2.TrustedProxies = prefixes("127.0.0.0/8")
	if resp := e2.do("GET", "/admin/whoami", master, "", "X-Forwarded-For", "192.0.2.10"); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("allowed client behind a trusted proxy: %d", resp.StatusCode)
	}
	if resp := e2.do("GET", "/admin/whoami", master, "", "X-Forwarded-For", "192.0.2.10, 198.51.100.1"); readAll(t, resp) == "" || resp.StatusCode != 403 {
		t.Fatalf("spoofed leftmost entry: %d", resp.StatusCode)
	}
}

func TestIPAllowlists(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.TrustedProxies = prefixes("127.0.0.0/8")
	e := newEnv(t, cfg)
	tok := map[string]string{
		"tom":   e.adminToken("tom", `{"role":"team_admin","team":"etabA"}`),
		"alice": e.adminToken("alice", `{"role":"org_admin","org":"etabA"}`),
	}
	key := e.createKey(`{"team":"etabA"}`)
	from := func(addr string) int {
		resp := e.do("GET", "/v1/models", key, "", "X-Forwarded-For", addr)
		readAll(t, resp)
		return resp.StatusCode
	}
	if code := from("198.51.100.1"); code != 200 {
		t.Fatalf("no allowlist: %d", code)
	}
	var keyID string
	for _, k := range listKeys(t, e) {
		if k.Team == "etabA" {
			keyID = k.ID
		}
	}
	e.check(tok, []call{
		{"tom", "PUT", "/admin/allowlists", `{"kind":"org","name":"etabA","cidrs":["198.51.100.0/24"]}`, 403},
		{"alice", "PUT", "/admin/allowlists", `{"kind":"org","name":"etabA","cidrs":["198.51.100.0/24"]}`, 200},
		{"alice", "PUT", "/admin/allowlists", `{"kind":"org","name":"etabB","cidrs":["198.51.100.0/24"]}`, 403},
		{"alice", "PUT", "/admin/allowlists", `{"kind":"org","name":"etabA","cidrs":["nope"]}`, 400},
		{"alice", "PUT", "/admin/allowlists", `{"kind":"tag","name":"x","cidrs":[]}`, 400},
		{"master", "PUT", "/admin/allowlists", `{"kind":"team","name":"nobody","cidrs":["10.0.0.0/8"]}`, 404},
	})
	if code := from("198.51.100.1"); code != 200 {
		t.Fatalf("inside the org list: %d", code)
	}
	if code := from("203.0.113.5"); code != 403 {
		t.Fatalf("outside the org list: %d", code)
	}
	// A key list narrows further; every list must match.
	e.check(tok, []call{{"tom", "PUT", "/admin/allowlists", `{"kind":"key","name":"` + keyID + `","cidrs":["198.51.100.7/32"]}`, 200}})
	if code := from("198.51.100.1"); code != 403 {
		t.Fatalf("in the org list, not the key list: %d", code)
	}
	if code := from("198.51.100.7"); code != 200 {
		t.Fatalf("in both lists: %d", code)
	}
	// A team admin sees the team's and keys' lists, not the organisation's.
	if out := readAll(t, e.do("GET", "/admin/allowlists", tok["tom"], "")); !strings.Contains(out, keyID) || strings.Contains(out, `"kind":"org"`) {
		t.Fatalf("team admin listing: %s", out)
	}
	if out := readAll(t, e.do("GET", "/admin/allowlists", tok["alice"], "")); !strings.Contains(out, keyID) || !strings.Contains(out, `"kind":"org"`) {
		t.Fatalf("org admin listing: %s", out)
	}
	// Removing the lists opens the key again.
	e.check(tok, []call{
		{"tom", "PUT", "/admin/allowlists", `{"kind":"key","name":"` + keyID + `","cidrs":[]}`, 200},
		{"alice", "PUT", "/admin/allowlists", `{"kind":"org","name":"etabA","cidrs":[]}`, 200},
	})
	if code := from("203.0.113.5"); code != 200 {
		t.Fatalf("after removal: %d", code)
	}
}
