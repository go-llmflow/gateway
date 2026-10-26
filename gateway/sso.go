package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/go-authn/oidc"
	"github.com/go-authn/rp"
	"github.com/go-llmflow/gateway/store"
)

// Single sign-on for the admin API. The OpenID Connect login itself
// (discovery, PKCE S256, state, nonce, ID token verification) is
// github.com/go-authn/rp; this file keeps what is the gateway's own: the
// login binding cookie, the server-side session, the CSRF token and the
// mapping of claims to role bindings.
//
// Cookies use the __Host- prefix (RFC 6265bis §4.1.3.2: Secure, Path=/, no
// Domain), HttpOnly and SameSite=Lax. A state-changing request authenticated
// by the session cookie must carry the session's CSRF token in X-CSRF-Token
// (OWASP CSRF cheat sheet, synchronizer token pattern) and must not come
// from another origin (Origin, Sec-Fetch-Site). Requests with a bearer token
// are not cookie-authenticated and need no CSRF token.

const (
	sessionCookie = "__Host-llmflow-session"
	loginCookie   = "__Host-llmflow-login"
	csrfHeader    = "X-CSRF-Token"
	loginTTL      = 10 * time.Minute
	maxSessions   = 10_000
	maxLogins     = 10_000
	// maxLoginsPerNet bounds the logins in progress per client network
	// (IPv4 address, IPv6 /64).
	maxLoginsPerNet = 32
)

type session struct {
	user     string
	bindings []store.Binding // from the ID token's claims, at login
	csrf     string
	expires  time.Time
}

type pendingLogin struct {
	flow    rp.Flow
	browser [32]byte // SHA-256 of the login cookie
	expires time.Time
	net     netip.Prefix
	// ui: after the login, go to the admin UI (?next=ui). A fixed
	// destination, never a URL from the request: no open redirect.
	ui bool
}

type ssoState struct {
	client *rp.Client

	mu       sync.Mutex
	sessions map[[32]byte]*session // by SHA-256 of the cookie value
	logins   map[string]pendingLogin
	perNet   map[netip.Prefix]int
}

var errSSOFull = errors.New("too many logins in progress")

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// startSSO discovers the provider and reads its keys, at start-up.
func (s *Server) startSSO(ctx context.Context) error {
	c := s.cfg.SSO
	if c == nil {
		return nil
	}
	p, err := rp.Discover(ctx, s.jwtClient, c.Issuer)
	if err != nil {
		return fmt.Errorf("gateway: sso: %w", err)
	}
	cl, err := rp.NewClient(ctx, p, rp.Config{ClientID: c.ClientID, Secret: s.secrets.SSOClientSecret, RedirectURI: c.RedirectURL,
		Scopes: c.Scopes, MinRefresh: c.MinRefresh})
	if err != nil {
		return fmt.Errorf("gateway: sso: %w", err)
	}
	s.sso = &ssoState{client: cl, sessions: map[[32]byte]*session{}, logins: map[string]pendingLogin{}, perNet: map[netip.Prefix]int{}}
	return nil
}

func (s *Server) ssoRoutes(mux *http.ServeMux) {
	wrap := func(f http.HandlerFunc) http.Handler { return s.withClientAddr(s.cfg.Routes.Admin, f) }
	mux.Handle("GET /admin/login", wrap(s.ssoLogin))
	mux.Handle("GET /admin/callback", wrap(s.ssoCallback))
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (st *ssoState) dropLoginLocked(state string) {
	l, ok := st.logins[state]
	if !ok {
		return
	}
	delete(st.logins, state)
	if st.perNet[l.net]--; st.perNet[l.net] <= 0 {
		delete(st.perNet, l.net)
	}
}

// purgeLocked drops expired logins and sessions; called when a map is full.
func (st *ssoState) purgeLocked(now time.Time) {
	for k, l := range st.logins {
		if now.After(l.expires) {
			st.dropLoginLocked(k)
		}
	}
	for k, se := range st.sessions {
		if now.After(se.expires) {
			delete(st.sessions, k)
		}
	}
}

// ssoLogin starts a login: a fresh flow, bound to this browser by a
// random cookie, and a redirect to the provider.
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	f := rp.NewFlow()
	browser := randomToken()
	now := s.now()
	cn, _ := clientNet(r)
	st := s.sso
	st.mu.Lock()
	if len(st.logins) >= maxLogins || st.perNet[cn] >= maxLoginsPerNet {
		st.purgeLocked(now)
	}
	// One network cannot hold more than a few logins in progress, so a
	// flood from one place cannot lock everybody else out of logging in.
	full := len(st.logins) >= maxLogins || st.perNet[cn] >= maxLoginsPerNet
	if !full {
		st.logins[f.State] = pendingLogin{flow: f, browser: sha256.Sum256([]byte(browser)), expires: now.Add(loginTTL), net: cn,
			ui: r.URL.Query().Get("next") == "ui"}
		st.perNet[cn]++
	}
	st.mu.Unlock()
	if full {
		s.log.Warn("sso: login refused", "reason", errSSOFull)
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", errSSOFull.Error())
		return
	}
	setCookie(w, loginCookie, browser, int(loginTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, st.client.AuthCodeURL(f, nil), http.StatusFound)
}

// ssoCallback finishes a login. Every failure is the same 400 to the
// browser; the reason goes to the log.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	cn, haveNet := clientNet(r)
	if haveNet && s.authFails.blocked(cn) {
		apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many failed authentications from this address")
		return
	}
	fail := func(status int, reason string, err error) {
		s.log.Warn("sso: login failed", "reason", reason, "err", err)
		if haveNet {
			s.authFails.fail(cn)
		}
		setCookie(w, loginCookie, "", -1)
		msg := "login failed"
		if status == http.StatusForbidden {
			msg = "no administrative role for this account"
		}
		apiError(w, status, "authentication_error", msg)
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		fail(http.StatusBadRequest, "provider error", errors.New(truncate(e, 64)))
		return
	}
	state, code := q.Get("state"), q.Get("code")
	if state == "" || code == "" || len(state) > 256 || len(code) > 4096 {
		fail(http.StatusBadRequest, "missing or oversized state or code", nil)
		return
	}
	st := s.sso
	st.mu.Lock()
	pl, ok := st.logins[state]
	st.dropLoginLocked(state) // one use, whatever happens next
	st.mu.Unlock()
	if !ok || s.now().After(pl.expires) {
		fail(http.StatusBadRequest, "unknown, used or expired state", nil)
		return
	}
	ck, err := r.Cookie(loginCookie)
	if err != nil {
		fail(http.StatusBadRequest, "no login cookie", err)
		return
	}
	if h := sha256.Sum256([]byte(ck.Value)); subtle.ConstantTimeCompare(h[:], pl.browser[:]) != 1 {
		fail(http.StatusBadRequest, "login cookie does not match the state", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, id, err := st.client.Exchange(ctx, code, pl.flow)
	if err != nil {
		fail(http.StatusBadRequest, "code exchange", err)
		return
	}
	user := id.Username()
	if user == "" || len(user) > 256 {
		fail(http.StatusBadRequest, "no usable username in the ID token", nil)
		return
	}
	bindings := s.ssoBindings(id, user)
	u, err := s.st.GetUser(ctx, user)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if len(bindings) == 0 {
			fail(http.StatusForbidden, "no role: not a gateway user and no role block matches", nil)
			return
		}
	case err != nil:
		s.log.Error("sso: user lookup failed", "err", err)
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
		return
	case u.Disabled:
		fail(http.StatusForbidden, "user disabled", nil)
		return
	}
	value := randomToken()
	now := s.now()
	st.mu.Lock()
	if len(st.sessions) >= maxSessions {
		st.purgeLocked(now)
	}
	full := len(st.sessions) >= maxSessions
	if !full {
		st.sessions[sha256.Sum256([]byte(value))] = &session{user: user, bindings: bindings, csrf: randomToken(), expires: now.Add(s.cfg.SSO.SessionTTL)}
	}
	st.mu.Unlock()
	if full {
		s.log.Warn("sso: session refused", "reason", "too many sessions")
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "too many sessions")
		return
	}
	s.audit("sso.login", "by", user, "user", user)
	setCookie(w, loginCookie, "", -1)
	setCookie(w, sessionCookie, value, int(s.cfg.SSO.SessionTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	if pl.ui {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/session", http.StatusSeeOther)
}

// ssoBindings maps the ID token's claims to role bindings.
func (s *Server) ssoBindings(id *oidc.Token, user string) []store.Binding {
	var out []store.Binding
	for _, m := range s.cfg.SSO.Roles {
		if slices.Contains(claimValues(id, m.Claim), m.Value) {
			out = append(out, store.Binding{User: user, Role: store.Role(m.Role), Org: m.Org, Team: m.Team})
		}
	}
	return out
}

// sessionPrincipal authenticates an admin request by its session cookie.
// ok=false with handled=false means there is no session cookie at all.
func (s *Server) sessionPrincipal(w http.ResponseWriter, r *http.Request) (p *principal, csrf string, handled bool) {
	if s.sso == nil {
		return nil, "", false
	}
	ck, err := r.Cookie(sessionCookie)
	if err != nil || ck.Value == "" {
		return nil, "", false
	}
	h := sha256.Sum256([]byte(ck.Value))
	now := s.now()
	st := s.sso
	st.mu.Lock()
	se := st.sessions[h]
	if se != nil && now.After(se.expires) {
		delete(st.sessions, h)
		se = nil
	}
	var cp session
	if se != nil {
		cp = *se
	}
	st.mu.Unlock()
	if se == nil {
		setCookie(w, sessionCookie, "", -1)
		apiError(w, http.StatusUnauthorized, "authentication_error", "session expired or unknown; log in again at /admin/login")
		return nil, "", true
	}
	if unsafeMethod(r.Method) {
		if !sameOrigin(r, s.cfg.SSO.Origin()) {
			apiError(w, http.StatusForbidden, "permission_denied", "cross-origin request refused")
			return nil, "", true
		}
		if got := r.Header.Get(csrfHeader); subtle.ConstantTimeCompare([]byte(got), []byte(cp.csrf)) != 1 {
			apiError(w, http.StatusForbidden, "permission_denied", "missing or wrong "+csrfHeader)
			return nil, "", true
		}
	}
	// Stored roles and the disabled flag are read on every request, so a
	// revocation takes effect at once.
	u, err := s.st.GetUser(r.Context(), cp.user)
	var stored []store.Binding
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
		return nil, "", true
	case u.Disabled:
		st.mu.Lock()
		delete(st.sessions, h)
		st.mu.Unlock()
		setCookie(w, sessionCookie, "", -1)
		apiError(w, http.StatusUnauthorized, "authentication_error", "user disabled")
		return nil, "", true
	default:
		if stored, err = s.st.Bindings(r.Context(), cp.user); err != nil {
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
			return nil, "", true
		}
	}
	return &principal{name: cp.user, bindings: append(slices.Clip(cp.bindings), stored...), session: h}, cp.csrf, true
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// sameOrigin refuses a request a browser marks as coming from elsewhere:
// Sec-Fetch-Site (Fetch Metadata) when present, else Origin when present.
func sameOrigin(r *http.Request, origin string) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	o := r.Header.Get("Origin")
	return o == "" || o == origin
}

// sessionInfo returns the session's user, roles and CSRF token.
func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request) {
	p, csrf, handled := s.sessionPrincipal(w, r)
	if !handled {
		apiError(w, http.StatusUnauthorized, "authentication_error", "no session; log in at /admin/login")
		return
	}
	if p == nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	bs := p.bindings
	if bs == nil {
		bs = []store.Binding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": p.name, "roles": bs, "csrf_token": csrf})
}

// logout ends the session here and returns the provider's end-session URL.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p, _, handled := s.sessionPrincipal(w, r)
	if !handled {
		apiError(w, http.StatusUnauthorized, "authentication_error", "no session")
		return
	}
	if p == nil {
		return
	}
	s.sso.mu.Lock()
	delete(s.sso.sessions, p.session)
	s.sso.mu.Unlock()
	s.audit("sso.logout", "by", p.name, "user", p.name)
	setCookie(w, sessionCookie, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"end_session_url": s.sso.client.EndSessionURL("")})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
