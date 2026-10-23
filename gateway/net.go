package gateway

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/go-llmflow/gateway/store"
)

type addrKey struct{}

// remoteAddr parses the connection's peer address.
func remoteAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// clientAddrOf returns the client address set by withClientAddr, or the
// connection's peer address.
func clientAddrOf(r *http.Request) (netip.Addr, bool) {
	if a, ok := r.Context().Value(addrKey{}).(netip.Addr); ok {
		return a, true
	}
	return remoteAddr(r)
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr determines the client of a request. X-Forwarded-For is read
// only when the connection comes from a trusted proxy, and then from the
// right: the first address that is not a trusted proxy is the client
// (MDN, X-Forwarded-For, "Trusted proxy list"). Leftmost entries can be
// written by anybody and are never used for security. An entry that does
// not parse stops the walk at the last trusted hop, which then stands for
// the client and fails any allowlist meant for real clients.
func ClientAddr(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	peer, ok := remoteAddr(r)
	if !ok || len(trusted) == 0 || !inAny(peer, trusted) {
		return peer, ok
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	if len(hops) > 64 {
		return peer, true // an absurd chain: trust nothing in it
	}
	last := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || a.Zone() != "" { // a zone (fe80::1%eth0) is meaningless across hosts
			return last, true
		}
		a = a.Unmap()
		if !inAny(a, trusted) {
			return a, true
		}
		last = a
	}
	return last, true
}

// withClientAddr computes the client address once per request and checks
// the route class's network restriction (nil: any address).
func (s *Server) withClientAddr(allow []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := ClientAddr(r, s.cfg.TrustedProxies)
		if !ok {
			apiError(w, http.StatusForbidden, "permission_denied", "address not allowed")
			return
		}
		if allow != nil && !inAny(a, allow) {
			apiError(w, http.StatusForbidden, "permission_denied", "address not allowed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), addrKey{}, a)))
	})
}

// allowed checks the IP allowlists of a key's organisation, team and the
// key itself: the client must be in every list that exists.
func (s *Server) addrAllowed(r *http.Request, k store.Key) bool {
	a, ok := clientAddrOf(r)
	if !ok {
		return false
	}
	return s.budget.addrAllowed(a, k)
}
