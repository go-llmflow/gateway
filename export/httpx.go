package export

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/secret"
)

// maxObjectBytes bounds one object written to GCS or Azure Blob (the queue
// itself holds at most DefaultMaxQueueBytes).
const maxObjectBytes = 256 << 20

// maxErrBody bounds the part of an error response kept in an error.
const maxErrBody = 2048

// ErrExists is what a write-once destination answers when the key is taken.
var ErrExists = errors.New("export: object already exists")

// WriteOnce is implemented by the sinks whose Put refuses to replace an
// existing key (GCS: ifGenerationMatch=0; Azure: If-None-Match: *; a file:
// a hard link). It is about the PUT, not about the credentials' power to
// delete; Locked says whether the destination also enforces retention.
type WriteOnce interface{ WriteOnce() bool }

// IsWriteOnce reports whether s refuses to overwrite an existing key.
func IsWriteOnce(s Sink) bool {
	w, ok := s.(WriteOnce)
	return ok && w.WriteOnce()
}

// httpClient is the client of the HTTP destinations: TLS 1.2 or later, no
// redirect (a redirect would carry the credentials to another host), the
// timeout set by the caller's context.
func httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// cleanBase validates a destination endpoint (https, or http to loopback)
// and returns it without a trailing slash.
func cleanBase(raw string) (string, error) {
	if err := secret.ValidateAddress(raw); err != nil {
		return "", err
	}
	return strings.TrimRight(raw, "/"), nil
}

// redact drops the URL from a transport error: a SAS token or an upload
// session lives in it.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// statusError reads the start of an error response into an error.
func statusError(what string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	msg := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, string(b))
	return fmt.Errorf("%s: HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(msg))
}

// do sends req and returns the response; the caller closes the body.
func do(ctx context.Context, hc *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := hc.Do(req.WithContext(ctx))
	if err != nil {
		return nil, redact(err)
	}
	return resp, nil
}

// readBounded reads at most MaxObject bytes of a response and refuses more.
func readBounded(r io.Reader, what string) ([]byte, error) { return readBoundedN(r, MaxObject, what) }

func readBoundedN(r io.Reader, n int, what ...string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(n)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > n {
		w := "response"
		if len(what) > 0 {
			w = what[0]
		}
		return nil, fmt.Errorf("%s is larger than %d bytes", w, n)
	}
	return b, nil
}

// maxListed bounds the keys a listing returns.
const maxListed = 1 << 20

// tokenSource yields an OAuth 2 bearer token.
type tokenSource interface {
	Token(ctx context.Context) (string, error)
}

type staticToken string

func (t staticToken) Token(context.Context) (string, error) { return string(t), nil }

// expiryMargin renews a token this long before it expires.
const expiryMargin = time.Minute
