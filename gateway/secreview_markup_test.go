package gateway

// Security review 2026-11-05 (v0.28.0), finding F5: the billing method
// stamped on the fluids and chargeback reports (billing.go stampMethod,
// billingMethod) carries the markup of EVERY organisation, and is written
// whatever the reader's scope: an org_admin of etabB reads etabA's markup
// (the commercial term of another partner's agreement) in the
// X-Llmflow-Method header and in the body's "method".

import (
	"net/http"
	"strings"
	"testing"
)

func TestSecReviewOtherOrgMarkupDisclosed(t *testing.T) {
	up := newFakeVLLM(t)
	dc := newFakeDCGM(t)
	cfg := fluidsConfig(up.URL(), dc.srv.URL)
	for i := range cfg.Orgs {
		if cfg.Orgs[i].Name == "etabA" {
			cfg.Orgs[i].Markup = 0.37
		}
	}
	e := newEnv(t, cfg)
	ob := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	for _, path := range []string{"/admin/fluids", "/admin/chargeback"} {
		resp := e.do("GET", path+"?from=2026-10-01&to=2026-11-01", ob, "")
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Logf("%s: %d %s", path, resp.StatusCode, body)
			continue
		}
		h := resp.Header.Get("X-Llmflow-Method")
		if strings.Contains(h, `"etabA":0.37`) || strings.Contains(body, `"etabA":0.37`) {
			t.Errorf("EXPLOIT: org_admin of etabB reads etabA's markup on %s: header %s", path, h)
		}
	}
}
