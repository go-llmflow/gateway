package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Bedrock example stays valid and says what the README says.
func TestBedrockExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "bedrock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	var g *ModelGroup
	for i := range c.ModelGroups {
		if c.ModelGroups[i].Name == "claude-sonnet-bedrock" {
			g = &c.ModelGroups[i]
		}
	}
	if g == nil || g.External == nil || g.External.Provider != ProviderBedrock || g.External.Bedrock == nil {
		t.Fatalf("claude-sonnet-bedrock: %+v", g)
	}
	b := g.External.Bedrock
	if b.Region != "eu-west-3" || b.Endpoint != "https://bedrock-runtime.eu-west-3.amazonaws.com" ||
		g.UpstreamModel != "eu.anthropic.claude-sonnet-4-5-20250929-v1:0" || len(g.Upstreams) != 1 || g.Upstreams[0].BaseURL != b.Endpoint {
		t.Fatalf("bedrock %+v upstream model %q upstreams %+v", b, g.UpstreamModel, g.Upstreams)
	}
	if g.MaxModelLen != 200000 || g.MaxOutputTokens != 64000 || g.Cost.Unit != "USD" || g.Cost.CacheWrite1hPerMTok != 6 {
		t.Fatalf("bounds %d %d cost %+v", g.MaxModelLen, g.MaxOutputTokens, g.Cost)
	}
	files := c.KeyFiles()
	for _, want := range []string{"vault:secret/llmflow/bedrock-etabA#secret_access_key", "vault:secret/llmflow/bedrock-etabA#session_token", "/etc/llmflow/bedrock-etabB.apikey"} {
		if !slices.Contains(files, want) {
			t.Errorf("key files %v lack %s", files, want)
		}
	}
	if b.CredsFor("etabB").BearerTokenFile == "" || b.CredsFor("etabA").AccessKeyIDFile == "" {
		t.Errorf("credentials %+v", b)
	}
}

// Mixing the two ways to authenticate, half a key pair and a session token
// alone are refused.
func TestBedrockCredentialShapes(t *testing.T) {
	src := func(creds string) string {
		return `
master_key_file = "/m"
cache_salt_key_file = "/c"
org "a" {
  share = 1
  team "ta" {}
}
pool "p" { capacity = 1 }
model "m" {
  provider = "bedrock"
  region = "eu-west-3"
  model_id = "anthropic.claude-3-haiku-20240307-v1:0"
  pool = "p"
  allow_external = true
  orgs = ["a"]
  cost {
    unit = "USD"
    price_date = "2026-10-31"
  }
  ` + creds + `
}
guardrail "pii" {
  type = "pii"
  orgs = ["a"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["a"]
}
`
	}
	ok := []string{
		`credentials { bearer_token_file = "/b" }`,
		`credentials {
    access_key_id_file = "/i"
    secret_access_key_file = "/s"
  }`,
	}
	for _, c := range ok {
		if _, err := Parse([]byte(src(c)), "x.hcl"); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	bad := map[string]string{
		`credentials { access_key_id_file = "/i" }`: "go together",
		`credentials { session_token_file = "/t" }`: "go together",
		`credentials {
    bearer_token_file = "/b"
    access_key_id_file = "/i"
    secret_access_key_file = "/s"
  }`: "two ways to authenticate",
		`credentials {}`: "no credentials",
		``:               "no org_credentials and the model has no credentials block",
	}
	for c, want := range bad {
		if _, err := Parse([]byte(src(c)), "x.hcl"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", c, err, want)
		}
	}
}

func FuzzBedrockModelID(f *testing.F) {
	for _, s := range []string{"", "anthropic.claude-3-haiku-20240307-v1:0", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:eu-west-3::foundation-model/anthropic.claude-3-haiku-20240307-v1:0",
		"arn:aws:bedrock:eu-west-3:123456789012:default-prompt-router/anthropic.claude:1",
		"arn:aws:bedrock:eu-west-3:123456789012:provisioned-model/abcdefghijkl", "arn:::::", "a/../b", "\x00", strings.Repeat("a", 3000)} {
		f.Add(s, "eu-west-3")
	}
	f.Fuzz(func(t *testing.T, id, region string) {
		err := CheckBedrockModelID(id, region)
		if err != nil {
			return
		}
		// Accepted: an Anthropic model or a foundation-model / inference-profile
		// ARN, in the region, with nothing that could change the path.
		if strings.ContainsAny(id, " ?#%\\\x00\r\n") || len(id) > 2048 {
			t.Fatalf("accepted %q", id)
		}
		for _, banned := range []string{"prompt-router", "provisioned-model", ":prompt/", "imported-model", "custom-model", "sagemaker"} {
			if strings.Contains(id, banned) {
				t.Fatalf("accepted %q (%s)", id, banned)
			}
		}
		if strings.HasPrefix(id, "arn:") && !strings.HasPrefix(id, "arn:aws") {
			// any partition is allowed, but it is a bedrock ARN in the region
			if parts := strings.SplitN(id, ":", 6); len(parts) != 6 || parts[2] != "bedrock" {
				t.Fatalf("accepted %q", id)
			}
		}
	})
}
