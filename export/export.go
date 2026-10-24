// Package export writes objects outside the gateway's database: to
// S3-compatible storage (MinIO and Ceph RGW first, AWS too) through
// github.com/go-volumes/s3, or to a local directory. GCS and Azure Blob are
// behind the same interface and answer "not implemented".
//
// Objects are written once. On S3 they go under Object Lock retention when
// the bucket has Object Lock enabled (it must be enabled when the bucket is
// created, and needs versioning: MinIO and Ceph RGW both say so); a local
// directory refuses to overwrite a file. A Queue writes in the background,
// bounded, with retries; what it drops or gives up on is counted, never
// silent.
package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-volumes/s3/client"
)

// MaxObject bounds an object read back (checkpoints, anchors).
const MaxObject = 1 << 20

// ErrNotImplemented is what GCS and Azure answer.
var ErrNotImplemented = errors.New("export: not implemented")

// PutOptions describe an object.
type PutOptions struct {
	ContentType     string
	ContentEncoding string
	// Retain puts the object under the export's retention, when the
	// destination can enforce it.
	Retain bool
}

// Sink is a destination.
type Sink interface {
	Name() string
	// Put writes an object; writing a key twice is refused where the
	// destination can refuse it.
	Put(ctx context.Context, key string, body []byte, o PutOptions) error
	// Get reads an object of at most MaxObject bytes.
	Get(ctx context.Context, key string) ([]byte, error)
	// List returns the keys under prefix, in lexicographic order.
	List(ctx context.Context, prefix string) ([]string, error)
	// Locked reports whether objects written with Retain are under
	// write-once retention.
	Locked() bool
}

// Credentials are an S3 export's keys, resolved by the caller.
type Credentials struct{ AccessKey, SecretKey string }

// Open returns the sink an export block describes. An S3 sink reads the
// bucket's Object Lock configuration now.
func Open(ctx context.Context, e config.Export, creds Credentials, now func() time.Time) (Sink, error) {
	if now == nil {
		now = time.Now
	}
	switch e.Kind {
	case "s3":
		c, err := client.New(client.Config{Endpoint: e.Endpoint, Region: e.Region, Bucket: e.Bucket,
			AccessKey: creds.AccessKey, SecretKey: creds.SecretKey, PathStyle: e.PathStyle})
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", e.Name, err)
		}
		ol, err := c.ObjectLockConfiguration(ctx)
		if err != nil {
			return nil, fmt.Errorf("export %s: reading the bucket's object lock configuration: %w", e.Name, err)
		}
		return &s3Sink{name: e.Name, c: c, prefix: e.Prefix, lock: ol.Enabled, mode: e.LockMode, retention: e.Retention, now: now}, nil
	case "file":
		if err := os.MkdirAll(e.Directory, 0o700); err != nil {
			return nil, fmt.Errorf("export %s: %w", e.Name, err)
		}
		return &fileSink{name: e.Name, dir: e.Directory, prefix: e.Prefix}, nil
	case "gcs", "azure":
		return nil, fmt.Errorf("export %s: kind %s: %w", e.Name, e.Kind, ErrNotImplemented)
	}
	return nil, fmt.Errorf("export %s: unknown kind %q", e.Name, e.Kind)
}

// ValidKey reports whether a key is safe on every destination: segments
// of [A-Za-z0-9._-], none "." or "..", no empty segment.
func ValidKey(key string) bool {
	if key == "" || len(key) > 512 {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

type s3Sink struct {
	name      string
	c         *client.Client
	prefix    string
	lock      bool
	mode      string
	retention time.Duration
	now       func() time.Time
}

func (s *s3Sink) Name() string { return s.name }
func (s *s3Sink) Locked() bool { return s.lock && s.retention > 0 }

func (s *s3Sink) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	if !ValidKey(key) {
		return fmt.Errorf("export %s: invalid key %q", s.name, key)
	}
	po := client.PutOptions{ContentType: o.ContentType, ContentEncoding: o.ContentEncoding}
	if o.Retain && s.Locked() {
		po.LockMode, po.RetainUntil = s.mode, s.now().Add(s.retention)
	}
	return s.c.PutWithOptions(ctx, s.prefix+key, body, po)
}

func (s *s3Sink) Get(ctx context.Context, key string) ([]byte, error) {
	if !ValidKey(key) {
		return nil, fmt.Errorf("export %s: invalid key %q", s.name, key)
	}
	return s.c.GetRange(ctx, s.prefix+key, 0, MaxObject)
}

func (s *s3Sink) List(ctx context.Context, prefix string) ([]string, error) {
	objs, err := s.c.List(ctx, s.prefix+prefix, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, strings.TrimPrefix(o.Key, s.prefix))
	}
	return out, nil
}

// fileSink writes under a directory, never overwriting a file.
type fileSink struct {
	name, dir, prefix string
}

func (f *fileSink) Name() string { return f.name }

// Locked is false: a local directory does not stop its owner.
func (f *fileSink) Locked() bool { return false }

func (f *fileSink) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("export %s: invalid key %q", f.name, key)
	}
	p := filepath.Join(f.dir, filepath.FromSlash(f.prefix+key))
	if rel, err := filepath.Rel(f.dir, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("export %s: key %q leaves the directory", f.name, key)
	}
	return p, nil
}

func (f *fileSink) Put(_ context.Context, key string, body []byte, _ PutOptions) error {
	p, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A hard link fails when the name exists: written once, and the file
	// appears whole or not at all.
	if err := os.Link(tmp.Name(), p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("export %s: %s already exists", f.name, key)
		}
		return err
	}
	return nil
}

func (f *fileSink) Get(_ context.Context, key string) ([]byte, error) {
	p, err := f.path(key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, MaxObject+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxObject {
		return nil, fmt.Errorf("export %s: %s is larger than %d bytes", f.name, key, MaxObject)
	}
	return b, nil
}

func (f *fileSink) List(_ context.Context, prefix string) ([]string, error) {
	root := filepath.Join(f.dir, filepath.FromSlash(f.prefix))
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
