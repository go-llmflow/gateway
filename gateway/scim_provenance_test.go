package gateway

// Security review 2026-11-05 (v0.27.0), finding F3: the SCIM client acts on
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

func TestSecReviewSCIMDisablesUnprovisionedProxyAdmin(t *testing.T) {
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

func TestSecReviewSCIMReenablesUserDisabledByProxyAdmin(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "mallory", 1))
	// The proxy admin cuts mallory off.
	e.check(nil, []call{{"master", "DELETE", "/admin/users/mallory", "", 204}})
	if !e.disabled("mallory") {
		t.Fatal("not disabled")
	}
	// Any later provisioning write (an unrelated user here) re-enables her.
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "someone-else", 1))
	if !e.disabled("mallory") {
		t.Errorf("EXPLOIT: an unrelated SCIM write re-enabled %q, whom a proxy admin had disabled", "mallory")
	}
}
