package ha

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// Regression test for the security review of 2026-11-05, finding 7: the replay
// windows are in memory, so a request captured before a receiver restarted was
// accepted once by the restarted receiver (inside the 5-minute timestamp
// window), and the MAC did not name the receiver, so a request for one replica
// was valid at another. A request is now bound to its receiver's advertised
// address and incarnation.

func signed(n *Node, addr, epoch, path string, seq uint64, body []byte) *http.Request {
	s := strconv.FormatUint(seq, 10)
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set(hdrInstance, "peer")
	req.Header.Set(hdrSeq, s)
	req.Header.Set(hdrTime, ts)
	req.Header.Set(hdrEpoch, epoch)
	req.Header.Set(hdrSig, mac(n.rpcKey, "req", http.MethodPost, path, "peer", addr, epoch, s, ts, bodyHash(body)))
	return req
}

func startNode(t *testing.T, advertise string) *Node {
	t.Helper()
	cfg := testConfig("owner", advertise)
	n, err := New(cfg, NewMemStore(nil))
	if err != nil {
		t.Fatal(err)
	}
	n.Start(context.Background())
	t.Cleanup(func() { n.Close(context.Background()) })
	return n
}

func TestReplayAfterReceiverRestartIsRefused(t *testing.T) {
	body := []byte(`{"pool":"p","releases":[]}`)
	n1 := startNode(t, "127.0.0.1:7001")
	req := signed(n1, n1.cfg.Advertise, n1.epoch, pathRelease, 1, body)
	w := httptest.NewRecorder()
	n1.Handler().ServeHTTP(w, req.Clone(req.Context()))
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusMisdirectedRequest {
		t.Fatalf("the genuine request: %d", w.Code)
	}
	// The same request, replayed to the same incarnation: the window refuses.
	req2 := signed(n1, n1.cfg.Advertise, n1.epoch, pathRelease, 1, body)
	w = httptest.NewRecorder()
	n1.Handler().ServeHTTP(w, req2)
	if w.Code == http.StatusOK {
		t.Errorf("a replay inside one incarnation was accepted")
	}
	// The receiver restarts: same address, same secret, empty windows. The
	// captured request (signed for the previous incarnation) must not work.
	n2 := startNode(t, "127.0.0.1:7001")
	if n2.epoch == n1.epoch {
		t.Fatal("two incarnations with one epoch")
	}
	captured := signed(n1, n1.cfg.Advertise, n1.epoch, pathRelease, 1, body)
	w = httptest.NewRecorder()
	n2.Handler().ServeHTTP(w, captured)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("EXPLOIT: a request captured before the restart: %d", w.Code)
	}
	// The sender learns the new epoch from the signed 421 and goes on.
	if w.Header().Get(hdrEpoch) != n2.epoch {
		t.Errorf("the 421 does not name the epoch: %q", w.Header().Get(hdrEpoch))
	}
	fresh := signed(n2, n2.cfg.Advertise, n2.epoch, pathRelease, 2, body)
	w = httptest.NewRecorder()
	n2.Handler().ServeHTTP(w, fresh)
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusMisdirectedRequest {
		t.Errorf("a request bound to the new epoch: %d", w.Code)
	}
}

func TestRequestForAnotherReplicaIsRefused(t *testing.T) {
	a, b := startNode(t, "127.0.0.1:7001"), startNode(t, "127.0.0.1:7002")
	req := signed(a, a.cfg.Advertise, a.epoch, pathRelease, 1, []byte(`{"pool":"p","releases":[]}`))
	w := httptest.NewRecorder()
	b.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("EXPLOIT: a request meant for replica a was valid at replica b: %d", w.Code)
	}
}
