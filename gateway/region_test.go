package gateway

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

func regionGroup(region, policy string, ups ...*upstream) *group {
	g := &group{cfg: config.ModelGroup{RegionPolicy: policy}, region: region}
	g.setMembers(ups)
	return g
}

func upIn(region string, inflight int64, healthy bool) *upstream {
	u := newUpstream("http://x", "k")
	u.region = region
	u.inflight.Store(inflight)
	u.healthy.Store(healthy)
	return u
}

// TestPickByRegion: local available, then (prefer) remote available, then the
// last resorts; "require" never leaves the region; no region, no preference.
func TestPickByRegion(t *testing.T) {
	now := time.Now()
	euBusy, euIdle := upIn("eu", 5, true), upIn("eu", 1, true)
	us := upIn("us", 0, true)
	none := upIn("", 0, true)

	g := regionGroup("eu", "prefer", euBusy, us, euIdle, none)
	if u := g.pick(map[*upstream]bool{}, now, false); u != euIdle {
		t.Fatal("the least loaded upstream of the region comes first, though another region is idler")
	}
	if u := g.pick(map[*upstream]bool{euIdle: true}, now, false); u != euBusy {
		t.Fatal("then the other in the region")
	}
	// Both local ones tried: prefer goes elsewhere (us is idle, unspecified too: the least loaded, first listed).
	if u := g.pick(map[*upstream]bool{euIdle: true, euBusy: true}, now, false); u != us {
		t.Fatalf("prefer left the region for %v", u.region)
	}
	// The local ones down: an available remote beats a local last resort.
	euDown := upIn("eu", 0, false)
	g = regionGroup("eu", "prefer", euDown, us)
	if u := g.pick(map[*upstream]bool{}, now, false); u != us {
		t.Fatal("an available remote upstream should beat a local one that is down")
	}
	// Everything down: the local last resort.
	usDown := upIn("us", 0, false)
	g = regionGroup("eu", "prefer", usDown, euDown)
	if u := g.pick(map[*upstream]bool{}, now, false); u != euDown {
		t.Fatal("the last resort is local first")
	}

	// require: never outside, not even as the last resort.
	g = regionGroup("eu", "require", us, none, euIdle)
	if u := g.pick(map[*upstream]bool{}, now, false); u != euIdle {
		t.Fatal("require picks the region")
	}
	if u := g.pick(map[*upstream]bool{euIdle: true}, now, false); u != nil {
		t.Fatalf("require left the region: %q", u.region)
	}
	g = regionGroup("eu", "require", us, none)
	if u := g.pick(map[*upstream]bool{}, now, false); u != nil {
		t.Fatal("require with no upstream in the region must pick none")
	}

	// A gateway without a region ignores the labels.
	g = regionGroup("", "prefer", euBusy, us)
	if u := g.pick(map[*upstream]bool{}, now, false); u != us {
		t.Fatal("no region: the least loaded")
	}
}

func regionEnv(t *testing.T, policy string) (*env, *fakeVLLM, *fakeVLLM) {
	eu, us := newFakeVLLM(t), newFakeVLLM(t)
	cfg := testConfig(eu.URL(), us.URL())
	cfg.Region = "eu"
	cfg.ModelGroups[0].RegionPolicy = policy
	cfg.ModelGroups[0].Upstreams[0].Region = "eu"
	cfg.ModelGroups[0].Upstreams[1].Region = "us"
	return newEnv(t, cfg), eu, us
}

func TestRegionRoutingEndToEnd(t *testing.T) {
	e, eu, us := regionEnv(t, "prefer")
	_, key := e.newKey("etabA")
	for i := 0; i < 6; i++ {
		if c := e.chatWith(key, chat); c != 200 {
			t.Fatalf("chat: %d", c)
		}
	}
	if eu.count() != 6 || us.count() != 0 {
		t.Fatalf("eu %d, us %d: the region's upstream serves while it can", eu.count(), us.count())
	}
	// The local upstream fails: prefer retries outside the region, and says so.
	eu.mu.Lock()
	eu.status = 500
	eu.mu.Unlock()
	if c := e.chatWith(key, chat); c != 200 {
		t.Fatalf("chat with the local upstream failing: %d", c)
	}
	if us.count() != 1 {
		t.Fatalf("us served %d", us.count())
	}
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_cross_region_total{group="llama",region="us"} 1`) {
		t.Fatalf("no cross-region count:\n%s", m)
	}
}

func TestRegionRequireStaysInTheRegion(t *testing.T) {
	e, eu, us := regionEnv(t, "require")
	_, key := e.newKey("etabA")
	if c := e.chatWith(key, chat); c != 200 {
		t.Fatalf("chat: %d", c)
	}
	eu.mu.Lock()
	eu.status = 500
	eu.mu.Unlock()
	if c := e.chatWith(key, chat); c == 200 {
		t.Fatal("a require request was answered from another region")
	}
	if us.count() != 0 {
		t.Fatalf("the other region received %d request(s)", us.count())
	}
}

func TestRegionConfig(t *testing.T) {
	base := `
master_key_file = "/m"
cache_isolation = "none"
%s
org "o" {
  share = 1
  team "t" {}
}
pool "p" { capacity = 1 }
model "m" {
  pool = "p"
  key_file = "/k"
  %s
  upstream "u" {
    base_url = "http://127.0.0.1:1"
    %s
  }
}
`
	for _, c := range []struct {
		name, top, model, up, want string
	}{
		{"ok", `region = "eu-west"`, `region_policy = "require"`, `region = "eu-west"`, ""},
		{"bad region", `region = "EU West"`, "", "", "Invalid region"},
		{"bad upstream region", `region = "eu"`, "", `region = "a b"`, "Invalid region"},
		{"bad policy", `region = "eu"`, `region_policy = "always"`, "", "Invalid region_policy"},
		{"require without region", "", `region_policy = "require"`, "", "region_policy without region"},
	} {
		_, err := config.Parse([]byte(fmt.Sprintf(base, c.top, c.model, c.up)), "t.hcl")
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
