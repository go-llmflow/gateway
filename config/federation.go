package config

import (
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// Federation is a peer dispatcher that routes its own users' requests to this
// gateway as one node (ILaaS: CentraleSupelec/aristote-dispatcher). Under
// Prefix the inference API of the root answers for the keys of Org only, and
// GET <Prefix>/metrics tells the dispatcher how busy this node is, in the
// shape its least-busy strategy reads (vLLM's time-to-first-token histogram).
type Federation struct {
	Name   string
	Prefix string
	Org    string
	// MetricsNetworks may read <Prefix>/metrics, which carries no
	// authentication (the dispatcher sends none).
	MetricsNetworks []netip.Prefix
}

type federationHCL struct {
	Name         string    `hcl:"name,label"`
	NameRange    hcl.Range `hcl:"name,label_range"`
	Prefix       string    `hcl:"prefix"`
	PrefixRange  hcl.Range `hcl:"prefix,attr_value_range"`
	Org          string    `hcl:"org"`
	OrgRange     hcl.Range `hcl:"org,attr_value_range"`
	Networks     []string  `hcl:"metrics_networks"`
	NetworksRang hcl.Range `hcl:"metrics_networks,attr_value_range"`
}

// MaxFederations bounds the federation blocks.
const MaxFederations = 16

var (
	federationName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	federationPrefix = regexp.MustCompile(`^(/[A-Za-z0-9_-][A-Za-z0-9._-]{0,31}){1,3}$`)
	// reservedFirst are the first segments the gateway serves itself.
	reservedFirst = map[string]bool{"v1": true, "v2": true, "admin": true, "ui": true, "scim": true,
		"mcp": true, "metrics": true, "healthz": true, "tokenize": true, "detokenize": true, "sso": true, "auth": true}
)

// federations validates the federation blocks. A federation's organisation
// must exist and hold no share of any pool a model uses: the peer is a third
// party served from spare capacity, and a share would give it reserved
// capacity and a priority that the least-busy signal does not describe. This
// is a refusal, not a warning: check-config has no warnings, and a peer that
// reads "never used" forever is the failure this block exists to prevent.
func (v *validator) federations(raw []federationHCL, orgs map[string]bool, c *Config) []Federation {
	if len(raw) > MaxFederations {
		v.errf(raw[MaxFederations].NameRange, "Too many federation blocks", "at most %d", MaxFederations)
		return nil
	}
	// ⛔ One model. The peer merges the histograms of a node bucket by bucket
	// (the last series it reads wins), and a mount answers for every model of the
	// gateway, a fallback included: with several, the node's latency would be
	// that of whichever model sorts last. The mount cannot be narrowed to a model
	// (the inference API of the root is what it serves), so the gateway behind a
	// federation serves one: run one gateway per model for a peer that needs
	// several.
	if len(raw) > 0 && len(c.ModelGroups) > 1 {
		names := make([]string, 0, len(c.ModelGroups))
		for _, g := range c.ModelGroups {
			names = append(names, g.Name)
		}
		slices.Sort(names)
		v.errf(raw[0].NameRange, "Federation reaches several models", "federation %q: this gateway serves several models (%s) and the peer dispatcher merges a node's histograms bucket by bucket, so the last model would speak for all of them; a gateway behind a federation serves one model (run one gateway per model for a peer that needs several)", raw[0].Name, strings.Join(names, ", "))
	}
	var out []Federation
	names := map[string]bool{}
	for _, f := range raw {
		where := "federation " + strconv.Quote(f.Name)
		if !federationName.MatchString(f.Name) || names[f.Name] {
			v.errf(f.NameRange, "Invalid federation name", "%s: want [A-Za-z0-9_-]{1,32}, declared once", where)
		}
		names[f.Name] = true
		if msg := FederationPrefixProblem(f.Prefix); msg != "" {
			v.errf(f.PrefixRange, "Invalid prefix", "%s: prefix %q: %s", where, f.Prefix, msg)
		}
		for _, o := range out {
			if f.Prefix == o.Prefix || strings.HasPrefix(f.Prefix, o.Prefix+"/") || strings.HasPrefix(o.Prefix, f.Prefix+"/") {
				v.errf(f.PrefixRange, "Overlapping prefix", "%s: prefix %q overlaps %q, which is federation %q's", where, f.Prefix, o.Prefix, o.Name)
			}
		}
		v.known(orgs, f.Org, f.OrgRange, "organisation", where+" org")
		nets := v.prefixes(f.Networks, f.NetworksRang, where+" metrics_networks")
		if len(f.Networks) == 0 {
			v.errf(f.NetworksRang, "Empty metrics_networks", "%s: name the networks of the dispatcher (the metrics are served without a key, to those networks only)", where)
		}
		for _, p := range nets {
			if p.Bits() == 0 {
				v.errf(f.NetworksRang, "Open metrics_networks", "%s: %s would serve the metrics to every address; name the dispatcher's networks", where, p)
			}
		}
		out = append(out, Federation{Name: f.Name, Prefix: f.Prefix, Org: f.Org, MetricsNetworks: nets})
		seen := map[string]bool{}
		for _, g := range c.ModelGroups {
			if seen[g.Pool] {
				continue
			}
			seen[g.Pool] = true
			if c.PoolShare(g.Pool, f.Org) > 0 {
				v.errf(f.OrgRange, "Federation organisation holds a share", "%s: organisation %q holds a share of pool %q: the peer would have reserved capacity, and the metrics it reads describe spare capacity only; give it no share (a pool with borrow = \"spare\" answers it 429 when full, which is what the metrics report)", where, f.Org, g.Pool)
			}
		}
	}
	return out
}

// FederationPrefixProblem says why a mount point is not usable ("" when it
// is): one to three segments, each [A-Za-z0-9_-][A-Za-z0-9._-]{0,31}, no
// trailing slash, not a path the gateway serves itself.
func FederationPrefixProblem(p string) string {
	if !federationPrefix.MatchString(p) {
		return "want 1 to 3 segments like /fed/ilaas: each [A-Za-z0-9_-][A-Za-z0-9._-]{0,31}, no trailing slash, no \".\" or \"..\" segment"
	}
	first, _, _ := strings.Cut(p[1:], "/")
	if reservedFirst[first] {
		return "the first segment " + strconv.Quote(first) + " is served by the gateway itself"
	}
	return ""
}
