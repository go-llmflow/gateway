package config

import (
	"fmt"
	"math"
	"net"
	"net/mail"
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
	Listen               *string    `hcl:"listen,optional"`
	ListenRange          hcl.Range  `hcl:"listen,attr_value_range"`
	MasterKeyFile        string     `hcl:"master_key_file"`
	MasterKeyFileRange   hcl.Range  `hcl:"master_key_file,attr_value_range"`
	MetricsTokenFile     *string    `hcl:"metrics_token_file,optional"`
	Retries              *int       `hcl:"retries,optional"`
	RetriesRange         hcl.Range  `hcl:"retries,attr_value_range"`
	BudgetRefresh        *string    `hcl:"budget_refresh,optional"`
	BudgetRefreshRange   hcl.Range  `hcl:"budget_refresh,attr_value_range"`
	KeyCacheTTL          *string    `hcl:"key_cache_ttl,optional"`
	KeyCacheTTLRange     hcl.Range  `hcl:"key_cache_ttl,attr_value_range"`
	ShutdownGrace        *string    `hcl:"shutdown_grace,optional"`
	ShutdownGraceRange   hcl.Range  `hcl:"shutdown_grace,attr_value_range"`
	ExposeOverheadHeader *bool      `hcl:"expose_overhead_header,optional"`
	CacheIsolation       *string    `hcl:"cache_isolation,optional"`
	CacheIsolationRange  hcl.Range  `hcl:"cache_isolation,attr_value_range"`
	CacheSaltKeyFile     *string    `hcl:"cache_salt_key_file,optional"`
	AuthFailures         *int       `hcl:"auth_failures_per_minute,optional"`
	AuthFailuresRange    hcl.Range  `hcl:"auth_failures_per_minute,attr_value_range"`
	Store                *storeHCL  `hcl:"store,block"`
	Limits               *limitsHCL `hcl:"limits,block"`
	FairShare            *fairHCL   `hcl:"fairshare,block"`
	Health               *healthHCL `hcl:"health,block"`
	Orgs                 []orgHCL   `hcl:"org,block"`
	Tags                 *tagsHCL   `hcl:"tags,block"`
	Alerts               *alertsHCL `hcl:"alerts,block"`
	Pools                []poolHCL  `hcl:"pool,block"`
	Models               []modelHCL `hcl:"model,block"`
	Vault                *vaultHCL  `hcl:"vault,block"`
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
	DefRange  hcl.Range `hcl:",def_range"`
}

type poolHCL struct {
	Name          string    `hcl:"name,label"`
	NameRange     hcl.Range `hcl:"name,label_range"`
	Capacity      int       `hcl:"capacity"`
	CapacityRange hcl.Range `hcl:"capacity,attr_value_range"`
	DefRange      hcl.Range `hcl:",def_range"`
}

type costHCL struct {
	InputPerMTok  *float64  `hcl:"input_per_mtok,optional"`
	OutputPerMTok *float64  `hcl:"output_per_mtok,optional"`
	CachedPerMTok *float64  `hcl:"cached_per_mtok,optional"`
	DefRange      hcl.Range `hcl:",def_range"`
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
	Cost               *costHCL      `hcl:"cost,block"`
	Upstreams          []upstreamHCL `hcl:"upstream,block"`
	DefRange           hcl.Range     `hcl:",def_range"`
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
	c.Store = Store{Kind: "memory", FailClosed: &t, QueueSize: DefaultQueueSize, BatchSize: DefaultBatchSize}
	if s := raw.Store; s != nil {
		c.Store.Kind = strOr(s.Kind, "memory")
		c.Store.DSNFile = strOr(s.DSNFile, "")
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
		default:
			v.errf(s.KindRange, "Invalid store kind", "kind = %q: want \"memory\" or \"postgres\"", c.Store.Kind)
		}
	}

	// limits
	c.Limits = Limits{
		MaxBodyBytes: DefaultMaxBodyBytes, MaxHeaderBytes: DefaultMaxHeaderBytes,
		ReadHeaderTimeout: DefaultReadHeaderTimeout, ReadTimeout: DefaultReadTimeout, IdleTimeout: DefaultIdleTimeout,
		UpstreamHeaderTimeout: DefaultUpstreamHeaderTimeout, MaxRequestDuration: DefaultMaxRequestDuration,
		MaxResponseBytes: DefaultMaxResponseBytes,
	}
	if l := raw.Limits; l != nil {
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
	for _, m := range raw.Models {
		g := ModelGroup{Name: m.Name, UpstreamModel: strOr(m.UpstreamModel, m.Name), Pool: strOr(m.Pool, m.Name), Class: Class(strOr(m.Class, string(Interactive)))}
		if m.Name == "" || len(m.Name) > 256 || strings.ContainsAny(m.Name, "\x00\r\n") {
			v.errf(m.NameRange, "Invalid model name", "%q: want 1..256 bytes without control characters", m.Name)
		}
		if models[m.Name] {
			v.errf(m.DefRange, "Duplicate model", "model %q is declared twice", m.Name)
		}
		models[m.Name] = true
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
		if g.Class != Interactive && g.Class != Batch {
			v.errf(m.ClassRange, "Invalid class", "class = %q: want \"interactive\" or \"batch\"", g.Class)
		}
		if m.Cost != nil {
			for _, x := range []*float64{m.Cost.InputPerMTok, m.Cost.OutputPerMTok, m.Cost.CachedPerMTok} {
				if x != nil && !(*x >= 0 && *x < math.Inf(1)) {
					v.errf(m.Cost.DefRange, "Invalid cost", "costs must be finite and non-negative")
					break
				}
			}
			g.Cost = Cost{InputPerMTok: f64(m.Cost.InputPerMTok), OutputPerMTok: f64(m.Cost.OutputPerMTok), CachedPerMTok: f64(m.Cost.CachedPerMTok)}
		}
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
			if key == "" {
				v.errf(u.DefRange, "Missing key file", "upstream %q of model %q has no key_file (set it on the upstream or the model)", u.Name, m.Name)
			}
			g.Upstreams = append(g.Upstreams, Upstream{Name: u.Name, BaseURL: u.BaseURL, KeyFile: key})
		}
		c.ModelGroups = append(c.ModelGroups, g)
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
	if c.Alerts.SMTP != nil {
		add(c.Alerts.SMTP.PasswordFile)
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
