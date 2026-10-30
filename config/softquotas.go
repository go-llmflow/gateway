package config

import (
	"math"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/internal/warnat"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Soft quotas (#96): warnings before a window runs out, the way Anthropic
// warns at 75% and 95% of its 5-hour session and weekly limits. The
// thresholds are configured from the most general level to the most
// specific, which wins (internal/warnat resolves them):
//
//	soft_quotas { warn_at = [75, 95]  language = "fr"  timezone = "Europe/Paris" }   # the gateway
//	org "etabA" { soft_quotas { warn_at = [80, 95] } }                                # an organisation
//	profile "2d" { days = 2 ... soft_quotas { warn_at = [50, 75, 95] } }              # a profile
//	                                                       # a user budget: PUT /admin/budgets warn_at
//
// Omitted means [75, 95]; [] turns warnings off for that level. Runtime
// overrides set in the admin UI (store) replace the configured value of the
// same level.
type SoftQuotas struct {
	// WarnAt is the gateway-wide list; nil: not set (the default applies).
	WarnAt []int
	// Language is the language of the warnings and refusals when the request
	// names none (Accept-Language): "en" (default) or "fr".
	Language string
	// Timezone names the zone the reset times are spelled in (default UTC).
	Timezone string
	// Notify lists who is told when a threshold is passed: "user" (the
	// person, at their address) and/or "team_admin". Default: user.
	Notify []string
}

// Soft-quota notification recipients.
const (
	NotifyUser      = "user"
	NotifyTeamAdmin = "team_admin"
)

// Languages of the warnings.
const (
	LangEN = "en"
	LangFR = "fr"
)

// MaxWarnElems bounds the list expression read before validation.
const maxWarnElems = 64

type softQuotasHCL struct {
	WarnAt        hcl.Expression `hcl:"warn_at,optional"`
	Language      *string        `hcl:"language,optional"`
	LanguageRange hcl.Range      `hcl:"language,attr_value_range"`
	Timezone      *string        `hcl:"timezone,optional"`
	TimezoneRange hcl.Range      `hcl:"timezone,attr_value_range"`
	Notify        []string       `hcl:"notify,optional"`
	NotifyRange   hcl.Range      `hcl:"notify,attr_value_range"`
	DefRange      hcl.Range      `hcl:",def_range"`
}

// softQuotasLevelHCL is the block of an organisation or a profile: the
// thresholds only.
type softQuotasLevelHCL struct {
	WarnAt   hcl.Expression `hcl:"warn_at,optional"`
	DefRange hcl.Range      `hcl:",def_range"`
}

// warnAt reads and validates a warn_at expression: nil when absent.
func (v *validator) warnAt(e hcl.Expression, where string) []int {
	if e == nil {
		return nil
	}
	val, diags := e.Value(nil)
	if diags.HasErrors() {
		v.diags = append(v.diags, diags...)
		return nil
	}
	if val.IsNull() {
		return nil
	}
	rng := e.Range()
	if !val.IsKnown() || !(val.Type().IsTupleType() || val.Type().IsListType()) {
		v.errf(rng, "Invalid warn_at", "%s: warn_at: want a list of percentages such as [75, 95], or [] for none", where)
		return nil
	}
	if val.LengthInt() > maxWarnElems {
		v.errf(rng, "Invalid warn_at", "%s: warn_at: at most %d thresholds", where, warnat.MaxLevels)
		return nil
	}
	out := []int{}
	for it := val.ElementIterator(); it.Next(); {
		_, el := it.Element()
		if el.IsNull() || !el.IsKnown() || el.Type() != cty.Number {
			v.errf(rng, "Invalid warn_at", "%s: warn_at: every threshold is an integer percentage", where)
			return nil
		}
		f, _ := el.AsBigFloat().Float64()
		if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
			v.errf(rng, "Invalid warn_at", "%s: warn_at: %v is not an integer from %d to %d", where, f, warnat.Min, warnat.Max)
			return nil
		}
		out = append(out, int(f))
	}
	if err := warnat.Validate(out); err != nil {
		v.errf(rng, "Invalid warn_at", "%s: %v", where, err)
		return nil
	}
	return out
}

// softQuotas validates the gateway-level block.
func (v *validator) softQuotas(raw *fileHCL, c *Config) {
	c.SoftQuotas = SoftQuotas{Language: LangEN, Timezone: "UTC", Notify: []string{NotifyUser}}
	b := raw.SoftQuotas
	if b == nil {
		return
	}
	c.SoftQuotas.WarnAt = v.warnAt(b.WarnAt, "soft_quotas")
	if b.Language != nil {
		if *b.Language != LangEN && *b.Language != LangFR {
			v.errf(b.LanguageRange, "Invalid language", "soft_quotas language = %q: want en or fr", *b.Language)
		} else {
			c.SoftQuotas.Language = *b.Language
		}
	}
	if b.Timezone != nil {
		tz := *b.Timezone
		if len(tz) > 64 || strings.ContainsAny(tz, "\x00\r\n") {
			v.errf(b.TimezoneRange, "Invalid timezone", "soft_quotas timezone: want an IANA name such as Europe/Paris")
		} else if _, err := time.LoadLocation(tz); err != nil {
			v.errf(b.TimezoneRange, "Invalid timezone", "soft_quotas timezone = %q: %v", tz, err)
		} else {
			c.SoftQuotas.Timezone = tz
		}
	}
	if b.Notify != nil {
		seen := map[string]bool{}
		var out []string
		for _, n := range b.Notify {
			switch {
			case n != NotifyUser && n != NotifyTeamAdmin:
				v.errf(b.NotifyRange, "Invalid notify", "soft_quotas notify = %q: want user or team_admin", n)
			case seen[n]:
				v.errf(b.NotifyRange, "Invalid notify", "soft_quotas notify: %q twice", n)
			default:
				seen[n] = true
				out = append(out, n)
			}
		}
		c.SoftQuotas.Notify = out
	}
}

// WarnAtOf is the list in force for a scope: the most specific set level
// wins, among budget (nil when none), the person's profile and organisation,
// and the gateway; Anthropic's [75, 95] when none is set. Runtime overrides
// are applied by the caller (they replace the configured level).
func (c *Config) WarnAtOf(budget []int, profile, org string) []int {
	var p, o []int
	if pr, ok := c.Profiles.Get(profile); ok {
		p = pr.WarnAt
	}
	for _, x := range c.Orgs {
		if x.Name == org {
			o = x.WarnAt
		}
	}
	return warnat.Resolve(budget, p, o, c.SoftQuotas.WarnAt)
}
