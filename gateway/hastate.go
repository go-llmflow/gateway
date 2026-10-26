package gateway

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/go-authn/rp"
	"github.com/go-llmflow/gateway/ha"
	"github.com/go-llmflow/gateway/store"
)

// What several replicas share besides pool ownership (#74), with an ha
// block: SSO logins and sessions, the soft-budget alert outbox, and key
// revocations. Without one, all three stay in this process as before.

// sealer encrypts what the store keeps of a login or a session (AES-256-GCM,
// a random 96-bit nonce per seal, the entry's kind and hashed id as
// associated data, so a sealed value moved to another row does not open).
// The key is derived from the ha secret; the store sees only the SHA-256 of
// the cookie or state, never the value itself.
type sealer struct{ aead cipher.AEAD }

func newSealer(secret []byte) (*sealer, error) {
	b, err := aes.NewCipher(ha.DeriveKey(secret, "sso sessions v1"))
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: g}, nil
}

func aad(kind string, id [32]byte) []byte { return append([]byte("llmflow "+kind+" "), id[:]...) }

func (s *sealer) seal(kind string, id [32]byte, v any) ([]byte, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, aad(kind, id)), nil
}

var errUnseal = errors.New("gateway: a stored session does not open (wrong key or tampered)")

func (s *sealer) open(kind string, id [32]byte, b []byte, v any) error {
	n := s.aead.NonceSize()
	if len(b) < n+s.aead.Overhead() {
		return errUnseal
	}
	plain, err := s.aead.Open(nil, b[:n], b[n:], aad(kind, id))
	if err != nil {
		return errUnseal
	}
	return json.Unmarshal(plain, v)
}

// sharedSSO keeps logins and sessions in PostgreSQL.
type sharedSSO struct {
	st     store.HAStateStore
	sealer *sealer
}

const (
	kindLogin   = "sso_login"
	kindSession = "sso_session"
)

// The sealed forms.
type sealedLogin struct {
	State, Nonce, Verifier string
	Browser                [32]byte
	UI                     bool
	Net                    string
}

type sealedSession struct {
	User     string
	Bindings []store.Binding
	CSRF     string
}

func (s *Server) putLogin(ctx context.Context, state string, pl pendingLogin) error {
	st := s.sso
	if sh := st.shared; sh != nil {
		id := sha256.Sum256([]byte(state))
		b, err := sh.sealer.seal(kindLogin, id, sealedLogin{State: pl.flow.State, Nonce: pl.flow.Nonce, Verifier: pl.flow.Verifier,
			Browser: pl.browser, UI: pl.ui, Net: pl.net.String()})
		if err != nil {
			return err
		}
		err = sh.st.PutSealed(ctx, kindLogin, id, pl.net.String(), b, loginTTL, store.SealedLimits{Total: maxLogins, PerNet: maxLoginsPerNet})
		if errors.Is(err, store.ErrFull) {
			return errSSOFull
		}
		return err
	}
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.logins) >= maxLogins || st.perNet[pl.net] >= maxLoginsPerNet {
		st.purgeLocked(now)
	}
	// One network cannot hold more than a few logins in progress, so a
	// flood from one place cannot lock everybody else out of logging in.
	if len(st.logins) >= maxLogins || st.perNet[pl.net] >= maxLoginsPerNet {
		return errSSOFull
	}
	st.logins[state] = pl
	st.perNet[pl.net]++
	return nil
}

// takeLogin returns a live login in progress and forgets it (one use,
// whatever happens next).
func (s *Server) takeLogin(ctx context.Context, state string) (pendingLogin, bool, error) {
	st := s.sso
	if sh := st.shared; sh != nil {
		id := sha256.Sum256([]byte(state))
		b, ok, err := sh.st.TakeSealed(ctx, kindLogin, id)
		if err != nil || !ok {
			return pendingLogin{}, false, err
		}
		var sl sealedLogin
		if err := sh.sealer.open(kindLogin, id, b, &sl); err != nil {
			return pendingLogin{}, false, err
		}
		net, _ := netip.ParsePrefix(sl.Net)
		return pendingLogin{flow: rp.Flow{State: sl.State, Nonce: sl.Nonce, Verifier: sl.Verifier}, browser: sl.Browser,
			ui: sl.UI, net: net, expires: s.now().Add(time.Minute)}, true, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	pl, ok := st.logins[state]
	st.dropLoginLocked(state)
	if !ok || s.now().After(pl.expires) {
		return pendingLogin{}, false, nil
	}
	return pl, true, nil
}

func (s *Server) putSession(ctx context.Context, h [32]byte, se session) error {
	st := s.sso
	if sh := st.shared; sh != nil {
		b, err := sh.sealer.seal(kindSession, h, sealedSession{User: se.user, Bindings: se.bindings, CSRF: se.csrf})
		if err != nil {
			return err
		}
		err = sh.st.PutSealed(ctx, kindSession, h, "", b, s.cfg.SSO.SessionTTL, store.SealedLimits{Total: maxSharedSessions})
		if errors.Is(err, store.ErrFull) {
			return errSessionsFull
		}
		return err
	}
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.sessions) >= maxSessions {
		st.purgeLocked(now)
	}
	if len(st.sessions) >= maxSessions {
		return errSessionsFull
	}
	cp := se
	st.sessions[h] = &cp
	return nil
}

// getSession returns a live session.
func (s *Server) getSession(ctx context.Context, h [32]byte) (session, bool, error) {
	st := s.sso
	if sh := st.shared; sh != nil {
		b, ok, err := sh.st.GetSealed(ctx, kindSession, h)
		if err != nil || !ok {
			return session{}, false, err
		}
		var ss sealedSession
		if err := sh.sealer.open(kindSession, h, b, &ss); err != nil {
			return session{}, false, err
		}
		return session{user: ss.User, bindings: ss.Bindings, csrf: ss.CSRF}, true, nil
	}
	now := s.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	se := st.sessions[h]
	if se != nil && now.After(se.expires) {
		delete(st.sessions, h)
		se = nil
	}
	if se == nil {
		return session{}, false, nil
	}
	return *se, true, nil
}

func (s *Server) dropSession(ctx context.Context, h [32]byte) {
	st := s.sso
	if sh := st.shared; sh != nil {
		if err := sh.st.DropSealed(ctx, kindSession, h); err != nil {
			s.log.Error("sso: session not deleted", "err", err)
		}
		return
	}
	st.mu.Lock()
	delete(st.sessions, h)
	st.mu.Unlock()
}

// maxSharedSessions bounds the sessions kept in PostgreSQL (all replicas).
const maxSharedSessions = 100_000

var errSessionsFull = errors.New("too many sessions")

// ---- key revocations ----

// dropCached forgets a revoked identity from this replica's caches and,
// with an ha block, from every other replica's at once (NOTIFY). A
// notification missed (a replica reconnecting) is covered twice: the
// listener clears its caches when it reconnects, and key_cache_ttl bounds
// the rest.
func (s *Server) dropCached(kind, a, b string) {
	s.forgetKeys(revocationMatch(kind, a, b))
	if s.shared == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.shared.NotifyKeys(ctx, kind+":"+a+"/"+b); err != nil {
		s.log.Warn("ha: revocation not broadcast; other replicas drop it within key_cache_ttl", "kind", kind, "err", err)
	}
}

func revocationMatch(kind, a, b string) func(store.Key) bool {
	switch kind {
	case "key":
		return func(k store.Key) bool { return k.ID == a }
	case "team":
		return func(k store.Key) bool { return k.Team == a }
	case "org":
		return func(k store.Key) bool { return k.Org == a }
	case "project":
		return func(k store.Key) bool { return k.Team == a && k.Project == b }
	}
	return func(store.Key) bool { return false }
}

// onRevocation applies a revocation another replica broadcast.
func (s *Server) onRevocation(payload string) {
	kind, rest, ok := strings.Cut(payload, ":")
	if !ok {
		return
	}
	a, b, _ := strings.Cut(rest, "/")
	s.forgetKeys(revocationMatch(kind, a, b))
	s.met.revocations.add(1, kind)
}

// listenRevocations runs until ctx ends.
func (s *Server) listenRevocations(ctx context.Context) {
	first := true
	s.shared.ListenKeys(ctx, func() {
		if !first {
			// Notifications sent while disconnected are lost.
			s.forgetKeys(func(store.Key) bool { return true })
		}
		first = false
	}, s.onRevocation)
}

// ---- soft-budget alert outbox ----

// alertLease hides a claimed alert from the other replicas while one
// sends it; a replica that dies meanwhile leaves it to the next one.
const alertLease = 2 * time.Minute

// enqueueAlert records an alert's deliveries, one per channel, once.
func (s *Server) enqueueAlert(al softAlert) {
	p := alertPayload{Type: "soft_budget", Budget: al.b.BudgetKey, Period: al.b.Period, PeriodStart: al.start,
		SoftLimit: al.b.SoftLimit, MaxCost: al.b.MaxCost, Spent: al.spent}
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	var ch []string
	if s.cfg.Alerts.WebhookURL != "" {
		ch = append(ch, "webhook")
	}
	if s.cfg.Alerts.SMTP != nil {
		ch = append(ch, "smtp")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.shared.EnqueueAlert(ctx, idempotencyKey(p), ch, body); err != nil {
		s.log.Error("soft-budget alert not queued", "budget", al.b.BudgetKey.String(), "err", err)
	}
}

// outboxLoop delivers due alerts; every replica runs it, SKIP LOCKED and
// the lease keep two from sending the same one.
func (s *Server) outboxLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	purge := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.deliverDue(ctx)
		if time.Since(purge) > time.Minute {
			purge = time.Now()
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := s.shared.PurgeSealed(pctx); err != nil {
				s.log.Warn("ha: purge failed", "err", err)
			}
			cancel()
		}
	}
}

func (s *Server) deliverDue(ctx context.Context) {
	if s.alerts == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	due, err := s.shared.ClaimAlerts(cctx, 16, alertLease)
	cancel()
	if err != nil {
		return
	}
	for _, a := range due {
		var p alertPayload
		if json.Unmarshal(a.Payload, &p) != nil {
			s.finishAlert(ctx, a, "failed", 0, "undecodable payload")
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, time.Minute)
		var err error
		switch a.Channel {
		case "webhook":
			err = s.alerts.webhook(sctx, p)
		case "smtp":
			err = s.alerts.mail(p)
		default:
			err = permanent{errors.New("unknown channel")}
		}
		cancel()
		var perm permanent
		switch {
		case err == nil:
			s.finishAlert(ctx, a, "sent", 0, "")
		case errors.As(err, &perm) || a.Attempts >= s.alerts.attempts:
			s.alerts.failed.add(1, a.Channel)
			s.log.Error("soft-budget alert given up", "channel", a.Channel, "attempts", a.Attempts, "err", err)
			s.finishAlert(ctx, a, "failed", 0, err.Error())
		default:
			s.alerts.retried.add(1, a.Channel)
			s.finishAlert(ctx, a, "pending", s.alerts.backoff(a.Attempts-1), err.Error())
		}
	}
}

func (s *Server) finishAlert(ctx context.Context, a store.OutboxAlert, state string, retry time.Duration, msg string) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.shared.FinishAlert(fctx, a.Key, a.Channel, state, retry, msg); err != nil {
		s.log.Warn("ha: alert state not recorded; it is retried after its lease", "err", err)
	}
}
