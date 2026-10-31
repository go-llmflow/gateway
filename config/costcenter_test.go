package config

import (
	"strings"
	"testing"
)

// valid declares org etabA with team etabA; see config_test.go. The tests
// splice cost-centre syntax into its org block.
func withCC(t *testing.T, extra string) (*Config, error) {
	t.Helper()
	const org = `org "etabA" { share = 0.8125 }`
	if !strings.Contains(valid, org) {
		t.Fatal("the base configuration changed")
	}
	return parse(strings.Replace(valid, org, "org \"etabA\" {\n share = 0.8125\n team \"t1\" {\n lab = \"genomics\"\n }\n"+extra+"\n}", 1))
}

func TestCostCenters(t *testing.T) {
	c, err := withCC(t, "cost_center = \"GL-7001\"\n lab \"genomics\" {\n cost_center = \"GL-7002\"\n }\n project \"t1\" \"p9\" {\n cost_center = \"GL-7003\"\n }")
	if err != nil {
		t.Fatal(err)
	}
	o := c.Orgs[0]
	if o.CostCenter != "GL-7001" || c.CostCenterOf("etabA", "", "t1", "") != "GL-7001" || c.CostCenterOf("etabA", "genomics", "t1", "") != "GL-7002" ||
		c.CostCenterOf("etabA", "genomics", "t1", "p9") != "GL-7003" || c.CostCenterOf("nobody", "", "", "") != "" {
		t.Fatalf("%+v", o)
	}
	// Most specific wins: project over lab over organisation.
	o = Org{Name: "o", CostCenter: "O", LabCostCenter: map[string]string{"l": "L"}, ProjectCostCenter: map[[2]string]string{{"t", "p"}: "P"}}
	c = &Config{Orgs: []Org{o}}
	for _, k := range []struct{ lab, team, project, want string }{
		{"l", "t", "p", "P"}, {"l", "t", "q", "L"}, {"l", "t", "", "L"}, {"", "t", "p", "P"}, {"x", "t", "", "O"}, {"", "", "", "O"},
	} {
		if got := c.CostCenterOf("o", k.lab, k.team, k.project); got != k.want {
			t.Errorf("%+v: %q", k, got)
		}
	}
}

func TestCostCenterErrors(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"formula", `cost_center = "=1+1"`, "Invalid cost_center"},
		{"comma", `cost_center = "A,B"`, "Invalid cost_center"},
		{"long", `cost_center = "` + strings.Repeat("A", 33) + `"`, "Invalid cost_center"},
		{"unknown lab", "lab \"nolab\" {\n cost_center = \"X\"\n}", "Unknown lab"},
		{"unknown team", "project \"ghost\" \"p\" {\n cost_center = \"X\"\n}", "Unknown team"},
	} {
		if _, err := withCC(t, c.extra); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func FuzzCostCenter(f *testing.F) {
	f.Add("GL-7001")
	f.Add("=cmd|' /C calc'!A0")
	f.Fuzz(func(t *testing.T, s string) {
		if ValidCostCenter(s) && strings.ContainsAny(s, ",\"\n\r\t =+@;|") {
			t.Fatalf("accepted %q", s)
		}
	})
}

func TestVarianceThresholdPct(t *testing.T) {
	c, err := parse(valid)
	if err != nil || c.VarianceThresholdPct != 15 {
		t.Fatalf("default: %v %v", c.VarianceThresholdPct, err)
	}
	if c, err = parse(valid + "\nvariance_threshold_pct = 12.5\n"); err != nil || c.VarianceThresholdPct != 12.5 {
		t.Fatalf("12.5: %v %v", c.VarianceThresholdPct, err)
	}
	for _, bad := range []string{"0", "-3", "1000.5", "1e9"} {
		if _, err := parse(valid + "\nvariance_threshold_pct = " + bad + "\n"); err == nil || !strings.Contains(err.Error(), "Invalid variance_threshold_pct") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestAdoptionMinUsersAndDecimals(t *testing.T) {
	c, err := parse(valid)
	if err != nil || c.AdoptionMinUsers != 5 {
		t.Fatalf("default: %v %v", c.AdoptionMinUsers, err)
	}
	if c, err = parse(valid + "\nadoption_min_users = 3\n"); err != nil || c.AdoptionMinUsers != 3 {
		t.Fatalf("3: %v %v", c, err)
	}
	for _, bad := range []string{"1", "0", "-2", "5000"} {
		if _, err := parse(valid + "\nadoption_min_users = " + bad + "\n"); err == nil || !strings.Contains(err.Error(), "Invalid adoption_min_users") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if c, err = parse(valid + "\nfluids {\n currency = \"JPY\"\n currency_decimals = 0\n}\n"); err != nil || c.Fluids.CurrencyDecimals != 0 {
		t.Fatalf("JPY: %v", err)
	}
	if c, _ = parse(valid + "\nfluids {\n pue = 1.2\n}\n"); c.Fluids.CurrencyDecimals != 2 {
		t.Fatal("default decimals")
	}
	if _, err := parse(valid + "\nfluids {\n currency_decimals = 9\n}\n"); err == nil {
		t.Fatal("9 decimals accepted")
	}
}
