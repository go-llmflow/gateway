package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/internal/warnat"
)

// The issue's example, written one attribute per line.
const softQuotaBlocks = `
soft_quotas {
  warn_at  = [70, 90]
  language = "fr"
  timezone = "Europe/Paris"
  notify   = ["user", "team_admin"]
}
workday { gpu_hours = 0.8 }
profile "2d" {
  days = 2
  session_share = 0.5
  soft_quotas {
    warn_at = [50, 75, 95]
  }
}
profile "off" {
  days = 1
  session_share = 0.5
  soft_quotas {
    warn_at = []
  }
}
profile "plain" {
  days = 1
  session_share = 0.5
}
`

func TestSoftQuotasParse(t *testing.T) {
	// valid declares org etabA; add an override to it.
	src := strings.Replace(valid, `org "etabA" { share = 0.8125 }`, "org \"etabA\" {\n  share = 0.8125\n  soft_quotas {\n    warn_at = [80, 95]\n  }\n}", 1)
	c, err := Parse([]byte(src+softQuotaBlocks), "s.hcl")
	if err != nil {
		t.Fatal(err)
	}
	sq := c.SoftQuotas
	if !reflect.DeepEqual(sq.WarnAt, []int{70, 90}) || sq.Language != "fr" || sq.Timezone != "Europe/Paris" || !reflect.DeepEqual(sq.Notify, []string{"user", "team_admin"}) {
		t.Fatalf("%+v", sq)
	}
	// The resolution chain gateway -> org -> profile -> budget.
	for name, tc := range map[string]struct {
		budget       []int
		profile, org string
		want         []int
	}{
		"gateway only":      {nil, "plain", "other", []int{70, 90}},
		"org beats gateway": {nil, "plain", "etabA", []int{80, 95}},
		"profile beats org": {nil, "2d", "etabA", []int{50, 75, 95}},
		"budget beats all":  {[]int{60}, "2d", "etabA", []int{60}},
		"profile off":       {nil, "off", "etabA", []int{}},
		"no profile":        {nil, "", "etabA", []int{80, 95}},
	} {
		if got := c.WarnAtOf(tc.budget, tc.profile, tc.org); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestSoftQuotasOmittedMeansAnthropics(t *testing.T) {
	c, err := Parse([]byte(valid+ml(`workday { gpu_hours = 1 }
profile "x" { days = 1  session_share = 0.5 }`)), "s.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftQuotas.WarnAt != nil || c.SoftQuotas.Language != "en" || c.SoftQuotas.Timezone != "UTC" || !reflect.DeepEqual(c.SoftQuotas.Notify, []string{"user"}) {
		t.Fatalf("%+v", c.SoftQuotas)
	}
	if got := c.WarnAtOf(nil, "x", "etabA"); !reflect.DeepEqual(got, warnat.Default()) {
		t.Fatalf("%v", got)
	}
}

func TestSoftQuotasInvalid(t *testing.T) {
	for name, blk := range map[string]string{
		"zero":             "soft_quotas {\n warn_at = [0]\n}",
		"100":              "soft_quotas {\n warn_at = [100]\n}",
		"decreasing":       "soft_quotas {\n warn_at = [95, 75]\n}",
		"duplicate":        "soft_quotas {\n warn_at = [75, 75]\n}",
		"six":              "soft_quotas {\n warn_at = [1,2,3,4,5,6]\n}",
		"fraction":         "soft_quotas {\n warn_at = [7.5]\n}",
		"string element":   "soft_quotas {\n warn_at = [\"75\"]\n}",
		"not a list":       "soft_quotas {\n warn_at = 75\n}",
		"null element":     "soft_quotas {\n warn_at = [null]\n}",
		"huge":             "soft_quotas {\n warn_at = [1e30]\n}",
		"language":         "soft_quotas {\n language = \"de\"\n}",
		"timezone":         "soft_quotas {\n timezone = \"Mars/Olympus\"\n}",
		"notify":           "soft_quotas {\n notify = [\"everyone\"]\n}",
		"notify twice":     "soft_quotas {\n notify = [\"user\", \"user\"]\n}",
		"org decreasing":   "org \"zzz\" {\n share = 0\n soft_quotas {\n  warn_at = [90, 10]\n }\n}",
		"profile too many": "workday { gpu_hours = 1 }\nprofile \"p\" {\n days = 1\n session_share = 0.5\n soft_quotas {\n  warn_at = [1,2,3,4,5,6]\n }\n}",
		"org language":     "org \"zzz\" {\n share = 0\n soft_quotas {\n  language = \"fr\"\n }\n}",
	} {
		if _, err := Parse([]byte(valid+"\n"+blk), "s.hcl"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// FuzzSoftQuotas: any warn_at expression either fails to parse or yields a
// list warnat.Validate accepts, whatever the level it is written at.
func FuzzSoftQuotas(f *testing.F) {
	for _, s := range []string{"[75, 95]", "[]", "[0]", "[1,2,3,4,5,6]", "[7.5]", `["a"]`, "75", "[null]", "[1e30]", "[-1]", "[99, 100]", "{}", "[[1]]", "tolist([1])", "[for i in [1]: i]"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		if len(expr) > 200 {
			return
		}
		for _, wrap := range []string{
			"soft_quotas {\n warn_at = %s\n}",
			"org \"zzz\" {\n share = 0\n soft_quotas {\n  warn_at = %s\n }\n}",
			"workday { gpu_hours = 1 }\nprofile \"p\" {\n days = 1\n session_share = 0.5\n soft_quotas {\n  warn_at = %s\n }\n}",
		} {
			src := valid + "\n" + strings.Replace(wrap, "%s", expr, 1)
			c, err := Parse([]byte(src), "fuzz.hcl")
			if err != nil {
				continue
			}
			lists := [][]int{c.SoftQuotas.WarnAt}
			for _, o := range c.Orgs {
				lists = append(lists, o.WarnAt)
			}
			for _, p := range c.Profiles.List {
				lists = append(lists, p.WarnAt)
			}
			for _, l := range lists {
				if err := warnat.Validate(l); err != nil {
					t.Fatalf("%q parsed to %v: %v", expr, l, err)
				}
			}
		}
	})
}
