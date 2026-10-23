// Package config loads and validates the gateway configuration, written in
// HCL2 native syntax (https://github.com/hashicorp/hcl, hclsyntax/spec.md).
//
// Secrets are never part of the configuration: the master key, the vLLM
// keys and the PostgreSQL DSN are each read from a file whose path the
// configuration names. The configuration is evaluated with no variables and
// no functions, so it cannot read the environment or other files either.
package config

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// Class is the service class of a model group.
type Class string

const (
	// Interactive requests are latency sensitive (chat, IDE completion).
	Interactive Class = "interactive"
	// Batch requests are throughput oriented (offline evaluation, synthetic data).
	Batch Class = "batch"
)

// Config is the validated configuration.
type Config struct {
	Listen            string
	MasterKeyFile     string
	MetricsTokenFile  string
	Store             Store
	Limits            Limits
	FairShare         FairShare
	Health            Health
	Retries           int
	Orgs              []Org
	Pools             []Pool
	ModelGroups       []ModelGroup
	ShutdownGrace     time.Duration
	BudgetRefresh     time.Duration
	KeyCacheTTL       time.Duration
	ExposeOverheadHdr bool
	// CacheIsolation is the tenant scope of vLLM's prefix cache: "org"
	// (default), "team", "key" or "none". CacheSaltKeyFile holds the HMAC
	// key the per-scope cache_salt is derived from (required unless none).
	CacheIsolation   string
	CacheSaltKeyFile string
	// Tags configures request tags; Alerts the soft-budget alert channels.
	Tags   Tags
	Alerts Alerts
	// AuthFailuresPerMinute is the number of failed authentications a
	// client network (IPv4 address, IPv6 /64) may make per minute before it
	// gets 429; 0 disables the limit.
	AuthFailuresPerMinute int
}

// Tags lets clients label requests through one header, restricted to an
// allow-list. Keys carry their own tags too (set by an administrator).
type Tags struct {
	// Header is the request header holding comma-separated tags; empty
	// disables request tags.
	Header string
	// Allowed lists the tags a request may carry; others are refused.
	Allowed []string
}

// Alerts are the channels soft-budget alerts go to.
type Alerts struct {
	WebhookURL       string
	WebhookTokenFile string
	SMTP             *SMTP
}

// SMTP is a mail relay for alerts.
type SMTP struct {
	Server       string // host:port
	From         string
	To           []string
	Username     string
	PasswordFile string
}

// Store selects where persistent state lives.
type Store struct {
	// Kind is "memory" (tests, single-node trials; state is lost on
	// restart) or "postgres".
	Kind string
	// DSNFile is the path of a file holding the PostgreSQL DSN.
	DSNFile string
	// FailClosed denies requests with 503 when the budget state cannot be
	// refreshed from the store. Default true.
	FailClosed *bool
	// QueueSize bounds the asynchronous accounting queue.
	QueueSize int
	// BatchSize is the number of usage records written per batch.
	BatchSize int
}

// Limits are the HTTP hardening limits.
type Limits struct {
	MaxBodyBytes          int64
	MaxHeaderBytes        int
	ReadHeaderTimeout     time.Duration
	ReadTimeout           time.Duration
	IdleTimeout           time.Duration
	UpstreamHeaderTimeout time.Duration
	MaxRequestDuration    time.Duration
	MaxResponseBytes      int64
}

// FairShare tunes the fair-share scheduler.
type FairShare struct {
	// HalfLife is the decay half-life of past usage (Slurm
	// PriorityDecayHalfLife). Default 168h (7 days, Slurm's default).
	HalfLife time.Duration
}

// Health tunes upstream health checking.
type Health struct {
	Interval time.Duration
	Timeout  time.Duration
	Cooldown time.Duration
	Path     string
}

// Org is an organisation (an institution) with its share of every pool
// and the teams (labs) declared under it. Teams and projects can also be
// created through the admin API.
type Org struct {
	Name  string
	Share float64
	Teams []string
}

// Pool is a capacity domain whose sustainable concurrency C is measured
// externally.
type Pool struct {
	Name     string
	Capacity int
}

// Cost is the internal price of tokens, in GPU-hours per million tokens.
type Cost struct {
	InputPerMTok  float64
	OutputPerMTok float64
	CachedPerMTok float64
}

// Upstream is one vLLM server of a model group.
type Upstream struct {
	Name    string
	BaseURL string
	// KeyFile holds the vLLM --api-key of this server (the model's
	// key_file when the upstream block does not set one).
	KeyFile string
}

// ModelGroup is a model name exposed to clients, served by one or more
// vLLM upstreams.
type ModelGroup struct {
	Name          string
	UpstreamModel string
	Pool          string
	Class         Class
	Upstreams     []Upstream
	Cost          Cost
}

// Defaults.
const (
	DefaultListen                = "127.0.0.1:8080"
	DefaultMaxBodyBytes          = 8 << 20
	DefaultMaxHeaderBytes        = 64 << 10
	DefaultReadHeaderTimeout     = 10 * time.Second
	DefaultReadTimeout           = 60 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultUpstreamHeaderTimeout = 10 * time.Minute
	DefaultMaxRequestDuration    = 30 * time.Minute
	DefaultMaxResponseBytes      = 64 << 20
	DefaultHalfLife              = 7 * 24 * time.Hour
	DefaultHealthInterval        = 5 * time.Second
	DefaultHealthTimeout         = 2 * time.Second
	DefaultCooldown              = 30 * time.Second
	DefaultHealthPath            = "/health"
	DefaultRetries               = 2
	DefaultQueueSize             = 10000
	DefaultBatchSize             = 256
	DefaultShutdownGrace         = 30 * time.Second
	DefaultBudgetRefresh         = 10 * time.Second
	DefaultKeyCacheTTL           = 30 * time.Second
	DefaultAuthFailuresPerMinute = 60
	maxConfigBytes               = 4 << 20
)

// Load reads, decodes and validates a configuration file. A configuration
// error is a *Error whose message lists every problem with its
// file:line:column.
func Load(path string) (*Config, error) {
	data, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data, path)
}

// ValidateUpstream accepts only absolute http(s) URLs with a host and no
// credentials, query or fragment. Upstreams come from the configuration
// only; nothing a client sends can name one.
func ValidateUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("upstream %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("upstream %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("upstream %q: host is required", raw)
	}
	if u.User != nil {
		return fmt.Errorf("upstream %q: credentials in a URL are refused; use key_file", u.Redacted())
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("upstream %q: query and fragment are not allowed", raw)
	}
	return nil
}

// ReadSecret reads a secret file, trims surrounding white space and
// refuses an empty file. The error never contains the file's content.
func ReadSecret(path string) (string, error) {
	data, err := readBounded(path, 64<<10)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("secret file %s is empty", path)
	}
	if strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("secret file %s holds more than one line", path)
	}
	return s, nil
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

// ValidName reports whether s is an acceptable team or pool name.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// FailClosed reports whether budget enforcement fails closed.
func (c *Config) FailClosed() bool { return c.Store.FailClosed == nil || *c.Store.FailClosed }

// OrgShare returns the configured share of an organisation (0 when
// unknown).
func (c *Config) OrgShare(name string) float64 {
	for _, o := range c.Orgs {
		if o.Name == name {
			return o.Share
		}
	}
	return 0
}

// KeyFiles lists every distinct upstream key file, in configuration order.
func (c *Config) KeyFiles() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range c.ModelGroups {
		for _, u := range g.Upstreams {
			if !seen[u.KeyFile] {
				seen[u.KeyFile] = true
				out = append(out, u.KeyFile)
			}
		}
	}
	return out
}
