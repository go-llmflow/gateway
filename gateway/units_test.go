package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

// unitsConfig has a GPU-hours model and a model priced in EUR, in two
// pools (a pool has one unit).
func unitsConfig(t *testing.T, upstream string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 1
  team "etabA" {}
}
pool "b300" { capacity = 4 }
pool "eur" { capacity = 4 }
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  cost {
    input_per_mtok  = 1000
    output_per_mtok = 5000
    cached_per_mtok = 100
  }
  upstream "u" { base_url = "`+upstream+`" }
}
model "priced" {
  upstream_model = "meta/llama"
  pool     = "eur"
  key_file = "/unused"
  cost {
    unit            = "EUR"
    price_date      = "2026-10-26"
    input_per_mtok  = 1000
    output_per_mtok = 5000
    cached_per_mtok = 100
  }
  upstream "u" { base_url = "`+upstream+`" }
}
`), "units.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A budget counts the costs of its own unit only: EUR never fills a
// GPU-hours budget, GPU-hours never fill a EUR budget.
func TestBudgetsKeepUnitsApart(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, unitsConfig(t, up.URL()))
	key := e.createKey(`{"team":"etabA"}`)
	priced := strings.Replace(chat, `"llama"`, `"priced"`, 1)

	// 0.164 per request in either unit (see budgets_test.go).
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":0.2,"unit":"EUR"}`)
	expect(t, "EUR 1", e.chatWith(key, priced), 200)
	expect(t, "EUR 2 (0.164 + 0.164 > 0.2 only after the first)", e.chatWith(key, priced), 200)
	expect(t, "EUR 3", e.chatWith(key, priced), 429)
	for i := 0; i < 3; i++ {
		expect(t, "GPU-hours requests are not in the EUR budget", e.chatWith(key, chat), 200)
	}
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":0.1,"unit":"gpu_hours"}`)
	expect(t, "GPU-hours budget already exhausted by GPU-hours", e.chatWith(key, chat), 429)
	expect(t, "the EUR model is still refused by its own budget", e.chatWith(key, priced), 429)

	out := readAll(t, e.do("GET", "/admin/budgets", master, ""))
	if !strings.Contains(out, `"unit":"EUR"`) || !strings.Contains(out, `"unit":"gpu_hours"`) {
		t.Fatalf("budget listing does not name the units: %s", out)
	}
	// A unit no model costs in is refused: such a budget could never fill.
	resp := e.do("PUT", "/admin/budgets", master, `{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
	if body := readAll(t, resp); resp.StatusCode != 400 || !strings.Contains(body, "no model costs in it") {
		t.Fatalf("USD budget: %d %s", resp.StatusCode, body)
	}
	if resp := e.do("DELETE", "/admin/budgets?kind=org&name=etabA&unit=EUR", master, ""); resp.StatusCode != 204 {
		t.Fatalf("delete the EUR budget: %d", resp.StatusCode)
	}
	expect(t, "EUR after its budget is gone", e.chatWith(key, priced), 200)
	expect(t, "GPU-hours budget still there", e.chatWith(key, chat), 429)

	metrics := readAll(t, e.do("GET", "/metrics", "", ""))
	if !strings.Contains(metrics, `llmflow_cost_total{team="etabA",pool="eur",unit="EUR"}`) {
		t.Fatalf("no EUR cost series:\n%s", metrics)
	}
	if strings.Contains(metrics, `llmflow_gpu_hours_total{team="etabA",pool="eur"}`) {
		t.Fatal("EUR counted as GPU-hours")
	}

	e.srv.Close(context.Background()) // flush the records
	rep := readAll(t, e.do("GET", "/admin/spend?from=2000-01-01&to=2100-01-01&group_by=org", master, ""))
	if !strings.Contains(rep, `"unit":"per_row"`) || !strings.Contains(rep, `"unit":"EUR"`) || !strings.Contains(rep, `"unit":"gpu_hours"`) {
		t.Fatalf("spend report not split by unit: %s", rep)
	}
	csv := readAll(t, e.do("GET", "/admin/spend?from=2000-01-01&to=2100-01-01&group_by=org&format=csv", master, ""))
	if !strings.Contains(csv, ",cost,unit\n") || !strings.Contains(csv, ",EUR\n") || !strings.Contains(csv, ",gpu_hours\n") {
		t.Fatalf("CSV report: %s", csv)
	}
}

func TestConfigUnits(t *testing.T) {
	base := func(cost string, pool string) string {
		return `
master_key_file = "/k"
cache_salt_key_file = "/k"
org "a" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 1 }
pool "q" { capacity = 1 }
model "x" {
  pool = "p"
  key_file = "/k"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
model "y" {
  pool = "` + pool + `"
  key_file = "/k"
  cost {
` + cost + `
  }
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`
	}
	for name, tc := range map[string]struct{ cost, pool, want string }{
		"currency without price_date": {`unit = "USD"`, "q", "needs price_date"},
		"bad price_date":              {`unit = "USD"` + "\n" + `price_date = "26/10/2026"`, "q", "want YYYY-MM-DD"},
		"bad unit":                    {`unit = "US$"` + "\n" + `price_date = "2026-10-26"`, "q", "Invalid unit"},
		"two units in one pool":       {`unit = "USD"` + "\n" + `price_date = "2026-10-26"`, "p", "Mixed cost units"},
	} {
		_, err := config.Parse([]byte(base(tc.cost, tc.pool)), "u.hcl")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	c, err := config.Parse([]byte(base(`unit = "gpu_hours"`, "p")), "u.hcl")
	if err != nil || c.ModelGroups[1].Cost.Unit != "" {
		t.Fatalf("gpu_hours spelled out: %v", err)
	}
	c, err = config.Parse([]byte(base(`unit = "USD"`+"\n"+`price_date = "2026-10-26"`, "q")), "u.hcl")
	if err != nil || strings.Join(c.Units(), ",") != ",USD" {
		t.Fatalf("units %v %v", c.Units(), err)
	}
}
