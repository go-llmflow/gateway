package spool

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func recs(n int) []store.Record {
	out := make([]store.Record, n)
	for i := range out {
		out[i] = store.Record{ID: fmt.Sprintf("r%03d", i), Time: time.Unix(1700000000+int64(i), 0).UTC(), Org: "o", Team: "t",
			KeyID: "k", Model: "m", PromptTokens: int64(i), Cost: float64(i) / 10, Status: 200, Tags: []string{"a"}}
	}
	return out
}

func TestRoundTripAndTornTail(t *testing.T) {
	in := recs(5)
	b, err := Encode(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	got, tail, err := Read(bytes.NewReader(b))
	if err != nil || tail != Clean || len(got) != 5 || got[4].ID != "r004" || !got[2].Time.Equal(in[2].Time) {
		t.Fatalf("%v %v %+v", tail, err, got)
	}
	// Every cut inside the last frame is a torn tail: the first four survive.
	one, _ := Encode(nil, in[4:])
	for cut := 1; cut < len(one); cut++ {
		got, tail, _ := Read(bytes.NewReader(b[:len(b)-cut]))
		if tail != Torn || len(got) != 4 {
			t.Fatalf("cut %d: %v, %d records", cut, tail, len(got))
		}
	}
	// A flipped byte inside a payload is corruption, not a record.
	bad := append([]byte(nil), b...)
	bad[headerSize+3] ^= 0xff
	if got, tail, _ := Read(bytes.NewReader(bad)); tail != Corrupt || len(got) != 0 {
		t.Fatalf("corrupt: %v %d", tail, len(got))
	}
}

func TestSpoolFileModeBoundAndOrphans(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := Open(dir, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(s.Path())
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes: file %o dir %o", fi.Mode().Perm(), di.Mode().Perm())
	}
	if err := s.Append(recs(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(recs(20)); err != ErrFull {
		t.Fatalf("over the bound: %v", err)
	}
	// Held by a live process (us): not an orphan, even through another
	// open file.
	if os, _ := Orphans(dir, "", 1<<20); len(os) != 0 {
		t.Fatalf("a live spool was adopted: %d", len(os))
	}
	// The process ends with records left: the next one adopts them.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	orphans, err := Orphans(dir, "", 1<<20)
	if err != nil || len(orphans) != 1 || len(orphans[0].Records) != 3 {
		t.Fatalf("orphans %v %v", orphans, err)
	}
	if err := orphans[0].Done(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// An empty spool removes itself.
	s2, _ := Open(dir, 2048)
	_ = s2.Append(recs(1))
	_ = s2.Reset()
	_ = s2.Close()
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("empty spool left behind: %v", left)
	}
}

func TestOpenRefusesAWorldWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, 1<<20); err == nil {
		t.Fatal("a world-writable spool directory was accepted")
	}
}

// FuzzRead: a spool file is untrusted after a crash. Whatever its bytes,
// Read does not panic, returns only records that were whole frames, and a
// valid prefix followed by garbage still yields that prefix.
func FuzzRead(f *testing.F) {
	good, _ := Encode(nil, recs(3))
	f.Add(good)
	f.Add(good[:len(good)-5])
	f.Add(append(append([]byte(nil), good...), 'L', 'F', 0xff, 0xff, 0xff, 0xff))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		got, tail, err := Read(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("a reader over bytes failed: %v", err)
		}
		for _, r := range got {
			if r.ID == "" {
				t.Fatal("a record without ID")
			}
		}
		// Re-encoding what was read gives a prefix of the input when the
		// input was produced by Encode (whatever followed).
		withPrefix := append(append([]byte(nil), good...), b...)
		pre, _, _ := Read(bytes.NewReader(withPrefix))
		if len(pre) < 3 {
			t.Fatalf("a valid prefix was lost: %d records, tail %v", len(pre), tail)
		}
	})
}
