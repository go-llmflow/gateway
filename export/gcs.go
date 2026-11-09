package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google Cloud Storage through the JSON API (objects.insert, objects.get,
// objects.list; https://docs.cloud.google.com/storage/docs/json_api/v1/objects/insert).
//
// An object is created with the precondition ifGenerationMatch=0
// (https://docs.cloud.google.com/storage/docs/request-preconditions): the
// request fails with 412 when the name exists, so Put never replaces an
// object. With a retention period the object carries retention {mode,
// retainUntilTime}: the bucket must have object retention enabled, and the
// request is refused otherwise, so a retention that cannot be enforced
// fails closed instead of being dropped.

// gcsMultipartMax is the largest object sent in one multipart request;
// larger ones use a resumable upload session.
const gcsMultipartMax = 8 << 20

type gcsSink struct {
	name, bucket, prefix, base string
	hc                         *http.Client
	tok                        tokenSource
	mode                       string // Locked or Unlocked
	retention                  time.Duration
	now                        func() time.Time
}

func (g *gcsSink) Name() string { return g.name }

// Locked reports a retention period: the service enforces it (or refuses
// the write).
func (g *gcsSink) Locked() bool { return g.retention > 0 }

// WriteOnce is true: Put sends ifGenerationMatch=0.
func (g *gcsSink) WriteOnce() bool { return true }

func (g *gcsSink) auth(ctx context.Context, req *http.Request) error {
	t, err := g.tok.Token(ctx)
	if err != nil {
		return fmt.Errorf("export %s: %w", g.name, err)
	}
	req.Header.Set("Authorization", "Bearer "+t)
	return nil
}

func (g *gcsSink) objectsURL(upload bool) string {
	p := "/storage/v1/b/"
	if upload {
		p = "/upload/storage/v1/b/"
	}
	return g.base + p + url.PathEscape(g.bucket) + "/o"
}

// gcsMetadata is the object resource sent with the upload.
func (g *gcsSink) gcsMetadata(name string, o PutOptions) []byte {
	m := map[string]any{"name": name}
	if o.ContentType != "" {
		m["contentType"] = o.ContentType
	}
	if o.ContentEncoding != "" {
		m["contentEncoding"] = o.ContentEncoding
	}
	if o.Retain && g.retention > 0 {
		m["retention"] = map[string]string{"mode": g.mode, "retainUntilTime": g.now().UTC().Add(g.retention).Format(time.RFC3339)}
	}
	b, _ := json.Marshal(m)
	return b
}

func (g *gcsSink) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	if !ValidKey(key) {
		return fmt.Errorf("export %s: invalid key %q", g.name, key)
	}
	if len(body) > maxObjectBytes {
		return fmt.Errorf("export %s: %s is larger than %d bytes", g.name, key, maxObjectBytes)
	}
	name := g.prefix + key
	meta := g.gcsMetadata(name, o)
	var resp *http.Response
	var err error
	if len(body) <= gcsMultipartMax {
		resp, err = g.multipart(ctx, meta, body, o)
	} else {
		resp, err = g.resumable(ctx, meta, body, o)
	}
	if err != nil {
		return fmt.Errorf("export %s: %w", g.name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusPreconditionFailed:
		return fmt.Errorf("export %s: %s: %w", g.name, key, ErrExists)
	}
	return fmt.Errorf("export %s: %w", g.name, statusError("objects.insert "+key, resp))
}

func (g *gcsSink) multipart(ctx context.Context, meta, body []byte, o PutOptions) (*http.Response, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	p, _ := w.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}})
	p.Write(meta)
	ct := o.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	p, _ = w.CreatePart(textproto.MIMEHeader{"Content-Type": {ct}})
	p.Write(body)
	w.Close()
	req, err := http.NewRequest(http.MethodPost, g.objectsURL(true)+"?uploadType=multipart&ifGenerationMatch=0", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+w.Boundary())
	if err := g.auth(ctx, req); err != nil {
		return nil, err
	}
	return do(ctx, g.hc, req)
}

// resumable opens an upload session and sends the whole object in one
// request to it; a failure is retried by the queue from the start.
func (g *gcsSink) resumable(ctx context.Context, meta, body []byte, o PutOptions) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, g.objectsURL(true)+"?uploadType=resumable&ifGenerationMatch=0", bytes.NewReader(meta))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	if o.ContentType != "" {
		req.Header.Set("X-Upload-Content-Type", o.ContentType)
	}
	req.Header.Set("X-Upload-Content-Length", strconv.Itoa(len(body)))
	if err := g.auth(ctx, req); err != nil {
		return nil, err
	}
	resp, err := do(ctx, g.hc, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return resp, nil // the caller reads the error
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	// The session URI must stay on the endpoint: the credentials go with it.
	lu, err := url.Parse(loc)
	bu, _ := url.Parse(g.base)
	if err != nil || lu.Scheme != bu.Scheme || lu.Host != bu.Host {
		return nil, fmt.Errorf("the upload session is not on the endpoint")
	}
	put, err := http.NewRequest(http.MethodPut, loc, bytes.NewReader(body))
	if err != nil {
		return nil, redact(err)
	}
	if err := g.auth(ctx, put); err != nil {
		return nil, err
	}
	return do(ctx, g.hc, put)
}

func (g *gcsSink) Get(ctx context.Context, key string) ([]byte, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("export %s: invalid key %q", g.name, key)
	}
	req, err := http.NewRequest(http.MethodGet, g.objectsURL(false)+"/"+url.PathEscape(g.prefix+key)+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	if err := g.auth(ctx, req); err != nil {
		return nil, err
	}
	resp, err := do(ctx, g.hc, req)
	if err != nil {
		return nil, fmt.Errorf("export %s: %w", g.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("export %s: %w", g.name, statusError("objects.get "+key, resp))
	}
	return readBounded(resp.Body, key)
}

// gcsList is the part of an objects.list response that is read.
type gcsList struct {
	Items []struct {
		Name string `json:"name"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ParseGCSList reads one objects.list page: the names and the next page
// token. Names that are not valid keys are skipped by the caller.
func ParseGCSList(b []byte) (names []string, next string, err error) {
	var l gcsList
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, "", err
	}
	for _, it := range l.Items {
		names = append(names, it.Name)
	}
	return names, l.NextPageToken, nil
}

func (g *gcsSink) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	token := ""
	for page := 0; page < 1<<12; page++ {
		q := url.Values{"prefix": {g.prefix + prefix}, "fields": {"items(name),nextPageToken"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		req, err := http.NewRequest(http.MethodGet, g.objectsURL(false)+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if err := g.auth(ctx, req); err != nil {
			return nil, err
		}
		resp, err := do(ctx, g.hc, req)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", g.name, err)
		}
		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			return nil, fmt.Errorf("export %s: %w", g.name, statusError("objects.list", resp))
		}
		b, err := readBoundedN(resp.Body, 16<<20, "listing page")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", g.name, err)
		}
		names, next, err := ParseGCSList(b)
		if err != nil {
			return nil, fmt.Errorf("export %s: objects.list: %w", g.name, err)
		}
		for _, n := range names {
			out = append(out, strings.TrimPrefix(n, g.prefix))
		}
		if len(out) > maxListed {
			return nil, fmt.Errorf("export %s: more than %d objects under %q", g.name, maxListed, prefix)
		}
		if next == "" {
			return out, nil
		}
		token = next
	}
	return nil, fmt.Errorf("export %s: listing did not end", g.name)
}
