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
	"slices"
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
	// Guard models (Llama Guard, Prompt Guard) serve the gateway's
	// guardrails only: clients cannot call them and /v1/models omits them.
	Guard Class = "guard"
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
	// Attribution names the project header and the default project.
	Attribution Attribution
	// Slurm is the batch accounting input of the reconciliation reports.
	Slurm Slurm
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
	// Budgets tunes the hard budgets' escrow (budgets.go).
	Budgets Budgets
	// Fluids prices and attributes measured energy; nil without a fluids
	// block (#97).
	Fluids *Fluids
	// AdoptionMinUsers is the small-cell threshold of the adoption figures: a
	// day, a week or a model used by fewer distinct people is not shown
	// (default 5, at least 2: one person is never a figure).
	AdoptionMinUsers int
	// DecisionReport schedules the monthly decision report into an export;
	// nil: reports are only downloaded (#97).
	DecisionReport *DecisionReport
	// Profiles are the workload profiles (profiles.go, #90).
	Profiles Profiles
	// SoftQuotas are the warnings before a window runs out (softquotas.go, #96).
	SoftQuotas SoftQuotas
	// HA, when set, runs this gateway as one of several replicas sharing
	// the pools through PostgreSQL (pool ownership by lease, package ha).
	// Absent: one replica, everything in its memory, as before.
	HA *HA
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
	// Logging sends per-request logs of the teams it names to an export.
	// Teams in no logging block have none: spend metadata only, in the
	// store.
	Logging []Logging
	// Guardrails inspect prompts before vLLM and completions after; each
	// applies to the orgs and teams it names and to the keys created with
	// it.
	Guardrails []Guardrail
	// Tracing, when set, exports OpenTelemetry traces over OTLP.
	Tracing *Tracing
	// MCPServers are the tool servers exposed at /mcp.
	MCPServers []MCPServer
}

// Tracing configures OpenTelemetry traces (OTLP/HTTP or OTLP/gRPC).
type Tracing struct {
	// Endpoint is the collector: a URL for http (…:4318, /v1/traces is
	// added), host:port for grpc (…:4317).
	Endpoint string
	Protocol string // "http" or "grpc"
	// Insecure allows http:// and plaintext gRPC.
	Insecure    bool
	SampleRatio float64
	ServiceName string
	// TokenFile is a secret reference to a bearer token for the collector.
	TokenFile string
	// TrustClientContext continues a client's W3C trace (traceparent);
	// false (default): every request starts a new trace, as the W3C
	// guidance advises for an entry point (no sampling forced by callers,
	// no correlation across tenants).
	TrustClientContext bool
}

// Logging is a per-team request log.
type Logging struct {
	Name        string
	Teams       []string
	Destination string // an export
	// Content adds the prompt and the completion (each cut at
	// MaxContentBytes). False: metadata only.
	Content         bool
	MaxContentBytes int
	// Records are written in batches of BatchRecords, or every
	// BatchInterval, whichever comes first.
	BatchRecords  int
	BatchInterval time.Duration
	// Lock puts the objects under the export's Object Lock retention.
	// Off by default: a content log under COMPLIANCE cannot be erased, not
	// even on a lawful request.
	Lock bool
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
	// Retention is how long entries of each kind are kept (absent: for
	// ever). Pruning removes whole segments, up to a signed checkpoint,
	// whose every entry is past its kind's retention.
	Retention     map[string]time.Duration
	PruneInterval time.Duration
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

// HA configures several replicas (see package ha).
type HA struct {
	// ReplicaID names the replica in logs and leases (a random suffix is
	// added at start: a restarted replica is a new instance).
	ReplicaID string
	// Listen is the internal listener (replicas only: admission
	// forwarding); Advertise is the IP:port the others reach it at
	// (default: Listen).
	Listen, Advertise string
	// PeerNetworks are the networks replicas live in: an owner address
	// read from the database outside them is never dialled.
	PeerNetworks []netip.Prefix
	// SecretFile holds the shared secret (at least 32 bytes) that
	// authenticates the internal channel; the SSO session key is derived
	// from it too.
	SecretFile string
	// LeaseTTL is the pool lease's duration (renewed every third);
	// Heartbeat how often replicas report their grants to owners;
	// ForwardTimeout bounds one call to an owner; UsagePersist how often an
	// owner persists the decayed usage.
	LeaseTTL, Heartbeat, ForwardTimeout, UsagePersist time.Duration
	// OnOwnerUnreachable: "lowest_priority" (default) admits at the lowest
	// borrowing priority, "reject" answers 503.
	OnOwnerUnreachable string
	// TLSCertFile, TLSKeyFile and TLSCAFile (secret references, all three
	// or none) put the internal channel under mutual TLS 1.3: every replica
	// presents its certificate and accepts only peers signed by the CA,
	// whose certificate carries TLSServerName.
	TLSCertFile, TLSKeyFile, TLSCAFile, TLSServerName string
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
	// MinRefresh is how long the ID-token key set waits before it is
	// fetched again for an unknown kid (default 1m, at least
	// MinKeySetRefresh). A provider that rotates its keys more often than
	// that needs it shorter, or logins right after a rotation are refused.
	MinRefresh time.Duration
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
	// SpoolDir holds the durable accounting spool: every batch of usage
	// records is appended there (fsync'd) before it is written to the store,
	// and replayed when the store is back or the gateway restarts. Required
	// with kind "postgres".
	SpoolDir string
	// SpoolMaxBytes bounds the spool of one process (default 256 MiB).
	SpoolMaxBytes int64
}

// MinKeySetRefresh is the shortest min_refresh of the jwt and sso blocks:
// each unknown kid may make the gateway fetch the key set at most once per
// min_refresh, so the bound keeps a flood of forged kids from turning the
// gateway into a load on the provider.
const MinKeySetRefresh = 5 * time.Second

// DefaultSpoolMaxBytes bounds a process's accounting spool.
const DefaultSpoolMaxBytes = 256 << 20

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
	// MaxAudioBytes bounds an audio upload (/v1/audio/transcriptions); the
	// default is vLLM's own (VLLM_MAX_AUDIO_CLIP_FILESIZE_MB, 25 MB).
	MaxAudioBytes int64
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
	// Labs maps a configured team to its lab (team "t" { lab = "x" }): an
	// optional accounting level between the organisation and its teams,
	// for reports only (RBAC does not use it). Teams without a lab, and
	// teams created through the API, belong to no lab.
	Labs map[string]string
	// WarnAt overrides the soft-quota thresholds of the organisation's people
	// (nil: not set; #96).
	WarnAt []int
	// CostCenter is the organisation's general-ledger code for the chargeback
	// (#97); LabCostCenter and ProjectCostCenter (by team and project) give a
	// lab or a project its own, which then wins over the organisation's.
	CostCenter        string
	LabCostCenter     map[string]string
	ProjectCostCenter map[[2]string]string
}

// CostCenterOf resolves the cost centre of a consumer: the project's, else
// the lab's, else the organisation's; "" when none is configured. The
// organisation is looked up by name, whatever the other arguments.
func (c *Config) CostCenterOf(org, lab, team, project string) string {
	for _, o := range c.Orgs {
		if o.Name != org {
			continue
		}
		if cc := o.ProjectCostCenter[[2]string{team, project}]; cc != "" && project != "" {
			return cc
		}
		if cc := o.LabCostCenter[lab]; cc != "" && lab != "" {
			return cc
		}
		return o.CostCenter
	}
	return ""
}

// Attribution says how a request names its project.
type Attribution struct {
	// ProjectHeader is the request header naming the project, within the
	// key's own team (default X-Llmflow-Project).
	ProjectHeader string
	// DefaultProject applies when neither the header nor the key names a
	// project, if the key's team has a project of that name.
	DefaultProject string
}

// Slurm is the optional batch accounting input of the reconciliation
// reports.
type Slurm struct {
	// SreportFile is the output of `sreport cluster AccountUtilizationByUser
	// -T gres/gpu -P`, read when a report is built ("" for none).
	SreportFile string
	// Accounts maps a Slurm account to a gateway organisation; an account
	// absent from the map counts for the organisation of the same name.
	Accounts map[string]string
}

// OrgOfAccount returns the organisation a Slurm account counts for.
func (s Slurm) OrgOfAccount(acct string) string {
	if o, ok := s.Accounts[acct]; ok {
		return o
	}
	return acct
}

// DefaultProjectHeader is the default attribution header.
const DefaultProjectHeader = "X-Llmflow-Project"

// LabOf returns the lab of a configured team, or "".
func (c *Config) LabOf(team string) string {
	for _, o := range c.Orgs {
		if l, ok := o.Labs[team]; ok {
			return l
		}
	}
	return ""
}

// Pool is a capacity domain whose sustainable concurrency C is measured
// externally.
type Pool struct {
	Name     string
	Capacity int
}

// Cost is the price of tokens per million, in Unit: GPU-hours ("") for a
// vLLM model by default, or a currency ("USD") for an external provider.
// Costs of different units are never added: budgets and reports keep
// them apart, and every model of a pool must share one unit (fair-share
// usage is a sum over the pool).
type Cost struct {
	InputPerMTok  float64
	OutputPerMTok float64
	CachedPerMTok float64
	// AudioPerHour is the price of transcription, in GPU-hours per hour of
	// audio (vLLM reports a transcription's usage as a duration).
	AudioPerHour float64
	// Prompt-cache write prices of an external provider (Anthropic: a
	// 5-minute and a 1-hour lifetime); reads use CachedPerMTok. Zero for
	// both: writes are priced as input.
	CacheWrite5mPerMTok float64
	CacheWrite1hPerMTok float64
	// LongContext, when set, replaces the prices of a request whose input
	// (cached or not) exceeds LongContextAbove tokens (Claude Haiku 5.5
	// above 100 000).
	LongContextAbove int64
	LongContext      *Cost
	// Unit is "" for GPU-hours, otherwise a currency code (see
	// store.NormalizeUnit); PriceDate (YYYY-MM-DD) records when the prices
	// were read from the provider's price list, required with a currency.
	Unit      string
	PriceDate string
}

// Upstream is one vLLM server of a model group.
type Upstream struct {
	Name    string
	BaseURL string
	// KeyFile holds the vLLM --api-key of this server (the model's
	// key_file when the upstream block does not set one).
	KeyFile string
	// DcgmURL is the node's NVIDIA DCGM exporter, scraped for energy when a
	// fluids block exists ("" for none).
	DcgmURL string
}

// ModelGroup is a model name exposed to clients, served by one or more
// vLLM upstreams.
type ModelGroup struct {
	Name          string
	UpstreamModel string
	Pool          string
	Class         Class
	Upstreams     []Upstream
	// Slurm, when set, adds the running Slurm jobs of the convention
	// llmflow:<model>:<port>[:<capacity>] as upstreams (#75).
	Slurm *SlurmJobs
	Cost  Cost
	// Fallbacks are tried in order when no upstream of this group can take
	// the request (all down, or vLLM answers 429/503) before anything was
	// sent to the client.
	Fallbacks []string
	// FallbackOn says when a fallback is taken: "unavailable" (the above,
	// the default) and/or "saturated": a BORROWING request that arrives
	// while this group's pool is full (C or more in flight), or while vLLM
	// reports SaturationWaiting or more requests waiting, moves to the first
	// fallback whose pool has room (#58).
	FallbackOn []string
	// SaturationWaiting, when > 0, also reads vLLM's vllm:num_requests_waiting
	// on each upstream's /metrics (at the health interval, with its timeout)
	// and calls the group saturated when their sum reaches it.
	SaturationWaiting int
	// External is set for a model served by an external provider
	// (external.go); nil for vLLM.
	External *External
	// MaxModelLen is the context length that bounds a request's input (hard
	// budgets): max_model_len for vLLM (0: read from the upstreams'
	// /v1/models), context_window for an external provider (0: unknown, so
	// the model is refused under a budget).
	MaxModelLen int64
	// MaxOutputTokens, when > 0, caps the max_tokens a request may ask for:
	// a larger one is refused, a missing one becomes this cap (or less, to
	// fit a budget).
	MaxOutputTokens int64
}

// FallsBackOn reports whether the group takes its fallbacks on reason.
func (g ModelGroup) FallsBackOn(reason string) bool {
	if len(g.FallbackOn) == 0 {
		return reason == "unavailable"
	}
	return slices.Contains(g.FallbackOn, reason)
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
	DefaultMaxAudioBytes         = 25 << 20
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

// normalizeUnit maps "gpu_hours" to "" and checks a currency unit, like
// store.NormalizeUnit (the config does not import the store).
func normalizeUnit(u string) (string, bool) {
	if u == "" || u == "gpu_hours" {
		return "", true
	}
	if len(u) > 16 {
		return u, false
	}
	for _, r := range u {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return u, false
		}
	}
	return u, true
}

func unitName(u string) string {
	if u == "" {
		return "gpu_hours"
	}
	return u
}

// Units lists the cost units the models use, GPU-hours ("") first.
func (c *Config) Units() []string {
	seen := map[string]bool{"": true}
	out := []string{""}
	for _, g := range c.ModelGroups {
		if !seen[g.Cost.Unit] {
			seen[g.Cost.Unit] = true
			out = append(out, g.Cost.Unit)
		}
	}
	return out
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

// KeyFiles lists every distinct upstream key file, in configuration order:
// those of the upstreams, then an external provider's per-organisation
// keys.
func (c *Config) KeyFiles() []string {
	seen := map[string]bool{"": true}
	var out []string
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, g := range c.ModelGroups {
		for _, u := range g.Upstreams {
			add(u.KeyFile)
		}
		if g.External != nil {
			for _, o := range g.External.OrgKeyOrder {
				add(g.External.OrgKeys[o])
			}
		}
		if g.Slurm != nil {
			// The discovered instances' vLLM key, and slurmrestd's JWT.
			add(g.Slurm.KeyFile)
			add(g.Slurm.TokenFile)
		}
	}
	return out
}
