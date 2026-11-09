package export

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Azure Blob Storage through the REST API: Put Blob
// (https://learn.microsoft.com/en-us/rest/api/storageservices/put-blob),
// Get Blob and List Blobs, authorised by Shared Key
// (https://learn.microsoft.com/en-us/rest/api/storageservices/authorize-with-shared-key),
// a SAS token, or a Microsoft Entra bearer token.
//
// A block blob is created with "If-None-Match: *" (conditional headers:
// https://learn.microsoft.com/en-us/rest/api/storageservices/specifying-conditional-headers-for-blob-service-operations):
// the write fails when the blob exists. With a retention period the blob
// carries an immutability policy (x-ms-immutability-policy-until-date and
// x-ms-immutability-policy-mode), which the container must allow (version-
// level immutability): the service refuses the write otherwise, so the
// retention fails closed. An append blob (objects of at most 4 MiB, one
// Append Block) is created, then appended at position 0: a retry after a
// crash between the two steps completes the empty blob, and a blob that
// holds data is refused.

const azureVersion = "2023-11-03"

// azureAppendMax is the largest Append Block (4 MiB, append-block).
const azureAppendMax = 4 << 20

type azureSink struct {
	name, account, container, prefix string
	base                             *url.URL
	hc                               *http.Client
	authMode                         string // shared_key, sas, bearer, client_credentials
	key                              []byte
	sas                              url.Values
	tok                              tokenSource
	appendBlob                       bool
	mode                             string // locked or unlocked
	retention                        time.Duration
	now                              func() time.Time
}

func (a *azureSink) Name() string { return a.name }

// Locked reports an immutability policy the service enforces (block blobs).
func (a *azureSink) Locked() bool { return a.retention > 0 && !a.appendBlob }

// WriteOnce is true: every write is conditional on the blob not existing.
func (a *azureSink) WriteOnce() bool { return true }

// blobPath is the escaped URL path of a blob (or of the container for "").
func (a *azureSink) blobPath(blob string) string {
	p := a.base.EscapedPath() + "/" + url.PathEscape(a.container)
	if blob != "" {
		segs := strings.Split(a.prefix+blob, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		p += "/" + strings.Join(segs, "/")
	}
	return p
}

// request builds and authorises a request. h holds the x-ms-* and
// conditional headers.
func (a *azureSink) request(ctx context.Context, method, path string, q url.Values, body []byte, h map[string]string) (*http.Request, error) {
	if a.sas != nil {
		q = cloneValues(q)
		for k, v := range a.sas {
			if q.Has(k) {
				return nil, fmt.Errorf("export %s: the SAS token sets %q", a.name, k)
			}
			q[k] = v
		}
	}
	u := *a.base
	u.RawPath = path
	u.Path, _ = url.PathUnescape(path)
	u.RawQuery = q.Encode()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if rd != nil {
		req, err = http.NewRequest(method, u.String(), rd)
	} else {
		req, err = http.NewRequest(method, u.String(), nil)
	}
	if err != nil {
		return nil, redact(err)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-ms-version", azureVersion)
	req.Header.Set("x-ms-date", a.now().UTC().Format(http.TimeFormat))
	switch a.authMode {
	case "shared_key":
		req.Header.Set("Authorization", "SharedKey "+a.account+":"+sharedKeySignature(a.key, a.account, req))
	case "bearer", "client_credentials":
		t, err := a.tok.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return req, nil
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, s := range v {
		out[k] = append([]string(nil), s...)
	}
	return out
}

// sharedKeySignature signs req as the Shared Key scheme says: HMAC-SHA256,
// with the decoded account key, over the verb, the standard headers (empty
// when absent, Content-Length empty when zero), the canonicalised x-ms-
// headers and the canonicalised resource.
func sharedKeySignature(key []byte, account string, req *http.Request) string {
	h := req.Header
	cl := ""
	if req.ContentLength > 0 {
		cl = strconv.FormatInt(req.ContentLength, 10)
	}
	var sb strings.Builder
	sb.WriteString(req.Method + "\n")
	for _, f := range []string{"Content-Encoding", "Content-Language"} {
		sb.WriteString(h.Get(f) + "\n")
	}
	sb.WriteString(cl + "\n")
	for _, f := range []string{"Content-MD5", "Content-Type", "Date", "If-Modified-Since", "If-Match", "If-None-Match", "If-Unmodified-Since", "Range"} {
		sb.WriteString(h.Get(f) + "\n")
	}
	var names []string
	for k := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		sb.WriteString(n + ":" + strings.Join(strings.Fields(strings.Join(h.Values(n), ",")), " ") + "\n")
	}
	sb.WriteString("/" + account + req.URL.EscapedPath())
	q := req.URL.Query()
	qn := make([]string, 0, len(q))
	for k := range q {
		qn = append(qn, k)
	}
	sort.Slice(qn, func(i, j int) bool { return strings.ToLower(qn[i]) < strings.ToLower(qn[j]) })
	for _, k := range qn {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		sb.WriteString("\n" + strings.ToLower(k) + ":" + strings.Join(vs, ","))
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(sb.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (a *azureSink) send(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := do(ctx, a.hc, req)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", a.name, err)
	}
	return resp, nil
}

func (a *azureSink) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	if !ValidKey(key) {
		return fmt.Errorf("export %s: invalid key %q", a.name, key)
	}
	if len(body) > maxObjectBytes || a.appendBlob && len(body) > azureAppendMax {
		return fmt.Errorf("export %s: %s is larger than this blob type takes", a.name, key)
	}
	h := map[string]string{"If-None-Match": "*"}
	if o.ContentType != "" {
		h["x-ms-blob-content-type"] = o.ContentType
	}
	if o.ContentEncoding != "" {
		h["x-ms-blob-content-encoding"] = o.ContentEncoding
	}
	if o.Retain && a.Locked() {
		h["x-ms-immutability-policy-until-date"] = a.now().UTC().Add(a.retention).Format(http.TimeFormat)
		h["x-ms-immutability-policy-mode"] = a.mode
	}
	path := a.blobPath(key)
	if !a.appendBlob {
		h["x-ms-blob-type"] = "BlockBlob"
		req, err := a.request(ctx, http.MethodPut, path, nil, append([]byte{}, body...), h)
		if err != nil {
			return fmt.Errorf("export %s: %w", a.name, err)
		}
		return a.finishPut(ctx, key, req)
	}
	h["x-ms-blob-type"] = "AppendBlob"
	req, err := a.request(ctx, http.MethodPut, path, nil, []byte{}, h)
	if err != nil {
		return fmt.Errorf("export %s: %w", a.name, err)
	}
	if err := a.finishPut(ctx, key, req); err != nil && !isExists(err) {
		return err
	}
	// Append at position 0: refused when the blob holds data already.
	req, err = a.request(ctx, http.MethodPut, path, url.Values{"comp": {"appendblock"}}, append([]byte{}, body...),
		map[string]string{"x-ms-blob-condition-appendpos": "0"})
	if err != nil {
		return fmt.Errorf("export %s: %w", a.name, err)
	}
	return a.finishPut(ctx, key, req)
}

func isExists(err error) bool { return err != nil && strings.Contains(err.Error(), ErrExists.Error()) }

func (a *azureSink) finishPut(ctx context.Context, key string, req *http.Request) error {
	resp, err := a.send(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict, http.StatusPreconditionFailed:
		return fmt.Errorf("export %s: %s: %w", a.name, key, ErrExists)
	}
	return fmt.Errorf("export %s: %w", a.name, statusError("Put Blob "+key, resp))
}

func (a *azureSink) Get(ctx context.Context, key string) ([]byte, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("export %s: invalid key %q", a.name, key)
	}
	req, err := a.request(ctx, http.MethodGet, a.blobPath(key), nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", a.name, err)
	}
	resp, err := a.send(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("export %s: %w", a.name, statusError("Get Blob "+key, resp))
	}
	return readBounded(resp.Body, key)
}

// azureList is a List Blobs response.
type azureList struct {
	Names      []string `xml:"Blobs>Blob>Name"`
	NextMarker string   `xml:"NextMarker"`
}

// ParseAzureList reads one List Blobs page: the blob names and the
// continuation marker.
func ParseAzureList(b []byte) (names []string, next string, err error) {
	var l azureList
	if err := xml.Unmarshal(b, &l); err != nil {
		return nil, "", err
	}
	return l.Names, l.NextMarker, nil
}

func (a *azureSink) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	marker := ""
	for page := 0; page < 1<<12; page++ {
		q := url.Values{"restype": {"container"}, "comp": {"list"}, "prefix": {a.prefix + prefix}, "maxresults": {"5000"}}
		if marker != "" {
			q.Set("marker", marker)
		}
		req, err := a.request(ctx, http.MethodGet, a.blobPath(""), q, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", a.name, err)
		}
		resp, err := a.send(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return nil, fmt.Errorf("export %s: %w", a.name, statusError("List Blobs", resp))
		}
		b, err := readBoundedN(resp.Body, 16<<20, "listing page")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", a.name, err)
		}
		names, next, err := ParseAzureList(b)
		if err != nil {
			return nil, fmt.Errorf("export %s: List Blobs: %w", a.name, err)
		}
		for _, n := range names {
			out = append(out, strings.TrimPrefix(n, a.prefix))
		}
		if len(out) > maxListed {
			return nil, fmt.Errorf("export %s: more than %d blobs under %q", a.name, maxListed, prefix)
		}
		if next == "" {
			sort.Strings(out)
			return out, nil
		}
		marker = next
	}
	return nil, fmt.Errorf("export %s: listing did not end", a.name)
}
