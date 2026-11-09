package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/go-authn/oidc"
	"github.com/go-authn/rp"
	"github.com/go-llmflow/gateway/config"
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
	// provider and subject are the identity the session was opened with
	// (OpenID Connect Core §2: sub is unique within its issuer only).
	provider, subject string
}

type pendingLogin struct {
	// provider is the provider this login was sent to: its callback, and only
	// its callback, may finish it (RFC 9700 §4.4.2).
	provider string
	flow     rp.Flow
	browser  [32]byte // SHA-256 of the login cookie
	expires  time.Time
	net      netip.Prefix
	// ui: after the login, go to the admin UI (?next=ui). A fixed
	// destination, never a URL from the request: no open redirect.
	ui bool
}

// ssoProvider is one configured OpenID provider and its client.
type ssoProvider struct {
	cfg    config.SSOProvider
	client *rp.Client
}

// rpKey is the name a provider has in the rp.Set: the single sso issuer of
// earlier versions has none in the configuration.
func rpKey(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

type ssoState struct {
	set       *rp.Set                 // the providers, with the mix-up defences of go-authn/rp
	providers map[string]*ssoProvider // by name ("" for the single sso issuer)
	order     []*ssoProvider

	mu       sync.Mutex
	sessions map[[32]byte]*session // by SHA-256 of the cookie value
	logins   map[string]pendingLogin
	perNet   map[netip.Prefix]int
	// shared, with an ha block, keeps logins and sessions in PostgreSQL
	// instead of the maps above (hastate.go).
	shared *sharedSSO
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
	st := &ssoState{set: rp.NewSet(), providers: map[string]*ssoProvider{}, sessions: map[[32]byte]*session{}, logins: map[string]pendingLogin{}, perNet: map[netip.Prefix]int{}}
	for _, pc := range c.ProviderList() {
		p, err := rp.Discover(ctx, s.jwtClient, pc.Issuer)
		if err != nil {
			return fmt.Errorf("gateway: sso provider %q: %w", pc.Name, err)
		}
		secret := s.secrets.SSOClientSecret
		if pc.Name != "" {
			secret = s.secrets.SSOClientSecrets[pc.Name]
		}
		cl, err := rp.NewClient(ctx, p, rp.Config{ClientID: pc.ClientID, Secret: secret, RedirectURI: pc.RedirectURL,
			Scopes: pc.Scopes, MinRefresh: pc.MinRefresh})
		if err != nil {
			return fmt.Errorf("gateway: sso provider %q: %w", pc.Name, err)
		}
		if err := st.set.Add(rpKey(pc.Name), cl); err != nil {
			return fmt.Errorf("gateway: sso provider %q: %w", pc.Name, err)
		}
		sp := &ssoProvider{cfg: pc, client: cl}
		st.providers[pc.Name] = sp
		st.order = append(st.order, sp)
	}
	s.sso = st
	if s.shared != nil {
		// Several replicas: a login started on one finishes on another, a
		// session outlives the replica that created it.
		// The replicas' secret, or alone the master key: sessions are sealed
		// under a key derived from it (domain-separated, never the secret).
		secret := s.secrets.HASecret
		if secret == "" {
			secret = s.secrets.MasterKey
		}
		se, err := newSealer([]byte(secret))
		if err != nil {
			return err
		}
		s.sso.shared = &sharedSSO{st: s.shared, sealer: se}
	}
	return nil
}

func (s *Server) ssoRoutes(mux *http.ServeMux) {
	wrap := func(f http.HandlerFunc) http.Handler { return s.withClientAddr(s.cfg.Routes.Admin, f) }
	mux.Handle("GET /admin/login", wrap(s.ssoLoginPage))
	mux.Handle("GET /admin/login/{provider}", wrap(func(w http.ResponseWriter, r *http.Request) { s.ssoLogin(w, r, r.PathValue("provider")) }))
	// The single sso issuer of earlier versions keeps /admin/callback; a
	// provider block has its own callback, a distinct redirect URI per
	// issuer (RFC 9700 §4.4.2.2).
	mux.Handle("GET /admin/callback", wrap(func(w http.ResponseWriter, r *http.Request) { s.ssoCallback(w, r, "") }))
	mux.Handle("GET /admin/callback/{provider}", wrap(func(w http.ResponseWriter, r *http.Request) { s.ssoCallback(w, r, r.PathValue("provider")) }))
}

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Sign in</title><meta name="viewport" content="width=device-width, initial-scale=1"></head>
<body><h1>Sign in</h1><p>Choose your institution's identity provider.</p>
<ul>{{range .}}<li><a href="{{.URL}}">{{.Name}}</a></li>{{end}}</ul></body></html>
`))

// ssoLoginPage starts the login at the one provider, or lists them: a
// server-rendered page, no script.
func (s *Server) ssoLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	if len(s.sso.order) == 1 {
		s.ssoLogin(w, r, s.sso.order[0].cfg.Name)
		return
	}
	type entry struct{ Name, URL string }
	var es []entry
	for _, p := range s.sso.order {
		u := "/admin/login/" + url.PathEscape(p.cfg.Name)
		if r.URL.Query().Get("next") == "ui" {
			u += "?next=ui"
		}
		es = append(es, entry{p.cfg.Name, u})
	}
	uiHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginPage.Execute(w, es)
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
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request, provider string) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	prov := s.sso.providers[provider]
	if prov == nil {
		http.NotFound(w, r)
		return
	}
	f, err := s.sso.set.NewFlow(rpKey(provider))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	browser := randomToken()
	now := s.now()
	cn, _ := clientNet(r)
	err = s.putLogin(r.Context(), f.State, pendingLogin{provider: provider, flow: f, browser: sha256.Sum256([]byte(browser)), expires: now.Add(loginTTL), net: cn,
		ui: r.URL.Query().Get("next") == "ui"})
	if err != nil {
		s.log.Warn("sso: login refused", "reason", err)
		msg := errSSOFull.Error()
		if !errors.Is(err, errSSOFull) {
			msg = "session store unreachable"
		}
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
		return
	}
	setCookie(w, loginCookie, browser, int(loginTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, prov.client.AuthCodeURL(f, nil), http.StatusFound)
}

// ssoCallback finishes a login. Every failure is the same 400 to the
// browser; the reason goes to the log.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request, provider string) {
	if s.sso == nil {
		http.NotFound(w, r)
		return
	}
	prov := s.sso.providers[provider]
	if prov == nil {
		http.NotFound(w, r)
		return
	}
	var shown string // what the browser is told, when it is more than "login failed"
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
		if shown != "" {
			msg = shown
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
	pl, ok, err := s.takeLogin(r.Context(), state) // one use, whatever happens next
	if err != nil {
		s.log.Error("sso: login store unreachable", "err", err)
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "session store unreachable")
		return
	}
	if !ok {
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
	// go-authn/rp: the login went to one provider and only that provider's
	// callback finishes it; an iss response parameter must be its issuer
	// (RFC 9700 §4.4.2, RFC 9207 §2.4); the ID token's iss is verified against
	// this provider by go-authn/oidc.
	_, id, err := s.sso.set.Exchange(ctx, rpKey(provider), code, pl.flow, q)
	if err != nil {
		reason := "code exchange"
		if errors.Is(err, rp.ErrMixUp) {
			reason = "mix-up"
		}
		fail(http.StatusBadRequest, reason, err)
		return
	}
	// The institution filter comes before anything else.
	// The person's ONE institution (their name's scope) when anything keys on the
	// institution claim, accept or not (see person.go); roles never read the raw
	// claim.
	if ref := oidc.Check(prov.cfg.Accept, id); ref != nil {
		shown = acceptMessage(ref)
		fail(http.StatusForbidden, "institution not accepted", ref)
		return
	}
	inst := ""
	claims := make([]string, 0, len(prov.cfg.Roles))
	for _, m := range prov.cfg.Roles {
		claims = append(claims, m.Claim)
	}
	if usesInstitution(prov.cfg.Accept, claims...) {
		var ierr error
		if inst, ierr = institutionOf(id, prov.cfg.NameScopes(), prov.cfg.Domain); ierr != nil {
			shown = "your identity provider states an institution that is not the one of your name"
			fail(http.StatusForbidden, "name scope is not the institution stated", ierr)
			return
		}
		if !institutionAccepted(prov.cfg.Accept, inst) {
			shown = "your institution is not accepted by this gateway"
			fail(http.StatusForbidden, "institution not accepted", errors.New(inst))
			return
		}
	}
	user, err := s.scopedPerson(ctx, id, prov.cfg.NameScopes(), prov.cfg.Domain)
	if errors.Is(err, errPersonStore) {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
		return
	}
	if err != nil || len(user) > 256 {
		shown = "your identity provider gave no name with an institution this gateway accepts for it"
		fail(http.StatusForbidden, "no usable scoped username in the ID token", err)
		return
	}
	subject := id.Subject()
	if subject == "" || len(subject) > 256 {
		fail(http.StatusBadRequest, "no usable subject in the ID token", nil)
		return
	}
	bindings := s.ssoBindings(prov, id, user, inst)
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
	default:
		// An identity is (provider, subject): a stored user belongs to the
		// provider that first vouched for them, so another provider asserting
		// the same name is refused, whatever it says.
		ok, err := s.bindIdentity(ctx, prov, user, subject)
		if err != nil {
			s.log.Error("sso: identity link failed", "err", err)
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
			return
		}
		if !ok {
			fail(http.StatusForbidden, "identity: user is bound to another provider or subject", fmt.Errorf("user %q, provider %q, sub %q", user, provider, truncate(subject, 64)))
			return
		}
	}
	value := randomToken()
	now := s.now()
	if err := s.putSession(ctx, sha256.Sum256([]byte(value)), session{user: user, bindings: bindings, csrf: randomToken(), expires: now.Add(s.cfg.SSO.SessionTTL),
		provider: provider, subject: subject}); err != nil {
		s.log.Warn("sso: session refused", "reason", err)
		msg := "too many sessions"
		if !errors.Is(err, errSessionsFull) {
			msg = "session store unreachable"
		}
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", msg)
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

// ssoBindings maps the ID token's claims to role bindings, through the
// provider's role blocks. A role outside the organisations the provider may
// vouch for is refused and logged: an institution's provider never makes
// anyone an administrator of another institution.
func (s *Server) ssoBindings(p *ssoProvider, id *oidc.Token, user, institution string) []store.Binding {
	var out []store.Binding
	for _, m := range p.cfg.Roles {
		if !claimMatches(id, m.Claim, m.Value, institution) {
			continue
		}
		if !p.cfg.MayGrant(s.cfg, m) {
			s.log.Warn("sso: role refused, outside the provider's organisations", "provider", p.cfg.Name, "role", m.Role, "org", m.Org, "team", m.Team, "user", user)
			continue
		}
		out = append(out, store.Binding{User: user, Role: store.Role(m.Role), Org: m.Org, Team: m.Team})
	}
	return out
}

// bindIdentity links a stored user to (provider, subject) the first time, and
// afterwards accepts only that identity. A user without a link (stored before
// v0.27.0, or provisioned and not logged in yet) is adopted by the single
// unrestricted sso issuer of earlier versions, or by a provider block whose
// organisations cover every role the user holds (a user with no role: only a
// provider with adopt = true).
func (s *Server) bindIdentity(ctx context.Context, p *ssoProvider, user, subject string) (bool, error) {
	want := store.IdPLink{Provider: p.cfg.Name, Subject: subject}
	cur, err := s.st.UserIdP(ctx, user)
	switch {
	case err == nil:
		return cur == want, nil
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	if p.cfg.Restricted {
		// A provider block adopts a user only when it may vouch for every
		// role they hold (proxy-wide roles need allow_proxy_roles), and a
		// user with no role only with adopt = true.
		bs, err := s.st.Bindings(ctx, user)
		if err != nil {
			return false, err
		}
		if len(bs) == 0 && !p.cfg.Adopt {
			return false, nil
		}
		for _, b := range bs {
			if !p.cfg.MayGrant(s.cfg, config.SSORole{Role: string(b.Role), Org: b.Org, Team: b.Team}) {
				return false, nil
			}
		}
	}
	got, err := s.st.LinkUserIdP(ctx, user, want)
	return got == want, err
}

// acceptMessage is what the person is told when an accept block refuses them:
// the institution value (already cleaned by go-authn/oidc), no internals.
func acceptMessage(r *oidc.Refusal) string {
	if r.Value == "" {
		return "your identity provider did not state your institution; login refused"
	}
	return fmt.Sprintf("institution %q is not accepted by this gateway", r.Value)
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
	cp, found, err := s.getSession(r.Context(), h)
	if err != nil {
		s.log.Error("sso: session store unreachable", "err", err)
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "session store unreachable")
		return nil, "", true
	}
	if !found {
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
	linked := false
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
		s.dropSession(r.Context(), h)
		setCookie(w, sessionCookie, "", -1)
		apiError(w, http.StatusUnauthorized, "authentication_error", "user disabled")
		return nil, "", true
	default:
		// The stored roles are the user's own, granted to the identity the
		// user is linked to: a session opened by another provider (or
		// another subject) does not inherit them.
		link, lerr := s.st.UserIdP(r.Context(), cp.user)
		switch {
		case lerr != nil && !errors.Is(lerr, store.ErrNotFound):
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
			return nil, "", true
		case lerr == nil && link == (store.IdPLink{Provider: cp.provider, Subject: cp.subject}):
			linked = true
			if stored, err = s.st.Bindings(r.Context(), cp.user); err != nil {
				apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
				return nil, "", true
			}
		default:
			// The user exists now and is not (yet) bound to the identity this
			// session was opened with: a session opened before the user was
			// stored, or for another provider, must not become theirs. Log in
			// again, which goes through the adoption rule.
			s.dropSession(r.Context(), h)
			setCookie(w, sessionCookie, "", -1)
			apiError(w, http.StatusUnauthorized, "authentication_error", "session no longer valid; log in again at /admin/login")
			return nil, "", true
		}
	}
	return &principal{name: cp.user, bindings: append(slices.Clip(cp.bindings), stored...), session: h, provider: cp.provider, linked: linked}, cp.csrf, true
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
	s.dropSession(r.Context(), p.session)
	s.audit("sso.logout", "by", p.name, "user", p.name)
	setCookie(w, sessionCookie, "", -1)
	end := ""
	if prov := s.sso.providers[p.provider]; prov != nil {
		end = prov.client.EndSessionURL("")
	}
	writeJSON(w, http.StatusOK, map[string]any{"end_session_url": end})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
