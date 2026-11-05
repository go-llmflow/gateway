package config

import (
	"math"

	"github.com/hashicorp/hcl/v2"
)

// Billing terms that are not energy: the organisation's markup, the prices of
// the metered network and storage. See Fluids for the rest.

// MaxMarkup bounds an organisation's markup (a fraction: 1 doubles the bill).
const MaxMarkup = 10

type orgBillingHCL struct {
	Markup      *float64  `hcl:"markup,optional"`
	MarkupRange hcl.Range `hcl:"markup,attr_value_range"`
}

// orgBilling validates the markup of an organisation.
func (v *validator) orgBilling(org *Org, o orgHCL) {
	b := o.Billing
	if b == nil || b.Markup == nil {
		return
	}
	m := *b.Markup
	if math.IsNaN(m) || m < 0 || m > MaxMarkup {
		v.errf(b.MarkupRange, "Invalid markup", "org %q billing markup = %v: want 0..%v (a fraction: 0.15 adds 15 %%)", org.Name, m, MaxMarkup)
		return
	}
	org.Markup = m
}

// MarkupOf returns the organisation's markup (0 when none).
func (c *Config) MarkupOf(org string) float64 {
	for i := range c.Orgs {
		if c.Orgs[i].Name == org {
			return c.Orgs[i].Markup
		}
	}
	return 0
}
