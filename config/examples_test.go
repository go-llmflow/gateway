package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A beta that can widen the context window needs a context_window that
// covers it (review F1 of #87).
func TestContextBetaNeedsItsWindow(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "examples", "anthropic.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		betas, window string
		ok            bool
	}{
		{`["context-1m-2025-08-07"]`, "200000", false},
		{`["*"]`, "200000", false},
		{`["*"]`, "1000000", true},
		{`["context-1m-2025-08-07"]`, "1000000", true},
		{`["compact-2026-01-12"]`, "200000", true}, // refused per request under a budget, not here
	} {
		s := strings.Replace(string(src), `anthropic_betas = ["context-management-2025-06-27"]`, "anthropic_betas = "+c.betas, 1)
		s = strings.Replace(s, "context_window    = 1000000", "context_window    = "+c.window, 1)
		_, err := Parse([]byte(s), "anthropic.hcl")
		if (err == nil) != c.ok || err != nil && !strings.Contains(err.Error(), "Context beta wider than context_window") {
			t.Errorf("%s / %s: %v", c.betas, c.window, err)
		}
	}
}

// The Anthropic example stays valid and says what the README says.
func TestAnthropicExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "anthropic.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	var g *ModelGroup
	for i := range c.ModelGroups {
		if c.ModelGroups[i].Name == "claude-sonnet" {
			g = &c.ModelGroups[i]
		}
	}
	if g == nil || g.External == nil || g.External.Provider != ProviderAnthropic || !g.External.RequireGuardrails {
		t.Fatalf("claude-sonnet: %+v", g)
	}
	if g.MaxModelLen != 1000000 || g.MaxOutputTokens != 128000 {
		t.Fatalf("token bounds %d %d", g.MaxModelLen, g.MaxOutputTokens)
	}
	if g.Cost.Unit != "USD" || g.Cost.PriceDate != "2026-10-26" || g.Cost.CacheWrite1hPerMTok != 4 {
		t.Fatalf("cost %+v", g.Cost)
	}
	if len(g.External.OrgKeys) != 2 || !slices.Contains(c.KeyFiles(), "/etc/llmflow/anthropic-etabB.key") {
		t.Fatalf("org keys %v, key files %v", g.External.OrgKeys, c.KeyFiles())
	}
	if !g.External.AllowsBeta("context-management-2025-06-27") || g.External.AllowsBeta("oauth-2025-04-20") || g.External.AllowsBeta("other-2026-01-01") {
		t.Fatal("beta allow-list")
	}
}
