package config

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/secret"
	"github.com/hashicorp/hcl/v2"
)

// Slurm makes the running Slurm jobs of a convention the upstreams of a
// model (#75): vLLM instances run as long jobs of a service account and
// come and go with the cluster's schedule; the gateway follows them.
//
// References opened 2026-10-26: Slurm master
// src/slurmrestd/plugins/openapi/slurmctld/api.c (GET
// /slurm/{data_parser}/jobs/), src/plugins/data_parser/v0.0.4{3,4,5,6}/
// parsers.c (PARSER_ARRAY(JOB_INFO): job_id, name, comment, account,
// partition, user_name, batch_host, job_state), src/slurmrestd/plugins/
// auth/jwt/jwt.c (X-SLURM-USER-TOKEN, X-SLURM-USER-NAME), and
// slurm.schedmd.com/jwt.html.
type Slurm struct {
	// URL is slurmrestd's base URL (configuration only: never derived
	// from anything a job or client says).
	URL string
	// APIVersion is the data_parser version in the path ("v0.0.44").
	APIVersion string
	// TokenFile is the secret reference of the JWT (scontrol token
	// lifespan=…), sent as X-SLURM-USER-TOKEN; UserName, when set, as
	// X-SLURM-USER-NAME.
	TokenFile string
	UserName  string
	// KeyFile is the vLLM --api-key of the discovered instances (the
	// model's key_file).
	KeyFile string
	// Only jobs in these accounts and partitions (and of these users, when
	// set) are trusted: anyone else's job claiming the convention could
	// capture prompts.
	Accounts   []string
	Partitions []string
	Users      []string
	// Interval between polls; StaleAfter: how long the last known set is
	// trusted while slurmrestd cannot be read (no instance is admitted
	// after that; healthy members are kept).
	Interval   time.Duration
	StaleAfter time.Duration
	// InstanceCapacity is an instance's contribution to its pool's C when
	// the job does not declare one; MaxInstanceCapacity bounds a declared
	// one.
	InstanceCapacity    int
	MaxInstanceCapacity int
	// Scheme of the discovered instances' URLs ("http" or "https").
	Scheme string
}

// DefaultSlurmAPIVersion is Slurm 25.05's data_parser.
const DefaultSlurmAPIVersion = "v0.0.44"

type slurmHCL struct {
	URL                 string    `hcl:"url"`
	URLRange            hcl.Range `hcl:"url,attr_value_range"`
	APIVersion          *string   `hcl:"api_version,optional"`
	APIVersionRange     hcl.Range `hcl:"api_version,attr_value_range"`
	TokenFile           string    `hcl:"token_file"`
	TokenFileRange      hcl.Range `hcl:"token_file,attr_value_range"`
	UserName            *string   `hcl:"user_name,optional"`
	Accounts            []string  `hcl:"accounts"`
	AccountsRange       hcl.Range `hcl:"accounts,attr_value_range"`
	Partitions          []string  `hcl:"partitions"`
	PartitionsRange     hcl.Range `hcl:"partitions,attr_value_range"`
	Users               []string  `hcl:"users,optional"`
	UsersRange          hcl.Range `hcl:"users,attr_value_range"`
	Interval            *string   `hcl:"interval,optional"`
	IntervalRange       hcl.Range `hcl:"interval,attr_value_range"`
	StaleAfter          *string   `hcl:"stale_after,optional"`
	StaleAfterRange     hcl.Range `hcl:"stale_after,attr_value_range"`
	InstanceCapacity    *int      `hcl:"instance_capacity,optional"`
	MaxInstanceCapacity *int      `hcl:"max_instance_capacity,optional"`
	CapacityRange       hcl.Range `hcl:"instance_capacity,attr_value_range"`
	Scheme              *string   `hcl:"scheme,optional"`
	SchemeRange         hcl.Range `hcl:"scheme,attr_value_range"`
	DefRange            hcl.Range `hcl:",def_range"`
}

var (
	apiVersionRe = regexp.MustCompile(`^v0\.0\.[0-9]{2}$`)
	// slurmNameRe: Slurm account, partition and user names as the gateway
	// accepts them in its allow-lists.
	slurmNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// slurm converts and checks a model's slurm block.
func (v *validator) slurm(s *slurmHCL, model, keyFile string) *Slurm {
	out := &Slurm{
		URL: strings.TrimRight(s.URL, "/"), APIVersion: strOr(s.APIVersion, DefaultSlurmAPIVersion), TokenFile: s.TokenFile,
		UserName: strOr(s.UserName, ""), KeyFile: keyFile, Accounts: s.Accounts, Partitions: s.Partitions, Users: s.Users,
		Interval:   v.dur(s.Interval, s.IntervalRange, "interval", 15*time.Second),
		StaleAfter: v.dur(s.StaleAfter, s.StaleAfterRange, "stale_after", 5*time.Minute),
		Scheme:     strOr(s.Scheme, "http"),
	}
	where := "model " + model + " slurm"
	if err := ValidateUpstream(out.URL); err != nil {
		v.errf(s.URLRange, "Invalid slurmrestd url", "%s: %v", where, err)
	} else if u, _ := url.Parse(out.URL); u.Path != "" {
		v.errf(s.URLRange, "Invalid slurmrestd url", "%s: url = %q: give the base URL only (scheme://host:port); the gateway adds /slurm/%s/jobs/", where, s.URL, out.APIVersion)
	}
	if !apiVersionRe.MatchString(out.APIVersion) {
		v.errf(s.APIVersionRange, "Invalid api_version", "%s: api_version = %q: want a data_parser version such as %q (slurmrestd -d list)", where, out.APIVersion, DefaultSlurmAPIVersion)
	}
	if _, err := secret.Parse(s.TokenFile); err != nil || s.TokenFile == "" {
		v.errf(s.TokenFileRange, "Invalid token_file", "%s: token_file must be a secret reference (the JWT is never taken from the environment or the command line)", where)
	}
	if out.UserName != "" && !slurmNameRe.MatchString(out.UserName) {
		v.errf(s.DefRange, "Invalid user_name", "%s: user_name = %q", where, out.UserName)
	}
	if keyFile == "" {
		v.errf(s.DefRange, "Missing key file", "%s: the discovered vLLM instances need the model's key_file (their --api-key)", where)
	}
	for _, l := range []struct {
		list     []string
		r        hcl.Range
		name     string
		required bool
	}{{s.Accounts, s.AccountsRange, "accounts", true}, {s.Partitions, s.PartitionsRange, "partitions", true}, {s.Users, s.UsersRange, "users", false}} {
		if l.required && len(l.list) == 0 {
			v.errf(l.r, "Empty "+l.name, "%s: %s must name at least one: a job outside them is never trusted", where, l.name)
		}
		for _, n := range l.list {
			if !slurmNameRe.MatchString(n) {
				v.errf(l.r, "Invalid "+l.name, "%s: %q: want [A-Za-z0-9_.-]{1,64}", where, n)
			}
		}
	}
	out.InstanceCapacity, out.MaxInstanceCapacity = 1, 64
	if s.InstanceCapacity != nil {
		out.InstanceCapacity = *s.InstanceCapacity
	}
	if s.MaxInstanceCapacity != nil {
		out.MaxInstanceCapacity = *s.MaxInstanceCapacity
	}
	if out.InstanceCapacity < 1 || out.MaxInstanceCapacity < out.InstanceCapacity || out.MaxInstanceCapacity > 4096 {
		v.errf(s.CapacityRange, "Invalid instance capacity", "%s: want 1 <= instance_capacity (%d) <= max_instance_capacity (%d) <= 4096", where, out.InstanceCapacity, out.MaxInstanceCapacity)
	}
	if out.Scheme != "http" && out.Scheme != "https" {
		v.errf(s.SchemeRange, "Invalid scheme", "%s: scheme = %q: want \"http\" or \"https\"", where, out.Scheme)
	}
	if out.StaleAfter < out.Interval {
		v.errf(s.StaleAfterRange, "Invalid stale_after", "%s: stale_after (%s) is shorter than interval (%s)", where, out.StaleAfter, out.Interval)
	}
	return out
}
