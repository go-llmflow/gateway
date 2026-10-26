package config

import (
	"fmt"
	"math"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/secret"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// The HCL schema. Optional attributes are pointers so that "absent" (take
// the default) and "zero" (e.g. retries = 0) stay distinct. Each value that
// is checked after decoding has its value range captured, so the
// diagnostic points at it.

type fileHCL struct {
	Listen               *string         `hcl:"listen,optional"`
	ListenRange          hcl.Range       `hcl:"listen,attr_value_range"`
	MasterKeyFile        string          `hcl:"master_key_file"`
	MasterKeyFileRange   hcl.Range       `hcl:"master_key_file,attr_value_range"`
	MetricsTokenFile     *string         `hcl:"metrics_token_file,optional"`
	Retries              *int            `hcl:"retries,optional"`
	RetriesRange         hcl.Range       `hcl:"retries,attr_value_range"`
	BudgetRefresh        *string         `hcl:"budget_refresh,optional"`
	BudgetRefreshRange   hcl.Range       `hcl:"budget_refresh,attr_value_range"`
	KeyCacheTTL          *string         `hcl:"key_cache_ttl,optional"`
	KeyCacheTTLRange     hcl.Range       `hcl:"key_cache_ttl,attr_value_range"`
	ShutdownGrace        *string         `hcl:"shutdown_grace,optional"`
	ShutdownGraceRange   hcl.Range       `hcl:"shutdown_grace,attr_value_range"`
	ExposeOverheadHeader *bool           `hcl:"expose_overhead_header,optional"`
	ExternalGuardrails   *bool           `hcl:"external_requires_guardrails,optional"`
	ExternalGuardsRange  hcl.Range       `hcl:"external_requires_guardrails,attr_value_range"`
	CacheIsolation       *string         `hcl:"cache_isolation,optional"`
	CacheIsolationRange  hcl.Range       `hcl:"cache_isolation,attr_value_range"`
	CacheSaltKeyFile     *string         `hcl:"cache_salt_key_file,optional"`
	AuthFailures         *int            `hcl:"auth_failures_per_minute,optional"`
	AuthFailuresRange    hcl.Range       `hcl:"auth_failures_per_minute,attr_value_range"`
	Store                *storeHCL       `hcl:"store,block"`
	Limits               *limitsHCL      `hcl:"limits,block"`
	FairShare            *fairHCL        `hcl:"fairshare,block"`
	Health               *healthHCL      `hcl:"health,block"`
	Orgs                 []orgHCL        `hcl:"org,block"`
	Tags                 *tagsHCL        `hcl:"tags,block"`
	Attribution          *attributionHCL `hcl:"attribution,block"`
	Slurm                *slurmHCL       `hcl:"slurm,block"`
	Alerts               *alertsHCL      `hcl:"alerts,block"`
	Pools                []poolHCL       `hcl:"pool,block"`
	Models               []modelHCL      `hcl:"model,block"`
	Vault                *vaultHCL       `hcl:"vault,block"`
	TrustedProxies       []string        `hcl:"trusted_proxies,optional"`
	TrustedProxiesRange  hcl.Range       `hcl:"trusted_proxies,attr_value_range"`
	Routes               *routesHCL      `hcl:"routes,block"`
	JWT                  *jwtHCL         `hcl:"jwt,block"`
	SSO                  *ssoHCL         `hcl:"sso,block"`
	KeyRotation          *rotHCL         `hcl:"key_rotation,block"`
	SCIM                 *scimHCL        `hcl:"scim,block"`
	HA                   *haHCL          `hcl:"ha,block"`
	Policies             []policyHCL     `hcl:"policy,block"`
	Audit                *auditHCL       `hcl:"audit,block"`
	Exports              []exportHCL     `hcl:"export,block"`
	Logging              []loggingHCL    `hcl:"logging,block"`
	Guardrails           []guardrailHCL  `hcl:"guardrail,block"`
	Tracing              *tracingHCL     `hcl:"tracing,block"`
	MCPServers           []mcpServerHCL  `hcl:"mcp_server,block"`
}

type loggingHCL struct {
	Name             string    `hcl:"name,label"`
	NameRange        hcl.Range `hcl:"name,label_range"`
	Teams            []string  `hcl:"teams"`
	TeamsRange       hcl.Range `hcl:"teams,attr_value_range"`
	Destination      string    `hcl:"destination"`
	DestinationRange hcl.Range `hcl:"destination,attr_value_range"`
	Content          *bool     `hcl:"content,optional"`
	MaxContentBytes  *int      `hcl:"max_content_bytes,optional"`
	BatchRecords     *int      `hcl:"batch_records,optional"`
	BatchInterval    *string   `hcl:"batch_interval,optional"`
	BatchRange       hcl.Range `hcl:"batch_interval,attr_value_range"`
	Lock             *bool     `hcl:"lock,optional"`
	DefRange         hcl.Range `hcl:",def_range"`
}

type auditHCL struct {
	SigningKeyFile     *string           `hcl:"signing_key_file,optional"`
	CheckpointInterval *string           `hcl:"checkpoint_interval,optional"`
	IntervalRange      hcl.Range         `hcl:"checkpoint_interval,attr_value_range"`
	Anchor             *string           `hcl:"anchor,optional"`
	AnchorRange        hcl.Range         `hcl:"anchor,attr_value_range"`
	AnchorMaxLag       *string           `hcl:"anchor_max_lag,optional"`
	AnchorMaxLagRange  hcl.Range         `hcl:"anchor_max_lag,attr_value_range"`
	Retention          map[string]string `hcl:"retention,optional"`
	RetentionRange     hcl.Range         `hcl:"retention,attr_value_range"`
	PruneInterval      *string           `hcl:"prune_interval,optional"`
	PruneIntervalRange hcl.Range         `hcl:"prune_interval,attr_value_range"`
}

// AuditKinds are the kinds of audit entries (retention is set per kind).
var AuditKinds = []string{"admin", "auth", "scim", "system", "guardrail", "tool"}

type exportHCL struct {
	Name           string    `hcl:"name,label"`
	NameRange      hcl.Range `hcl:"name,label_range"`
	Kind           string    `hcl:"kind"`
	KindRange      hcl.Range `hcl:"kind,attr_value_range"`
	Endpoint       *string   `hcl:"endpoint,optional"`
	EndpointRange  hcl.Range `hcl:"endpoint,attr_value_range"`
	Region         *string   `hcl:"region,optional"`
	Bucket         *string   `hcl:"bucket,optional"`
	PathStyle      *bool     `hcl:"path_style,optional"`
	AccessKeyFile  *string   `hcl:"access_key_file,optional"`
	SecretKeyFile  *string   `hcl:"secret_key_file,optional"`
	Prefix         *string   `hcl:"prefix,optional"`
	PrefixRange    hcl.Range `hcl:"prefix,attr_value_range"`
	LockMode       *string   `hcl:"lock_mode,optional"`
	LockModeRange  hcl.Range `hcl:"lock_mode,attr_value_range"`
	Retention      *string   `hcl:"retention,optional"`
	RetentionRange hcl.Range `hcl:"retention,attr_value_range"`
	Directory      *string   `hcl:"directory,optional"`
	QueueSize      *int      `hcl:"queue_size,optional"`
	DefRange       hcl.Range `hcl:",def_range"`
}

// ValidObjectKey reports whether s may start or name an object: printable
// ASCII without "..", backslashes or a leading "/".
func ValidObjectKey(s string) bool {
	if len(s) > 512 || strings.HasPrefix(s, "/") || strings.Contains(s, "..") || strings.Contains(s, "//") {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '\\' {
			return false
		}
	}
	return true
}

type policyHCL struct {
	Name             string    `hcl:"name,label"`
	NameRange        hcl.Range `hcl:"name,label_range"`
	Teams            []string  `hcl:"teams,optional"`
	TeamsRange       hcl.Range `hcl:"teams,attr_value_range"`
	Require          []string  `hcl:"require,optional"`
	RequireRange     hcl.Range `hcl:"require,attr_value_range"`
	RequireTags      *bool     `hcl:"require_tags,optional"`
	MaxBodyBytes     *int64    `hcl:"max_body_bytes,optional"`
	MaxResponseBytes *int64    `hcl:"max_response_bytes,optional"`
	DefRange         hcl.Range `hcl:",def_range"`
}

type scimHCL struct {
	TokenFile    string         `hcl:"token_file"`
	BaseURL      string         `hcl:"base_url"`
	BaseURLRange hcl.Range      `hcl:"base_url,attr_value_range"`
	Groups       []scimGroupHCL `hcl:"group,block"`
}

type scimGroupHCL struct {
	Name      string    `hcl:"name,label"`
	Team      string    `hcl:"team"`
	TeamRange hcl.Range `hcl:"team,attr_value_range"`
	Role      *string   `hcl:"role,optional"`
	DefRange  hcl.Range `hcl:",def_range"`
}

type haHCL struct {
	ReplicaID           *string   `hcl:"replica_id,optional"`
	ReplicaIDRange      hcl.Range `hcl:"replica_id,attr_value_range"`
	Listen              string    `hcl:"listen"`
	ListenRange         hcl.Range `hcl:"listen,attr_value_range"`
	Advertise           *string   `hcl:"advertise,optional"`
	AdvertiseRange      hcl.Range `hcl:"advertise,attr_value_range"`
	PeerNetworks        []string  `hcl:"peer_networks"`
	PeerNetworksRange   hcl.Range `hcl:"peer_networks,attr_value_range"`
	SecretFile          string    `hcl:"secret_file"`
	SecretFileRange     hcl.Range `hcl:"secret_file,attr_value_range"`
	LeaseTTL            *string   `hcl:"lease_ttl,optional"`
	LeaseTTLRange       hcl.Range `hcl:"lease_ttl,attr_value_range"`
	Heartbeat           *string   `hcl:"heartbeat,optional"`
	HeartbeatRange      hcl.Range `hcl:"heartbeat,attr_value_range"`
	ForwardTimeout      *string   `hcl:"forward_timeout,optional"`
	ForwardTimeoutRange hcl.Range `hcl:"forward_timeout,attr_value_range"`
	UsagePersist        *string   `hcl:"usage_persist,optional"`
	UsagePersistRange   hcl.Range `hcl:"usage_persist,attr_value_range"`
	OnUnreachable       *string   `hcl:"on_owner_unreachable,optional"`
	OnUnreachableRange  hcl.Range `hcl:"on_owner_unreachable,attr_value_range"`
	DefRange            hcl.Range `hcl:",def_range"`
}

// HA defaults.
const (
	DefaultLeaseTTL       = 6 * time.Second
	DefaultHeartbeat      = time.Second
	DefaultForwardTimeout = 250 * time.Millisecond
	DefaultUsagePersist   = 10 * time.Second
)

// buildHA validates the ha block: PostgreSQL is the only shared component,
// so it needs the postgres store; the advertised address is an IP literal
// inside peer_networks (an owner's address is read from the database and
// dialled only there); the lease outlives three heartbeats and a forward
// timeout fits in a heartbeat.
func (v *validator) buildHA(h *haHCL, c *Config) *HA {
	out := &HA{
		ReplicaID: strOr(h.ReplicaID, ""), Listen: h.Listen, SecretFile: h.SecretFile,
		LeaseTTL:           v.dur(h.LeaseTTL, h.LeaseTTLRange, "lease_ttl", DefaultLeaseTTL),
		Heartbeat:          v.dur(h.Heartbeat, h.HeartbeatRange, "heartbeat", DefaultHeartbeat),
		ForwardTimeout:     v.dur(h.ForwardTimeout, h.ForwardTimeoutRange, "forward_timeout", DefaultForwardTimeout),
		UsagePersist:       v.dur(h.UsagePersist, h.UsagePersistRange, "usage_persist", DefaultUsagePersist),
		OnOwnerUnreachable: strOr(h.OnUnreachable, "lowest_priority"),
	}
	out.Advertise = strOr(h.Advertise, h.Listen)
	if out.ReplicaID != "" && !ValidName(out.ReplicaID) {
		v.errf(h.ReplicaIDRange, "Invalid replica_id", "replica_id %q: want [A-Za-z0-9_.-]{1,64}", out.ReplicaID)
	}
	if c.Store.Kind != "postgres" {
		v.errf(h.DefRange, "HA without PostgreSQL", "the ha block shares state through PostgreSQL: set store { kind = \"postgres\" }")
	}
	if _, _, err := net.SplitHostPort(h.Listen); err != nil {
		v.errf(h.ListenRange, "Invalid listen", "listen %q: want host:port", h.Listen)
	}
	if h.Listen == c.Listen {
		v.errf(h.ListenRange, "Internal listener on the client one", "ha listen %q must differ from the client listener", h.Listen)
	}
	if h.SecretFile == "" {
		v.errf(h.SecretFileRange, "Missing secret_file", "the ha block needs secret_file (a shared secret of at least 32 bytes)")
	}
	out.PeerNetworks = v.prefixes(h.PeerNetworks, h.PeerNetworksRange, "peer_networks")
	if len(h.PeerNetworks) == 0 {
		v.errf(h.PeerNetworksRange, "Missing peer_networks", "peer_networks must list the networks the replicas live in")
	}
	r := h.AdvertiseRange
	if h.Advertise == nil {
		r = h.ListenRange
	}
	ap, err := netip.ParseAddrPort(out.Advertise)
	switch {
	case err != nil || ap.Port() == 0 || ap.Addr().IsUnspecified():
		v.errf(r, "Invalid advertise address", "advertise %q: want the IP:port other replicas reach this one at (an IP literal, not 0.0.0.0)", out.Advertise)
	default:
		in := false
		for _, p := range out.PeerNetworks {
			in = in || p.Contains(ap.Addr().Unmap())
		}
		if !in && len(out.PeerNetworks) > 0 {
			v.errf(r, "Advertise address outside peer_networks", "advertise %q is in none of peer_networks: the other replicas would refuse to dial it", out.Advertise)
		}
	}
	if out.LeaseTTL < time.Second || out.LeaseTTL > 5*time.Minute {
		v.errf(h.LeaseTTLRange, "Invalid lease_ttl", "lease_ttl %v: between 1s and 5m", out.LeaseTTL)
	}
	if out.Heartbeat*3 > out.LeaseTTL {
		v.errf(h.HeartbeatRange, "Invalid heartbeat", "heartbeat %v: at most a third of lease_ttl (%v)", out.Heartbeat, out.LeaseTTL)
	}
	if out.ForwardTimeout > out.Heartbeat || out.ForwardTimeout > 5*time.Second {
		v.errf(h.ForwardTimeoutRange, "Invalid forward_timeout", "forward_timeout %v: at most the heartbeat (%v) and 5s", out.ForwardTimeout, out.Heartbeat)
	}
	if out.UsagePersist < out.Heartbeat || out.UsagePersist > time.Hour {
		v.errf(h.UsagePersistRange, "Invalid usage_persist", "usage_persist %v: between the heartbeat and 1h", out.UsagePersist)
	}
	switch out.OnOwnerUnreachable {
	case "lowest_priority", "reject":
	default:
		v.errf(h.OnUnreachableRange, "Invalid on_owner_unreachable", "on_owner_unreachable %q: want \"lowest_priority\" or \"reject\"", out.OnOwnerUnreachable)
	}
	return out
}

type rotHCL struct {
	VaultPrefix        string    `hcl:"vault_prefix"`
	VaultPrefixRange   hcl.Range `hcl:"vault_prefix,attr_value_range"`
	CheckInterval      *string   `hcl:"check_interval,optional"`
	CheckIntervalRange hcl.Range `hcl:"check_interval,attr_value_range"`
}

type ssoHCL struct {
	Issuer           string       `hcl:"issuer"`
	IssuerRange      hcl.Range    `hcl:"issuer,attr_value_range"`
	ClientID         string       `hcl:"client_id"`
	ClientIDRange    hcl.Range    `hcl:"client_id,attr_value_range"`
	ClientSecretFile *string      `hcl:"client_secret_file,optional"`
	RedirectURL      string       `hcl:"redirect_url"`
	RedirectRange    hcl.Range    `hcl:"redirect_url,attr_value_range"`
	Scopes           []string     `hcl:"scopes,optional"`
	ScopesRange      hcl.Range    `hcl:"scopes,attr_value_range"`
	SessionTTL       *string      `hcl:"session_ttl,optional"`
	SessionTTLRange  hcl.Range    `hcl:"session_ttl,attr_value_range"`
	MinRefresh       *string      `hcl:"min_refresh,optional"`
	MinRefreshRange  hcl.Range    `hcl:"min_refresh,attr_value_range"`
	Roles            []ssoRoleHCL `hcl:"role,block"`
}

type ssoRoleHCL struct {
	Claim     *string   `hcl:"claim,optional"`
	Value     string    `hcl:"value"`
	Role      string    `hcl:"role"`
	Org       *string   `hcl:"org,optional"`
	OrgRange  hcl.Range `hcl:"org,attr_value_range"`
	Team      *string   `hcl:"team,optional"`
	TeamRange hcl.Range `hcl:"team,attr_value_range"`
	DefRange  hcl.Range `hcl:",def_range"`
}

// validRoleScope mirrors the store's binding rules: proxy roles have no
// scope, org_admin an org, team roles a team, viewer one of the two.
func validRoleScope(role, org, team string) bool {
	switch role {
	case "proxy_admin", "proxy_viewer":
		return org == "" && team == ""
	case "org_admin":
		return org != "" && team == "" && ValidName(org)
	case "team_admin", "member":
		return org == "" && team != "" && ValidName(team)
	case "viewer":
		return (org == "") != (team == "") && ValidName(org+team)
	}
	return false
}

type jwtHCL struct {
	Issuer          string      `hcl:"issuer"`
	IssuerRange     hcl.Range   `hcl:"issuer,attr_value_range"`
	Audience        string      `hcl:"audience"`
	AudienceRange   hcl.Range   `hcl:"audience,attr_value_range"`
	JWKSURL         *string     `hcl:"jwks_url,optional"`
	JWKSURLRange    hcl.Range   `hcl:"jwks_url,attr_value_range"`
	Type            *string     `hcl:"type,optional"`
	UsernameClaim   *string     `hcl:"username_claim,optional"`
	GroupsClaim     *string     `hcl:"groups_claim,optional"`
	ClockSkew       *string     `hcl:"clock_skew,optional"`
	ClockSkewRange  hcl.Range   `hcl:"clock_skew,attr_value_range"`
	MinRefresh      *string     `hcl:"min_refresh,optional"`
	MinRefreshRange hcl.Range   `hcl:"min_refresh,attr_value_range"`
	RPM             *int        `hcl:"rpm,optional"`
	TPM             *int        `hcl:"tpm,optional"`
	MaxInFlight     *int        `hcl:"max_inflight,optional"`
	LimitsRange     hcl.Range   `hcl:",def_range"`
	Maps            []jwtMapHCL `hcl:"map,block"`
}

type jwtMapHCL struct {
	Claim     *string   `hcl:"claim,optional"`
	Value     string    `hcl:"value"`
	Team      string    `hcl:"team"`
	TeamRange hcl.Range `hcl:"team,attr_value_range"`
	Project   *string   `hcl:"project,optional"`
	DefRange  hcl.Range `hcl:",def_range"`
}

// validIssuer accepts https URLs (http only to loopback, for tests).
func validIssuer(raw string) bool {
	return secret.ValidateAddress(raw) == nil && !strings.ContainsAny(raw, "?#")
}

type routeHCL struct {
	Allow      []string  `hcl:"allow"`
	AllowRange hcl.Range `hcl:"allow,attr_value_range"`
}

type routesHCL struct {
	Inference *routeHCL `hcl:"inference,block"`
	Admin     *routeHCL `hcl:"admin,block"`
	Metrics   *routeHCL `hcl:"metrics,block"`
	Healthz   *routeHCL `hcl:"healthz,block"`
}

// prefixes parses a CIDR list, reporting at r.
func (v *validator) prefixes(list []string, r hcl.Range, what string) []netip.Prefix {
	if len(list) > 1024 {
		v.errf(r, "Too many prefixes", "%s: at most 1024", what)
		return nil
	}
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			v.errf(r, "Invalid prefix", "%s: %q is not a CIDR prefix (e.g. 10.0.0.0/8, 2001:db8::/32)", what, s)
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

type vaultHCL struct {
	Address       string    `hcl:"address"`
	AddressRange  hcl.Range `hcl:"address,attr_value_range"`
	TokenFile     string    `hcl:"token_file"`
	TokenRange    hcl.Range `hcl:"token_file,attr_value_range"`
	CAFile        *string   `hcl:"ca_file,optional"`
	Namespace     *string   `hcl:"namespace,optional"`
	CacheTTL      *string   `hcl:"cache_ttl,optional"`
	CacheTTLRange hcl.Range `hcl:"cache_ttl,attr_value_range"`
}

type storeHCL struct {
	Kind       *string   `hcl:"kind,optional"`
	KindRange  hcl.Range `hcl:"kind,attr_value_range"`
	DSNFile    *string   `hcl:"dsn_file,optional"`
	FailClosed *bool     `hcl:"fail_closed,optional"`
	QueueSize  *int      `hcl:"queue_size,optional"`
	QueueRange hcl.Range `hcl:"queue_size,attr_value_range"`
	BatchSize  *int      `hcl:"batch_size,optional"`
	BatchRange hcl.Range `hcl:"batch_size,attr_value_range"`
	SpoolDir   *string   `hcl:"spool_dir,optional"`
	SpoolMax   *int64    `hcl:"spool_max_bytes,optional"`
	SpoolRange hcl.Range `hcl:"spool_max_bytes,attr_value_range"`
	DefRange   hcl.Range `hcl:",def_range"`
}

type limitsHCL struct {
	MaxBodyBytes               *int64    `hcl:"max_body_bytes,optional"`
	MaxBodyBytesRange          hcl.Range `hcl:"max_body_bytes,attr_value_range"`
	MaxHeaderBytes             *int      `hcl:"max_header_bytes,optional"`
	MaxHeaderBytesRange        hcl.Range `hcl:"max_header_bytes,attr_value_range"`
	ReadHeaderTimeout          *string   `hcl:"read_header_timeout,optional"`
	ReadHeaderTimeoutRange     hcl.Range `hcl:"read_header_timeout,attr_value_range"`
	ReadTimeout                *string   `hcl:"read_timeout,optional"`
	ReadTimeoutRange           hcl.Range `hcl:"read_timeout,attr_value_range"`
	IdleTimeout                *string   `hcl:"idle_timeout,optional"`
	IdleTimeoutRange           hcl.Range `hcl:"idle_timeout,attr_value_range"`
	UpstreamHeaderTimeout      *string   `hcl:"upstream_header_timeout,optional"`
	UpstreamHeaderTimeoutRange hcl.Range `hcl:"upstream_header_timeout,attr_value_range"`
	MaxRequestDuration         *string   `hcl:"max_request_duration,optional"`
	MaxRequestDurationRange    hcl.Range `hcl:"max_request_duration,attr_value_range"`
	MaxResponseBytes           *int64    `hcl:"max_response_bytes,optional"`
	MaxResponseBytesRange      hcl.Range `hcl:"max_response_bytes,attr_value_range"`
	MaxAudioBytes              *int64    `hcl:"max_audio_bytes,optional"`
	MaxAudioBytesRange         hcl.Range `hcl:"max_audio_bytes,attr_value_range"`
}

type fairHCL struct {
	HalfLife      *string   `hcl:"half_life,optional"`
	HalfLifeRange hcl.Range `hcl:"half_life,attr_value_range"`
}

type healthHCL struct {
	Interval      *string   `hcl:"interval,optional"`
	IntervalRange hcl.Range `hcl:"interval,attr_value_range"`
	Timeout       *string   `hcl:"timeout,optional"`
	TimeoutRange  hcl.Range `hcl:"timeout,attr_value_range"`
	Cooldown      *string   `hcl:"cooldown,optional"`
	CooldownRange hcl.Range `hcl:"cooldown,attr_value_range"`
	Path          *string   `hcl:"path,optional"`
	PathRange     hcl.Range `hcl:"path,attr_value_range"`
}

type tagsHCL struct {
	Header       string    `hcl:"header"`
	HeaderRange  hcl.Range `hcl:"header,attr_value_range"`
	Allowed      []string  `hcl:"allowed"`
	AllowedRange hcl.Range `hcl:"allowed,attr_value_range"`
}

type alertsHCL struct {
	WebhookURL       *string   `hcl:"webhook_url,optional"`
	WebhookURLRange  hcl.Range `hcl:"webhook_url,attr_value_range"`
	WebhookTokenFile *string   `hcl:"webhook_token_file,optional"`
	SMTP             *smtpHCL  `hcl:"smtp,block"`
	DefRange         hcl.Range `hcl:",def_range"`
}

type smtpHCL struct {
	Server       string    `hcl:"server"`
	ServerRange  hcl.Range `hcl:"server,attr_value_range"`
	From         string    `hcl:"from"`
	FromRange    hcl.Range `hcl:"from,attr_value_range"`
	To           []string  `hcl:"to"`
	ToRange      hcl.Range `hcl:"to,attr_value_range"`
	Username     *string   `hcl:"username,optional"`
	PasswordFile *string   `hcl:"password_file,optional"`
	DefRange     hcl.Range `hcl:",def_range"`
}

type orgHCL struct {
	Name       string       `hcl:"name,label"`
	NameRange  hcl.Range    `hcl:"name,label_range"`
	Share      float64      `hcl:"share"`
	ShareRange hcl.Range    `hcl:"share,attr_value_range"`
	Teams      []orgTeamHCL `hcl:"team,block"`
	DefRange   hcl.Range    `hcl:",def_range"`
}

type orgTeamHCL struct {
	Name      string    `hcl:"name,label"`
	NameRange hcl.Range `hcl:"name,label_range"`
	Lab       *string   `hcl:"lab,optional"`
	LabRange  hcl.Range `hcl:"lab,attr_value_range"`
	DefRange  hcl.Range `hcl:",def_range"`
}

type attributionHCL struct {
	ProjectHeader  *string   `hcl:"project_header,optional"`
	HeaderRange    hcl.Range `hcl:"project_header,attr_value_range"`
	DefaultProject *string   `hcl:"default_project,optional"`
	DefaultRange   hcl.Range `hcl:"default_project,attr_value_range"`
}

type slurmHCL struct {
	SreportFile   *string           `hcl:"sreport_file,optional"`
	FileRange     hcl.Range         `hcl:"sreport_file,attr_value_range"`
	Accounts      map[string]string `hcl:"accounts,optional"`
	AccountsRange hcl.Range         `hcl:"accounts,attr_value_range"`
}

type poolHCL struct {
	Name          string    `hcl:"name,label"`
	NameRange     hcl.Range `hcl:"name,label_range"`
	Capacity      int       `hcl:"capacity"`
	CapacityRange hcl.Range `hcl:"capacity,attr_value_range"`
	DefRange      hcl.Range `hcl:",def_range"`
}

type costHCL struct {
	InputPerMTok  *float64 `hcl:"input_per_mtok,optional"`
	OutputPerMTok *float64 `hcl:"output_per_mtok,optional"`
	CachedPerMTok *float64 `hcl:"cached_per_mtok,optional"`
	AudioPerHour  *float64 `hcl:"audio_per_hour,optional"`
	// Prompt-cache write prices and the long-context tier of an external
	// provider (external.go).
	CacheWrite5m   *float64        `hcl:"cache_write_5m_per_mtok,optional"`
	CacheWrite1h   *float64        `hcl:"cache_write_1h_per_mtok,optional"`
	LongContext    *longContextHCL `hcl:"long_context,block"`
	Unit           *string         `hcl:"unit,optional"`
	UnitRange      hcl.Range       `hcl:"unit,attr_value_range"`
	PriceDate      *string         `hcl:"price_date,optional"`
	PriceDateRange hcl.Range       `hcl:"price_date,attr_value_range"`
	DefRange       hcl.Range       `hcl:",def_range"`
}

type upstreamHCL struct {
	Name         string    `hcl:"name,label"`
	NameRange    hcl.Range `hcl:"name,label_range"`
	BaseURL      string    `hcl:"base_url"`
	BaseURLRange hcl.Range `hcl:"base_url,attr_value_range"`
	KeyFile      *string   `hcl:"key_file,optional"`
	DefRange     hcl.Range `hcl:",def_range"`
}

type modelHCL struct {
	Name               string        `hcl:"name,label"`
	NameRange          hcl.Range     `hcl:"name,label_range"`
	UpstreamModel      *string       `hcl:"upstream_model,optional"`
	UpstreamModelRange hcl.Range     `hcl:"upstream_model,attr_value_range"`
	Pool               *string       `hcl:"pool,optional"`
	PoolRange          hcl.Range     `hcl:"pool,attr_value_range"`
	Class              *string       `hcl:"class,optional"`
	ClassRange         hcl.Range     `hcl:"class,attr_value_range"`
	KeyFile            *string       `hcl:"key_file,optional"`
	Fallbacks          []string      `hcl:"fallbacks,optional"`
	FallbacksRange     hcl.Range     `hcl:"fallbacks,attr_value_range"`
	FallbackOn         []string      `hcl:"fallback_on,optional"`
	FallbackOnRange    hcl.Range     `hcl:"fallback_on,attr_value_range"`
	SatWaiting         *int          `hcl:"saturation_waiting,optional"`
	SatWaitingRange    hcl.Range     `hcl:"saturation_waiting,attr_value_range"`
	Cost               *costHCL      `hcl:"cost,block"`
	Upstreams          []upstreamHCL `hcl:"upstream,block"`
	// An external provider (external.go).
	Provider          *string     `hcl:"provider,optional"`
	ProviderRange     hcl.Range   `hcl:"provider,attr_value_range"`
	AllowExternal     *bool       `hcl:"allow_external,optional"`
	Orgs              []string    `hcl:"orgs,optional"`
	OrgsRange         hcl.Range   `hcl:"orgs,attr_value_range"`
	Teams             []string    `hcl:"teams,optional"`
	TeamsRange        hcl.Range   `hcl:"teams,attr_value_range"`
	AnthropicVersions []string    `hcl:"anthropic_versions,optional"`
	VersionsRange     hcl.Range   `hcl:"anthropic_versions,attr_value_range"`
	AnthropicBetas    []string    `hcl:"anthropic_betas,optional"`
	BetasRange        hcl.Range   `hcl:"anthropic_betas,attr_value_range"`
	OrgKeys           []orgKeyHCL `hcl:"org_key,block"`
	DefRange          hcl.Range   `hcl:",def_range"`
}

// Error is a configuration error: HCL diagnostics with file:line:column
// ranges, from the parser, the decoder or the validation below.
type Error struct {
	Diags hcl.Diagnostics
}

func (e *Error) Error() string {
	var b strings.Builder
	n := 0
	for _, d := range e.Diags {
		if d.Severity != hcl.DiagError {
			continue
		}
		if n > 0 {
			b.WriteByte('\n')
		}
		n++
		if d.Subject != nil {
			fmt.Fprintf(&b, "%s:%d:%d: ", d.Subject.Filename, d.Subject.Start.Line, d.Subject.Start.Column)
		}
		b.WriteString(d.Summary)
		if d.Detail != "" {
			b.WriteString(": ")
			b.WriteString(d.Detail)
		}
	}
	return b.String()
}

// Parse decodes and validates an HCL document. filename is used in
// diagnostics only.
//
// The evaluation context is nil: no variables and no functions exist, so
// an expression such as env.HOME, file("/etc/shadow") or "${x}" is an
// error. Quoted strings remain HCL templates: a literal "${" or "%{" must
// be written "$${" or "%%{".
func Parse(src []byte, filename string) (*Config, error) {
	f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, &Error{diags}
	}
	var raw fileHCL
	diags = append(diags, gohcl.DecodeBody(f.Body, nil, &raw)...)
	if diags.HasErrors() {
		return nil, &Error{diags}
	}
	c, vdiags := build(&raw)
	diags = append(diags, vdiags...)
	if diags.HasErrors() {
		return nil, &Error{diags}
	}
	return c, nil
}

type validator struct{ diags hcl.Diagnostics }

func (v *validator) errf(r hcl.Range, summary, format string, a ...any) {
	rr := r
	v.diags = append(v.diags, &hcl.Diagnostic{
		Severity: hcl.DiagError, Summary: summary, Detail: fmt.Sprintf(format, a...), Subject: &rr,
	})
}

func (v *validator) dur(p *string, r hcl.Range, name string, def time.Duration) time.Duration {
	if p == nil {
		return def
	}
	d, err := time.ParseDuration(*p)
	if err != nil || d <= 0 {
		v.errf(r, "Invalid duration", "%s = %q: want a positive Go duration such as \"30s\" or \"168h\"", name, *p)
		return def
	}
	return d
}

// known reports a reference to an organisation or team that no org block
// declares. A misspelt name must not silently disable what it configures
// (a request log, a policy, a role), so every reference is checked.
func (v *validator) known(declared map[string]bool, name string, r hcl.Range, kind, where string) {
	if !declared[name] {
		v.errf(r, "Unknown "+kind, "%s names %s %q, which no org block declares", where, kind, name)
	}
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

func build(raw *fileHCL) (*Config, hcl.Diagnostics) {
	v := &validator{}
	c := &Config{
		Listen:            strOr(raw.Listen, DefaultListen),
		MasterKeyFile:     raw.MasterKeyFile,
		MetricsTokenFile:  strOr(raw.MetricsTokenFile, ""),
		BudgetRefresh:     v.dur(raw.BudgetRefresh, raw.BudgetRefreshRange, "budget_refresh", DefaultBudgetRefresh),
		KeyCacheTTL:       v.dur(raw.KeyCacheTTL, raw.KeyCacheTTLRange, "key_cache_ttl", DefaultKeyCacheTTL),
		ShutdownGrace:     v.dur(raw.ShutdownGrace, raw.ShutdownGraceRange, "shutdown_grace", DefaultShutdownGrace),
		ExposeOverheadHdr: raw.ExposeOverheadHeader != nil && *raw.ExposeOverheadHeader,
		Retries:           DefaultRetries,
	}
	if c.MasterKeyFile == "" {
		v.errf(raw.MasterKeyFileRange, "Missing master key file", "master_key_file must name a file")
	}
	c.CacheIsolation = strOr(raw.CacheIsolation, "org")
	c.CacheSaltKeyFile = strOr(raw.CacheSaltKeyFile, "")
	switch c.CacheIsolation {
	case "org", "team", "key":
		if c.CacheSaltKeyFile == "" {
			r := raw.CacheIsolationRange
			if raw.CacheIsolation == nil {
				r = raw.MasterKeyFileRange
			}
			v.errf(r, "Missing cache salt key", "cache_isolation = %q needs cache_salt_key_file (a file holding a random secret): "+
				"without it tenants share vLLM's prefix cache, a timing side channel", c.CacheIsolation)
		}
	case "none":
	default:
		v.errf(raw.CacheIsolationRange, "Invalid cache isolation", "cache_isolation = %q: want \"org\", \"team\", \"key\" or \"none\"", c.CacheIsolation)
	}
	c.AuthFailuresPerMinute = DefaultAuthFailuresPerMinute
	if raw.AuthFailures != nil {
		c.AuthFailuresPerMinute = *raw.AuthFailures
		if c.AuthFailuresPerMinute < 0 || c.AuthFailuresPerMinute > 1_000_000 {
			v.errf(raw.AuthFailuresRange, "Invalid limit", "auth_failures_per_minute = %d: want 0 (off)..1000000", c.AuthFailuresPerMinute)
		}
	}
	if raw.Retries != nil {
		c.Retries = *raw.Retries
		if c.Retries < 0 || c.Retries > 10 {
			v.errf(raw.RetriesRange, "Invalid retries", "retries = %d: want 0..10", c.Retries)
		}
	}

	// store
	t := true
	c.Store = Store{Kind: "memory", FailClosed: &t, QueueSize: DefaultQueueSize, BatchSize: DefaultBatchSize, SpoolMaxBytes: DefaultSpoolMaxBytes}
	if s := raw.Store; s != nil {
		c.Store.Kind = strOr(s.Kind, "memory")
		c.Store.DSNFile = strOr(s.DSNFile, "")
		c.Store.SpoolDir = strOr(s.SpoolDir, "")
		if s.SpoolMax != nil {
			c.Store.SpoolMaxBytes = *s.SpoolMax
			if c.Store.SpoolMaxBytes < 1<<20 || c.Store.SpoolMaxBytes > 64<<30 {
				v.errf(s.SpoolRange, "Invalid spool size", "spool_max_bytes = %d: want 1 MiB..64 GiB", c.Store.SpoolMaxBytes)
			}
		}
		if s.FailClosed != nil {
			fc := *s.FailClosed
			c.Store.FailClosed = &fc
		}
		if s.QueueSize != nil {
			c.Store.QueueSize = *s.QueueSize
			if c.Store.QueueSize < 1 || c.Store.QueueSize > 10_000_000 {
				v.errf(s.QueueRange, "Invalid queue size", "queue_size = %d: want 1..10000000", c.Store.QueueSize)
			}
		}
		if s.BatchSize != nil {
			c.Store.BatchSize = *s.BatchSize
			if c.Store.BatchSize < 1 || c.Store.BatchSize > 100_000 {
				v.errf(s.BatchRange, "Invalid batch size", "batch_size = %d: want 1..100000", c.Store.BatchSize)
			}
		}
		switch c.Store.Kind {
		case "memory":
		case "postgres":
			if c.Store.DSNFile == "" {
				v.errf(s.DefRange, "Missing DSN file", "store kind \"postgres\" needs dsn_file")
			}
			// Usage is billed between institutions: it must survive a store
			// outage and a crash (README, accounting spool).
			if c.Store.SpoolDir == "" {
				v.errf(s.DefRange, "Missing spool directory", "store kind \"postgres\" needs spool_dir (the durable accounting spool)")
			}
		default:
			v.errf(s.KindRange, "Invalid store kind", "kind = %q: want \"memory\" or \"postgres\"", c.Store.Kind)
		}
	}

	// limits
	c.Limits = Limits{
		MaxBodyBytes: DefaultMaxBodyBytes, MaxHeaderBytes: DefaultMaxHeaderBytes,
		ReadHeaderTimeout: DefaultReadHeaderTimeout, ReadTimeout: DefaultReadTimeout, IdleTimeout: DefaultIdleTimeout,
		UpstreamHeaderTimeout: DefaultUpstreamHeaderTimeout, MaxRequestDuration: DefaultMaxRequestDuration,
		MaxResponseBytes: DefaultMaxResponseBytes, MaxAudioBytes: DefaultMaxAudioBytes,
	}
	if l := raw.Limits; l != nil {
		if l.MaxAudioBytes != nil {
			c.Limits.MaxAudioBytes = *l.MaxAudioBytes
			if c.Limits.MaxAudioBytes <= 0 || c.Limits.MaxAudioBytes > 1<<30 {
				v.errf(l.MaxAudioBytesRange, "Invalid audio limit", "max_audio_bytes = %d: want 1..1073741824", c.Limits.MaxAudioBytes)
			}
		}
		if l.MaxBodyBytes != nil {
			c.Limits.MaxBodyBytes = *l.MaxBodyBytes
			if c.Limits.MaxBodyBytes <= 0 || c.Limits.MaxBodyBytes > 1<<30 {
				v.errf(l.MaxBodyBytesRange, "Invalid body limit", "max_body_bytes = %d: want 1..1073741824", c.Limits.MaxBodyBytes)
			}
		}
		if l.MaxHeaderBytes != nil {
			c.Limits.MaxHeaderBytes = *l.MaxHeaderBytes
			if c.Limits.MaxHeaderBytes < 1024 || c.Limits.MaxHeaderBytes > 1<<20 {
				v.errf(l.MaxHeaderBytesRange, "Invalid header limit", "max_header_bytes = %d: want 1024..1048576", c.Limits.MaxHeaderBytes)
			}
		}
		if l.MaxResponseBytes != nil {
			c.Limits.MaxResponseBytes = *l.MaxResponseBytes
			if c.Limits.MaxResponseBytes <= 0 || c.Limits.MaxResponseBytes > 1<<32 {
				v.errf(l.MaxResponseBytesRange, "Invalid response limit", "max_response_bytes = %d: want 1..4294967296", c.Limits.MaxResponseBytes)
			}
		}
		c.Limits.ReadHeaderTimeout = v.dur(l.ReadHeaderTimeout, l.ReadHeaderTimeoutRange, "read_header_timeout", DefaultReadHeaderTimeout)
		c.Limits.ReadTimeout = v.dur(l.ReadTimeout, l.ReadTimeoutRange, "read_timeout", DefaultReadTimeout)
		c.Limits.IdleTimeout = v.dur(l.IdleTimeout, l.IdleTimeoutRange, "idle_timeout", DefaultIdleTimeout)
		c.Limits.UpstreamHeaderTimeout = v.dur(l.UpstreamHeaderTimeout, l.UpstreamHeaderTimeoutRange, "upstream_header_timeout", DefaultUpstreamHeaderTimeout)
		c.Limits.MaxRequestDuration = v.dur(l.MaxRequestDuration, l.MaxRequestDurationRange, "max_request_duration", DefaultMaxRequestDuration)
	}

	c.FairShare.HalfLife = DefaultHalfLife
	if f := raw.FairShare; f != nil {
		c.FairShare.HalfLife = v.dur(f.HalfLife, f.HalfLifeRange, "half_life", DefaultHalfLife)
	}

	c.Health = Health{Interval: DefaultHealthInterval, Timeout: DefaultHealthTimeout, Cooldown: DefaultCooldown, Path: DefaultHealthPath}
	if h := raw.Health; h != nil {
		c.Health.Interval = v.dur(h.Interval, h.IntervalRange, "interval", DefaultHealthInterval)
		c.Health.Timeout = v.dur(h.Timeout, h.TimeoutRange, "timeout", DefaultHealthTimeout)
		c.Health.Cooldown = v.dur(h.Cooldown, h.CooldownRange, "cooldown", DefaultCooldown)
		c.Health.Path = strOr(h.Path, DefaultHealthPath)
		if !strings.HasPrefix(c.Health.Path, "/") || strings.ContainsAny(c.Health.Path, "?#\r\n ") {
			v.errf(h.PathRange, "Invalid health path", "path = %q: want an absolute path without query", c.Health.Path)
		}
	}

	// organisations and their teams
	orgs, teams := map[string]bool{}, map[string]bool{}
	sum := 0.0
	for _, o := range raw.Orgs {
		if !ValidName(o.Name) {
			v.errf(o.NameRange, "Invalid organisation name", "%q: want [A-Za-z0-9_.-]{1,64}", o.Name)
		}
		if orgs[o.Name] {
			v.errf(o.DefRange, "Duplicate organisation", "org %q is declared twice", o.Name)
		}
		orgs[o.Name] = true
		if !(o.Share >= 0 && o.Share <= 1) {
			v.errf(o.ShareRange, "Invalid share", "org %q share = %v: want 0..1", o.Name, o.Share)
		}
		sum += o.Share
		org := Org{Name: o.Name, Share: o.Share}
		for _, t := range o.Teams {
			if !ValidName(t.Name) {
				v.errf(t.NameRange, "Invalid team name", "%q: want [A-Za-z0-9_.-]{1,64}", t.Name)
			}
			if teams[t.Name] {
				v.errf(t.DefRange, "Duplicate team", "team %q is declared twice (team names are global)", t.Name)
			}
			teams[t.Name] = true
			org.Teams = append(org.Teams, t.Name)
			if t.Lab != nil {
				if !ValidName(*t.Lab) {
					v.errf(t.LabRange, "Invalid lab name", "team %q lab %q: want [A-Za-z0-9_.-]{1,64}", t.Name, *t.Lab)
				}
				if org.Labs == nil {
					org.Labs = map[string]string{}
				}
				org.Labs[t.Name] = *t.Lab
			}
		}
		c.Orgs = append(c.Orgs, org)
	}
	if sum > 1+1e-9 {
		r := hcl.Range{}
		if n := len(raw.Orgs); n > 0 {
			r = raw.Orgs[n-1].ShareRange
		}
		v.errf(r, "Shares exceed the pool", "organisation shares sum to %v: want at most 1", sum)
	}

	// pools
	pools := map[string]bool{}
	for _, p := range raw.Pools {
		if !ValidName(p.Name) {
			v.errf(p.NameRange, "Invalid pool name", "%q: want [A-Za-z0-9_.-]{1,64}", p.Name)
		}
		if pools[p.Name] {
			v.errf(p.DefRange, "Duplicate pool", "pool %q is declared twice", p.Name)
		}
		pools[p.Name] = true
		if p.Capacity <= 0 || p.Capacity > 1<<20 {
			v.errf(p.CapacityRange, "Invalid capacity", "pool %q capacity = %d: want 1..1048576", p.Name, p.Capacity)
		}
		c.Pools = append(c.Pools, Pool{Name: p.Name, Capacity: p.Capacity})
	}

	// models
	models := map[string]bool{}
	modelClass := map[string]Class{}
	poolUnit := map[string]string{}
	for _, m := range raw.Models {
		g := ModelGroup{Name: m.Name, UpstreamModel: strOr(m.UpstreamModel, m.Name), Pool: strOr(m.Pool, m.Name), Class: Class(strOr(m.Class, string(Interactive)))}
		if m.Name == "" || len(m.Name) > 256 || strings.ContainsAny(m.Name, "\x00\r\n") {
			v.errf(m.NameRange, "Invalid model name", "%q: want 1..256 bytes without control characters", m.Name)
		}
		if models[m.Name] {
			v.errf(m.DefRange, "Duplicate model", "model %q is declared twice", m.Name)
		}
		models[m.Name] = true
		modelClass[m.Name] = g.Class
		if g.UpstreamModel == "" || strings.ContainsAny(g.UpstreamModel, "\x00\r\n") {
			v.errf(m.UpstreamModelRange, "Invalid upstream model", "%q", g.UpstreamModel)
		}
		if !pools[g.Pool] {
			r := m.PoolRange
			if m.Pool == nil {
				r = m.DefRange
			}
			v.errf(r, "Unknown pool", "model %q uses pool %q, which is not declared", m.Name, g.Pool)
		}
		if g.Class != Interactive && g.Class != Batch && g.Class != Guard {
			v.errf(m.ClassRange, "Invalid class", "class = %q: want \"interactive\", \"batch\" or \"guard\"", g.Class)
		}
		if m.Cost != nil {
			for _, x := range []*float64{m.Cost.InputPerMTok, m.Cost.OutputPerMTok, m.Cost.CachedPerMTok, m.Cost.AudioPerHour} {
				if x != nil && !(*x >= 0 && *x < math.Inf(1)) {
					v.errf(m.Cost.DefRange, "Invalid cost", "costs must be finite and non-negative")
					break
				}
			}
			g.Cost = Cost{InputPerMTok: f64(m.Cost.InputPerMTok), OutputPerMTok: f64(m.Cost.OutputPerMTok), CachedPerMTok: f64(m.Cost.CachedPerMTok),
				AudioPerHour: f64(m.Cost.AudioPerHour), PriceDate: strOr(m.Cost.PriceDate, "")}
			unit, ok := normalizeUnit(strOr(m.Cost.Unit, ""))
			if !ok {
				v.errf(m.Cost.UnitRange, "Invalid unit", "unit = %q: want \"gpu_hours\" or a currency code such as \"USD\" (letters, digits, _; at most 16)", strOr(m.Cost.Unit, ""))
			}
			g.Cost.Unit = unit
			if m.Cost.PriceDate != nil {
				if _, err := time.Parse("2006-01-02", *m.Cost.PriceDate); err != nil {
					v.errf(m.Cost.PriceDateRange, "Invalid price date", "price_date = %q: want YYYY-MM-DD, the day the prices were read", *m.Cost.PriceDate)
				}
			} else if unit != "" {
				v.errf(m.Cost.DefRange, "Missing price date", "model %q: a cost in %s needs price_date (YYYY-MM-DD), the day the prices were read from the provider", m.Name, unit)
			}
		}
		if prev, ok := poolUnit[g.Pool]; ok && prev != g.Cost.Unit {
			v.errf(m.DefRange, "Mixed cost units", "pool %q: model %q costs in %s, another model of the pool in %s; fair share sums the usage of a pool, so its models share one unit",
				g.Pool, m.Name, unitName(g.Cost.Unit), unitName(prev))
		}
		poolUnit[g.Pool] = g.Cost.Unit
		if len(m.Upstreams) == 0 {
			v.errf(m.DefRange, "No upstream", "model %q needs at least one upstream block", m.Name)
		}
		names := map[string]bool{}
		for _, u := range m.Upstreams {
			if names[u.Name] {
				v.errf(u.DefRange, "Duplicate upstream", "upstream %q is declared twice in model %q", u.Name, m.Name)
			}
			names[u.Name] = true
			if err := ValidateUpstream(u.BaseURL); err != nil {
				v.errf(u.BaseURLRange, "Invalid base_url", "%v", err)
			}
			key := strOr(u.KeyFile, strOr(m.KeyFile, ""))
			if key == "" && m.Provider == nil { // an external provider's keys: external.go
				v.errf(u.DefRange, "Missing key file", "upstream %q of model %q has no key_file (set it on the upstream or the model)", u.Name, m.Name)
			}
			g.Upstreams = append(g.Upstreams, Upstream{Name: u.Name, BaseURL: u.BaseURL, KeyFile: key})
		}
		c.ModelGroups = append(c.ModelGroups, g)
	}
	// Fallbacks name other model groups, checked once all are known.
	for i, m := range raw.Models {
		if i >= len(c.ModelGroups) {
			continue
		}
		where := "model " + strconv.Quote(m.Name)
		seenOn := map[string]bool{}
		for _, r := range m.FallbackOn {
			if r != "unavailable" && r != "saturated" || seenOn[r] {
				v.errf(m.FallbackOnRange, "Invalid fallback_on", "%s: fallback_on %q: want \"unavailable\" and/or \"saturated\", each once", where, r)
			}
			seenOn[r] = true
		}
		if m.FallbackOnRange.Filename != "" && len(m.FallbackOn) == 0 {
			v.errf(m.FallbackOnRange, "Invalid fallback_on", "%s: an empty fallback_on would never fall back; remove fallbacks instead", where)
		}
		c.ModelGroups[i].FallbackOn = m.FallbackOn
		if m.SatWaiting != nil {
			if *m.SatWaiting < 0 || *m.SatWaiting > 1_000_000 {
				v.errf(m.SatWaitingRange, "Invalid saturation_waiting", "%s: saturation_waiting %d: want 0..1000000", where, *m.SatWaiting)
			}
			if !seenOn["saturated"] {
				v.errf(m.SatWaitingRange, "Invalid saturation_waiting", "%s: saturation_waiting needs fallback_on to include \"saturated\"", where)
			}
			c.ModelGroups[i].SaturationWaiting = *m.SatWaiting
		}
		if len(m.FallbackOn) > 0 && len(m.Fallbacks) == 0 {
			v.errf(m.FallbackOnRange, "fallback_on without fallbacks", "%s: fallback_on names when to fall back, but the model has no fallbacks", where)
		}
		if len(m.Fallbacks) == 0 {
			continue
		}
		if c.ModelGroups[i].Class == Guard {
			v.errf(m.FallbacksRange, "Fallback on a guard model", "%s: a guard model has no fallbacks", where)
		}
		if len(m.Fallbacks) > 4 {
			v.errf(m.FallbacksRange, "Too many fallbacks", "%s: at most 4", where)
		}
		seen := map[string]bool{}
		for _, fb := range m.Fallbacks {
			cl, ok := modelClass[fb]
			switch {
			case !ok:
				v.errf(m.FallbacksRange, "Unknown model", "%s: fallback %q names no model block", where, fb)
			case fb == m.Name || seen[fb]:
				v.errf(m.FallbacksRange, "Invalid fallback", "%s: fallback %q is the model itself or repeated", where, fb)
			case cl == Guard:
				v.errf(m.FallbacksRange, "Invalid fallback", "%s: fallback %q is a guard model", where, fb)
			}
			seen[fb] = true
		}
		c.ModelGroups[i].Fallbacks = m.Fallbacks
	}
	c.Attribution = Attribution{ProjectHeader: DefaultProjectHeader}
	if a := raw.Attribution; a != nil {
		if a.ProjectHeader != nil {
			c.Attribution.ProjectHeader = *a.ProjectHeader
			if !validHeaderName(*a.ProjectHeader) || strings.EqualFold(*a.ProjectHeader, "Authorization") {
				v.errf(a.HeaderRange, "Invalid project_header", "project_header %q: want a header name", *a.ProjectHeader)
			}
		}
		if a.DefaultProject != nil {
			c.Attribution.DefaultProject = *a.DefaultProject
			if !ValidName(*a.DefaultProject) {
				v.errf(a.DefaultRange, "Invalid default_project", "default_project %q: want [A-Za-z0-9_.-]{1,64}", *a.DefaultProject)
			}
		}
	}
	if sl := raw.Slurm; sl != nil {
		if sl.SreportFile != nil {
			c.Slurm.SreportFile = *sl.SreportFile
			if !filepath.IsAbs(*sl.SreportFile) {
				v.errf(sl.FileRange, "Invalid sreport_file", "sreport_file %q: want an absolute path", *sl.SreportFile)
			}
		}
		for acct, org := range sl.Accounts {
			if acct == "" || len(acct) > 64 || !ValidName(org) {
				v.errf(sl.AccountsRange, "Invalid accounts", "accounts: %q = %q: want a Slurm account and an organisation name", acct, org)
			}
		}
		c.Slurm.Accounts = sl.Accounts
	}
	if t := raw.Tags; t != nil {
		if !validHeaderName(t.Header) {
			v.errf(t.HeaderRange, "Invalid header", "header = %q: want an HTTP header name such as \"X-Llmflow-Tags\"", t.Header)
		}
		if len(t.Allowed) == 0 || len(t.Allowed) > 1000 {
			v.errf(t.AllowedRange, "Invalid tags", "allowed must list 1..1000 tags")
		}
		for _, tag := range t.Allowed {
			if !ValidName(tag) {
				v.errf(t.AllowedRange, "Invalid tag", "%q: want [A-Za-z0-9_.-]{1,64}", tag)
			}
		}
		c.Tags = Tags{Header: t.Header, Allowed: t.Allowed}
	}
	if a := raw.Alerts; a != nil {
		c.Alerts.WebhookURL = strOr(a.WebhookURL, "")
		c.Alerts.WebhookTokenFile = strOr(a.WebhookTokenFile, "")
		if c.Alerts.WebhookURL != "" {
			if err := ValidateUpstream(c.Alerts.WebhookURL); err != nil {
				v.errf(a.WebhookURLRange, "Invalid webhook_url", "%v", err)
			}
		}
		if s := a.SMTP; s != nil {
			if _, _, err := net.SplitHostPort(s.Server); err != nil {
				v.errf(s.ServerRange, "Invalid SMTP server", "server = %q: want host:port", s.Server)
			}
			if _, err := mail.ParseAddress(s.From); err != nil || strings.ContainsAny(s.From, "\r\n") {
				v.errf(s.FromRange, "Invalid address", "from = %q", s.From)
			}
			if len(s.To) == 0 {
				v.errf(s.ToRange, "Invalid address", "to must list at least one address")
			}
			for _, to := range s.To {
				if _, err := mail.ParseAddress(to); err != nil || strings.ContainsAny(to, "\r\n") {
					v.errf(s.ToRange, "Invalid address", "to: %q", to)
				}
			}
			c.Alerts.SMTP = &SMTP{Server: s.Server, From: s.From, To: s.To, Username: strOr(s.Username, ""), PasswordFile: strOr(s.PasswordFile, "")}
			if (c.Alerts.SMTP.Username == "") != (c.Alerts.SMTP.PasswordFile == "") {
				v.errf(s.DefRange, "Incomplete SMTP credentials", "username and password_file go together")
			}
		}
		if c.Alerts.WebhookURL == "" && c.Alerts.SMTP == nil {
			v.errf(a.DefRange, "Empty alerts block", "set webhook_url, an smtp block, or both")
		}
	}

	if len(raw.Models) == 0 {
		v.errf(hcl.Range{Filename: raw.MasterKeyFileRange.Filename, Start: hcl.InitialPos, End: hcl.InitialPos},
			"No model", "at least one model block is required")
	}

	if vb := raw.Vault; vb != nil {
		c.Vault = &Vault{Address: vb.Address, TokenFile: vb.TokenFile, CAFile: strOr(vb.CAFile, ""), Namespace: strOr(vb.Namespace, "")}
		if err := secret.ValidateAddress(vb.Address); err != nil {
			v.errf(vb.AddressRange, "Invalid vault address", "%v", err)
		}
		if r, err := secret.Parse(vb.TokenFile); err != nil || r.IsVault() {
			v.errf(vb.TokenRange, "Invalid vault token file", "token_file must be a file, not a vault reference")
		}
		c.Vault.CacheTTL = v.dur(vb.CacheTTL, vb.CacheTTLRange, "cache_ttl", 5*time.Minute)
	}
	c.TrustedProxies = v.prefixes(raw.TrustedProxies, raw.TrustedProxiesRange, "trusted_proxies")
	c.Routes.Metrics, c.Routes.Healthz = LoopbackOnly, LoopbackOnly
	if rt := raw.Routes; rt != nil {
		set := func(h *routeHCL, dst *[]netip.Prefix, what string) {
			if h != nil {
				*dst = v.prefixes(h.Allow, h.AllowRange, what)
				if len(h.Allow) == 0 {
					v.errf(h.AllowRange, "Empty allow list", "%s: an empty list would close the route; remove the block for the default", what)
				}
			}
		}
		set(rt.Inference, &c.Routes.Inference, "routes.inference.allow")
		set(rt.Admin, &c.Routes.Admin, "routes.admin.allow")
		set(rt.Metrics, &c.Routes.Metrics, "routes.metrics.allow")
		set(rt.Healthz, &c.Routes.Healthz, "routes.healthz.allow")
	}
	if j := raw.JWT; j != nil {
		cj := &JWT{Issuer: j.Issuer, Audience: j.Audience, JWKSURL: strOr(j.JWKSURL, ""), Type: strOr(j.Type, ""),
			UsernameClaim: strOr(j.UsernameClaim, ""), GroupsClaim: strOr(j.GroupsClaim, ""),
			ClockSkew:  v.dur(j.ClockSkew, j.ClockSkewRange, "clock_skew", time.Minute),
			MinRefresh: v.dur(j.MinRefresh, j.MinRefreshRange, "min_refresh", time.Minute)}
		if !validIssuer(j.Issuer) {
			v.errf(j.IssuerRange, "Invalid issuer", "issuer %q: want an https URL (it is compared exactly with the token's iss)", j.Issuer)
		}
		if j.Audience == "" {
			v.errf(j.AudienceRange, "Missing audience", "a verifier without an audience accepts every token the issuer signed")
		}
		if cj.JWKSURL != "" && !validIssuer(cj.JWKSURL) {
			v.errf(j.JWKSURLRange, "Invalid jwks_url", "jwks_url %q: want an https URL", cj.JWKSURL)
		}
		if cj.ClockSkew > 10*time.Minute {
			v.errf(j.ClockSkewRange, "Clock skew too large", "clock_skew %v: at most 10m", cj.ClockSkew)
		}
		if cj.MinRefresh < MinKeySetRefresh {
			v.errf(j.MinRefreshRange, "Key set refresh too short", "min_refresh %v: at least %v (it bounds how often forged kids make the gateway ask the issuer)", cj.MinRefresh, MinKeySetRefresh)
		}
		for _, p := range []*int{j.RPM, j.TPM, j.MaxInFlight} {
			if p != nil && *p < 0 {
				v.errf(j.LimitsRange, "Invalid limit", "rpm, tpm and max_inflight must be non-negative")
			}
		}
		if j.RPM != nil {
			cj.RPM = *j.RPM
		}
		if j.TPM != nil {
			cj.TPM = *j.TPM
		}
		if j.MaxInFlight != nil {
			cj.MaxInFlight = *j.MaxInFlight
		}
		if len(j.Maps) == 0 {
			v.errf(j.LimitsRange, "No map", "a jwt block needs at least one map { value, team } block")
		}
		for _, m := range j.Maps {
			cm := JWTMap{Claim: strOr(m.Claim, "groups"), Value: m.Value, Team: m.Team, Project: strOr(m.Project, "")}
			if !ValidName(m.Team) || cm.Project != "" && !ValidName(cm.Project) || m.Value == "" {
				v.errf(m.DefRange, "Invalid map", "map needs a value and a valid team (and project) name")
			} else {
				v.known(teams, m.Team, m.TeamRange, "team", "jwt map "+strconv.Quote(m.Value))
			}
			cj.Maps = append(cj.Maps, cm)
		}
		c.JWT = cj
	}
	if o := raw.SSO; o != nil {
		cs := &SSO{Issuer: o.Issuer, ClientID: o.ClientID, ClientSecretFile: strOr(o.ClientSecretFile, ""),
			RedirectURL: o.RedirectURL, Scopes: o.Scopes,
			SessionTTL: v.dur(o.SessionTTL, o.SessionTTLRange, "session_ttl", 8*time.Hour),
			MinRefresh: v.dur(o.MinRefresh, o.MinRefreshRange, "min_refresh", time.Minute)}
		if cs.MinRefresh < MinKeySetRefresh {
			v.errf(o.MinRefreshRange, "Key set refresh too short", "min_refresh %v: at least %v (it bounds how often forged kids make the gateway ask the provider)", cs.MinRefresh, MinKeySetRefresh)
		}
		if len(cs.Scopes) == 0 {
			cs.Scopes = []string{"openid", "profile", "email"}
		}
		if !validIssuer(o.Issuer) {
			v.errf(o.IssuerRange, "Invalid issuer", "issuer %q: want an https URL", o.Issuer)
		}
		if o.ClientID == "" {
			v.errf(o.ClientIDRange, "Missing client_id", "the client registered at the provider")
		}
		if u, err := url.Parse(o.RedirectURL); err != nil || !validIssuer(o.RedirectURL) || u.Path != "/admin/callback" {
			v.errf(o.RedirectRange, "Invalid redirect_url", "redirect_url %q: want https://<this gateway>/admin/callback", o.RedirectURL)
		}
		if !slices.Contains(cs.Scopes, "openid") {
			v.errf(o.ScopesRange, "Invalid scopes", "scopes must include openid")
		}
		if cs.SessionTTL > 7*24*time.Hour {
			v.errf(o.SessionTTLRange, "Session too long", "session_ttl %v: at most 168h", cs.SessionTTL)
		}
		for _, r := range o.Roles {
			cr := SSORole{Claim: strOr(r.Claim, "groups"), Value: r.Value, Role: r.Role, Org: strOr(r.Org, ""), Team: strOr(r.Team, "")}
			if r.Value == "" || !validRoleScope(cr.Role, cr.Org, cr.Team) {
				v.errf(r.DefRange, "Invalid role", "role %q needs a value and its scope (proxy roles: none; org_admin: org; team_admin, member: team; viewer: org or team)", r.Role)
			} else {
				where := "sso role " + strconv.Quote(r.Value)
				if cr.Org != "" {
					v.known(orgs, cr.Org, r.OrgRange, "organisation", where)
				}
				if cr.Team != "" {
					v.known(teams, cr.Team, r.TeamRange, "team", where)
				}
			}
			cs.Roles = append(cs.Roles, cr)
		}
		c.SSO = cs
	}
	if raw.HA != nil {
		c.HA = v.buildHA(raw.HA, c)
	}
	if kr := raw.KeyRotation; kr != nil {
		ck := &KeyRotation{VaultPrefix: strings.Trim(kr.VaultPrefix, "/"),
			CheckInterval: v.dur(kr.CheckInterval, kr.CheckIntervalRange, "check_interval", time.Hour)}
		if r, err := secret.Parse("vault:" + ck.VaultPrefix + "/t/k#f"); err != nil || !r.IsVault() {
			v.errf(kr.VaultPrefixRange, "Invalid vault_prefix", "vault_prefix %q: want mount/path with [A-Za-z0-9._-] segments", kr.VaultPrefix)
		}
		if c.Vault == nil {
			v.errf(kr.VaultPrefixRange, "Key rotation without vault block", "scheduled rotation delivers new secrets to OpenBao/Vault: add a vault block")
		}
		if ck.CheckInterval < time.Minute {
			v.errf(kr.CheckIntervalRange, "Invalid check_interval", "check_interval %v: at least 1m", ck.CheckInterval)
		}
		c.KeyRotation = ck
	}
	c.Audit.CheckpointInterval = 5 * time.Minute
	if a := raw.Audit; a != nil {
		c.Audit.SigningKeyFile = strOr(a.SigningKeyFile, "")
		c.Audit.CheckpointInterval = v.dur(a.CheckpointInterval, a.IntervalRange, "checkpoint_interval", 5*time.Minute)
		if c.Audit.CheckpointInterval < time.Second || c.Audit.CheckpointInterval > 24*time.Hour {
			v.errf(a.IntervalRange, "Invalid checkpoint_interval", "checkpoint_interval %v: between 1s and 24h", c.Audit.CheckpointInterval)
		}
	}
	exports := map[string]bool{}
	for _, x := range raw.Exports {
		e := Export{Name: x.Name, Kind: x.Kind, Endpoint: strOr(x.Endpoint, ""), Region: strOr(x.Region, "us-east-1"),
			Bucket: strOr(x.Bucket, ""), AccessKeyFile: strOr(x.AccessKeyFile, ""), SecretKeyFile: strOr(x.SecretKeyFile, ""),
			Prefix: strOr(x.Prefix, ""), LockMode: strOr(x.LockMode, "COMPLIANCE"), Directory: strOr(x.Directory, ""),
			Retention: v.dur(x.Retention, x.RetentionRange, "retention", 0)}
		if x.PathStyle != nil {
			e.PathStyle = *x.PathStyle
		}
		e.QueueSize = 1024
		if x.QueueSize != nil {
			e.QueueSize = *x.QueueSize
		}
		if !ValidName(x.Name) || exports[x.Name] {
			v.errf(x.NameRange, "Invalid export name", "export %q: want [A-Za-z0-9_.-]{1,64}, declared once", x.Name)
		}
		exports[x.Name] = true
		if !ValidObjectKey(e.Prefix) {
			v.errf(x.PrefixRange, "Invalid prefix", "export %q: prefix %q: printable ASCII, no \"..\", \"//\", backslash or leading \"/\"", x.Name, e.Prefix)
		}
		if e.QueueSize < 1 || e.QueueSize > 1<<20 {
			v.errf(x.DefRange, "Invalid queue_size", "export %q: queue_size between 1 and 1048576", x.Name)
		}
		if e.Retention < 0 || e.Retention > 100*365*24*time.Hour {
			v.errf(x.RetentionRange, "Invalid retention", "export %q: retention between 0 and 100 years", x.Name)
		}
		switch e.Kind {
		case "s3":
			if err := secret.ValidateAddress(e.Endpoint); err != nil {
				v.errf(x.EndpointRange, "Invalid endpoint", "export %q: %v", x.Name, err)
			}
			if e.Bucket == "" || e.AccessKeyFile == "" || e.SecretKeyFile == "" {
				v.errf(x.DefRange, "Incomplete S3 export", "export %q needs bucket, access_key_file and secret_key_file", x.Name)
			}
			if e.LockMode != "GOVERNANCE" && e.LockMode != "COMPLIANCE" {
				v.errf(x.LockModeRange, "Invalid lock_mode", "export %q: GOVERNANCE or COMPLIANCE", x.Name)
			}
		case "file":
			if !filepath.IsAbs(e.Directory) {
				v.errf(x.DefRange, "Invalid directory", "export %q needs an absolute directory", x.Name)
			}
		case "gcs", "azure":
			// Declared so that a configuration can name them; New refuses
			// them with "not implemented".
		default:
			v.errf(x.KindRange, "Invalid export kind", "export %q: kind %q: want s3, file, gcs or azure", x.Name, e.Kind)
		}
		c.Exports = append(c.Exports, e)
	}
	loggedBy := map[string]string{}
	lnames := map[string]bool{}
	for _, l := range raw.Logging {
		cl := Logging{Name: l.Name, Teams: l.Teams, Destination: l.Destination, MaxContentBytes: 1 << 20, BatchRecords: 500,
			BatchInterval: v.dur(l.BatchInterval, l.BatchRange, "batch_interval", time.Minute)}
		if l.Content != nil {
			cl.Content = *l.Content
		}
		if l.Lock != nil {
			cl.Lock = *l.Lock
		}
		if l.MaxContentBytes != nil {
			cl.MaxContentBytes = *l.MaxContentBytes
		}
		if l.BatchRecords != nil {
			cl.BatchRecords = *l.BatchRecords
		}
		// The name is a path segment of the objects' keys.
		if !ValidName(l.Name) || l.Name == "." || l.Name == ".." || lnames[l.Name] {
			v.errf(l.NameRange, "Invalid logging name", "logging %q: want [A-Za-z0-9_.-]{1,64}, declared once", l.Name)
		}
		lnames[l.Name] = true
		if !exports[l.Destination] {
			v.errf(l.DestinationRange, "Unknown export", "logging %q: destination %q names no export block", l.Name, l.Destination)
		}
		if len(l.Teams) == 0 {
			v.errf(l.TeamsRange, "No team", "logging %q names no team", l.Name)
		}
		for _, t := range l.Teams {
			if !ValidName(t) {
				v.errf(l.TeamsRange, "Invalid team", "logging %q: team %q", l.Name, t)
			} else if other, ok := loggedBy[t]; ok {
				v.errf(l.TeamsRange, "Team in two logging blocks", "team %q is in logging %q and %q", t, other, l.Name)
			} else {
				// A misspelt team would silently have no log.
				v.known(teams, t, l.TeamsRange, "team", "logging "+strconv.Quote(l.Name))
			}
			loggedBy[t] = l.Name
		}
		if cl.MaxContentBytes < 1 || cl.MaxContentBytes > 16<<20 || cl.BatchRecords < 1 || cl.BatchRecords > 100000 ||
			cl.BatchInterval < time.Second || cl.BatchInterval > 24*time.Hour {
			v.errf(l.DefRange, "Invalid logging limits", "logging %q: max_content_bytes 1..16 MiB, batch_records 1..100000, batch_interval 1s..24h", l.Name)
		}
		c.Logging = append(c.Logging, cl)
	}
	if a := raw.Audit; a != nil {
		c.Audit.Anchor = strOr(a.Anchor, "")
		c.Audit.AnchorMaxLag = v.dur(a.AnchorMaxLag, a.AnchorMaxLagRange, "anchor_max_lag", 3*c.Audit.CheckpointInterval)
		if c.Audit.Anchor != "" && !exports[c.Audit.Anchor] {
			v.errf(a.AnchorRange, "Unknown export", "audit anchor %q names no export block", c.Audit.Anchor)
		}
		if c.Audit.Anchor != "" && c.Audit.SigningKeyFile == "" {
			v.errf(a.AnchorRange, "Anchor without signing key", "an anchor exports signed checkpoints: set signing_key_file")
		}
		c.Audit.PruneInterval = v.dur(a.PruneInterval, a.PruneIntervalRange, "prune_interval", 24*time.Hour)
		if c.Audit.PruneInterval < time.Minute {
			v.errf(a.PruneIntervalRange, "Invalid prune_interval", "prune_interval %v: at least 1m", c.Audit.PruneInterval)
		}
		if len(a.Retention) > 0 {
			c.Audit.Retention = map[string]time.Duration{}
			for kind, s := range a.Retention {
				d, err := time.ParseDuration(s)
				switch {
				case !slices.Contains(AuditKinds, kind):
					v.errf(a.RetentionRange, "Invalid retention", "retention of kind %q: the kinds are %s", kind, strings.Join(AuditKinds, ", "))
				case err != nil || d < 24*time.Hour:
					v.errf(a.RetentionRange, "Invalid retention", "retention of %s %q: a duration of at least 24h", kind, s)
				}
				c.Audit.Retention[kind] = d
			}
			if c.Audit.SigningKeyFile == "" {
				v.errf(a.RetentionRange, "Retention without signing key", "pruning cuts at signed checkpoints: set signing_key_file")
			}
		}
	}
	policyOf := map[string]string{} // team -> policy
	names := map[string]bool{}
	for _, p := range raw.Policies {
		cp := Policy{Name: p.Name, Teams: p.Teams, Require: p.Require}
		if p.RequireTags != nil {
			cp.RequireTags = *p.RequireTags
		}
		if p.MaxBodyBytes != nil {
			cp.MaxBodyBytes = *p.MaxBodyBytes
		}
		if p.MaxResponseBytes != nil {
			cp.MaxResponseBytes = *p.MaxResponseBytes
		}
		if !ValidName(p.Name) || names[p.Name] {
			v.errf(p.NameRange, "Invalid policy name", "policy %q: want [A-Za-z0-9_.-]{1,64}, declared once", p.Name)
		}
		names[p.Name] = true
		for _, t := range p.Teams {
			if !ValidName(t) {
				v.errf(p.TeamsRange, "Invalid team", "policy %q: team %q", p.Name, t)
			} else if other, ok := policyOf[t]; ok {
				v.errf(p.TeamsRange, "Team in two policies", "team %q is in policies %q and %q", t, other, p.Name)
			} else {
				v.known(teams, t, p.TeamsRange, "team", "policy "+strconv.Quote(p.Name))
			}
			policyOf[t] = p.Name
		}
		for _, f := range p.Require {
			if !ValidFieldPath(f) {
				v.errf(p.RequireRange, "Invalid required field", "policy %q: %q: want a name or a dotted path of names", p.Name, f)
			}
		}
		if len(p.Require) > 32 {
			v.errf(p.RequireRange, "Too many required fields", "policy %q: at most 32", p.Name)
		}
		if cp.MaxBodyBytes < 0 || cp.MaxBodyBytes > maxPolicyBytes || cp.MaxResponseBytes < 0 || cp.MaxResponseBytes > maxPolicyBytes {
			v.errf(p.DefRange, "Invalid size limit", "policy %q: sizes between 1 byte and 1 GiB", p.Name)
		}
		if len(cp.Require) == 0 && !cp.RequireTags && cp.MaxBodyBytes == 0 && cp.MaxResponseBytes == 0 {
			v.errf(p.DefRange, "Empty policy", "policy %q sets nothing", p.Name)
		}
		c.Policies = append(c.Policies, cp)
	}
	if sc := raw.SCIM; sc != nil {
		cs := &SCIM{TokenFile: sc.TokenFile, BaseURL: strings.TrimRight(sc.BaseURL, "/")}
		if u, err := url.Parse(cs.BaseURL); err != nil || !validIssuer(cs.BaseURL) || u.Path != "/scim/v2" {
			v.errf(sc.BaseURLRange, "Invalid base_url", "base_url %q: want https://<this gateway>/scim/v2", sc.BaseURL)
		}
		seen := map[string]bool{}
		for _, g := range sc.Groups {
			cg := SCIMGroup{Name: g.Name, Team: g.Team, Role: strOr(g.Role, "member")}
			switch {
			case strings.TrimSpace(g.Name) == "" || seen[strings.ToLower(g.Name)]:
				v.errf(g.DefRange, "Invalid group", "group %q: empty or declared twice (names compare without case)", g.Name)
			case !ValidName(g.Team):
				v.errf(g.DefRange, "Invalid team", "group %q: team %q", g.Name, g.Team)
			case !teams[g.Team]:
				v.known(teams, g.Team, g.TeamRange, "team", "scim group "+strconv.Quote(g.Name))
			case cg.Role != "member" && cg.Role != "team_admin" && cg.Role != "viewer":
				v.errf(g.DefRange, "Invalid role", "group %q: role %q: want member, team_admin or viewer", g.Name, cg.Role)
			}
			seen[strings.ToLower(g.Name)] = true
			cs.Groups = append(cs.Groups, cg)
		}
		c.SCIM = cs
	}
	c.Guardrails = v.guardrails(raw.Guardrails, orgs, teams, modelClass)
	c.Tracing = v.tracing(raw.Tracing)
	c.MCPServers = v.mcpServers(raw.MCPServers)
	v.external(raw, c)
	// Every *_file attribute is a secret reference: a file, or a field of
	// an OpenBao/Vault KV v2 secret (package secret).
	for _, ref := range c.SecretRefs() {
		r, err := secret.Parse(ref)
		switch {
		case err != nil:
			v.errf(raw.MasterKeyFileRange, "Invalid secret reference", "%v", err)
		case r.IsVault() && c.Vault == nil:
			v.errf(raw.MasterKeyFileRange, "Vault reference without vault block", "%s needs a vault { address, token_file } block", ref)
		}
	}
	return c, v.diags
}

// SecretRefs lists every secret reference of the configuration.
func (c *Config) SecretRefs() []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	add(c.MasterKeyFile)
	add(c.MetricsTokenFile)
	add(c.Store.DSNFile)
	add(c.CacheSaltKeyFile)
	add(c.Alerts.WebhookTokenFile)
	if c.SSO != nil {
		add(c.SSO.ClientSecretFile)
	}
	if c.SCIM != nil {
		add(c.SCIM.TokenFile)
	}
	if c.HA != nil {
		add(c.HA.SecretFile)
	}
	add(c.Audit.SigningKeyFile)
	for _, e := range c.Exports {
		add(e.AccessKeyFile)
		add(e.SecretKeyFile)
	}
	if c.Alerts.SMTP != nil {
		add(c.Alerts.SMTP.PasswordFile)
	}
	for _, g := range c.Guardrails {
		add(g.TokenFile)
	}
	if c.Tracing != nil {
		add(c.Tracing.TokenFile)
	}
	for _, m := range c.MCPServers {
		add(m.TokenFile)
	}
	out = append(out, c.KeyFiles()...)
	return out
}

// validHeaderName accepts RFC 9110 token characters.
func validHeaderName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func f64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
