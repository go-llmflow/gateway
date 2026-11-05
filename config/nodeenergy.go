package config

import (
	"math"

	"github.com/go-llmflow/gateway/energy"
	"github.com/hashicorp/hcl/v2"
)

// Node energy beyond the GPUs (fluids { node_overhead_factor, meter } and the
// upstream's redfish_* attributes).
//
// DCGM's DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION is the GPUs' energy only: the
// CPUs, memory, NVSwitch, NICs and fans of the node are not in it. Two ways to
// meter the rest, per node (the upstreams sharing a dcgm_url):
//
//   - redfish_url, the node's BMC Chassis EnvironmentMetrics resource (DMTF
//     DSP2046, schema EnvironmentMetrics v1.3.1): its EnergykWh.Reading, a
//     counter of the total energy in kWh. A counter, not sampled watts, so
//     what the node used between two scrapes is exact. The node's energy minus
//     its GPUs' is the overhead, split like the GPUs' energy.
//   - node_overhead_factor (≥ 1, calibrated by the operator): the fallback
//     when no BMC is configured or it could not be read in that interval:
//     overhead = GPU energy × (factor - 1).

// MaxNodeOverhead bounds node_overhead_factor.
const MaxNodeOverhead = 5

// SubMeter is a monthly reading of the racks' sub-meter in kWh, for a pool
// ("" the whole platform).
type SubMeter struct {
	Month string
	Pool  string
	KWh   float64
}

type subMeterHCL struct {
	Month      string    `hcl:"month,label"`
	MonthRange hcl.Range `hcl:"month,label_range"`
	KWh        float64   `hcl:"kwh"`
	KWhRange   hcl.Range `hcl:"kwh,attr_value_range"`
	Pool       *string   `hcl:"pool,optional"`
	PoolRange  hcl.Range `hcl:"pool,attr_value_range"`
}

// nodeEnergy validates the overhead factor and the sub-meter readings.
func (v *validator) nodeEnergy(f *fluidsHCL, out *Fluids, c *Config) {
	out.NodeOverheadFactor = 1
	if f.NodeOverhead != nil {
		x := *f.NodeOverhead
		if math.IsNaN(x) || x < 1 || x > MaxNodeOverhead {
			v.errf(f.NodeOverRange, "Invalid node_overhead_factor", "node_overhead_factor = %v: want 1..%v", x, float64(MaxNodeOverhead))
		} else {
			out.NodeOverheadFactor = x
		}
	}
	type key struct{ month, pool string }
	seen := map[key]bool{}
	for _, m := range f.Meters {
		if !monthRE.MatchString(m.Month) {
			v.errf(m.MonthRange, "Invalid meter", "meter %q: want a month, YYYY-MM", m.Month)
			continue
		}
		if math.IsNaN(m.KWh) || m.KWh < 0 || m.KWh > 1e9 {
			v.errf(m.KWhRange, "Invalid kwh", "meter %q: kwh = %v: want 0..1e9", m.Month, m.KWh)
			continue
		}
		pool := strOr(m.Pool, "")
		if pool != "" {
			known := false
			for _, p := range c.Pools {
				known = known || p.Name == pool
			}
			if !known {
				v.errf(m.PoolRange, "Unknown pool", "meter %q: pool %q is not declared", m.Month, pool)
				continue
			}
		}
		if seen[key{m.Month, pool}] {
			v.errf(m.MonthRange, "Duplicate meter", "meter %q for pool %q is declared twice", m.Month, pool)
			continue
		}
		seen[key{m.Month, pool}] = true
		out.Meters = append(out.Meters, SubMeter{Month: m.Month, Pool: pool, KWh: m.KWh})
	}
}

// upstreamNodeEnergy validates the BMC attributes of an upstream.
func (v *validator) upstreamNodeEnergy(u upstreamHCL, out *Upstream) {
	if u.NodeOverheadFactor != nil {
		x := *u.NodeOverheadFactor
		if math.IsNaN(x) || x < 1 || x > MaxNodeOverhead {
			v.errf(u.NodeOverheadRange, "Invalid node_overhead_factor", "node_overhead_factor = %v: want 1..%v", x, float64(MaxNodeOverhead))
		} else {
			out.NodeOverheadFactor = x
		}
	}
	if u.RedfishURL == nil {
		if u.RedfishUser != nil || u.RedfishPasswordFile != nil || u.RedfishCAFile != nil {
			v.errf(u.DefRange, "Missing redfish_url", "redfish_user, redfish_password_file and redfish_ca_file need redfish_url")
		}
		return
	}
	if out.DcgmURL == "" {
		v.errf(u.RedfishURLRange, "Missing dcgm_url", "redfish_url meters the energy beyond the GPUs of a node: name the node's dcgm_url too")
		return
	}
	if err := energy.ValidateRedfishURL(*u.RedfishURL); err != nil {
		v.errf(u.RedfishURLRange, "Invalid redfish_url", "%v", err)
		return
	}
	if u.RedfishUser == nil || *u.RedfishUser == "" || u.RedfishPasswordFile == nil || *u.RedfishPasswordFile == "" {
		v.errf(u.RedfishURLRange, "Missing credentials", "redfish_url needs redfish_user and redfish_password_file (a secret reference): a BMC is never read without authentication")
		return
	}
	out.RedfishURL, out.RedfishUser, out.RedfishPasswordFile = *u.RedfishURL, *u.RedfishUser, *u.RedfishPasswordFile
	out.RedfishCAFile = strOr(u.RedfishCAFile, "")
}

// redfishNodes refuses two upstreams of one node (one dcgm_url) that name
// different BMCs.
func (v *validator) redfishNodes(raw *fileHCL, c *Config) {
	bmc := map[string]string{}
	for i, g := range c.ModelGroups {
		for j, u := range g.Upstreams {
			if u.DcgmURL == "" || u.RedfishURL == "" {
				continue
			}
			if first, ok := bmc[u.DcgmURL]; ok && first != u.RedfishURL && i < len(raw.Models) && j < len(raw.Models[i].Upstreams) {
				v.errf(raw.Models[i].Upstreams[j].RedfishURLRange, "Conflicting redfish_url", "the node %s has two BMCs: %s and %s", redactDCGM(u.DcgmURL), redactDCGM(first), redactDCGM(u.RedfishURL))
				continue
			}
			bmc[u.DcgmURL] = u.RedfishURL
		}
	}
}
