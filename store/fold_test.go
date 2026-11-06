package store

import (
	"context"
	"errors"
	"testing"
)

// Second-pass audit. Names are scoped names, which go-authn/oidc folds ASCII
// only: kate@example.org and "Kate@example.org" with a Kelvin sign (U+212A) are
// two accounts at the IdP. strings.EqualFold folds Unicode, so the in-memory
// store treated them as one user: the second was refused as a duplicate, and a
// lookup of "kate" returned the Kelvin account.
func TestSCIMNamesThatDifferByTheKelvinSignAreTwoUsers(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	const kate, kelvin = "kate@example.org", "Kate@example.org"
	if err := s.SCIMCreate(ctx, SCIMDoc{Kind: "User", ID: "u1", Name: kate, Data: []byte(`{}`), Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SCIMCreate(ctx, SCIMDoc{Kind: "User", ID: "u2", Name: kelvin, Data: []byte(`{}`), Version: "v1"}); err != nil {
		t.Fatalf("a different account was refused as a duplicate of kate: %v", err)
	}
	got, err := s.SCIMList(ctx, "User", kate)
	if err != nil || len(got) != 1 || got[0].ID != "u1" {
		t.Errorf("a lookup of kate returned %d users %v (%v)", len(got), got, err)
	}
	// ASCII case is still folded: it is the same account.
	if err := s.SCIMCreate(ctx, SCIMDoc{Kind: "User", ID: "u3", Name: "KATE@Example.ORG", Data: []byte(`{}`), Version: "v1"}); !errors.Is(err, ErrExists) {
		t.Errorf("KATE@Example.ORG: %v, want ErrExists", err)
	}
	if got, _ := s.SCIMList(ctx, "User", "KATE@EXAMPLE.ORG"); len(got) != 1 || got[0].ID != "u1" {
		t.Errorf("an upper-case lookup returned %v", got)
	}
}
