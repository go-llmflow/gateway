package slurm

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// parsable2 is what `sreport -P cluster AccountUtilizationByUser -T gres/gpu
// start=… end=…` prints (the man page's columns, with TRES Name as -T adds).
const parsable2 = `--------------------------------------------------------------------------------
Cluster/Account/User Utilization 2026-10-01T00:00:00 - 2026-10-31T23:59:59 (2678400 secs)
Usage reported in TRES Minutes
--------------------------------------------------------------------------------
Cluster|Account|Login|Proper Name|TRES Name|Used|Energy
jz|root|||gres/gpu|90000|0
jz|etaba|||gres/gpu|60000|0
jz|etaba|alice|Alice Martin|gres/gpu|45000|0
jz|etabb|||gres/gpu|30000|0
jz|etabb|bob|Bob|gres/gpu|30000|0
`

// fixed is the default layout (the man page's example, with -T).
const fixed = `--------------------------------------------------------------------------------
Cluster/Account/User Utilization 2026-10-01T00:00:00 - 2026-10-31T23:59:59 (2678400 secs)
Usage reported in TRES Hours
--------------------------------------------------------------------------------
  Cluster         Account     Login     Proper Name      TRES Name     Used   Energy
--------- --------------- --------- --------------- -------------- -------- --------
       jz            root                                 gres/gpu     1500        0
       jz           etaba                                 gres/gpu     1000        0
       jz           etaba     alice  Alice Martin        gres/gpu      750        0
       jz           etabb                                 gres/gpu      500        0
`

func TestParse(t *testing.T) {
	for name, in := range map[string]string{"parsable2": parsable2, "fixed": fixed,
		"parsable": strings.ReplaceAll(parsable2, "|0\n", "|0|\n"), "csv": strings.ReplaceAll(parsable2, "|", ",")} {
		r, err := Parse(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		by := r.ByAccount()
		if r.Period != "2026-10-01T00:00:00 - 2026-10-31T23:59:59" || by["etaba"] != 1000 || by["etabb"] != 500 || by["root"] != 1500 || len(r.Rows) < 4 {
			t.Fatalf("%s: %+v %v", name, r, by)
		}
		if name == "fixed" && (r.Unit != "hours" || r.Rows[2].Login != "alice") {
			t.Fatalf("fixed: %+v", r)
		}
	}
	// Seconds; a CPU row skipped; no header lines (default minutes).
	in := "Usage reported in TRES Seconds\nCluster|Account|Login|TRES Name|Used\nc|a||cpu|7200\nc|a||gres/gpu|7200\n"
	if r, err := Parse(strings.NewReader(in)); err != nil || r.ByAccount()["a"] != 2 {
		t.Fatalf("seconds: %+v %v", r, err)
	}
	if r, err := Parse(strings.NewReader("Account|Used\na|120\n")); err != nil || r.ByAccount()["a"] != 2 {
		t.Fatalf("minimal: %+v %v", r, err)
	}
}

func TestParseRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"percent":    "Usage reported in Percentage of Total\nAccount|Used\na|12%\n",
		"no header":  "a|b|c\n1|2|3\n",
		"no used":    "Account|Login\na|b\n",
		"fields":     "Account|Used\na|1|2\n",
		"negative":   "Account|Used\na|-1\n",
		"nan":        "Account|Used\na|NaN\n",
		"inf":        "Account|Used\na|1e400\n",
		"huge":       "Account|Used\na|1e16\n",
		"no account": "Account|Used\n|1\n",
		"no rule":    "Account   Used\na   1\n",
		"binary":     "Account|Used\n\xff\xfe|1\n",
		"long line":  "Account|Used\n" + strings.Repeat("a", 5000) + "|1\n",
		"too big":    strings.Repeat("x", MaxBytes+1),
		"many lines": "Account|Used\n" + strings.Repeat("a|1\n", MaxLines),
		"many cols":  strings.Repeat("x|", MaxFields) + "Account|Used\n",
	} {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "too big" && !errors.Is(err, ErrTooLarge) {
			t.Errorf("too big: %v", err)
		}
	}
}

// FuzzParse: no panic; whatever is accepted is finite, non-negative and
// within the bounds.
func FuzzParse(f *testing.F) {
	f.Add(parsable2)
	f.Add(fixed)
	f.Add("Account|Used\na|1\n")
	f.Add("Usage reported in TRES Hours\nAccount,Used\na,1\n")
	f.Add("  Account     Used\n--------- -----\n  é é é é    1\n")
	f.Fuzz(func(t *testing.T, in string) {
		r, err := Parse(strings.NewReader(in))
		if err != nil {
			return
		}
		if len(r.Rows) > MaxLines {
			t.Fatal("too many rows")
		}
		for _, x := range r.Rows {
			if x.Account == "" || x.GPUHours < 0 || math.IsNaN(x.GPUHours) || math.IsInf(x.GPUHours, 0) {
				t.Fatalf("bad row %+v", x)
			}
		}
		for a, v := range r.ByAccount() {
			if a == "" || v < 0 || math.IsInf(v, 0) {
				t.Fatalf("bad total %q %v", a, v)
			}
		}
	})
}
