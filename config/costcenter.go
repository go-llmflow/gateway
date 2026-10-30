package config

import (
	"regexp"

	"github.com/hashicorp/hcl/v2"
)

// Cost centres (#97): the general-ledger codes a chargeback file carries.
//
//	org "etabA" {
//	  share       = 0.5
//	  cost_center = "GL-7001"
//	  team "t1" { lab = "genomics" }
//	  lab "genomics" { cost_center = "GL-7002" }
//	  project "t1" "p9" { cost_center = "GL-7003" }
//	}
//
// The most specific wins: project, then lab, then organisation.

type labCCHCL struct {
	Name            string    `hcl:"name,label"`
	NameRange       hcl.Range `hcl:"name,label_range"`
	CostCenter      string    `hcl:"cost_center"`
	CostCenterRange hcl.Range `hcl:"cost_center,attr_value_range"`
}

type projectCCHCL struct {
	Team            string    `hcl:"team,label"`
	Name            string    `hcl:"name,label"`
	NameRange       hcl.Range `hcl:"name,label_range"`
	CostCenter      string    `hcl:"cost_center"`
	CostCenterRange hcl.Range `hcl:"cost_center,attr_value_range"`
}

// costCenterRE is what a ledger code may look like: it ends up in a CSV file
// read by a finance system, so no separator, quote, space or control
// character, and no leading sign (a spreadsheet would read it as a formula).
var costCenterRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,31}$`)

// Adoption privacy bounds (#97): the figures are aggregates, and an aggregate
// of one person is that person.
const (
	DefaultAdoptionMinUsers = 5
	MinAdoptionMinUsers     = 2
)

func (v *validator) adoptionPrivacy(raw *fileHCL, c *Config) {
	c.AdoptionMinUsers = DefaultAdoptionMinUsers
	if raw.AdoptionMinUsers == nil {
		return
	}
	k := *raw.AdoptionMinUsers
	if k < MinAdoptionMinUsers || k > 1000 {
		v.errf(raw.AdoptionMinRange, "Invalid adoption_min_users", "adoption_min_users = %d: want %d..1000 (a figure over one person is that person)", k, MinAdoptionMinUsers)
		return
	}
	c.AdoptionMinUsers = k
}

// ValidCostCenter reports whether s is an acceptable general-ledger code.
func ValidCostCenter(s string) bool { return costCenterRE.MatchString(s) }

func (v *validator) costCenters(org *Org, o orgHCL) {
	check := func(code string, r hcl.Range) bool {
		if !ValidCostCenter(code) {
			v.errf(r, "Invalid cost_center", "cost_center = %q: want 1 to 32 characters of letters, digits, _ . / -, starting with a letter or digit", code)
			return false
		}
		return true
	}
	if o.CostCenter != nil && check(*o.CostCenter, o.CostCenterRange) {
		org.CostCenter = *o.CostCenter
	}
	labs := map[string]bool{}
	for _, l := range org.Labs {
		labs[l] = true
	}
	for _, l := range o.LabCCs {
		switch {
		case !labs[l.Name]:
			v.errf(l.NameRange, "Unknown lab", "org %q: lab %q is not the lab of any of its teams", org.Name, l.Name)
		case org.LabCostCenter[l.Name] != "":
			v.errf(l.NameRange, "Duplicate lab", "org %q: lab %q has two cost_center blocks", org.Name, l.Name)
		case check(l.CostCenter, l.CostCenterRange):
			if org.LabCostCenter == nil {
				org.LabCostCenter = map[string]string{}
			}
			org.LabCostCenter[l.Name] = l.CostCenter
		}
	}
	teams := map[string]bool{}
	for _, t := range org.Teams {
		teams[t] = true
	}
	for _, p := range o.ProjectCCs {
		key := [2]string{p.Team, p.Name}
		switch {
		case !teams[p.Team]:
			v.errf(p.NameRange, "Unknown team", "org %q: project %q names team %q, which is not one of its teams", org.Name, p.Name, p.Team)
		case !ValidName(p.Name):
			v.errf(p.NameRange, "Invalid project name", "%q: want [A-Za-z0-9_.-]{1,64}", p.Name)
		case org.ProjectCostCenter[key] != "":
			v.errf(p.NameRange, "Duplicate project", "org %q: project %q of team %q has two cost_center blocks", org.Name, p.Name, p.Team)
		case check(p.CostCenter, p.CostCenterRange):
			if org.ProjectCostCenter == nil {
				org.ProjectCostCenter = map[[2]string]string{}
			}
			org.ProjectCostCenter[key] = p.CostCenter
		}
	}
}
