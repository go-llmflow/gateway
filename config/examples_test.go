package config

import (
	"path/filepath"
	"slices"
	"testing"
)

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
