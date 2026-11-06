package gateway

// Security audit 2026-11-05 (2): untracked proof tests.

import (
	"context"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// A hand-made user holds team_admin on etabA, granted by a proxy admin. The
// SCIM client never provisioned this user; by POSTing a SCIM User of the
// same userName, it strips the role (reconcile marks every named user as
// "provisioned", whatever its origin).
func TestSecAudit2SCIMRevokesHandGrantedRoleOfHandMadeUser(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()
	if err := st.CreateUser(ctx, "boss@example.org"); err != nil {
		t.Fatal(err)
	}
	ta := store.Binding{User: "boss@example.org", Role: store.RoleTeamAdmin, Team: "etabA", Provenance: store.ProvProxyAdmin}
	if err := st.Grant(ctx, ta); err != nil {
		t.Fatal(err)
	}
	if u, _ := st.GetUser(ctx, "boss@example.org"); u.Origin == "scim" {
		t.Fatalf("precondition: origin %q", u.Origin)
	}
	e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "boss@example.org", 1))
	if !e.hasBinding(ta) {
		t.Errorf("EXPLOIT: SCIM revoked team_admin@etabA that a proxy admin granted to hand-made user boss")
	}
}

// The same hand-made user, put by SCIM into a mapped group, receives a role.
func TestSecAudit2SCIMGrantsRoleToHandMadeUser(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()
	if err := st.CreateUser(ctx, "boss@example.org"); err != nil {
		t.Fatal(err)
	}
	id := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "boss@example.org", 1))["id"].(string)
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"lab leads","members":[{"value":"`+id+`"}]}`)
	if e.hasBinding(store.Binding{User: "boss@example.org", Role: store.RoleTeamAdmin, Team: "etabA"}) {
		t.Errorf("EXPLOIT: SCIM granted team_admin@etabA to hand-made user boss")
	}
}
