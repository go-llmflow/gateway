package config

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

// Workload profiles (#90): an administrator gives a person a profile in
// working days per week instead of a money budget. A workday is the
// consumption of one person working with the assistant for a full day, in
// each cost unit; a profile of d days a week is a weekly allowance of
// d × workday and a rolling session allowance of session_share × workday.
//
//	workday { gpu_hours = 0.8  USD = 13 }   # measured: see /ui/profiles/calibrate
//	profile "1d" { days = 1  session_share = 0.5  overflow_cap = 2 }
//	profile "q"  { label = "¼ day / week"  days = 0.25  session_share = 0.25 }
//	default_profile = "q"
//	session_window  = "5h"
//
// The workday values are calibrated from measured usage: a proxy admin
// accepts one in the admin UI (stored, it takes precedence over this
// block, which is the starting value). Labels default to the day count:
// commercial plan names are not reused, their ratios do not match a day
// scale (Anthropic Team Premium is 5× Standard, Max 20x 16×).
type Profiles struct {
	// Workday is the configured workday per unit ("" is GPU-hours).
	Workday map[string]float64
	List    []Profile
	// Default is the profile of a person with none assigned ("": none, no
	// profile budget applies to them).
	Default string
	// SessionWindow is the session allowance's rolling window (5h).
	SessionWindow time.Duration
}

// Profile is one workload profile.
type Profile struct {
	ID    string
	Label string
	// Days per week, 0..7.
	Days float64
	// SessionShare is the share of a workday one session window allows,
	// in (0, 1].
	SessionShare float64
	// OverflowCap is how many workdays a month a person of this profile may
	// use beyond their allowance, when an administrator allows them to
	// (0: never). Requests beyond the profile are refused without it.
	OverflowCap float64
	// WarnAt overrides the soft-quota thresholds of this profile's people
	// (nil: not set; #96).
	WarnAt []int
}

// Profile bounds (security audit #90).
const (
	MaxProfiles          = 64
	MaxProfileDays       = 7
	MaxOverflowCap       = 31 * 7
	MaxWorkday           = 1e9
	DefaultSessionWindow = 5 * time.Hour
	maxProfileLabel      = 64
)

// Get returns a profile by id.
func (p Profiles) Get(id string) (Profile, bool) {
	for _, x := range p.List {
		if x.ID == id {
			return x, true
		}
	}
	return Profile{}, false
}

// Enabled reports whether any profile is configured.
func (p Profiles) Enabled() bool { return len(p.List) > 0 }

// DaysLabel spells a number of days a week as the default label does:
// "¼ day / week", "½ day / week", "1 day / week", "2.5 days / week".
func DaysLabel(d float64) string {
	var n string
	switch d {
	case 0.25:
		n = "¼"
	case 0.5:
		n = "½"
	case 0.75:
		n = "¾"
	default:
		n = strconv.FormatFloat(d, 'f', -1, 64)
	}
	unit := "days"
	if d <= 1 && d > 0 {
		unit = "day"
	}
	return n + " " + unit + " / week"
}

type workdayHCL struct {
	Remain   hcl.Body  `hcl:",remain"`
	DefRange hcl.Range `hcl:",def_range"`
}

type profileHCL struct {
	ID                string              `hcl:"id,label"`
	IDRange           hcl.Range           `hcl:"id,label_range"`
	Label             *string             `hcl:"label,optional"`
	LabelRange        hcl.Range           `hcl:"label,attr_value_range"`
	Days              float64             `hcl:"days"`
	DaysRange         hcl.Range           `hcl:"days,attr_value_range"`
	SessionShare      float64             `hcl:"session_share"`
	SessionShareRange hcl.Range           `hcl:"session_share,attr_value_range"`
	OverflowCap       *float64            `hcl:"overflow_cap,optional"`
	OverflowCapRange  hcl.Range           `hcl:"overflow_cap,attr_value_range"`
	SoftQuotas        *softQuotasLevelHCL `hcl:"soft_quotas,block"`
}

// validProfileID accepts [a-z0-9_-]{1,32} (store.ValidProfileID).
func validProfileID(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// validLabel accepts printable text without control characters, at most
// 64 bytes.
func validLabel(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > maxProfileLabel {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return false
		}
	}
	return true
}

func finiteF(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// profiles validates the workday block, the profiles, default_profile and
// session_window, once the models (and so the units) are built.
func (v *validator) profiles(raw *fileHCL, c *Config) {
	c.Profiles = Profiles{Workday: map[string]float64{}, SessionWindow: DefaultSessionWindow}
	units := map[string]bool{}
	for _, u := range c.Units() {
		units[u] = true
	}
	if w := raw.Workday; w != nil {
		attrs, diags := w.Remain.JustAttributes()
		v.diags = append(v.diags, diags...)
		names := make([]string, 0, len(attrs))
		for n := range attrs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			a := attrs[n]
			val, diags := a.Expr.Value(nil)
			if diags.HasErrors() {
				v.diags = append(v.diags, diags...)
				continue
			}
			if val.IsNull() || !val.IsKnown() || val.Type() != cty.Number {
				v.errf(a.Range, "Invalid workday", "workday %s: want a number of units per person-day", n)
				continue
			}
			f, _ := val.AsBigFloat().Float64()
			u, ok := normalizeUnit(n)
			switch {
			case !ok:
				v.errf(a.NameRange, "Invalid unit", "workday %s: want gpu_hours or a currency code (letters, digits, _; at most 16)", n)
			case !units[u]:
				v.errf(a.NameRange, "Unknown unit", "workday %s: no model costs in it", n)
			case !(f > 0 && f <= MaxWorkday) || !finiteF(f):
				v.errf(a.Expr.Range(), "Invalid workday", "workday %s = %v: want more than 0 and at most %g", n, f, float64(MaxWorkday))
			default:
				if _, dup := c.Profiles.Workday[u]; dup {
					v.errf(a.NameRange, "Duplicate unit", "workday %s: the unit is set twice", n)
				}
				c.Profiles.Workday[u] = f
			}
		}
	}
	if len(raw.Profiles) > MaxProfiles {
		v.errf(raw.Profiles[MaxProfiles].IDRange, "Too many profiles", "at most %d profiles", MaxProfiles)
	}
	seen := map[string]bool{}
	for i, p := range raw.Profiles {
		if i >= MaxProfiles {
			break
		}
		cp := Profile{ID: p.ID, Days: p.Days, SessionShare: p.SessionShare}
		where := "profile " + strconv.Quote(p.ID)
		if p.SoftQuotas != nil {
			cp.WarnAt = v.warnAt(p.SoftQuotas.WarnAt, where)
		}
		switch {
		case !validProfileID(p.ID):
			v.errf(p.IDRange, "Invalid profile id", "%s: want [a-z0-9_-]{1,32}", where)
		case seen[p.ID]:
			v.errf(p.IDRange, "Duplicate profile", "%s is declared twice", where)
		}
		seen[p.ID] = true
		if !(p.Days >= 0 && p.Days <= MaxProfileDays) || !finiteF(p.Days) {
			v.errf(p.DaysRange, "Invalid days", "%s: days = %v: want 0 to %d days a week", where, p.Days, MaxProfileDays)
		}
		if !(p.SessionShare > 0 && p.SessionShare <= 1) || !finiteF(p.SessionShare) {
			v.errf(p.SessionShareRange, "Invalid session_share", "%s: session_share = %v: want more than 0 and at most 1 workday per session", where, p.SessionShare)
		}
		if p.OverflowCap != nil {
			cp.OverflowCap = *p.OverflowCap
			if !(cp.OverflowCap >= 0 && cp.OverflowCap <= MaxOverflowCap) || !finiteF(cp.OverflowCap) {
				v.errf(p.OverflowCapRange, "Invalid overflow_cap", "%s: overflow_cap = %v: want 0 to %d workdays a month", where, cp.OverflowCap, MaxOverflowCap)
			}
		}
		cp.Label = DaysLabel(p.Days)
		if p.Label != nil {
			cp.Label = *p.Label
			if !validLabel(cp.Label) {
				v.errf(p.LabelRange, "Invalid label", "%s: label: want 1 to %d printable bytes", where, maxProfileLabel)
			}
		}
		c.Profiles.List = append(c.Profiles.List, cp)
	}
	if raw.DefaultProfile != nil {
		c.Profiles.Default = *raw.DefaultProfile
		if _, ok := c.Profiles.Get(c.Profiles.Default); !ok {
			v.errf(raw.DefaultProfileRange, "Unknown profile", "default_profile = %q: no profile block declares it", c.Profiles.Default)
		}
	}
	if raw.SessionWindow != nil {
		d := v.dur(raw.SessionWindow, raw.SessionWindowRange, "session_window", DefaultSessionWindow)
		if d < 5*time.Minute || d > 24*time.Hour || d%(5*time.Minute) != 0 {
			v.errf(raw.SessionWindowRange, "Invalid session_window", "session_window = %s: want 5m to 24h in whole 5 minutes", d)
		}
		c.Profiles.SessionWindow = d
	}
	if (raw.Workday != nil || raw.DefaultProfile != nil || raw.SessionWindow != nil) && len(c.Profiles.List) == 0 {
		v.errf(raw.MasterKeyFileRange, "Profiles without profile blocks", "workday, default_profile and session_window configure workload profiles: declare at least one profile block")
	}
}
