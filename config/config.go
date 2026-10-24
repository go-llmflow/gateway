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
	"net/netip"
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
	// Vault, when set, is the OpenBao/Vault server that vault: secret
	// references are read from.
	Vault *Vault
	// TrustedProxies are the reverse proxies whose X-Forwarded-For
	// entries are believed; the client address is the rightmost entry not
	// in this list. Empty: X-Forwarded-For is ignored.
	TrustedProxies []netip.Prefix
	// Routes restricts route classes to client networks.
	Routes Routes
	// JWT, when set, accepts OpenID Connect access tokens on the inference
	// API besides virtual keys.
	JWT *JWT
	// SSO, when set, logs administrators in with OpenID Connect
	// (authorization code + PKCE) and keeps them in a session cookie.
	SSO *SSO
	// KeyRotation, when set, enables scheduled key rotation, delivering
	// new secrets to OpenBao/Vault under VaultPrefix/<team>/.
	KeyRotation *KeyRotation
	// SCIM, when set, serves SCIM 2.0 provisioning at /scim/v2.
	SCIM *SCIM
	// Audit configures the audit log's signed checkpoints. The log itself
	// is always kept.
	Audit Audit
	// Policies are request policies, applied to the teams they name and to
	// the keys created with them.
	Policies []Policy
	// Exports are destinations outside the database: S3-compatible
	// storage (MinIO, Ceph RGW, AWS), a local directory; GCS and Azure are
	// declared but not implemented.
	Exports []Export
}

// Audit configures the audit log.
type Audit struct {
	// SigningKeyFile is a secret reference to an Ed25519 seed (base64 or
	// hex) that signs checkpoints; empty: the chain is kept unsigned.
	SigningKeyFile string
	// CheckpointInterval is how often new entries are signed (default 5m).
	CheckpointInterval time.Duration
	// Anchor names the export every checkpoint, and the entries it covers,
	// are also written to: the copy outside the database that catches
	// truncation and rollback. Empty: no anchor.
	Anchor string
	// AnchorMaxLag is how old the newest exported checkpoint may get
	// before an alert (default 3 × CheckpointInterval).
	AnchorMaxLag time.Duration
}

// Export is a destination outside the database.
type Export struct {
	Name string
	Kind string // s3, file, gcs, azure
	// S3: endpoint (https, or http to loopback), region, bucket, path-style
	// addressing (MinIO, Ceph RGW), credentials as secret references.
	Endpoint, Region, Bucket string
	PathStyle                bool
	AccessKeyFile            string
	SecretKeyFile            string
	// Prefix starts every object key.
	Prefix string
	// LockMode (GOVERNANCE or COMPLIANCE) and Retention put objects under
	// Object Lock when the bucket has it enabled.
	LockMode  string
	Retention time.Duration
	// Directory is where a file export writes.
	Directory string
	// QueueSize bounds the objects waiting to be written (default 1024).
	QueueSize int
}

// Policy is a request policy: fields an inference request must carry, and
// size limits that replace the global ones.
type Policy struct {
	Name  string
	Teams []string
	// Require lists body fields that must be present and not empty: a
	// top-level name ("user") or a dotted path into objects
	// ("metadata.project").
	Require []string
	// RequireTags demands at least one request tag (from the key or the
	// tags header).
	RequireTags bool
	// MaxBodyBytes and MaxResponseBytes replace limits.max_body_bytes and
	// limits.max_response_bytes when set (0: the global value).
	MaxBodyBytes     int64
	MaxResponseBytes int64
}

// maxPolicyBytes bounds a policy's size limits.
const maxPolicyBytes = 1 << 30

// ValidFieldPath reports whether s is a dotted path of 1 to 8 names of
// [A-Za-z0-9_-].
func ValidFieldPath(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) > 8 || len(s) > 256 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

// SCIM configures provisioning by an identity provider: its users become
// gateway users, and the members of the mapped groups get a role on a team.
type SCIM struct {
	// TokenFile is a secret reference to the bearer token the identity
	// provider presents (at least 32 bytes).
	TokenFile string
	// BaseURL is https://<this gateway>/scim/v2 as the provider sees it.
	BaseURL string
	// Groups maps a group's displayName (without case) to a team role.
	Groups []SCIMGroup
}

// SCIMGroup gives the members of a provisioned group Role on Team.
type SCIMGroup struct {
	Name, Team, Role string
}

// KeyRotation configures scheduled key rotation.
type KeyRotation struct {
	// VaultPrefix is "mount/path": a key of team T may only be delivered
	// to vault:VaultPrefix/T/…#field.
	VaultPrefix   string
	CheckInterval time.Duration
}

// SSO configures the admin login at one OpenID provider.
type SSO struct {
	Issuer, ClientID string
	// ClientSecretFile is a secret reference; empty is a public client.
	ClientSecretFile string
	// RedirectURL is this gateway's /admin/callback as the browser sees
	// it; its origin is also the only Origin accepted on state-changing
	// cookie-authenticated requests.
	RedirectURL string
	Scopes      []string
	SessionTTL  time.Duration
	// Roles map ID token claims to role bindings, added to the bindings
	// the user has in the store.
	Roles []SSORole
}

// SSORole grants Role (on Org or Team, or proxy-wide) to whoever's Claim
// holds Value.
type SSORole struct {
	Claim, Value, Role, Org, Team string
}

// Origin is the scheme://host[:port] of the redirect URL.
func (s *SSO) Origin() string {
	u, err := url.Parse(s.RedirectURL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Routes holds the network restrictions per route class. A nil list means
// any address, except for Metrics and Healthz, which default to loopback.
type Routes struct {
	Inference []netip.Prefix
	Admin     []netip.Prefix
	Metrics   []netip.Prefix
	Healthz   []netip.Prefix
}

// JWT configures access tokens from one OpenID Connect issuer.
type JWT struct {
	Issuer, Audience, JWKSURL, Type string
	UsernameClaim, GroupsClaim      string
	ClockSkew, MinRefresh           time.Duration
	// Limits applied to every token holder, like a key's.
	RPM, TPM, MaxInFlight int
	// Maps are tried in order; the first match names the team.
	Maps []JWTMap
}

// JWTMap maps a claim value to a team (and optionally a project).
type JWTMap struct {
	Claim, Value, Team, Project string
}

// LoopbackOnly is the default for /metrics and /healthz.
var LoopbackOnly = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// Vault reaches an OpenBao or HashiCorp Vault KV v2 engine.
type Vault struct {
	Address   string
	TokenFile string // a file reference, never a vault: one
	CAFile    string
	Namespace string
	CacheTTL  time.Duration
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

// ValidUserName reports whether s is an acceptable user name: a team name's
// characters plus "@" and "+", so that an e-mail address or a userPrincipalName
// can be one, at most 128 bytes.
func ValidUserName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-', r == '@', r == '+':
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
