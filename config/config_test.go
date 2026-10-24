package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const valid = `
master_key_file = "/etc/llmflow/master.key"

org "etabA" { share = 0.8125 }
org "etabB" { share = 0.1875 }

pool "b300" { capacity = 256 }

model "llama" {
  pool     = "b300"
  key_file = "/etc/llmflow/vllm.key"
  cost {
    input_per_mtok  = 0.01
    output_per_mtok = 0.05
    cached_per_mtok = 0.001
  }
  upstream "gpu01" { base_url = "http://10.0.0.1:8000" }
  upstream "gpu02" {
    base_url = "http://10.0.0.2:8000"
    key_file = "/etc/llmflow/gpu02.key"
  }
}

model "llama-batch" {
  upstream_model = "llama"
  pool           = "b300"
  class          = "batch"
  upstream "gpu03" {
    base_url = "http://10.0.0.3:8000"
    key_file = "/etc/llmflow/vllm.key"
  }
}
cache_salt_key_file = "/etc/llmflow/cache-salt.key"
`

func parse(src string) (*Config, error) { return Parse([]byte(src), "test.hcl") }

func TestParseValid(t *testing.T) {
	c, err := parse(valid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.FairShare.HalfLife != 7*24*time.Hour || !c.FailClosed() || c.Retries != 2 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	g := c.ModelGroups[0]
	if g.UpstreamModel != "llama" || g.Class != Interactive || g.Cost.OutputPerMTok != 0.05 {
		t.Fatalf("group: %+v", g)
	}
	if g.Upstreams[0].KeyFile != "/etc/llmflow/vllm.key" || g.Upstreams[1].KeyFile != "/etc/llmflow/gpu02.key" {
		t.Fatalf("key file inheritance: %+v", g.Upstreams)
	}
	if c.ModelGroups[1].Class != Batch {
		t.Fatalf("class: %v", c.ModelGroups[1].Class)
	}
	if c.OrgShare("etabA") != 0.8125 || c.OrgShare("nobody") != 0 {
		t.Fatal("OrgShare")
	}
	if kf := c.KeyFiles(); len(kf) != 2 {
		t.Fatalf("key files %v", kf)
	}
	if c.CacheIsolation != "org" || c.CacheSaltKeyFile != "/etc/llmflow/cache-salt.key" {
		t.Fatalf("cache isolation %q %q", c.CacheIsolation, c.CacheSaltKeyFile)
	}
	// Secret references may name OpenBao/Vault fields once a vault block exists.
	c, err = parse(strings.Replace(valid, `"/etc/llmflow/cache-salt.key"`, `"vault:secret/llmflow/salt#key?version=2"`, 1) +
		"vault {\n  address = \"https://bao.example:8200\"\n  token_file = \"/etc/llmflow/bao.token\"\n  namespace = \"hpc\"\n  cache_ttl = \"1m\"\n}\n")
	if err != nil || c.Vault == nil || c.Vault.CacheTTL != time.Minute || c.Vault.Namespace != "hpc" || len(c.SecretRefs()) < 3 {
		t.Fatalf("vault: %+v %v", c, err)
	}
	// "none" (one tenant) needs no key.
	c, err = parse(strings.Replace(valid, `cache_salt_key_file = "/etc/llmflow/cache-salt.key"`, `cache_isolation = "none"`, 1))
	if err != nil || c.CacheIsolation != "none" {
		t.Fatalf("none: %v", err)
	}
}

func TestParseOptionalBlocks(t *testing.T) {
	c, err := parse(valid + `
listen = "0.0.0.0:9000"
retries = 0
budget_refresh = "5s"
key_cache_ttl = "1m"
shutdown_grace = "3s"
expose_overhead_header = true
metrics_token_file = "/m"
store {
  kind = "postgres"
  dsn_file = "/dsn"
  fail_closed = false
  queue_size = 10
  batch_size = 5
}
limits {
  max_body_bytes = 1024
  max_header_bytes = 2048
  read_header_timeout = "1s"
  read_timeout = "2s"
  idle_timeout = "3s"
  upstream_header_timeout = "4s"
  max_request_duration = "5s"
  max_response_bytes = 4096
}
fairshare { half_life = "1h" }
tags {
  header  = "X-Llmflow-Tags"
  allowed = ["eval", "prod"]
}
alerts {
  webhook_url        = "https://hooks.example.org/llmflow"
  webhook_token_file = "/hook"
  smtp {
    server        = "smtp.example.org:587"
    from          = "llmflow@example.org"
    to            = ["ops@example.org", "Admin <admin@example.org>"]
    username      = "llmflow"
    password_file = "/smtp"
  }
}
health {
  interval = "1s"
  timeout = "2s"
  cooldown = "3s"
  path = "/ping"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retries != 0 || c.FailClosed() || c.Store.Kind != "postgres" || c.Limits.MaxRequestDuration != 5*time.Second ||
		c.Health.Path != "/ping" || c.FairShare.HalfLife != time.Hour || !c.ExposeOverheadHdr || c.MetricsTokenFile != "/m" ||
		c.Limits.MaxResponseBytes != 4096 || c.Store.BatchSize != 5 || c.KeyCacheTTL != time.Minute ||
		c.Tags.Header != "X-Llmflow-Tags" || len(c.Tags.Allowed) != 2 || c.Alerts.SMTP == nil || len(c.Alerts.SMTP.To) != 2 ||
		c.Alerts.WebhookTokenFile != "/hook" || c.Alerts.SMTP.PasswordFile != "/smtp" {
		t.Fatalf("%+v", c)
	}
}

// Every rejection carries the file:line:column of the offending value.
func TestParseRejectsWithPosition(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unknown attribute", strings.Replace(valid, `master_key_file`, "temas = 1\nmaster_key_file", 1), `test.hcl:2:1: Unsupported argument`},
		{"shares > 1", strings.Replace(valid, "0.1875", "0.5", 1), `test.hcl:5:23: Shares exceed the pool`},
		{"bad scheme", strings.Replace(valid, "http://10.0.0.3:8000", "file:///etc/passwd", 1), `test.hcl:29:16: Invalid base_url`},
		{"userinfo", strings.Replace(valid, "http://10.0.0.3:8000", "http://u:p@10.0.0.3:8000", 1), "Invalid base_url"},
		{"query", strings.Replace(valid, "http://10.0.0.3:8000", "http://10.0.0.3:8000?x=1", 1), "Invalid base_url"},
		{"undeclared pool", strings.Replace(valid, `pool           = "b300"`, `pool           = "nope"`, 1), `test.hcl:26:20: Unknown pool`},
		{"bad class", strings.Replace(valid, `"batch"`, `"urgent"`, 1), "Invalid class"},
		{"no master", strings.Replace(valid, `master_key_file = "/etc/llmflow/master.key"`, "", 1), "Missing required argument"},
		{"bad org name", strings.Replace(valid, `"etabB"`, `"etab B"`, 1), `test.hcl:5:5: Invalid organisation name`},
		{"duplicate org", valid + `org "etabA" { share = 0 }`, "Duplicate organisation"},
		{"bad team name", valid + "org \"c\" {\n  share = 0\n  team \"a/b\" {}\n}\n", "Invalid team name"},
		{"duplicate team", valid + "org \"c\" {\n  share = 0\n  team \"t\" {}\n}\norg \"d\" {\n  share = 0\n  team \"t\" {}\n}\n", "Duplicate team"},
		{"team without org", valid + `team "t" {}`, "Unsupported block type"},
		{"org without share", valid + `org "c" {}`, "Missing required argument"},
		{"pg without dsn", valid + `store { kind = "postgres" }`, "Missing DSN file"},
		{"bad store kind", valid + `store { kind = "redis" }`, "Invalid store kind"},
		{"zero capacity", strings.Replace(valid, "capacity = 256", "capacity = 0", 1), `test.hcl:7:26: Invalid capacity`},
		{"negative cost", strings.Replace(valid, "input_per_mtok  = 0.01", "input_per_mtok  = -1", 1), "Invalid cost"},
		{"bad duration", valid + `fairshare { half_life = "a week" }`, "Invalid duration"},
		{"negative duration", valid + `health { interval = "-1s" }`, "Invalid duration"},
		{"bad health path", valid + `health { path = "health?x" }`, "Invalid health path"},
		{"no key file", strings.Replace(valid, "  key_file = \"/etc/llmflow/vllm.key\"\n  cost", "  cost", 1), "Missing key file"},
		{"no upstream", valid + `model "x" { pool = "b300" }`, "No upstream"},
		{"duplicate upstream", strings.Replace(valid, `"gpu02"`, `"gpu01"`, 1), "Duplicate upstream"},
		{"no model", "master_key_file = \"/m\"\n", "No model"},
		{"retries", valid + "retries = 11\n", "Invalid retries"},
		{"no cache salt key", strings.Replace(valid, `cache_salt_key_file = "/etc/llmflow/cache-salt.key"`, "", 1), "Missing cache salt key"},
		{"no cache salt key for key scope", strings.Replace(valid, `cache_salt_key_file = "/etc/llmflow/cache-salt.key"`, `cache_isolation = "key"`, 1), "test.hcl:33:19: Missing cache salt key"},
		{"bad cache isolation", valid + `cache_isolation = "user"` + "\n", "Invalid cache isolation"},
		{"vault ref without block", strings.Replace(valid, `"/etc/llmflow/cache-salt.key"`, `"vault:secret/llmflow#salt"`, 1), "Vault reference without vault block"},
		{"bad secret ref", strings.Replace(valid, `"/etc/llmflow/cache-salt.key"`, `"vault:secret/../x#k"`, 1), "Invalid secret reference"},
		{"vault cleartext", valid + "vault {\n  address = \"http://bao.example:8200\"\n  token_file = \"/t\"\n}\n", "Invalid vault address"},
		{"vault token from vault", valid + "vault {\n  address = \"https://bao.example:8200\"\n  token_file = \"vault:a/b#c\"\n}\n", "Invalid vault token file"},
		{"tag header", valid + "tags {\n  header = \"X Tags\"\n  allowed = [\"a\"]\n}\n", "Invalid header"},
		{"tag name", valid + "tags {\n  header = \"X-Tags\"\n  allowed = [\"a b\"]\n}\n", "Invalid tag"},
		{"no tags", valid + "tags {\n  header = \"X-Tags\"\n  allowed = []\n}\n", "Invalid tags"},
		{"empty alerts", valid + "alerts {}\n", "Empty alerts block"},
		{"webhook url", valid + "alerts { webhook_url = \"ftp://x\" }\n", "Invalid webhook_url"},
		{"smtp server", valid + "alerts {\n  smtp {\n    server = \"nohost\"\n    from = \"a@b.c\"\n    to = [\"d@e.f\"]\n  }\n}\n", "Invalid SMTP server"},
		{"smtp from", valid + "alerts {\n  smtp {\n    server = \"h:25\"\n    from = \"not an address\"\n    to = [\"d@e.f\"]\n  }\n}\n", "Invalid address"},
		{"smtp to", valid + "alerts {\n  smtp {\n    server = \"h:25\"\n    from = \"a@b.c\"\n    to = []\n  }\n}\n", "Invalid address"},
		{"smtp creds", valid + "alerts {\n  smtp {\n    server = \"h:25\"\n    from = \"a@b.c\"\n    to = [\"d@e.f\"]\n    username = \"u\"\n  }\n}\n", "Incomplete SMTP credentials"},
		{"body limit", valid + "limits { max_body_bytes = 0 }\n", "Invalid body limit"},
		{"jwt cleartext issuer", valid + jwtBlock("http://idp.example", "llmflow"), "Invalid issuer"},
		{"jwt issuer query", valid + jwtBlock("https://idp.example?x=1", "llmflow"), "Invalid issuer"},
		{"jwt no audience", valid + jwtBlock("https://idp.example", ""), "Missing audience"},
		{"jwt no map", valid + "jwt {\n  issuer = \"https://idp.example\"\n  audience = \"a\"\n}\n", "No map"},
		{"jwt bad team", valid + strings.Replace(jwtBlock("https://idp.example", "a"), `team = "etabA"`, `team = "a b"`, 1), "Invalid map"},
		{"jwt skew", valid + strings.Replace(jwtBlock("https://idp.example", "a"), "jwt {", "jwt {\n  clock_skew = \"1h\"", 1), "Clock skew too large"},
		{"jwt jwks cleartext", valid + strings.Replace(jwtBlock("https://idp.example", "a"), "jwt {", "jwt {\n  jwks_url = \"http://keys.example/k\"", 1), "Invalid jwks_url"},
		{"sso cleartext issuer", valid + ssoBlock("http://idp.example", "https://gw.example/admin/callback", ""), "Invalid issuer"},
		{"sso redirect path", valid + ssoBlock("https://idp.example", "https://gw.example/elsewhere", ""), "Invalid redirect_url"},
		{"sso cleartext redirect", valid + ssoBlock("https://idp.example", "http://gw.example/admin/callback", ""), "Invalid redirect_url"},
		{"sso proxy role with scope", valid + ssoBlock("https://idp.example", "https://gw.example/admin/callback", "role {\n    value = \"g\"\n    role = \"proxy_admin\"\n    team = \"t\"\n  }"), "Invalid role"},
		{"sso unknown role", valid + ssoBlock("https://idp.example", "https://gw.example/admin/callback", "role {\n    value = \"g\"\n    role = \"root\"\n  }"), "Invalid role"},
		{"sso no openid", valid + strings.Replace(ssoBlock("https://idp.example", "https://gw.example/admin/callback", ""), "sso {", "sso {\n  scopes = [\"profile\"]", 1), "Invalid scopes"},
		{"sso long session", valid + strings.Replace(ssoBlock("https://idp.example", "https://gw.example/admin/callback", ""), "sso {", "sso {\n  session_ttl = \"720h\"", 1), "Session too long"},
		{"policy name", valid + "policy \"a b\" { require = [\"user\"] }\n", "Invalid policy name"},
		{"policy twice", valid + "policy \"p\" { require = [\"user\"] }\npolicy \"p\" { require = [\"user\"] }\n", "Invalid policy name"},
		{"policy field", valid + "policy \"p\" { require = [\"metadata..project\"] }\n", "Invalid required field"},
		{"policy field chars", valid + "policy \"p\" { require = [\"a b\"] }\n", "Invalid required field"},
		{"policy team twice", valid + "policy \"p\" {\n  teams = [\"t\"]\n  require = [\"user\"]\n}\npolicy \"q\" {\n  teams = [\"t\"]\n  require_tags = true\n}\n", "Team in two policies"},
		{"policy size", valid + "policy \"p\" { max_body_bytes = 2000000000 }\n", "Invalid size limit"},
		{"policy negative size", valid + "policy \"p\" { max_response_bytes = -1 }\n", "Invalid size limit"},
		{"policy empty", valid + "policy \"p\" {}\n", "Empty policy"},
		{"scim base url", valid + "scim {\n  token_file = \"/t\"\n  base_url = \"https://gw.example/elsewhere\"\n}\n", "Invalid base_url"},
		{"scim cleartext", valid + "scim {\n  token_file = \"/t\"\n  base_url = \"http://gw.example/scim/v2\"\n}\n", "Invalid base_url"},
		{"scim role", valid + "scim {\n  token_file = \"/t\"\n  base_url = \"https://gw.example/scim/v2\"\n  group \"g\" {\n    team = \"etabA\"\n    role = \"org_admin\"\n  }\n}\n", "Invalid role"},
		{"scim team", valid + "scim {\n  token_file = \"/t\"\n  base_url = \"https://gw.example/scim/v2\"\n  group \"g\" { team = \"a b\" }\n}\n", "Invalid team"},
		{"scim duplicate group", valid + "scim {\n  token_file = \"/t\"\n  base_url = \"https://gw.example/scim/v2\"\n  group \"G\" { team = \"etabA\" }\n  group \"g\" { team = \"etabB\" }\n}\n", "Invalid group"},
		{"rotation without vault", valid + "key_rotation { vault_prefix = \"secret/llmflow/keys\" }\n", "Key rotation without vault block"},
		{"rotation bad prefix", valid + "key_rotation { vault_prefix = \"secret/../x\" }\n", "Invalid vault_prefix"},
		{"rotation interval", valid + "key_rotation {\n  vault_prefix = \"secret/k\"\n  check_interval = \"1s\"\n}\n", "Invalid check_interval"},
		{"jwt negative rpm", valid + strings.Replace(jwtBlock("https://idp.example", "a"), "jwt {", "jwt {\n  rpm = -1", 1), "Invalid limit"},
		{"syntax", valid + "team {", "test.hcl:"},
		{"string for number", strings.Replace(valid, "capacity = 256", `capacity = "many"`, 1), "Unsuitable value type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.src)
			if err == nil {
				t.Fatal("accepted")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("not a *config.Error: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func jwtBlock(issuer, aud string) string {
	return fmt.Sprintf("jwt {\n  issuer = %q\n  audience = %q\n  map {\n    value = \"lab-a\"\n    team = \"etabA\"\n  }\n}\n", issuer, aud)
}

func ssoBlock(issuer, redirect, extra string) string {
	return fmt.Sprintf("sso {\n  issuer = %q\n  client_id = \"llmflow-admin\"\n  redirect_url = %q\n  %s\n}\n", issuer, redirect, extra)
}

func TestParseSSO(t *testing.T) {
	c, err := parse(valid + `sso {
  issuer             = "https://sso.example.org"
  client_id          = "llmflow-admin"
  client_secret_file = "vault:secret/llmflow#sso"
  redirect_url       = "https://llm.example.org/admin/callback"
  role {
    value = "llm-admins"
    role  = "proxy_admin"
  }
  role {
    claim = "entitlement"
    value = "lab-a"
    role  = "team_admin"
    team  = "etabA"
  }
}
vault {
  address    = "https://bao.example:8200"
  token_file = "/t"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	s := c.SSO
	if s == nil || s.SessionTTL != 8*time.Hour || len(s.Scopes) != 3 || s.Origin() != "https://llm.example.org" ||
		s.Roles[0] != (SSORole{Claim: "groups", Value: "llm-admins", Role: "proxy_admin"}) ||
		s.Roles[1] != (SSORole{Claim: "entitlement", Value: "lab-a", Role: "team_admin", Team: "etabA"}) {
		t.Fatalf("%+v", s)
	}
	if !slices.Contains(c.SecretRefs(), "vault:secret/llmflow#sso") {
		t.Fatal("client_secret_file is not a secret reference")
	}
}

func TestParseSCIM(t *testing.T) {
	c, err := parse(valid + `scim {
  token_file = "/etc/llmflow/scim.token"
  base_url   = "https://llm.example.org/scim/v2/"
  group "Lab Physics" { team = "etabA" }
  group "Lab Leads" {
    team = "etabA"
    role = "team_admin"
  }
}
`)
	if err != nil {
		t.Fatal(err)
	}
	s := c.SCIM
	if s == nil || s.BaseURL != "https://llm.example.org/scim/v2" || len(s.Groups) != 2 ||
		s.Groups[0] != (SCIMGroup{Name: "Lab Physics", Team: "etabA", Role: "member"}) || s.Groups[1].Role != "team_admin" ||
		!slices.Contains(c.SecretRefs(), "/etc/llmflow/scim.token") {
		t.Fatalf("%+v", s)
	}
	for name, want := range map[string]bool{"alice@example.org": true, "a+b@x.y": true, "bob": true, "a b": false, "": false, "a/b": false} {
		if ValidUserName(name) != want {
			t.Errorf("ValidUserName(%q) != %v", name, want)
		}
	}
}

func TestParseKeyRotation(t *testing.T) {
	c, err := parse(valid + `key_rotation { vault_prefix = "/secret/llmflow/keys/" }
vault {
  address    = "https://bao.example:8200"
  token_file = "/t"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if c.KeyRotation == nil || c.KeyRotation.VaultPrefix != "secret/llmflow/keys" || c.KeyRotation.CheckInterval != time.Hour {
		t.Fatalf("%+v", c.KeyRotation)
	}
}

func TestParseJWT(t *testing.T) {
	c, err := parse(valid + `jwt {
  issuer         = "https://idp.example/realms/r"
  audience       = "llmflow"
  type           = "at+jwt"
  username_claim = "email"
  rpm            = 60
  map {
    value = "lab-a"
    team  = "etabA"
  }
  map {
    claim   = "department"
    value   = "physics"
    team    = "etabB"
    project = "p1"
  }
}
`)
	if err != nil {
		t.Fatal(err)
	}
	j := c.JWT
	if j == nil || j.Issuer != "https://idp.example/realms/r" || j.Type != "at+jwt" || j.UsernameClaim != "email" || j.RPM != 60 ||
		j.ClockSkew != time.Minute || j.MinRefresh != time.Minute || len(j.Maps) != 2 ||
		j.Maps[0] != (JWTMap{Claim: "groups", Value: "lab-a", Team: "etabA"}) ||
		j.Maps[1] != (JWTMap{Claim: "department", Value: "physics", Team: "etabB", Project: "p1"}) {
		t.Fatalf("%+v", j)
	}
	// Loopback issuers are accepted over http (tests, a local IdP).
	if _, err := parse(valid + jwtBlock("http://127.0.0.1:9000", "a")); err != nil {
		t.Fatal(err)
	}
}

// HCL quoted strings are templates. The evaluation context is empty, so
// interpolations of variables and function calls are refused, and the
// escaped forms produce the literal text.
func TestTemplatesAndStaticContext(t *testing.T) {
	refused := map[string]string{
		"variable in a URL":       strings.Replace(valid, "http://10.0.0.3:8000", "http://${host}:8000", 1),
		"environment":             strings.Replace(valid, `"/etc/llmflow/master.key"`, `"${env.HOME}/master.key"`, 1),
		"function call":           strings.Replace(valid, `"/etc/llmflow/master.key"`, `file("/etc/shadow")`, 1),
		"bare variable":           strings.Replace(valid, `capacity = 256`, `capacity = var.c`, 1),
		"directive with variable": strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "%{ if x }a%{ endif }"`, 1),
	}
	for name, src := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := parse(src)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	// Escapes give the literal characters.
	c, err := parse(strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "org/m$${x}%%{y}"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ModelGroups[1].UpstreamModel; got != "org/m${x}%{y}" {
		t.Fatalf("escaped template gave %q", got)
	}
	// A lone "$" or "%" needs no escape.
	c, err = parse(strings.Replace(valid, `upstream_model = "llama"`, `upstream_model = "50%-$cheap"`, 1))
	if err != nil || c.ModelGroups[1].UpstreamModel != "50%-$cheap" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestErrorFormat(t *testing.T) {
	_, err := parse("master_key_file = \"/m\"\nteam \"a\" { share = \"x\" }\npool \"p\" { capacity = \"y\" }\n")
	if err == nil || !strings.HasPrefix(err.Error(), "test.hcl:") {
		t.Fatalf("%v", err)
	}
	if strings.Count(err.Error(), "\n") < 1 {
		t.Fatalf("expected several diagnostics: %v", err)
	}
}

func TestReadSecret(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, []byte("  sk-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadSecret(p)
	if err != nil || s != "sk-secret-value" {
		t.Fatalf("%q %v", s, err)
	}
	if err := os.WriteFile(p, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil || strings.Contains(err.Error(), "second") {
		t.Fatalf("multi-line secret: %v", err)
	}
	if err := os.WriteFile(p, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(p); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := ReadSecret(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := ReadSecret(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing accepted")
	}
}

// The example in the README must stay valid.
func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "gateway.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ModelGroups) != 2 || c.Pools[0].Capacity != 256 || c.OrgShare("etabB") != 0.1875 || len(c.Orgs[0].Teams) != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.hcl")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p + ".missing"); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestValidateUpstream(t *testing.T) {
	for _, u := range []string{"http://h:1", "https://h/v"} {
		if err := ValidateUpstream(u); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"%", "ftp://h", "http://", "http://h?", "http://h#f"} {
		if ValidateUpstream(u) == nil {
			t.Fatalf("%s accepted", u)
		}
	}
}

// FuzzParse: the configuration is untrusted input. Parsing must not panic,
// and whatever it accepts must satisfy the invariants the gateway relies on.
const policyBlocks = `
policy "attributed" {
  teams        = ["etabA", "etabB"]
  require      = ["user", "metadata.project"]
  require_tags = true
}
policy "large" {
  max_body_bytes     = 33554432
  max_response_bytes = 134217728
}
`

func TestParsePolicies(t *testing.T) {
	c, err := parse(valid + policyBlocks)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policies) != 2 {
		t.Fatalf("%+v", c.Policies)
	}
	a, l := c.Policies[0], c.Policies[1]
	if a.Name != "attributed" || len(a.Teams) != 2 || len(a.Require) != 2 || !a.RequireTags || a.MaxBodyBytes != 0 ||
		l.MaxBodyBytes != 32<<20 || l.MaxResponseBytes != 128<<20 || l.RequireTags {
		t.Fatalf("%+v %+v", a, l)
	}
	for path, want := range map[string]bool{"user": true, "metadata.project": true, "a-b_c.d": true, "": false, ".a": false, "a.": false, "a..b": false,
		"a.b.c.d.e.f.g.h": true, "a.b.c.d.e.f.g.h.i": false, "é": false} {
		if ValidFieldPath(path) != want {
			t.Errorf("ValidFieldPath(%q) != %v", path, want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(valid))
	f.Add([]byte(valid + `store { kind = "postgres" dsn_file = "/d" }`))
	f.Add([]byte("master_key_file = \"$${x}\"\nmodel \"m\" {\n upstream \"u\" {\n  base_url = \"http://h\"\n  key_file = <<EOT\n/k\nEOT\n }\n}\npool \"m\" { capacity = 1 }\n"))
	f.Add([]byte(valid + policyBlocks))
	f.Fuzz(func(t *testing.T, src []byte) {
		c, err := Parse(src, "fuzz.hcl")
		if err != nil {
			return
		}
		// Policies that parse are usable: valid names and paths, bounded
		// sizes, a team in one policy at most.
		inPolicy := map[string]bool{}
		for _, p := range c.Policies {
			if !ValidName(p.Name) || p.MaxBodyBytes < 0 || p.MaxBodyBytes > maxPolicyBytes || p.MaxResponseBytes < 0 || p.MaxResponseBytes > maxPolicyBytes {
				t.Fatalf("bad policy %+v", p)
			}
			for _, r := range p.Require {
				if !ValidFieldPath(r) {
					t.Fatalf("bad required field %q", r)
				}
			}
			for _, tm := range p.Teams {
				if inPolicy[tm] {
					t.Fatalf("team %q in two policies", tm)
				}
				inPolicy[tm] = true
			}
		}
		sum := 0.0
		teams := map[string]bool{}
		for _, o := range c.Orgs {
			if !ValidName(o.Name) || o.Share < 0 || o.Share > 1 {
				t.Fatalf("bad org %+v", o)
			}
			sum += o.Share
			for _, tm := range o.Teams {
				if !ValidName(tm) || teams[tm] {
					t.Fatalf("bad or duplicate team %q", tm)
				}
				teams[tm] = true
			}
		}
		if sum > 1+1e-9 {
			t.Fatalf("shares sum %v", sum)
		}
		pools := map[string]bool{}
		for _, p := range c.Pools {
			if p.Capacity <= 0 {
				t.Fatalf("capacity %d", p.Capacity)
			}
			pools[p.Name] = true
		}
		if len(c.ModelGroups) == 0 || c.MasterKeyFile == "" {
			t.Fatal("accepted without model or master key")
		}
		for _, g := range c.ModelGroups {
			if !pools[g.Pool] || len(g.Upstreams) == 0 {
				t.Fatalf("group %+v", g)
			}
			for _, u := range g.Upstreams {
				if ValidateUpstream(u.BaseURL) != nil || u.KeyFile == "" {
					t.Fatalf("upstream %+v", u)
				}
			}
		}
	})
}
