package config

import (
	"slices"

	"github.com/hashicorp/hcl/v2"
)

// DecisionReport schedules the monthly decision report (#97):
//
//	decision_report {
//	  destination = "reports"     # an export block: S3-compatible storage or a directory
//	  formats     = ["pdf", "xlsx"]
//	  day         = 2             # of the month, UTC, 1..28; the previous month's report
//	}
//
// The gateway writes no email: delivery is an object in the export (its
// write-once retention and audit trail are the ones of the other exports),
// from where a mail gateway, a bucket notification or a person takes it.
type DecisionReport struct {
	Destination string
	Formats     []string
	Day         int
}

// Decision report defaults.
const DefaultDecisionDay = 2

type decisionHCL struct {
	Destination      string    `hcl:"destination"`
	DestinationRange hcl.Range `hcl:"destination,attr_value_range"`
	Formats          []string  `hcl:"formats,optional"`
	FormatsRange     hcl.Range `hcl:"formats,attr_value_range"`
	Day              *int      `hcl:"day,optional"`
	DayRange         hcl.Range `hcl:"day,attr_value_range"`
}

func (v *validator) decisionReport(raw *fileHCL, c *Config) {
	d := raw.DecisionReport
	if d == nil {
		return
	}
	out := &DecisionReport{Destination: d.Destination, Day: DefaultDecisionDay, Formats: []string{"pdf", "xlsx"}}
	known := false
	for _, e := range raw.Exports {
		known = known || e.Name == d.Destination
	}
	if !known {
		v.errf(d.DestinationRange, "Unknown export", "decision_report: destination %q names no export block", d.Destination)
	}
	if d.Formats != nil {
		out.Formats = nil
		for _, f := range d.Formats {
			switch {
			case f != "pdf" && f != "xlsx":
				v.errf(d.FormatsRange, "Invalid format", "decision_report: format %q: want pdf or xlsx", f)
			case !slices.Contains(out.Formats, f):
				out.Formats = append(out.Formats, f)
			}
		}
		if len(out.Formats) == 0 {
			v.errf(d.FormatsRange, "Invalid formats", "decision_report: formats is empty")
		}
	}
	if d.Day != nil {
		if *d.Day < 1 || *d.Day > 28 {
			v.errf(d.DayRange, "Invalid day", "decision_report: day = %d: want 1..28", *d.Day)
		}
		out.Day = *d.Day
	}
	c.DecisionReport = out
}
