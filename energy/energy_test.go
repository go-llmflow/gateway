package energy

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

const dcgmSample = `# HELP DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION Total energy consumption since boot (in mJ).
# TYPE DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION counter
DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{gpu="0",UUID="GPU-aaa",device="nvidia0",Hostname="n1"} 1000000
DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{gpu="1",UUID="GPU-bbb",device="nvidia1",Hostname="n1"} 2500000
DCGM_FI_DEV_POWER_USAGE{gpu="0",UUID="GPU-aaa"} 310.5
`

func TestParseText(t *testing.T) {
	s, st, err := ParseText(strings.NewReader(dcgmSample), MetricEnergy)
	if err != nil || len(s) != 2 || st.Skipped != 1 || st.Malformed != 0 {
		t.Fatalf("got %v %+v %v", s, st, err)
	}
	if s[0].Labels["UUID"] != "GPU-aaa" || s[1].Value != 2500000 {
		t.Fatalf("labels/values: %+v", s)
	}
	// Escapes, timestamp, special values, malformed lines.
	in := "a{x=\"q\\\"\\\\\\n\",} +Inf 1700000000000\nbad line here\nb{x=} 1\nc 1 2 3\n"
	s, st, _ = ParseText(strings.NewReader(in))
	if len(s) != 1 || s[0].Labels["x"] != "q\"\\\n" || s[0].Timestamp != 1700000000000 || st.Malformed != 3 {
		t.Fatalf("got %+v %+v", s, st)
	}
}

func TestParseBounds(t *testing.T) {
	if _, _, err := ParseText(strings.NewReader(strings.Repeat("a", MaxLine+10))); err == nil {
		t.Fatal("long line accepted")
	}
	big := strings.Repeat("x 1\n", MaxBytes/4+10)
	if _, _, err := ParseText(strings.NewReader(big), "y"); err == nil {
		t.Fatal("oversize body accepted")
	}
	many := strings.Repeat("m 1\n", MaxSeries+1)
	if _, _, err := ParseText(strings.NewReader(many), "m"); err == nil {
		t.Fatal("too many series accepted")
	}
}

func FuzzParseText(f *testing.F) {
	f.Add(dcgmSample)
	f.Add("a{b=\"c\"} 1 2\n")
	f.Add("{{{{\n\"\\")
	f.Fuzz(func(t *testing.T, in string) {
		s, st, err := ParseText(strings.NewReader(in))
		if err == nil && st.Samples != len(s) {
			t.Fatal("count mismatch")
		}
		for _, x := range s {
			if x.Name == "" {
				t.Fatal("empty name")
			}
		}
	})
}

func FuzzParseSacct(f *testing.F) {
	f.Add("Account|ConsumedEnergyRaw\nlab-a|1000\n")
	f.Add("JobID|Account|ConsumedEnergy\n1|a|5K\n1.0|a|3\n")
	f.Fuzz(func(t *testing.T, in string) {
		r, err := ParseSacct(strings.NewReader(in))
		if err == nil {
			for _, j := range r.Joules {
				if j < 0 {
					t.Fatal("negative energy")
				}
			}
		}
	})
}

func at(sec int) time.Time { return time.Unix(1_700_000_000+int64(sec), 0) }

func rd(sec int, mj map[string]uint64) Reading { return Reading{At: at(sec), MJ: mj} }

func TestMeterCounterReset(t *testing.T) {
	var m Meter
	m.Observe(rd(0, map[string]uint64{"a": 1000, "b": 5000}))
	iv, ok := m.Observe(rd(10, map[string]uint64{"a": 1600, "b": 5200}))
	if !ok || iv.MJ != 800 || iv.Resets != 0 {
		t.Fatalf("%+v", iv)
	}
	// Driver reload on a: the counter restarts, the new value is the energy since.
	iv, _ = m.Observe(rd(20, map[string]uint64{"a": 300, "b": 5300}))
	if iv.Resets != 1 || iv.PerGPU["a"] != 300 || iv.PerGPU["b"] != 100 || iv.MJ != 400 {
		t.Fatalf("%+v", iv)
	}
}

func TestMeterMissingScrape(t *testing.T) {
	m := Meter{MaxGap: 15 * time.Second}
	m.Observe(rd(0, map[string]uint64{"a": 0}))
	m.Observe(rd(10, map[string]uint64{"a": 100}))
	// Two scrapes failed: the next interval spans 40 s and loses nothing.
	iv, _ := m.Observe(rd(50, map[string]uint64{"a": 900}))
	if iv.MJ != 800 || !iv.Gap || iv.Seconds() != 40 {
		t.Fatalf("%+v", iv)
	}
	// A GPU absent from a scrape reappears with its whole delta; a new one adds nothing.
	m.Observe(rd(60, map[string]uint64{}))
	iv, _ = m.Observe(rd(70, map[string]uint64{"a": 1000, "z": 77}))
	if iv.MJ != 100 || iv.New != 1 {
		t.Fatalf("%+v", iv)
	}
	// A replayed or backwards reading is ignored.
	if _, ok := m.Observe(rd(70, map[string]uint64{"a": 5000})); ok {
		t.Fatal("stale reading used")
	}
}

// Property: whatever the weights and the total, the parts sum to the total.
func TestApportionExact(t *testing.T) {
	err := quick.Check(func(total uint32, raw []uint16) bool {
		ws := make([]float64, len(raw))
		pos := false
		for i, r := range raw {
			ws[i] = float64(r) * 0.37
			pos = pos || r > 0
		}
		parts := Apportion(int64(total), ws)
		if total == 0 || !pos {
			return parts == nil
		}
		var s int64
		for i, p := range parts {
			if p < 0 || ws[i] == 0 && p != 0 {
				return false
			}
			s += p
		}
		return s == int64(total)
	}, &quick.Config{MaxCount: 3000})
	if err != nil {
		t.Fatal(err)
	}
}

// Property: attributed active energy + idle energy == measured energy, per
// interval, for random scrapes (resets and gaps included) and random work.
func TestAttributionSumsToMeasured(t *testing.T) {
	rng := rand.New(rand.NewSource(97))
	for n := 0; n < 500; n++ {
		var m Meter
		counters := map[string]uint64{"g0": 0, "g1": 0, "g2": 0}
		m.Observe(rd(0, copyMJ(counters)))
		sec := 0
		for step := 0; step < 20; step++ {
			sec += 5 + rng.Intn(60) // missing scrapes make some intervals longer
			for k := range counters {
				if rng.Intn(15) == 0 {
					counters[k] = uint64(rng.Intn(1000)) // reload
				} else {
					counters[k] += uint64(rng.Intn(4_000_000))
				}
			}
			iv, ok := m.Observe(rd(sec, copyMJ(counters)))
			if !ok {
				continue
			}
			var work []Work
			for i := rng.Intn(6); i > 0; i-- {
				work = append(work, Work{
					Consumer: Consumer{Org: string(rune('a' + rng.Intn(3))), User: string(rune('u' + rng.Intn(4)))},
					Prefill:  int64(rng.Intn(5000)), Decode: int64(rng.Intn(2000)),
				})
			}
			active, idle := iv.SplitIdle(float64(rng.Intn(120)))
			if active+idle != iv.MJ {
				t.Fatalf("split %d+%d != %d", active, idle, iv.MJ)
			}
			att, left := AttributeActive(active, work, Weights{Prefill: 1, Decode: 3})
			var sum int64
			for _, a := range att {
				sum += a.MJ
			}
			idle += left
			if sum+left != active {
				t.Fatalf("active %d attributed %d left %d", active, sum, left)
			}
			shares := map[string]float64{"a": 3, "b": 1, "c": 1}
			mode := []string{IdleByShare, IdleByUsage}[rng.Intn(2)]
			usage := map[string]float64{}
			for _, a := range att {
				usage[a.Org] += float64(a.MJ)
			}
			alloc, err := AllocateIdle(idle, mode, shares, usage)
			if err != nil {
				t.Fatal(err)
			}
			var ia int64
			for _, v := range alloc {
				ia += v
			}
			if ia != idle || sum+ia != iv.MJ {
				t.Fatalf("interval %d mJ: active %d + idle %d (%s)", iv.MJ, sum, ia, mode)
			}
		}
	}
}

func TestIdleUnallocated(t *testing.T) {
	a, _ := AllocateIdle(100, IdleByUsage, nil, nil)
	if a[""] != 100 {
		t.Fatalf("%v", a)
	}
	if _, err := AllocateIdle(1, "x", nil, nil); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestCost(t *testing.T) {
	p := Params{PUE: 1.4, PricePerKWh: 0.18, Currency: "EUR"}
	got := p.Cost(MJPerKWh * 10) // 10 kWh IT
	if d := got - 10*1.4*0.18; d > 1e-9 || d < -1e-9 {
		t.Fatalf("cost %v", got)
	}
}

func TestSacct(t *testing.T) {
	in := "JobID|Account|ConsumedEnergyRaw\n1|lab-a|1000\n1.batch|lab-a|900\n2|lab-a|\n3|lab-b|2500.5\n"
	r, err := ParseSacct(strings.NewReader(in))
	if err != nil || r.Joules["lab-a"] != 1000 || r.Joules["lab-b"] != 2500.5 || r.Jobs != 3 || r.NoEnergy != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"Account|ConsumedEnergy\na|5K\n", "x|y\n1|2\n", "Account|ConsumedEnergyRaw\na|-3\n", ""} {
		if _, err := ParseSacct(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestScrape(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/metrics":
			_, _ = w.Write([]byte(dcgmSample))
		case "/redir":
			http.Redirect(w, r, "/metrics", http.StatusFound)
		case "/empty":
			_, _ = w.Write([]byte("# nothing\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sc := Scraper{URL: srv.URL + "/metrics", Timeout: time.Second}
	r, err := sc.Scrape(context.Background(), at(0))
	if err != nil || r.MJ["GPU-aaa"] != 1000000 || len(r.MJ) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, p := range []string{"/redir", "/empty", "/nope"} {
		sc.URL = srv.URL + p
		if _, err := sc.Scrape(context.Background(), at(0)); err == nil {
			t.Fatalf("%s accepted", p)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "http://u:p@h/x", "http://h/x?q=1", "http:///x", "ftp://h"} {
		if ValidateURL(u) == nil {
			t.Fatalf("%q accepted", u)
		}
	}
}
