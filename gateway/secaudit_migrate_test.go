package gateway

// Security audit 2026-11-05 (untracked proof). The records of a JWT caller with
// no assertable name carry the synthetic person "jwt-<hash(iss,sub)>"
// (attribution.go:72). RecordOnlyUsers returns it as a bare legacy name and
// migrate-users renames it to jwt-...@<legacy domain>: people of ANY issuer are
// moved under the legacy institution's namespace, and every later run does it
// again for the new synthetic records.

import (
	"context"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func TestSecauditMigrateRenamesSyntheticJWTPersons(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	if err := st.InsertRecords(ctx, []store.Record{{ID: "r1", Time: time.Now().UTC(), Org: "etabB", Team: "etabB",
		User: "jwt-AbCdEf_-0123456789xy", KeyID: "jwt-AbCdEf_-0123456789xy", Model: "llama", Pool: "b300", Cost: 1}}); err != nil {
		t.Fatal(err)
	}
	plan, _, err := MigrateUsers(ctx, st, "univ-a.fr", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan {
		if r.From == "jwt-AbCdEf_-0123456789xy" {
			t.Errorf("EXPLOIT: the synthetic (iss, sub) person of another issuer is renamed into the legacy domain: %+v", r)
		}
	}
	for _, r := range st.Records() {
		if r.User == "jwt-AbCdEf_-0123456789xy@univ-a.fr" {
			t.Errorf("EXPLOIT: etabB's record is now attributed to %q", r.User)
		}
	}
}
