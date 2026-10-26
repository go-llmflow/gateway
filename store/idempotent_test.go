package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// A batch replayed from the accounting spool after a crash may already be
// stored (the process died between the insert and the spool's truncation):
// inserting it again bills nothing twice. Records without an ID (written
// before v0.10.0) are not deduplicated.
func checkIdempotentInsert(t *testing.T, st Store, count func() int) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	batch := []Record{
		{ID: "r1", Time: now, Org: "o", Team: "t", KeyID: "k", Model: "m", Cost: 1, Status: 200},
		{ID: "r2", Time: now, Org: "o", Team: "t", KeyID: "k", Model: "m", Cost: 2, Status: 200},
	}
	if err := st.InsertRecords(ctx, batch); err != nil {
		t.Fatal(err)
	}
	// The replay: the same batch, plus one record the crash had not stored.
	replay := append(append([]Record(nil), batch...), Record{ID: "r3", Time: now, Org: "o", Team: "t", KeyID: "k", Model: "m", Cost: 3, Status: 200})
	if err := st.InsertRecords(ctx, replay); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("rows after the replay: %d, want 3", n)
	}
	legacy := []Record{{Time: now, Org: "o", Team: "t", KeyID: "k", Model: "m", Status: 200}}
	_ = st.InsertRecords(ctx, legacy)
	_ = st.InsertRecords(ctx, legacy)
	if n := count(); n != 5 {
		t.Fatalf("rows with legacy records: %d, want 5", n)
	}
}

func TestIdempotentInsertMemory(t *testing.T) {
	m := NewMemory()
	checkIdempotentInsert(t, m, func() int { return len(m.Records()) })
}

func TestIdempotentInsertPostgres(t *testing.T) {
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dropAll(t, p)
	p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	checkIdempotentInsert(t, p, func() int {
		ids, err := p.UsageRecordIDs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	})
}
