package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseBudgets(t *testing.T) {
	c, err := parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets != (Budgets{GrantShare: DefaultGrantShare, GrantIdle: DefaultGrantIdle, MinMaxTokens: DefaultMinMaxTokens}) {
		t.Fatalf("defaults %+v", c.Budgets)
	}
	if c.ModelGroups[0].MaxModelLen != 0 || c.ModelGroups[0].MaxOutputTokens != 0 {
		t.Fatalf("token bounds without configuration: %+v", c.ModelGroups[0])
	}
	src := strings.Replace(valid, `  pool     = "b300"`, "  pool     = \"b300\"\n  max_model_len = 32768\n  max_output_tokens = 4096", 1) +
		"budgets {\n  grant_share = 0.25\n  grant_idle = \"30s\"\n  min_max_tokens = 64\n}\n"
	c, err = parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets != (Budgets{GrantShare: 0.25, GrantIdle: 30 * time.Second, MinMaxTokens: 64}) {
		t.Fatalf("%+v", c.Budgets)
	}
	if g := c.ModelGroups[0]; g.MaxModelLen != 32768 || g.MaxOutputTokens != 4096 {
		t.Fatalf("%+v", g)
	}
}

func TestParseBudgetsErrors(t *testing.T) {
	model := func(attrs string) string {
		return strings.Replace(valid, `  pool     = "b300"`, "  pool     = \"b300\"\n"+attrs, 1)
	}
	for _, c := range []struct{ name, src, want string }{
		{"share zero", valid + "budgets { grant_share = 0 }", "Invalid grant_share"},
		{"share above one", valid + "budgets { grant_share = 1.5 }", "Invalid grant_share"},
		{"idle short", valid + "budgets { grant_idle = \"10ms\" }", "Invalid grant_idle"},
		{"idle long", valid + "budgets { grant_idle = \"1h\" }", "Invalid grant_idle"},
		{"floor", valid + "budgets { min_max_tokens = 0 }", "Invalid min_max_tokens"},
		{"len zero", model("  max_model_len = 0"), "Invalid max_model_len"},
		{"len huge", model("  max_model_len = 4294967296"), "Invalid max_model_len"},
		{"output above len", model("  max_model_len = 100\n  max_output_tokens = 101"), "Invalid max_output_tokens"},
		{"context window on vLLM", model("  context_window = 1000"), "Invalid context_window"},
		{"price above bound", strings.Replace(valid, "output_per_mtok = 0.05", "output_per_mtok = 2e6", 1), "Price too high"},
	} {
		_, err := parse(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestPricesOf(t *testing.T) {
	c := Cost{InputPerMTok: 1, LongContext: &Cost{OutputPerMTok: 3e6}}
	ps := pricesOf(c)
	if len(ps) != 11 || ps[0] != 1 || ps[7] != 3e6 {
		t.Fatalf("%v", ps)
	}
}
