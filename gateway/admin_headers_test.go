package gateway

import (
	"strings"
	"testing"
)

// Security audit 2026-10-26, A8: the admin API, SSO and SCIM answers
// (errors and the mux's own 404/405 included) carry nosniff, a CSP that
// loads nothing and forbids framing, and no-store. Only /ui/ had them.
func TestAdminResponsesHardened(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	for _, c := range []struct{ method, path, token string }{
		{"GET", "/admin/whoami", ""},
		{"GET", "/admin/whoami", master},
		{"GET", "/admin/no-such-route", master},
		{"PATCH", "/admin/keys", master},
		{"GET", "/admin/session", ""},
		{"GET", "/scim/v2/Users", ""},
	} {
		resp := e.do(c.method, c.path, c.token, "")
		readAll(t, resp)
		h := resp.Header
		if h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s (%d): headers %v", c.method, c.path, resp.StatusCode, h)
		}
	}
	// Inference answers are not touched.
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	readAll(t, resp)
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Errorf("inference answer got the admin CSP")
	}
}
