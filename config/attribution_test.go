package config

import (
	"strings"
	"testing"
)

// #68: team "t" { lab = "x" } and the attribution block.
func TestLabAndAttribution(t *testing.T) {
	src := strings.Replace(valid, `team "etabA" {}`, `team "etabA" { lab = "physics" }`, 1) + `
attribution {
  project_header  = "X-Cost-Center"
  default_project = "general"
}
`
	c, err := Parse([]byte(src), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if c.LabOf("etabA") != "physics" || c.LabOf("etabB") != "" {
		t.Fatalf("labs: %q %q", c.LabOf("etabA"), c.LabOf("etabB"))
	}
	if c.Attribution.ProjectHeader != "X-Cost-Center" || c.Attribution.DefaultProject != "general" {
		t.Fatalf("%+v", c.Attribution)
	}
	// Defaults: existing configurations are unchanged.
	c, err = Parse([]byte(valid), "t.hcl")
	if err != nil || c.Attribution.ProjectHeader != DefaultProjectHeader || c.Attribution.DefaultProject != "" {
		t.Fatalf("%+v %v", c.Attribution, err)
	}
	for name, s := range map[string]struct{ src, want string }{
		"bad lab":     {strings.Replace(valid, `team "etabA" {}`, `team "etabA" { lab = "a b" }`, 1), "Invalid lab name"},
		"bad header":  {valid + "attribution {\n  project_header = \"X Bad\"\n}\n", "Invalid project_header"},
		"auth header": {valid + "attribution {\n  project_header = \"authorization\"\n}\n", "Invalid project_header"},
		"bad default": {valid + "attribution {\n  default_project = \"../x\"\n}\n", "Invalid default_project"},
	} {
		if _, err := Parse([]byte(s.src), "t.hcl"); err == nil || !strings.Contains(err.Error(), s.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// #68 part 4: the slurm block.
func TestSlurmBlock(t *testing.T) {
	c, err := Parse([]byte(valid+"slurm {\n  sreport_file = \"/var/lib/llmflow/sreport.txt\"\n  accounts = { \"phys-a\" = \"etabA\" }\n}\n"), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if c.Slurm.SreportFile != "/var/lib/llmflow/sreport.txt" || c.Slurm.OrgOfAccount("phys-a") != "etabA" || c.Slurm.OrgOfAccount("etabB") != "etabB" {
		t.Fatalf("%+v", c.Slurm)
	}
	for name, s := range map[string]struct{ src, want string }{
		"relative": {valid + "slurm {\n  sreport_file = \"sreport.txt\"\n}\n", "Invalid sreport_file"},
		"bad org":  {valid + "slurm {\n  accounts = { \"a\" = \"../x\" }\n}\n", "Invalid accounts"},
	} {
		if _, err := Parse([]byte(s.src), "t.hcl"); err == nil || !strings.Contains(err.Error(), s.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
