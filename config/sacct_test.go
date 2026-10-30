package config

import (
	"strings"
	"testing"
)

func TestSacctFile(t *testing.T) {
	c, err := parse(valid + "\nslurm {\n sacct_file = \"/var/lib/slurm/sacct.txt\"\n accounts = { lab-a = \"etabA\" }\n}\n")
	if err != nil || c.Slurm.SacctFile != "/var/lib/slurm/sacct.txt" {
		t.Fatalf("%+v %v", c.Slurm, err)
	}
	if _, err := parse(valid + "\nslurm {\n sacct_file = \"relative.txt\"\n}\n"); err == nil || !strings.Contains(err.Error(), "Invalid sacct_file") {
		t.Fatalf("a relative path: %v", err)
	}
}
