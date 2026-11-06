package config

import (
	"strings"
	"testing"
)

// Group names compare without case, ASCII only (the same fold the lookup
// uses): "kate" and "Kate" with a Kelvin sign are two groups, "G" and "g" are one.
func TestSCIMGroupNamesFoldASCIIOnlyAtConfiguration(t *testing.T) {
	scim := func(a, b string) string {
		return valid + "scim {\n  token_file = \"/t\"\n  base_url = \"https://gw.example/scim/v2\"\n  domain = \"example.org\"\n  group \"" + a + "\" { team = \"etabA\" }\n  group \"" + b + "\" { team = \"etabB\" }\n}\n"
	}
	if _, err := parse(scim("kate", "Kate")); err != nil { // Kelvin sign
		t.Errorf("two different group names were refused as one: %v", err)
	}
	if _, err := parse(scim("G", "g")); err == nil || !strings.Contains(err.Error(), "Invalid group") {
		t.Errorf("G and g: %v, want Invalid group", err)
	}
}
