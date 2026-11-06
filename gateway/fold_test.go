package gateway

import "testing"

// A provisioned group is mapped to a team role by its display name. The names
// compare without case, ASCII only: strings.EqualFold also equates U+017F
// (long s) with "s" and U+212A (Kelvin) with "k", so a group the provisioner
// named "Lab Phyſics" received the members of "Lab Physics" their team role.
func TestAGroupNameIsMappedWithASCIICaseFoldingOnly(t *testing.T) {
	b := &scimBackend{s: &Server{cfg: scimConfig(t)}}
	for _, name := range []string{"Lab Physics", "LAB PHYSICS", "lab leads"} {
		if _, ok := b.mapping(name); !ok {
			t.Errorf("%q is not mapped", name)
		}
	}
	for _, name := range []string{"Lab Phyſics", "Kab Physics", "lab leadſ"} {
		if m, ok := b.mapping(name); ok {
			t.Errorf("%q was mapped to %+v", name, m)
		}
	}
}
