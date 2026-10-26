package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// FakeS3 is a path-style S3 for tests: objects, an optional Object Lock
// configuration, ListObjectsV2, and the lock headers of every put.
type FakeS3 struct {
	mu      sync.Mutex
	Objects map[string][]byte
	Locks   map[string]string // key -> mode + " " + retain-until
	Lock    bool
	Fail    atomic.Int64 // answer 503 this many times
	srv     *httptest.Server
}

func NewFakeS3(t testing.TB) *FakeS3 {
	f := &FakeS3{Objects: map[string][]byte{}, Locks: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *FakeS3) URL() string { return f.srv.URL }

func (f *FakeS3) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		w.WriteHeader(403)
		return
	}
	if f.Fail.Load() > 0 {
		f.Fail.Add(-1)
		w.WriteHeader(503)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/bkt"), "/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Has("object-lock"):
		if !f.Lock {
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>ObjectLockConfigurationNotFoundError</Code><Message>none</Message></Error>`)
			return
		}
		io.WriteString(w, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
	case r.Method == http.MethodGet && q.Get("list-type") == "2":
		var keys []string
		for k := range f.Objects {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("<ListBucketResult><IsTruncated>false</IsTruncated>")
		for _, k := range keys {
			fmt.Fprintf(&b, "<Contents><Key>%s</Key><Size>%d</Size></Contents>", k, len(f.Objects[k]))
		}
		b.WriteString("</ListBucketResult>")
		io.WriteString(w, b.String())
	case r.Method == http.MethodPut:
		if _, ok := f.Objects[key]; ok && f.Locks[key] != "" {
			w.WriteHeader(403)
			io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>locked</Message></Error>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.Objects[key] = b
		if m := r.Header.Get("X-Amz-Object-Lock-Mode"); m != "" {
			f.Locks[key] = m + " " + r.Header.Get("X-Amz-Object-Lock-Retain-Until-Date")
		}
	case r.Method == http.MethodGet:
		b, ok := f.Objects[key]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
			return
		}
		w.Write(b)
	default:
		w.WriteHeader(405)
	}
}

func s3Config(url string) config.Export {
	return config.Export{Name: "archive", Kind: "s3", Endpoint: url, Region: "us-east-1", Bucket: "bkt", PathStyle: true,
		Prefix: "llmflow/", LockMode: "COMPLIANCE", Retention: 90 * 24 * time.Hour, QueueSize: 4}
}

func TestS3Sink(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	for _, lock := range []bool{false, true} {
		f := NewFakeS3(t)
		f.Lock = lock
		s, err := Open(ctx, s3Config(f.URL()), Credentials{"ak", "sk"}, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if s.Locked() != lock {
			t.Fatalf("lock %v: Locked() = %v", lock, s.Locked())
		}
		if err := s.Put(ctx, "audit/checkpoints/1.json", []byte("cp"), PutOptions{ContentType: "application/json", Retain: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(ctx, "audit/x.json", []byte("plain"), PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.Objects["llmflow/audit/checkpoints/1.json"]; !ok {
			t.Fatalf("keys %v", f.Objects)
		}
		wantLock := ""
		if lock {
			wantLock = "COMPLIANCE 2027-01-22T08:00:00Z"
		}
		if f.Locks["llmflow/audit/checkpoints/1.json"] != wantLock || f.Locks["llmflow/audit/x.json"] != "" {
			t.Fatalf("lock %v: %v", lock, f.Locks)
		}
		b, err := s.Get(ctx, "audit/checkpoints/1.json")
		if err != nil || string(b) != "cp" {
			t.Fatalf("get %q %v", b, err)
		}
		keys, err := s.List(ctx, "audit/checkpoints/")
		if err != nil || len(keys) != 1 || keys[0] != "audit/checkpoints/1.json" {
			t.Fatalf("list %v %v", keys, err)
		}
		if err := s.Put(ctx, "../escape", nil, PutOptions{}); err == nil {
			t.Fatal("a key with .. accepted")
		}
		if _, err := s.Get(ctx, "a//b"); err == nil {
			t.Fatal("an empty segment accepted")
		}
	}
	f := NewFakeS3(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	if _, err := Open(ctx, s3Config(f.URL()), Credentials{"ak", "sk"}, nil); err == nil {
		t.Fatal("a bucket that refuses access opened")
	}
}

func TestFileSink(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, config.Export{Name: "local", Kind: "file", Directory: dir, Prefix: "gw1/"}, Credentials{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Locked() || s.Name() != "local" {
		t.Fatal("a directory claims to lock")
	}
	if err := s.Put(ctx, "audit/checkpoints/0001.json", []byte("one"), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "audit/checkpoints/0001.json", []byte("two"), PutOptions{}); err == nil {
		t.Fatal("a file was overwritten")
	}
	s.Put(ctx, "audit/checkpoints/0002.json", []byte("2"), PutOptions{})
	s.Put(ctx, "audit/entries/1-2.jsonl.gz", []byte("e"), PutOptions{})
	keys, err := s.List(ctx, "audit/checkpoints/")
	if err != nil || len(keys) != 2 || keys[1] != "audit/checkpoints/0002.json" {
		t.Fatalf("list %v %v", keys, err)
	}
	if b, err := s.Get(ctx, "audit/checkpoints/0001.json"); err != nil || string(b) != "one" {
		t.Fatalf("%q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "gw1", "audit", "checkpoints", "0001.json")); err != nil || fi.Mode().Perm()&0o077 != 0 && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi, err)
	}
	for _, k := range []string{"../x", "a/../../x", "/abs", "a\\b", "", "a/./b"} {
		if err := s.Put(ctx, k, nil, PutOptions{}); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
	os.WriteFile(filepath.Join(dir, "gw1", "big"), make([]byte, MaxObject+1), 0o600)
	if _, err := s.Get(ctx, "big"); err == nil {
		t.Fatal("an oversized object read")
	}
	empty, _ := Open(ctx, config.Export{Name: "e", Kind: "file", Directory: filepath.Join(dir, "none"), Prefix: "p/"}, Credentials{}, nil)
	if keys, err := empty.List(ctx, ""); err != nil || len(keys) != 0 {
		t.Fatalf("empty list %v %v", keys, err)
	}
}

func TestNotImplemented(t *testing.T) {
	for _, k := range []string{"gcs", "azure"} {
		if _, err := Open(context.Background(), config.Export{Name: k, Kind: k}, Credentials{}, nil); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("%s: %v", k, err)
		}
	}
	if _, err := Open(context.Background(), config.Export{Name: "x", Kind: "ftp"}, Credentials{}, nil); err == nil {
		t.Error("unknown kind opened")
	}
}

func TestQueue(t *testing.T) {
	ctx := context.Background()
	f := NewFakeS3(t)
	s, err := Open(ctx, s3Config(f.URL()), Credentials{"ak", "sk"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := NewQueue(s, 2)
	q.sleep = func(context.Context, time.Duration) error { return nil }
	var written []string
	var mu sync.Mutex
	q.OnWritten = func(k string) { mu.Lock(); written = append(written, k); mu.Unlock() }
	var failed []string
	q.OnFail = func(k string, err error) { mu.Lock(); failed = append(failed, k); mu.Unlock() }
	f.Fail.Store(3) // three 503s: retried
	q.Enqueue("a", []byte("1"), PutOptions{})
	q.Close(ctx)
	if q.Written.Load() != 1 || len(written) != 1 || f.Objects["llmflow/a"] == nil {
		t.Fatalf("written %d %v", q.Written.Load(), written)
	}

	// A destination that never answers: given up, counted.
	q2 := NewQueue(s, 1)
	q2.sleep = func(context.Context, time.Duration) error { return nil }
	q2.OnFail = q.OnFail
	f.Fail.Store(1000)
	q2.Enqueue("b", nil, PutOptions{})
	q2.Close(ctx)
	if q2.Failed.Load() != 1 || len(failed) != 1 {
		t.Fatalf("failed %d %v", q2.Failed.Load(), failed)
	}
	f.Fail.Store(0)

	// A full queue drops, and says so.
	block := make(chan struct{})
	slow := &blockingSink{Sink: s, block: block}
	q3 := NewQueue(slow, 1)
	var dropped []string
	q3.OnDrop = func(k string) { dropped = append(dropped, k) }
	q3.Enqueue("c1", nil, PutOptions{}) // taken by the writer, which blocks
	time.Sleep(20 * time.Millisecond)
	q3.Enqueue("c2", nil, PutOptions{}) // fills the queue
	if q3.Enqueue("c3", nil, PutOptions{}) || q3.Dropped.Load() != 1 || len(dropped) != 1 || dropped[0] != "c3" {
		t.Fatalf("drop: %d %v", q3.Dropped.Load(), dropped)
	}
	close(block)
	q3.Close(ctx)
	if q3.Written.Load() != 2 {
		t.Fatalf("written after unblocking: %d", q3.Written.Load())
	}

	// Close gives up at its deadline.
	block4 := make(chan struct{})
	q4 := NewQueue(&blockingSink{Sink: s, block: block4}, 4)
	q4.Enqueue("d", nil, PutOptions{})
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	q4.Close(cctx)
	if time.Since(start) > 5*time.Second {
		t.Fatal("Close did not give up")
	}
	close(block4)
}

// Security audit 2026-10-26, finding A5 (chaos: the export target down):
// the queue was bounded by its number of objects only (1024 by default),
// and a request-log batch can reach 8 MiB, so an unreachable S3 let up to
// 8 GiB of logs pile up in the gateway's memory before anything was
// dropped. The queue is bounded in bytes too.
func TestQueueBoundedInBytes(t *testing.T) {
	down := &downSink{block: make(chan struct{})}
	q := NewQueue(down, 1024)
	q.sleep = func(context.Context, time.Duration) error { return nil }
	defer q.Close(context.Background())
	defer close(down.block)      // first: the writer gives up, then Close drains
	batch := make([]byte, 8<<20) // one backing array: the test itself stays small
	for i := 0; i < 200; i++ {
		q.Enqueue(fmt.Sprintf("requests/%d", i), batch, PutOptions{})
	}
	if q.Dropped.Load() == 0 {
		t.Fatalf("1.6 GiB queued behind an unreachable sink, nothing dropped")
	}
	if held := q.queued.Load(); held > DefaultMaxQueueBytes {
		t.Fatalf("queue holds %d bytes, bound %d", held, DefaultMaxQueueBytes)
	}
}

// downSink is an export target that is down: every Put waits for block
// (or its context), then fails.
type downSink struct{ block chan struct{} }

func (d *downSink) Name() string { return "down" }
func (d *downSink) Locked() bool { return false }
func (d *downSink) Get(context.Context, string) ([]byte, error) {
	return nil, errors.New("connection refused")
}
func (d *downSink) List(context.Context, string) ([]string, error) {
	return nil, errors.New("connection refused")
}
func (d *downSink) Put(ctx context.Context, _ string, _ []byte, _ PutOptions) error {
	select {
	case <-d.block:
	case <-ctx.Done():
	}
	return errors.New("connection refused")
}

type blockingSink struct {
	Sink
	block chan struct{}
}

func (b *blockingSink) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	select {
	case <-b.block:
	case <-ctx.Done():
		return ctx.Err()
	}
	return b.Sink.Put(ctx, key, body, o)
}

// FuzzValidKey: what ValidKey accepts never leaves a directory.
func FuzzValidKey(f *testing.F) {
	for _, s := range []string{"audit/checkpoints/00000000000000000001.json", "../x", "a/..", "a//b", ".", "a\\b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, k string) {
		if !ValidKey(k) {
			return
		}
		p := filepath.Join("/base", filepath.FromSlash(k))
		if !strings.HasPrefix(p, "/base/") {
			t.Fatalf("%q escapes: %s", k, p)
		}
	})
}
