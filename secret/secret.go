// Package secret resolves secret references: a file on disk, or a field of
// an OpenBao / HashiCorp Vault KV version 2 secret.
//
// A reference is what every *_file attribute of the configuration holds:
//
//	/etc/llmflow/master.key                    a file (the v0.x form)
//	file:/etc/llmflow/master.key               the same, explicitly
//	vault:secret/llmflow/master#key            field "key" of the latest version of
//	                                           secret "llmflow/master" in mount "secret"
//	vault:secret/llmflow/master#key?version=3  a pinned version
//
// The KV v2 read is GET /v1/:mount/data/:path?version=N with the token in
// the X-Vault-Token header; the fields are under data.data (OpenBao API
// docs, website/content/docs/api/secret/kv/kv-v2.mdx, "Read secret
// version"; the Vault API is the same).
package secret

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Ref is a parsed secret reference.
type Ref struct {
	// File is set for a file reference.
	File string
	// Mount, Path, Field and Version are set for a vault reference.
	Mount, Path, Field string
	Version            int
}

// IsVault reports whether r names a KV v2 secret.
func (r Ref) IsVault() bool { return r.File == "" }

func (r Ref) String() string {
	if !r.IsVault() {
		return r.File
	}
	s := "vault:" + r.Mount + "/" + r.Path + "#" + r.Field
	if r.Version > 0 {
		s += "?version=" + strconv.Itoa(r.Version)
	}
	return s
}

func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// Parse parses a reference. Mount, path segments and field are restricted
// to [A-Za-z0-9._-] (no "." or ".." segment), so a reference cannot reach
// outside the KV mount or inject URL syntax.
func Parse(s string) (Ref, error) {
	switch {
	case strings.HasPrefix(s, "vault:"):
		rest := strings.TrimPrefix(s, "vault:")
		loc, field, ok := strings.Cut(rest, "#")
		if !ok {
			return Ref{}, fmt.Errorf("secret %q: a vault reference needs #field", s)
		}
		r := Ref{}
		if f, q, ok := strings.Cut(field, "?"); ok {
			field = f
			v, found := strings.CutPrefix(q, "version=")
			n, err := strconv.Atoi(v)
			if !found || err != nil || n < 1 {
				return Ref{}, fmt.Errorf("secret %q: want ?version=N with N >= 1", s)
			}
			r.Version = n
		}
		segs := strings.Split(loc, "/")
		if len(segs) < 2 || len(segs) > 16 {
			return Ref{}, fmt.Errorf("secret %q: want vault:mount/path#field", s)
		}
		for _, sg := range segs {
			if !validSegment(sg) {
				return Ref{}, fmt.Errorf("secret %q: path segment %q: want [A-Za-z0-9._-]", s, sg)
			}
		}
		if !validSegment(field) {
			return Ref{}, fmt.Errorf("secret %q: field %q: want [A-Za-z0-9._-]", s, field)
		}
		r.Mount, r.Path, r.Field = segs[0], strings.Join(segs[1:], "/"), field
		return r, nil
	case strings.HasPrefix(s, "file:"):
		s = strings.TrimPrefix(s, "file:")
		fallthrough
	default:
		if s == "" || strings.ContainsAny(s, "\x00\r\n") {
			return Ref{}, fmt.Errorf("secret file %q is invalid", s)
		}
		return Ref{File: s}, nil
	}
}

// VaultConfig reaches an OpenBao or Vault server.
type VaultConfig struct {
	// Address is the server's https URL (http only on loopback, for tests).
	Address string
	// Token is the client token (read from a file by the caller).
	Token string
	// CAFile, when set, is the PEM bundle trusted for the server.
	CAFile string
	// Namespace, when set, is sent as X-Vault-Namespace.
	Namespace string
	// CacheTTL bounds how long a read secret is reused (default 5 minutes).
	CacheTTL time.Duration
}

// Resolver resolves references.
type Resolver struct {
	vault  *VaultConfig
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	fields  map[string]string
	expires time.Time
}

const (
	maxSecretFile  = 64 << 10
	maxVaultBody   = 1 << 20
	maxCachedPaths = 1024
)

// ValidateAddress checks a vault address: https, or http to a loopback
// address only (a token sent in the clear is a token given away).
func ValidateAddress(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("vault address %q: want https://host[:port]", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return fmt.Errorf("vault address %q: TLS is required (https)", raw)
}

// New returns a resolver; vault may be nil when no reference names one.
func New(vault *VaultConfig) (*Resolver, error) {
	r := &Resolver{vault: vault, now: time.Now, cache: map[string]cached{}}
	if vault == nil {
		return r, nil
	}
	if err := ValidateAddress(vault.Address); err != nil {
		return nil, err
	}
	if vault.Token == "" {
		return nil, errors.New("vault: empty token")
	}
	if vault.CacheTTL <= 0 {
		vault.CacheTTL = 5 * time.Minute
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if vault.CAFile != "" {
		pem, err := readBounded(vault.CAFile, 1<<20)
		if err != nil {
			return nil, fmt.Errorf("vault ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("vault ca_file %s holds no PEM certificate", vault.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	r.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg},
		// The token must not follow a redirect elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return r, nil
}

// Resolve reads the secret a reference names: one line, trimmed, not
// empty. Errors never contain the secret.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	p, err := Parse(ref)
	if err != nil {
		return "", err
	}
	var v string
	if p.IsVault() {
		v, err = r.vaultField(ctx, p)
	} else {
		var data []byte
		data, err = readBounded(p.File, maxSecretFile)
		v = string(data)
	}
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(v)
	if s == "" {
		return "", fmt.Errorf("secret %s is empty", p)
	}
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("secret %s holds more than one line", p)
	}
	return s, nil
}

// PEM kinds ResolvePEM accepts.
const (
	// PEMCertificates is one or more CERTIFICATE blocks (a leaf and its
	// chain, or a bundle of CAs).
	PEMCertificates = "certificates"
	// PEMPrivateKey is exactly one PRIVATE KEY block (PKCS #8, EC or RSA).
	PEMPrivateKey = "private key"
)

// maxPEM bounds a PEM file or field (a CA bundle of a few hundred
// certificates).
const maxPEM = 1 << 20

// ResolvePEM reads PEM material a reference names: a file, or an
// OpenBao/Vault field, over several lines, unlike Resolve which reads one
// line (tokens, passwords). The content must be PEM of the kind asked for
// and nothing else. A private key in a file must not be readable by group
// or others (chmod 600). Errors never contain the material.
func (r *Resolver) ResolvePEM(ctx context.Context, ref, kind string) ([]byte, error) {
	p, err := Parse(ref)
	if err != nil {
		return nil, err
	}
	var data []byte
	if p.IsVault() {
		var v string
		if v, err = r.vaultField(ctx, p); err != nil {
			return nil, err
		}
		if len(v) > maxPEM {
			return nil, fmt.Errorf("secret %s is larger than %d bytes", p, maxPEM)
		}
		data = []byte(v)
	} else {
		if kind == PEMPrivateKey {
			st, err := os.Stat(p.File)
			if err != nil {
				return nil, err
			}
			if st.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("secret %s: a private key must not be readable by group or others (mode %#o; chmod 600)", p, st.Mode().Perm())
			}
		}
		if data, err = readBounded(p.File, maxPEM); err != nil {
			return nil, err
		}
	}
	if err := checkPEM(data, kind); err != nil {
		return nil, fmt.Errorf("secret %s: %w", p, err)
	}
	return bytes.TrimSpace(data), nil
}

// checkPEM accepts PEM blocks of one kind, and blank space between them.
func checkPEM(data []byte, kind string) error {
	rest := bytes.TrimSpace(data)
	n := 0
	for len(rest) > 0 {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return errors.New("not PEM (or text outside the PEM blocks)")
		}
		rest = bytes.TrimSpace(rest)
		n++
		switch kind {
		case PEMCertificates:
			if b.Type != "CERTIFICATE" {
				return fmt.Errorf("a %q block where certificates were expected", b.Type)
			}
			if _, err := x509.ParseCertificate(b.Bytes); err != nil {
				return errors.New("a certificate does not parse")
			}
		case PEMPrivateKey:
			if !strings.HasSuffix(b.Type, "PRIVATE KEY") {
				return fmt.Errorf("a %q block where a private key was expected", b.Type)
			}
		default:
			return fmt.Errorf("unknown PEM kind %q", kind)
		}
	}
	switch {
	case n == 0:
		return errors.New("empty")
	case kind == PEMPrivateKey && n != 1:
		return fmt.Errorf("%d private keys, want one", n)
	}
	return nil
}

func (r *Resolver) vaultField(ctx context.Context, p Ref) (string, error) {
	if r.vault == nil {
		return "", fmt.Errorf("secret %s: no vault block in the configuration", p)
	}
	key := p.Mount + "/" + p.Path + "?" + strconv.Itoa(p.Version)
	r.mu.Lock()
	c, ok := r.cache[key]
	r.mu.Unlock()
	if !ok || r.now().After(c.expires) {
		fields, err := r.read(ctx, p)
		if err != nil {
			return "", err
		}
		c = cached{fields: fields, expires: r.now().Add(r.vault.CacheTTL)}
		r.mu.Lock()
		if len(r.cache) >= maxCachedPaths {
			r.cache = map[string]cached{} // bounded; refilled on demand
		}
		r.cache[key] = c
		r.mu.Unlock()
	}
	v, ok := c.fields[p.Field]
	if !ok {
		return "", fmt.Errorf("secret %s: no such field", p)
	}
	return v, nil
}

func (r *Resolver) read(ctx context.Context, p Ref) (map[string]string, error) {
	u := strings.TrimRight(r.vault.Address, "/") + "/v1/" + p.Mount + "/data/" + p.Path
	if p.Version > 0 {
		u += "?version=" + strconv.Itoa(p.Version)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", r.vault.Token)
	if r.vault.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", r.vault.Namespace)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secret %s: %w", p, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultBody+1))
	if err != nil || len(body) > maxVaultBody {
		return nil, fmt.Errorf("secret %s: unreadable or oversized answer", p)
	}
	if resp.StatusCode != http.StatusOK {
		// The body may echo request details; only the status is reported.
		return nil, fmt.Errorf("secret %s: vault answered %d", p, resp.StatusCode)
	}
	return parseKV2(body)
}

// Write stores value in the field a vault reference names, leaving the
// secret's other fields alone: PATCH with a JSON merge patch (KV v2 "Patch
// secret", Content-Type application/merge-patch+json), or, when the secret
// does not exist yet, POST (KV v2 "Create/Update secret"). The token needs
// the patch and create capabilities on that path. File references and
// versioned references are refused: a key rotation writes only to vault.
func (r *Resolver) Write(ctx context.Context, p Ref, value string) error {
	if !p.IsVault() || p.Version != 0 {
		return fmt.Errorf("secret %s: only an unversioned vault reference can be written", p)
	}
	if r.vault == nil {
		return fmt.Errorf("secret %s: no vault block in the configuration", p)
	}
	body, err := json.Marshal(map[string]any{"data": map[string]string{p.Field: value}})
	if err != nil {
		return err
	}
	status, err := r.send(ctx, http.MethodPatch, p, body, "application/merge-patch+json")
	if err == nil && status == http.StatusNotFound {
		status, err = r.send(ctx, http.MethodPost, p, body, "application/json")
	}
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		return fmt.Errorf("secret %s: vault answered %d to the write", p, status)
	}
	r.mu.Lock()
	delete(r.cache, p.Mount+"/"+p.Path+"?0")
	r.mu.Unlock()
	return nil
}

func (r *Resolver) send(ctx context.Context, method string, p Ref, body []byte, ctype string) (int, error) {
	u := strings.TrimRight(r.vault.Address, "/") + "/v1/" + p.Mount + "/data/" + p.Path
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Vault-Token", r.vault.Token)
	if r.vault.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", r.vault.Namespace)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("secret %s: %w", p, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxVaultBody))
	return resp.StatusCode, nil
}

// parseKV2 extracts data.data from a KV v2 read answer. Values must be
// strings: a number or an object is not a secret this package hands out.
func parseKV2(body []byte) (map[string]string, error) {
	var env struct {
		Data *struct {
			Data map[string]json.RawMessage `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data == nil || env.Data.Data == nil {
		return nil, errors.New("vault: answer is not a KV v2 secret (deleted or destroyed version?)")
	}
	out := make(map[string]string, len(env.Data.Data))
	for k, raw := range env.Data.Data {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out[k] = s
		}
	}
	return out, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}
