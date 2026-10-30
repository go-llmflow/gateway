package gateway

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

func TestReconcileEnergyKnownValues(t *testing.T) {
	rows := reconcileEnergy(
		map[string]float64{"a": 3.6e6, "b": 7.2e6, "slurm-only": 3.6e3}, // 1 kWh is 3.6 MJ
		map[string]float64{"a": 1.25, "b": 0.5, "dcgm-only": 2})
	by := map[string]EnergyReconRow{}
	for _, r := range rows {
		by[r.Org] = r
	}
	if r := by["a"]; !near(r.SlurmKWh, 1) || !near(r.DiffKWh, 0.25) || r.DiffPct == nil || !near(*r.DiffPct, 25) {
		t.Fatalf("a: %+v", r)
	}
	// The planted mismatch: Slurm saw 2 kWh, DCGM attributed 0.5.
	if r := by["b"]; !near(r.SlurmKWh, 2) || !near(r.DiffKWh, -1.5) || r.DiffPct == nil || !near(*r.DiffPct, -75) {
		t.Fatalf("b: %+v", r)
	}
	if r := by["dcgm-only"]; r.HasSlurm || r.DiffPct != nil || !near(r.DiffKWh, 2) {
		t.Fatalf("dcgm-only: %+v", r)
	}
	if r := by["slurm-only"]; !r.HasSlurm || !near(r.SlurmKWh, 0.001) || !near(r.DiffKWh, -0.001) || r.DiffPct == nil || !near(*r.DiffPct, -100) {
		t.Fatalf("slurm-only: %+v", r)
	}
}

// Invariants for any inputs: no NaN, the difference is DCGM minus Slurm, and
// the percentage exists exactly when Slurm reported something to compare to.
func FuzzReconcileEnergy(f *testing.F) {
	f.Add(3.6e6, 1.25)
	f.Add(0.0, 5.0)
	f.Fuzz(func(t *testing.T, joules, kwh float64) {
		if math.IsNaN(joules) || math.IsInf(joules, 0) || joules < 0 || joules > 1e18 || math.IsNaN(kwh) || math.IsInf(kwh, 0) || kwh < 0 || kwh > 1e12 {
			return
		}
		for _, r := range reconcileEnergy(map[string]float64{"o": joules}, map[string]float64{"o": kwh}) {
			if math.IsNaN(r.DiffKWh) || math.IsInf(r.DiffKWh, 0) || math.Abs(r.DiffKWh-(r.DCGMKWh-r.SlurmKWh)) > 1e-9 {
				t.Fatalf("diff %+v", r)
			}
			if (r.DiffPct != nil) != (r.HasSlurm && r.SlurmKWh > 0) {
				t.Fatalf("pct %+v", r)
			}
			if r.DiffPct != nil && (math.IsNaN(*r.DiffPct) || math.IsInf(*r.DiffPct, 0)) {
				t.Fatalf("pct value %+v", r)
			}
		}
	})
}

const sacctFixture = "JobID|Account|ConsumedEnergyRaw\n" +
	"1|lab-a|2160000\n" + // 0.6 kWh (1 kWh is 3.6 MJ)
	"1.batch|lab-a|999\n" + // a job step: not counted twice
	"2|lab-a|1440000\n" + // 0.4 kWh: lab-a is etabA's account, 1 kWh together
	"3|etabB|7200000\n" + // 2 kWh for etabB
	"4|etabB|\n" // energy not collected: counted as nothing, not as an error

func reconEnv(t *testing.T, sacct string) *env {
	t.Helper()
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	if sacct != "" {
		p := filepath.Join(t.TempDir(), "sacct.txt")
		if err := os.WriteFile(p, []byte(sacct), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Slurm.SacctFile = p
		cfg.Slurm.Accounts = map[string]string{"lab-a": "etabA"}
	}
	e := newEnv(t, cfg)
	at := time.Now().UTC().Add(-time.Hour)
	// DCGM energy attributed: etabA 1.25 kWh (Slurm says 1: +25 %), etabB 0.5
	// kWh (Slurm says 2: the planted mismatch, -75 %).
	for i, x := range []struct {
		org string
		mj  int64
	}{{"etabA", 5 * kWhMJ / 4}, {"etabB", kWhMJ / 2}} {
		iv := store.EnergyInterval{Upstream: "n", Start: at.Add(time.Duration(i) * time.Minute), End: at.Add(time.Duration(i+1) * time.Minute), TotalMJ: x.mj}
		if err := e.st.SaveEnergy(context.Background(), iv, []energy.Attributed{{Consumer: energy.Consumer{Org: x.org, Team: x.org}, MJ: x.mj}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func decisionJSON(t *testing.T, e *env, tok string) (code int, body string) {
	t.Helper()
	day := time.Now().UTC()
	resp := e.do("GET", "/admin/decision?format=json&from="+day.AddDate(0, 0, -1).Format("2006-01-02")+"&to="+day.AddDate(0, 0, 2).Format("2006-01-02"), tok, "")
	return resp.StatusCode, readAll(t, resp)
}

// The sacct input, per partner: a known difference and a planted mismatch
// that the report must show, in the JSON, the workbook and the PDF.
func TestDecisionEnergyReconciliation(t *testing.T) {
	e := reconEnv(t, sacctFixture)
	code, body := decisionJSON(t, e, master)
	var a struct {
		Rows []EnergyReconRow `json:"energy_reconciliation"`
		Note string           `json:"energy_reconciliation_note"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &a) != nil || a.Note != "" {
		t.Fatalf("%d %s", code, body)
	}
	by := map[string]EnergyReconRow{}
	for _, r := range a.Rows {
		by[r.Org] = r
	}
	if r := by["etabA"]; !near(r.SlurmKWh, 1) || !near(r.DCGMKWh, 1.25) || !near(r.DiffKWh, 0.25) || r.DiffPct == nil || !near(*r.DiffPct, 25) {
		t.Fatalf("etabA: %+v", r)
	}
	if r := by["etabB"]; !near(r.SlurmKWh, 2) || !near(r.DCGMKWh, 0.5) || !near(r.DiffKWh, -1.5) || r.DiffPct == nil || !near(*r.DiffPct, -75) {
		t.Fatalf("the planted mismatch of etabB is not shown: %+v", r)
	}
	// An organisation's admin sees its own line only.
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	_, own := decisionJSON(t, e, oa)
	if !strings.Contains(own, `"etabA"`) || strings.Contains(own, "etabB") {
		t.Fatalf("org admin: %s", own)
	}
	// The workbook and the PDF carry it.
	for _, f := range []string{"xlsx", "pdf"} {
		day := time.Now().UTC()
		resp := e.do("GET", "/admin/decision?format="+f+"&from="+day.AddDate(0, 0, -1).Format("2006-01-02")+"&to="+day.AddDate(0, 0, 2).Format("2006-01-02"), master, "")
		if data := readAll(t, resp); resp.StatusCode != 200 || len(data) < 1000 {
			t.Fatalf("%s: %d %d bytes", f, resp.StatusCode, len(data))
		}
	}
	// Without the input there is no reconciliation, and an unreadable file is
	// said, not hidden.
	e2 := reconEnv(t, "")
	if _, body := decisionJSON(t, e2, master); !strings.Contains(body, `"energy_reconciliation":null`) {
		t.Fatalf("without sacct: %s", body)
	}
	e3 := reconEnv(t, "Account|Something\nx|y\n")
	if _, body := decisionJSON(t, e3, master); !strings.Contains(body, "could not be read") {
		t.Fatalf("a bad input must be said: %s", body)
	}
}
