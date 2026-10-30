package gateway

import (
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/energy"
)

// Energy reconciliation (#97): the energy Slurm measured for the jobs of a
// partner's account (sacct ConsumedEnergyRaw, in joules, read from
// slurm.sacct_file) next to the energy the gateway attributed to the partner
// from the DCGM counters, per partner for the period of the report. A
// difference is shown as an absolute value and as a percentage of Slurm's
// figure; it is not an error by itself (Slurm counts a job's allocation, and
// "only in the case of an exclusive job allocation does this value reflect the
// job's real energy consumption", sacct.html; DCGM counts the GPUs), it is
// what a steering committee asks about when the two disagree.
//
// The sacct file must cover the report's period: its rows carry no dates, the
// operator chooses them (`sacct -S ... -E ...`).

// EnergyReconRow is one partner of the reconciliation.
type EnergyReconRow struct {
	Org string `json:"org"`
	// SlurmKWh is Slurm's energy for the partner's accounts; HasSlurm is false
	// when the sacct input holds nothing for it.
	SlurmKWh float64 `json:"slurm_kwh"`
	HasSlurm bool    `json:"has_slurm"`
	// DCGMKWh is the IT energy the gateway attributed to the partner.
	DCGMKWh float64 `json:"dcgm_kwh"`
	// DiffKWh is DCGM − Slurm; DiffPct that over Slurm, in percent (nil when
	// Slurm reports nothing to compare to).
	DiffKWh float64  `json:"diff_kwh"`
	DiffPct *float64 `json:"diff_pct"`
	// Compared is false when there is nothing to compare (no Slurm data for the
	// partner, or a file that does not cover the period): no difference is
	// given, rather than one against zero.
	Compared bool `json:"compared"`
}

// reconcileEnergy pairs the two measurements for the given partners, sorted.
func reconcileEnergy(slurmJoules map[string]float64, dcgmKWh map[string]float64) []EnergyReconRow {
	orgs := map[string]bool{}
	for o := range slurmJoules {
		orgs[o] = true
	}
	for o := range dcgmKWh {
		orgs[o] = true
	}
	out := make([]EnergyReconRow, 0, len(orgs))
	for o := range orgs {
		r := EnergyReconRow{Org: o, DCGMKWh: dcgmKWh[o]}
		if j, ok := slurmJoules[o]; ok {
			r.HasSlurm, r.SlurmKWh = true, energy.KWh(int64(math.Round(j*1000))) // J → mJ
		}
		if r.HasSlurm {
			r.Compared = true
			r.DiffKWh = r.DCGMKWh - r.SlurmKWh
			if r.SlurmKWh > 0 {
				p := 100 * r.DiffKWh / r.SlurmKWh
				r.DiffPct = &p
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Org < out[j].Org })
	return out
}

// sacctEnergy reads slurm.sacct_file and attributes each job's energy to
// [from, to) in proportion to the part of its run inside it, assuming constant
// power: joules per organisation (accounts mapped by slurm.accounts), and what
// the period got out of the file. nil without the setting.
func (s *Server) sacctEnergy(from, to time.Time) (map[string]float64, *energy.SacctReport, energy.Coverage, error) {
	if s.cfg.Slurm.SacctFile == "" {
		return nil, nil, energy.Coverage{}, nil
	}
	f, err := os.Open(s.cfg.Slurm.SacctFile)
	if err != nil {
		return nil, nil, energy.Coverage{}, err
	}
	defer f.Close()
	rep, err := energy.ParseSacct(f)
	if err != nil {
		return nil, nil, energy.Coverage{}, err
	}
	byAcct, cov := rep.Prorate(from, to)
	out := map[string]float64{}
	for acct, j := range byAcct {
		out[s.cfg.Slurm.OrgOfAccount(acct)] += j
	}
	return out, rep, cov, nil
}

// coverageText says what the report used of the file: the reader must be able
// to tell a file for this period from one that happens to be there.
func coverageText(c energy.Coverage, rep *energy.SacctReport) string {
	span := "no job"
	if !c.Earliest.IsZero() {
		span = "jobs from " + c.Earliest.Format("2006-01-02 15:04") + " to " + c.Latest.Format("2006-01-02 15:04") + " UTC"
	}
	return fmt.Sprintf("Slurm file: %d jobs, %s; %d used (%d only partly inside the period, prorated), %d outside the period, %d still running; %d job steps, %d without energy and %d never started left out.",
		c.Jobs, span, c.Used, c.Prorated, c.Outside, c.Running, rep.Steps, rep.NoEnergy, rep.NotStarted)
}

// reconAssumption is printed next to the table.
const reconAssumption = "A job's energy is attributed to the period in proportion to the share of its run inside it, assuming constant power."
