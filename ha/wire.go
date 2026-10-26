package ha

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// The internal channel: HTTP between replicas, every request and every
// response authenticated with HMAC-SHA256 under a key derived (HKDF) from
// the shared secret. A request is bound to its sender's instance, a
// per-instance sequence number and a timestamp; a response is bound to the
// request it answers. Replays are refused with a sliding window per
// instance, as IPsec ESP does (RFC 4303, 3.4.3: the window's right edge is
// the highest validated number, numbers below its left edge are refused,
// numbers inside are checked against those already received, and the
// window moves only after the integrity check succeeded).

const (
	hdrInstance = "Llmflow-Ha-Instance"
	hdrSeq      = "Llmflow-Ha-Seq"
	hdrTime     = "Llmflow-Ha-Time"
	hdrSig      = "Llmflow-Ha-Sig"

	// maxSkew bounds the timestamp of a request: wide enough for clocks a
	// few minutes apart (the chaos test uses 2 minutes), the window is what
	// stops replays inside it.
	maxSkew = 5 * time.Minute
	// maxBody bounds a request or response body.
	maxBody = 4 << 20
	// windowBits is the replay window (4096 requests may arrive out of
	// order per instance).
	windowBits = 4096
	// maxInstances bounds the windows kept; the least recently seen goes.
	maxInstances = 4096
)

// MinSecret is the shortest shared secret accepted.
const MinSecret = 32

// deriveKey separates the keys taken from the shared secret by purpose.
func deriveKey(secret []byte, purpose string) []byte {
	k, err := hkdf.Key(sha256.New, secret, nil, "llmflow ha "+purpose, 32)
	if err != nil {
		panic(err) // only for a length above 255*32
	}
	return k
}

// DeriveKey returns a 32-byte key for a purpose (the SSO session
// encryption key is derived from the same secret, separated by purpose).
func DeriveKey(secret []byte, purpose string) []byte { return deriveKey(secret, purpose) }

func bodyHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func mac(key []byte, parts ...string) string {
	m := hmac.New(sha256.New, key)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{'\n'})
	}
	return hex.EncodeToString(m.Sum(nil))
}

// window is one instance's replay window.
type window struct {
	right uint64
	bits  [windowBits / 64]uint64
	seen  time.Time
}

// replayGuard keeps one window per instance.
type replayGuard struct {
	mu   sync.Mutex
	wins map[string]*window
}

func newReplayGuard() *replayGuard { return &replayGuard{wins: map[string]*window{}} }

// accept reports whether seq from instance is new, and records it. Call
// it only after the request's integrity was verified.
func (g *replayGuard) accept(instance string, seq uint64, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.wins[instance]
	if w == nil {
		if len(g.wins) >= maxInstances {
			var oldest string
			var t time.Time
			for k, v := range g.wins {
				if oldest == "" || v.seen.Before(t) {
					oldest, t = k, v.seen
				}
			}
			delete(g.wins, oldest)
		}
		w = &window{}
		g.wins[instance] = w
	}
	w.seen = now
	if seq == 0 {
		return false
	}
	switch {
	case seq > w.right:
		shift := seq - w.right
		if shift >= windowBits {
			w.bits = [windowBits / 64]uint64{}
		} else {
			// Bit i stands for right-i: moving the right edge by shift
			// moves every bit up by shift.
			ws, bs := int(shift/64), uint(shift%64)
			for i := len(w.bits) - 1; i >= 0; i-- {
				var v uint64
				if j := i - ws; j >= 0 {
					v = w.bits[j] << bs
					if bs > 0 && j-1 >= 0 {
						v |= w.bits[j-1] >> (64 - bs)
					}
				}
				w.bits[i] = v
			}
		}
		w.right = seq
		w.bits[0] |= 1
		return true
	case w.right-seq >= windowBits:
		return false
	default:
		off := w.right - seq
		word, bit := off/64, off%64
		if w.bits[word]&(1<<bit) != 0 {
			return false
		}
		w.bits[word] |= 1 << bit
		return true
	}
}

var (
	errAuth    = errors.New("ha: request not authenticated")
	errReplay  = errors.New("ha: request replayed")
	errSkew    = errors.New("ha: request timestamp outside the accepted window")
	errBadPeer = errors.New("ha: peer address outside peer_networks")
)

// verify authenticates an incoming request and returns its body.
func (n *Node) verify(r *http.Request) ([]byte, string, uint64, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, "", 0, err
	}
	if len(body) > maxBody {
		return nil, "", 0, errors.New("ha: body too large")
	}
	inst, seqS, tS, sig := r.Header.Get(hdrInstance), r.Header.Get(hdrSeq), r.Header.Get(hdrTime), r.Header.Get(hdrSig)
	seq, err1 := strconv.ParseUint(seqS, 10, 64)
	ms, err2 := strconv.ParseInt(tS, 10, 64)
	if inst == "" || len(inst) > 256 || err1 != nil || err2 != nil {
		return nil, "", 0, errAuth
	}
	want := mac(n.rpcKey, "req", r.Method, r.URL.Path, inst, seqS, tS, bodyHash(body))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return nil, "", 0, errAuth
	}
	now := n.cfg.Now()
	if d := now.Sub(time.UnixMilli(ms)); d > maxSkew || d < -maxSkew {
		return nil, "", 0, errSkew
	}
	if !n.replay.accept(inst, seq, now) {
		return nil, "", 0, errReplay
	}
	return body, inst, seq, nil
}

// reply writes a signed JSON response bound to the request.
func (n *Node) reply(w http.ResponseWriter, inst string, seq uint64, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(hdrSig, mac(n.rpcKey, "resp", inst, strconv.FormatUint(seq, 10), strconv.Itoa(status), bodyHash(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// allowed reports whether a peer address (an IP literal and a port) lies
// in peer_networks. The owner's address is read from the database: a
// value written there by someone else must not make replicas connect to
// an arbitrary host (SSRF).
func allowed(nets []netip.Prefix, addr string) bool {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	for _, p := range nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func newRPCClient(nets []netip.Prefix, timeout time.Duration, tc *tls.Config) *http.Client {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     tc,
			TLSHandshakeTimeout: timeout,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if !allowed(nets, addr) {
					return nil, errBadPeer
				}
				return d.DialContext(ctx, network, addr)
			},
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: timeout,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// statusError is a non-200 answer of a peer.
type statusError struct {
	status int
	body   []byte
}

func (e *statusError) Error() string { return fmt.Sprintf("ha: peer answered %d", e.status) }

// call sends a signed request to a peer and decodes its signed answer.
func (n *Node) call(ctx context.Context, addr, path string, in, out any) error {
	if !allowed(n.cfg.PeerNetworks, addr) {
		return errBadPeer
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, n.cfg.ForwardTimeout)
	defer cancel()
	scheme := "http://"
	if n.cfg.ClientTLS != nil {
		scheme = "https://"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scheme+addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	seq := strconv.FormatUint(n.nextSeq(addr), 10)
	ts := strconv.FormatInt(n.cfg.Now().UnixMilli(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hdrInstance, n.cfg.Instance)
	req.Header.Set(hdrSeq, seq)
	req.Header.Set(hdrTime, ts)
	req.Header.Set(hdrSig, mac(n.rpcKey, "req", http.MethodPost, path, n.cfg.Instance, seq, ts, bodyHash(body)))
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(rb) > maxBody {
		return errors.New("ha: answer too large")
	}
	want := mac(n.rpcKey, "resp", n.cfg.Instance, seq, strconv.Itoa(resp.StatusCode), bodyHash(rb))
	if !hmac.Equal([]byte(want), []byte(resp.Header.Get(hdrSig))) {
		return errAuth
	}
	if resp.StatusCode != http.StatusOK {
		return &statusError{status: resp.StatusCode, body: rb}
	}
	return json.Unmarshal(rb, out)
}
