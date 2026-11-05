package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func sessionName(t *testing.T, e *ssoEnv, sess string) string {
	t.Helper()
	resp := e.req("GET", "/admin/session", map[string]string{sessionCookie: sess}, "")
	var info struct{ Name string }
	if err := json.Unmarshal([]byte(readAll(t, resp)), &info); err != nil || resp.StatusCode != 200 {
		t.Fatalf("session: %d %v", resp.StatusCode, err)
	}
	return info.Name
}

// An account is relative to an institution's domain. The provider's domain
// scopes the names of accounts that predate scoped names, and a stored user
// with the unscoped name is the same person (the backwards-compatible
// migration); a new person is local@domain.
func TestSSOScopedNamesAndLegacyMigration(t *testing.T) {
	e := newSSOEnv(t, "")
	ctx := context.Background()
	// bob exists since before scoped names, with a role.
	if err := e.st.CreateUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Grant(ctx, store.Binding{User: "bob", Role: store.RoleTeamAdmin, Team: "etabA", Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	sess, _ := e.login(e.claims("bob"))
	if got := sessionName(t, e, sess); got != "bob" {
		t.Errorf("a legacy user is still bob: %q", got)
	}
	// carol is new: she is carol@example.org.
	sess, _ = e.login(e.claims("carol", "llm-admins"))
	if got := sessionName(t, e, sess); got != "carol@example.org" {
		t.Errorf("a new person: %q", got)
	}
	// A scoped name of the provider's domain, any case, is the same person.
	c := e.claims("dave", "llm-admins")
	c["preferred_username"] = "Dave@EXAMPLE.org"
	sess, _ = e.login(c)
	if got := sessionName(t, e, sess); got != "dave@example.org" {
		t.Errorf("a scoped name: %q", got)
	}
	// Another institution's scope is refused: this provider may not assert it.
	c = e.claims("eve", "llm-admins")
	c["preferred_username"] = "eve@other.example"
	authURL, ck := e.start()
	state, code := e.idp.issue(t, authURL, c)
	if resp := e.callback(state, code, ck); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a scope the provider may not assert: %d", resp.StatusCode)
	}
}

// Without a domain, an unscoped name is no identity at all.
func TestSSOUnscopedNameWithoutDomainIsRefused(t *testing.T) {
	e := newSSOEnv(t, "", func(s *config.SSO) { s.Domain = "" })
	authURL, ck := e.start()
	state, code := e.idp.issue(t, authURL, e.claims("frank", "llm-admins"))
	if resp := e.callback(state, code, ck); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an unscoped name, no domain: %d", resp.StatusCode)
	}
}
