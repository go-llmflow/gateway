package gateway

import (
	"math"
	"os"
	"sort"

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
		r.DiffKWh = r.DCGMKWh - r.SlurmKWh
		if r.HasSlurm && r.SlurmKWh > 0 {
			p := 100 * r.DiffKWh / r.SlurmKWh
			r.DiffPct = &p
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Org < out[j].Org })
	return out
}

// sacctEnergy reads slurm.sacct_file: joules per organisation (accounts mapped
// by slurm.accounts); nil, nil without the setting.
func (s *Server) sacctEnergy() (map[string]float64, error) {
	if s.cfg.Slurm.SacctFile == "" {
		return nil, nil
	}
	f, err := os.Open(s.cfg.Slurm.SacctFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rep, err := energy.ParseSacct(f)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for acct, j := range rep.Joules {
		out[s.cfg.Slurm.OrgOfAccount(acct)] += j
	}
	return out, nil
}
