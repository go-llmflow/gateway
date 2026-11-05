package gateway

// Regression tests for the security review of 2026-11-05 (v0.27.0), finding 2: the SCIM client acts on
// every gateway user by name, provisioned or not. It can disable a user it
// never provisioned (here a proxy admin) by creating a SCIM user of the same
// userName and deleting it, and any SCIM write re-enables a user a proxy
// admin disabled (DELETE /admin/users/{name}), which the admin API itself
// refuses to org admins (security audit 2026-10-26, A3).

import (
	"context"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

func TestSCIMDisablesUnprovisionedProxyAdmin(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()
	// root: created by hand, proxy admin, never provisioned.
	if err := st.CreateUser(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	if err := st.Grant(ctx, store.Binding{User: "root", Role: store.RoleProxyAdmin, Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	id := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "root", 1))["id"].(string)
	e.scimOK(204, "DELETE", "/Users/"+id, "")
	if e.disabled("root") {
		t.Errorf("EXPLOIT: the SCIM client disabled %q, a proxy admin it never provisioned", "root")
	}
}

func TestSCIMReenablesUserDisabledByProxyAdmin(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "mallory", 1))
	// The proxy admin cuts mallory off.
	e.check(nil, []call{{"master", "DELETE", "/admin/users/mallory@example.org", "", 204}})
	if !e.disabled("mallory@example.org") {
		t.Fatal("not disabled")
	}
	// Any later provisioning write (an unrelated user here) re-enables her.
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "someone-else", 1))
	if !e.disabled("mallory@example.org") {
		t.Errorf("EXPLOIT: an unrelated SCIM write re-enabled %q, whom a proxy admin had disabled", "mallory")
	}
}

// An account is relative to an institution's domain: the userName is
// local@domain with a domain the client may name, an unscoped one takes the
// scim domain, and a stored user with that unscoped name is the same person.
func TestSCIMScopedNames(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()
	// A user stored before scoped names, with a role.
	if err := st.CreateUser(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := st.Grant(ctx, store.Binding{User: "legacy", Role: store.RoleProxyAdmin, Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	// A userName of another institution: refused.
	if got, m := e.scim("POST", "/Users", strings.Replace(userBody, "%s", "mallory@other.example", 1)); got != 400 || m["scimType"] != "invalidValue" {
		t.Errorf("a scope this client may not name: %d %v", got, m)
	}
	// A new unscoped userName is local@domain; scoped ones are the same person
	// in any case.
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "newbie", 1))
	if u, err := st.GetUser(ctx, "newbie@example.org"); err != nil || u.Origin != "scim" {
		t.Errorf("a new person: %+v %v", u, err)
	}
	// The unscoped userName of a stored user is that user (not a second
	// person), and being hand-made it is not SCIM's to touch.
	id := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "legacy", 1))["id"].(string)
	if _, err := st.GetUser(ctx, "legacy@example.org"); err == nil {
		t.Error("a second person was created for a stored legacy user")
	}
	e.scimOK(204, "DELETE", "/Users/"+id, "")
	if e.disabled("legacy") {
		t.Error("SCIM disabled a hand-made user")
	}
}

// Users stored before origins existed: SCIM claims those it gave roles to and
// nobody else did; a user with a role anyone else granted is never claimed.
func TestSCIMClaimsOnlyUsersItProvisioned(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()
	for _, u := range []string{"old-scim@example.org", "old-admin@example.org"} {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "old-scim@example.org", 1))
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "old-admin@example.org", 1))
	// (a start-up claims before the first reconciliation removes anything)
	if err := st.Grant(ctx, store.Binding{User: "old-scim@example.org", Role: store.RoleMember, Team: "etabA", Provenance: store.ProvSCIM}); err != nil {
		t.Fatal(err)
	}
	if err := st.Grant(ctx, store.Binding{User: "old-admin@example.org", Role: store.RoleMember, Team: "etabA", Provenance: store.ProvSCIM}); err != nil {
		t.Fatal(err)
	}
	if err := st.Grant(ctx, store.Binding{User: "old-admin@example.org", Role: store.RoleProxyAdmin, Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.scimBackend.claimLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.GetUser(ctx, "old-scim@example.org"); u.Origin != "scim" {
		t.Errorf("a user only SCIM gave roles to: %+v", u)
	}
	if u, _ := st.GetUser(ctx, "old-admin@example.org"); u.Origin != "" {
		t.Errorf("a user with a role an administrator granted was claimed: %+v", u)
	}
}
