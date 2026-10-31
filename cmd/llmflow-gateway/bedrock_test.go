package main

import (
	"strings"
	"testing"
)

// check-config reads the Bedrock credentials the configuration names, proves
// a key pair signs, and never prints a value.
func TestCheckConfigBedrock(t *testing.T) {
	dir := t.TempDir()
	mk := writeFile(t, dir, "master", "m-secret\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	id := writeFile(t, dir, "akid", "AKIDEXAMPLE0123456789\n")
	secret := writeFile(t, dir, "secret", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY\n")
	conf := func(idFile string) string {
		return `
master_key_file = "` + mk + `"
cache_salt_key_file = "` + sk + `"
org "o" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 4 }
model "claude" {
  provider       = "bedrock"
  region         = "eu-west-3"
  model_id       = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
  pool           = "p"
  allow_external = true
  orgs           = ["o"]
  cost {
    unit       = "USD"
    price_date = "2026-10-31"
  }
  credentials {
    access_key_id_file     = "` + idFile + `"
    secret_access_key_file = "` + secret + `"
  }
}
guardrail "pii" {
  type = "pii"
  orgs = ["o"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["o"]
}
`
	}
	out, err := run(t, "check-config", "-c", writeFile(t, dir, "ok.hcl", conf(id)))
	if err != nil || !strings.Contains(out, "ok: 1 orgs") {
		t.Fatalf("%q %v", out, err)
	}
	// A key id with a separator in it would break the signature's scope.
	badID := writeFile(t, dir, "badid", "AKID/EXAMPLE\n")
	out, err = run(t, "check-config", "-c", writeFile(t, dir, "bad.hcl", conf(badID)))
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a malformed key id: %q %v", out, err)
	}
	if strings.Contains(err.Error(), "AKID/EXAMPLE") || strings.Contains(err.Error(), "wJalr") || strings.Contains(out, "wJalr") {
		t.Fatalf("a credential was printed: %v", err)
	}
	// A missing file names the file, not its content.
	if _, err := run(t, "check-config", "-c", writeFile(t, dir, "missing.hcl", conf(dir+"/nope"))); err == nil {
		t.Fatal("a missing credential file was accepted")
	}
}
