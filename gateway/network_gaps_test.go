package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Security review 2026-11-05 F8: the uploaded audio and the error bodies the
// gateway writes itself were not in the network meter.

// An audio upload counts the audio: the multipart file is kept aside from the
// JSON view the pipeline forwards.
func TestNetworkMeterCountsTheAudioUpload(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Limits.MaxAudioBytes = 100000
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	audio := bytes.Repeat([]byte{7}, 50000)
	ct, form := audioForm(t, "a.wav", audio, "model", "llama")
	if code, out, _ := e.transcribe(key, ct, form); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status == 200 })
	if r.BytesIn < int64(len(audio)) || r.BytesIn > int64(up.last().RawLen) {
		t.Fatalf("bytes in %d: the audio alone is %d, the upstream received %d", r.BytesIn, len(audio), up.last().RawLen)
	}
}

// An error the gateway writes itself (no upstream could serve the request) is
// bytes the client received.
func TestNetworkMeterCountsErrorBodies(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	up.srv.Close() // every upstream is down: the gateway answers itself
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 500 || len(b) == 0 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status >= 500 })
	if r.BytesOut != int64(len(b)) {
		t.Fatalf("bytes out %d, the client read %d", r.BytesOut, len(b))
	}
}

// Close waits for the storage meter calls the last flush started.
func TestRequestLogMeterIsAwaited(t *testing.T) {
	var done atomic.Bool
	l := &requestLogger{orgBytes: map[string]int64{"etabA": 10}}
	l.meter = func(day, org string, n int64) {
		time.Sleep(150 * time.Millisecond)
		done.Store(true)
	}
	l.meterBatch("2026-10-05", 100)
	if done.Load() {
		t.Fatal("the meter ran synchronously: the test proves nothing")
	}
	l.waitMeter()
	if !done.Load() {
		t.Fatal("waitMeter returned before the meter call ended")
	}
}

// The proxy roles read the whole method (every markup, the gateway version);
// an organisation's admin reads its own markup and no version.
func TestMethodIsScopedToItsReader(t *testing.T) {
	up := newFakeVLLM(t)
	dc := newFakeDCGM(t)
	cfg := fluidsConfig(up.URL(), dc.srv.URL)
	for i := range cfg.Orgs {
		cfg.Orgs[i].Markup = map[string]float64{"etabA": 0.37, "etabB": 0.11}[cfg.Orgs[i].Name]
	}
	e := newEnv(t, cfg)
	pa := e.adminToken("pa", `{"role":"proxy_admin"}`)
	ob := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	hdr := func(tok string) (string, string) {
		resp := e.do("GET", "/admin/chargeback?from=2026-10-01&to=2026-11-01", tok, "")
		readAll(t, resp)
		return resp.Header.Get("X-Llmflow-Method"), resp.Header.Get("X-Llmflow-Gateway-Version")
	}
	m, v := hdr(pa)
	if !strings.Contains(m, `"etabA":0.37`) || !strings.Contains(m, `"etabB":0.11`) || v == "" {
		t.Errorf("proxy admin: %s / %q", m, v)
	}
	m, v = hdr(ob)
	if strings.Contains(m, "etabA") || !strings.Contains(m, `"etabB":0.11`) || v != "" || strings.Contains(m, "gateway_version") {
		t.Errorf("org admin of etabB: %s / %q", m, v)
	}
}
