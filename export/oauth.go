package export

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultGoogleTokenURL is where a service-account assertion is exchanged
// (https://developers.google.com/identity/protocols/oauth2/service-account:
// grant type urn:ietf:params:oauth:grant-type:jwt-bearer, RS256). The "token_uri"
// of the key file is deliberately ignored: the file is a secret, and what it
// names must not decide where a signed assertion is sent.
const DefaultGoogleTokenURL = "https://oauth2.googleapis.com/token"

const gcsScope = "https://www.googleapis.com/auth/devstorage.read_write"

// maxCredentialBytes bounds a credential file's content.
const maxCredentialBytes = 64 << 10

// validToken reports whether s can sit in an Authorization header: printable
// ASCII, no space.
func validToken(s string) bool {
	if s == "" || len(s) > 16<<10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// cachedSource caches the token fetch returns until shortly before it
// expires.
type cachedSource struct {
	fetch func(ctx context.Context) (string, time.Duration, error)
	now   func() time.Time

	mu  sync.Mutex
	tok string
	exp time.Time
}

func (c *cachedSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.now().Before(c.exp) {
		return c.tok, nil
	}
	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	if !validToken(tok) {
		return "", errors.New("export: the token endpoint returned an unusable token")
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 5 * time.Minute
	}
	c.tok, c.exp = tok, c.now().Add(ttl-min(ttl/2, expiryMargin))
	return tok, nil
}

// tokenResponse is the part of an OAuth 2 token response that is read.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// postToken sends form to endpoint and reads the token it answers.
func postToken(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (string, time.Duration, error) {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := do(ctx, hc, req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, statusError("token endpoint", resp)
	}
	b, err := readBounded(resp.Body, "token response")
	if err != nil {
		return "", 0, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return "", 0, fmt.Errorf("token response: %w", err)
	}
	return tr.AccessToken, time.Duration(tr.ExpiresIn) * time.Second, nil
}

// ServiceAccount is the part of a Google service-account key file that is
// used.
type ServiceAccount struct {
	Email string
	KeyID string
	Key   *rsa.PrivateKey
}

// ParseServiceAccount reads a service-account key file (type
// "service_account", client_email, private_key as PKCS #8 or PKCS #1 PEM).
func ParseServiceAccount(b []byte) (*ServiceAccount, error) {
	if len(b) > maxCredentialBytes {
		return nil, errors.New("service account key: too large")
	}
	var f struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("service account key: %w", err)
	}
	if f.Type != "service_account" || f.ClientEmail == "" || strings.ContainsAny(f.ClientEmail, " \r\n\"\\") {
		return nil, errors.New("service account key: want type service_account and a client_email")
	}
	blk, _ := pem.Decode([]byte(f.PrivateKey))
	if blk == nil {
		return nil, errors.New("service account key: private_key holds no PEM block")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("service account key: private_key is not an RSA key")
		}
		key = rk
	} else if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		key = k
	} else {
		return nil, errors.New("service account key: private_key does not parse")
	}
	if key.N.BitLen() < 2048 {
		return nil, errors.New("service account key: RSA key under 2048 bits")
	}
	return &ServiceAccount{Email: f.ClientEmail, KeyID: f.PrivateKeyID, Key: key}, nil
}

// Assertion signs the JWT bearer assertion for scope, audience aud, valid
// for an hour from now.
func (sa *ServiceAccount) Assertion(scope, aud string, now time.Time) (string, error) {
	b64 := base64.RawURLEncoding.EncodeToString
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if sa.KeyID != "" {
		hdr["kid"] = sa.KeyID
	}
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(map[string]any{"iss": sa.Email, "scope": scope, "aud": aud, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return in + "." + b64(sig), nil
}

// newServiceAccountSource exchanges signed assertions for tokens at tokenURL.
func newServiceAccountSource(hc *http.Client, sa *ServiceAccount, tokenURL string, now func() time.Time) tokenSource {
	return &cachedSource{now: now, fetch: func(ctx context.Context) (string, time.Duration, error) {
		as, err := sa.Assertion(gcsScope, tokenURL, now())
		if err != nil {
			return "", 0, err
		}
		return postToken(ctx, hc, tokenURL, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {as}})
	}}
}

// newClientCredentialsSource is the Microsoft Entra client-credentials grant
// (https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow:
// grant_type=client_credentials, scope <resource>/.default, client_secret).
func newClientCredentialsSource(hc *http.Client, tokenURL, clientID, clientSecret string, now func() time.Time) tokenSource {
	return &cachedSource{now: now, fetch: func(ctx context.Context) (string, time.Duration, error) {
		return postToken(ctx, hc, tokenURL, url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID},
			"client_secret": {clientSecret}, "scope": {"https://storage.azure.com/.default"}})
	}}
}
