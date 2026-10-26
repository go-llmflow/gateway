package ha

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// FuzzInternalAPI sends correctly signed but arbitrary bodies to an owner's
// internal API: a peer holding the secret (or a bug in one) must not crash
// the owner or make it count what it should not.
func FuzzInternalAPI(f *testing.F) {
	f.Add(uint8(0), []byte(`{"pool":"p","id":"x","org":"a","class":0,"tokens":10}`))
	f.Add(uint8(1), []byte(`{"pool":"p","releases":[{"id":"x","org":"a","consumed":1e308}]}`))
	f.Add(uint8(2), []byte(`{"pool":"p","grants":[{"id":"y","org":"b","class":1,"priority":999,"borrowing":true,"epoch":0}],"releases":[]}`))
	f.Add(uint8(2), []byte(`{"pool":"p","grants":null}`))
	f.Add(uint8(0), []byte(`{"pool":"q"}`))
	st := NewMemStore(nil)
	cfg := testConfig("owner", "127.0.0.1:1")
	n, err := New(cfg, st)
	if err != nil {
		f.Fatal(err)
	}
	n.Start(context.Background())
	defer n.Close(context.Background())
	h := n.Handler()
	var seq atomic.Uint64
	paths := []string{pathAcquire, pathRelease, pathReport}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		path := paths[int(which)%len(paths)]
		s := strconv.FormatUint(seq.Add(1), 10)
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set(hdrInstance, "peer")
		req.Header.Set(hdrSeq, s)
		req.Header.Set(hdrTime, ts)
		req.Header.Set(hdrSig, mac(n.rpcKey, "req", http.MethodPost, path, "peer", s, ts, bodyHash(body)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusUnauthorized {
			t.Fatalf("a correctly signed request was refused")
		}
		for _, s := range n.Snapshot("p") {
			if s.InFlight < 0 || s.Borrowing < 0 || s.Borrowing > s.InFlight || s.Usage < 0 {
				t.Fatalf("inconsistent state after %q: %+v", body, s)
			}
		}
	})
}
