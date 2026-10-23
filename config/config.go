// Package config loads and validates the gateway configuration.
//
// Secrets are never part of the YAML document: the master key, the
// internal vLLM keys and the PostgreSQL DSN are each read from a file whose
// path the configuration names. Nothing is read from the environment or
// from the command line, so a secret cannot leak through `ps`, a shell
// history or a process environment dump.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Class is the service class of a model group.
type Class string

const (
	// Interactive requests are latency sensitive (chat, IDE completion).
	Interactive Class = "interactive"
	// Batch requests are throughput oriented (offline evaluation, synthetic data).
	Batch Class = "batch"
)

// Config is the root of the YAML document.
type Config struct {
	Listen            string        `yaml:"listen"`
	MasterKeyFile     string        `yaml:"master_key_file"`
	MetricsTokenFile  string        `yaml:"metrics_token_file"`
	Store             Store         `yaml:"store"`
	Limits            Limits        `yaml:"limits"`
	FairShare         FairShare     `yaml:"fairshare"`
	Health            Health        `yaml:"health"`
	Retries           int           `yaml:"retries"`
	Teams             []Team        `yaml:"teams"`
	Pools             []Pool        `yaml:"pools"`
	ModelGroups       []ModelGroup  `yaml:"model_groups"`
	ShutdownGrace     time.Duration `yaml:"shutdown_grace"`
	BudgetRefresh     time.Duration `yaml:"budget_refresh"`
	KeyCacheTTL       time.Duration `yaml:"key_cache_ttl"`
	TrustForwardedFor bool          `yaml:"trust_forwarded_for"`
	ExposeOverheadHdr bool          `yaml:"expose_overhead_header"`
}

// Store selects where persistent state lives.
type Store struct {
	// Kind is "memory" (tests, single-node trials; state is lost on
	// restart) or "postgres".
	Kind string `yaml:"kind"`
	// DSNFile is the path of a file holding the PostgreSQL DSN.
	DSNFile string `yaml:"dsn_file"`
	// FailClosed denies requests with 503 when the budget state cannot be
	// refreshed from the store. Default true.
	FailClosed *bool `yaml:"fail_closed"`
	// QueueSize bounds the asynchronous accounting queue.
	QueueSize int `yaml:"queue_size"`
	// BatchSize is the number of usage records written per INSERT batch.
	BatchSize int `yaml:"batch_size"`
}

// Limits are the HTTP hardening limits.
type Limits struct {
	MaxBodyBytes          int64         `yaml:"max_body_bytes"`
	MaxHeaderBytes        int           `yaml:"max_header_bytes"`
	ReadHeaderTimeout     time.Duration `yaml:"read_header_timeout"`
	ReadTimeout           time.Duration `yaml:"read_timeout"`
	IdleTimeout           time.Duration `yaml:"idle_timeout"`
	UpstreamHeaderTimeout time.Duration `yaml:"upstream_header_timeout"`
	MaxRequestDuration    time.Duration `yaml:"max_request_duration"`
	MaxResponseBytes      int64         `yaml:"max_response_bytes"`
}

// FairShare tunes the fair-share scheduler.
type FairShare struct {
	// HalfLife is the decay half-life of past usage (Slurm
	// PriorityDecayHalfLife). Default 168h (7 days, Slurm's default).
	HalfLife time.Duration `yaml:"half_life"`
}

// Health tunes upstream health checking.
type Health struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Cooldown time.Duration `yaml:"cooldown"`
	Path     string        `yaml:"path"`
}

// Team is a tenant (an institution) with its share of every pool.
type Team struct {
	Name  string  `yaml:"name"`
	Share float64 `yaml:"share"`
}

// Pool is a capacity domain: a set of GPUs whose sustainable concurrency
// C is measured externally.
type Pool struct {
	Name     string `yaml:"name"`
	Capacity int    `yaml:"capacity"`
}

// Cost is the internal price of tokens, in GPU-hours per million tokens.
type Cost struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
	CachedPerMTok float64 `yaml:"cached_per_mtok"`
}

// ModelGroup is a model name exposed to clients, served by one or more
// vLLM upstreams.
type ModelGroup struct {
	Name          string   `yaml:"name"`
	UpstreamModel string   `yaml:"upstream_model"`
	Pool          string   `yaml:"pool"`
	Class         Class    `yaml:"class"`
	Upstreams     []string `yaml:"upstreams"`
	APIKeyFile    string   `yaml:"api_key_file"`
	Cost          Cost     `yaml:"cost"`
}

// Defaults applied by Load.
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
	maxConfigBytes               = 4 << 20
)

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data)
}

// Parse decodes, defaults and validates a YAML document. Unknown fields
// are rejected so a misspelt key cannot silently disable a limit.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	setStr(&c.Listen, DefaultListen)
	setI64(&c.Limits.MaxBodyBytes, DefaultMaxBodyBytes)
	setInt(&c.Limits.MaxHeaderBytes, DefaultMaxHeaderBytes)
	setDur(&c.Limits.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	setDur(&c.Limits.ReadTimeout, DefaultReadTimeout)
	setDur(&c.Limits.IdleTimeout, DefaultIdleTimeout)
	setDur(&c.Limits.UpstreamHeaderTimeout, DefaultUpstreamHeaderTimeout)
	setDur(&c.Limits.MaxRequestDuration, DefaultMaxRequestDuration)
	setI64(&c.Limits.MaxResponseBytes, DefaultMaxResponseBytes)
	setDur(&c.FairShare.HalfLife, DefaultHalfLife)
	setDur(&c.Health.Interval, DefaultHealthInterval)
	setDur(&c.Health.Timeout, DefaultHealthTimeout)
	setDur(&c.Health.Cooldown, DefaultCooldown)
	setStr(&c.Health.Path, DefaultHealthPath)
	setDur(&c.ShutdownGrace, DefaultShutdownGrace)
	setDur(&c.BudgetRefresh, DefaultBudgetRefresh)
	setDur(&c.KeyCacheTTL, DefaultKeyCacheTTL)
	if c.Retries == 0 {
		c.Retries = DefaultRetries
	}
	setStr(&c.Store.Kind, "memory")
	setInt(&c.Store.QueueSize, DefaultQueueSize)
	setInt(&c.Store.BatchSize, DefaultBatchSize)
	if c.Store.FailClosed == nil {
		t := true
		c.Store.FailClosed = &t
	}
	for i := range c.ModelGroups {
		g := &c.ModelGroups[i]
		setStr(&g.UpstreamModel, g.Name)
		setStr(&g.Pool, g.Name)
		if g.Class == "" {
			g.Class = Interactive
		}
	}
}

// Validate checks the invariants the gateway relies on.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("config: "+format, a...)) }

	if c.MasterKeyFile == "" {
		add("master_key_file is required")
	}
	switch c.Store.Kind {
	case "memory":
	case "postgres":
		if c.Store.DSNFile == "" {
			add("store.dsn_file is required when store.kind is postgres")
		}
	default:
		add("store.kind %q: want memory or postgres", c.Store.Kind)
	}
	if c.Retries < 0 || c.Retries > 10 {
		add("retries %d: want 0..10", c.Retries)
	}
	if c.Limits.MaxBodyBytes <= 0 || c.Limits.MaxBodyBytes > 1<<30 {
		add("limits.max_body_bytes %d: want 1..1GiB", c.Limits.MaxBodyBytes)
	}

	teams := map[string]bool{}
	sum := 0.0
	for _, t := range c.Teams {
		if !validName(t.Name) {
			add("team name %q: want [A-Za-z0-9_.-]{1,64}", t.Name)
		}
		if teams[t.Name] {
			add("team %q declared twice", t.Name)
		}
		teams[t.Name] = true
		if t.Share < 0 || t.Share > 1 || math.IsNaN(t.Share) {
			add("team %q share %v: want 0..1", t.Name, t.Share)
		}
		sum += t.Share
	}
	if sum > 1+1e-9 {
		add("team shares sum to %v: want at most 1", sum)
	}

	pools := map[string]bool{}
	for _, p := range c.Pools {
		if !validName(p.Name) {
			add("pool name %q: want [A-Za-z0-9_.-]{1,64}", p.Name)
		}
		if pools[p.Name] {
			add("pool %q declared twice", p.Name)
		}
		pools[p.Name] = true
		if p.Capacity <= 0 || p.Capacity > 1<<20 {
			add("pool %q capacity %d: want 1..1048576", p.Name, p.Capacity)
		}
	}

	groups := map[string]bool{}
	for _, g := range c.ModelGroups {
		if g.Name == "" || len(g.Name) > 256 || strings.ContainsAny(g.Name, "\x00\r\n") {
			add("model group name %q is invalid", g.Name)
		}
		if groups[g.Name] {
			add("model group %q declared twice", g.Name)
		}
		groups[g.Name] = true
		if !pools[g.Pool] {
			add("model group %q: pool %q is not declared", g.Name, g.Pool)
		}
		if g.Class != Interactive && g.Class != Batch {
			add("model group %q: class %q: want interactive or batch", g.Name, g.Class)
		}
		if len(g.Upstreams) == 0 {
			add("model group %q: at least one upstream is required", g.Name)
		}
		for _, u := range g.Upstreams {
			if err := ValidateUpstream(u); err != nil {
				add("model group %q: %v", g.Name, err)
			}
		}
		if g.APIKeyFile == "" {
			add("model group %q: api_key_file is required", g.Name)
		}
		if g.Cost.InputPerMTok < 0 || g.Cost.OutputPerMTok < 0 || g.Cost.CachedPerMTok < 0 {
			add("model group %q: costs must be non-negative", g.Name)
		}
	}
	if len(c.ModelGroups) == 0 {
		add("at least one model group is required")
	}
	return errors.Join(errs...)
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
		return fmt.Errorf("upstream %q: credentials in a URL are refused; use api_key_file", u.Redacted())
	}
	if u.RawQuery != "" || u.Fragment != "" {
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
	if st.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
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

func validName(s string) bool {
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

// ValidName reports whether s is an acceptable team or pool name.
func ValidName(s string) bool { return validName(s) }

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}

func setI64(p *int64, v int64) {
	if *p == 0 {
		*p = v
	}
}

func setDur(p *time.Duration, v time.Duration) {
	if *p == 0 {
		*p = v
	}
}

// FailClosed reports whether budget enforcement fails closed.
func (c *Config) FailClosed() bool { return c.Store.FailClosed == nil || *c.Store.FailClosed }

// TeamShare returns the configured share of a team (0 when unknown).
func (c *Config) TeamShare(name string) float64 {
	for _, t := range c.Teams {
		if t.Name == name {
			return t.Share
		}
	}
	return 0
}
