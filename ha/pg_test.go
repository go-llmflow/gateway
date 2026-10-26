package ha

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
	"github.com/jackc/pgx/v5"
)

// The same properties against a real PostgreSQL (LLMFLOW_TEST_PG_DSN, a
// service container in CI), each test in a database of its own.
func pgStore(t *testing.T) *store.Postgres {
	t.Helper()
	admin := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if admin == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "ha_" + hex.EncodeToString(b[:])
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(admin)
	u.Path = "/" + name
	st, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return st
}

func TestPGLeaseRace(t *testing.T) { testLeaseRace(t, pgStore(t)) }

func TestPGPropertyOneOwnerPerEpoch(t *testing.T) { propertyOneOwnerPerEpoch(t, pgStore(t), 7) }

// Fencing at the storage: an owner whose lease was taken cannot write.
func TestPGUsageWriteFenced(t *testing.T) {
	st := pgStore(t)
	ctx := context.Background()
	l, err := claimLease(st, ctx, "p", "old", "127.0.0.1:1", 0, 50*time.Millisecond)
	if err != nil || l.Owner != "old" || l.Epoch != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	rows := []store.PoolUsage{{Org: "a", Usage: 3, InFlight: 2}}
	if err := st.SaveUsage(ctx, "p", "old", 1, rows); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	l, err = claimLease(st, ctx, "p", "new", "127.0.0.1:2", 0, time.Minute)
	if err != nil || l.Owner != "new" || l.Epoch != 2 {
		t.Fatalf("takeover: %+v %v", l, err)
	}
	if err := st.SaveUsage(ctx, "p", "old", 1, []store.PoolUsage{{Org: "a", Usage: 99}}); !errors.Is(err, store.ErrFenced) {
		t.Fatalf("stale owner's write: %v, want ErrFenced", err)
	}
	got, err := st.LoadUsage(ctx, "p")
	if err != nil || len(got) != 1 || got[0].Usage != 3 || got[0].InFlight != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	// Renewal keeps the epoch; the old owner cannot renew.
	if l, _ = claimLease(st, ctx, "p", "new", "127.0.0.1:2", 2, time.Minute); l.Epoch != 2 || l.Owner != "new" {
		t.Fatalf("renewal: %+v", l)
	}
	if l, _ = claimLease(st, ctx, "p", "old", "127.0.0.1:1", 1, time.Minute); l.Owner != "new" {
		t.Fatalf("the old owner renewed a lease it lost: %+v", l)
	}
}

func TestPGLeaseYield(t *testing.T) { testLeaseYield(t, pgStore(t)) }
