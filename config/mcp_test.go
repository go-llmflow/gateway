package config

import (
	"slices"
	"testing"
	"time"
)

func TestParseMCPServers(t *testing.T) {
	c, err := parse(valid + `
mcp_server "github" {
  url        = "https://mcp.example.org/github/mcp"
  token_file = "/etc/llmflow/mcp-github.token"
}
mcp_server "local_files" {
  url      = "http://10.0.0.5:8080/mcp"
  protocol = "2025-11-25"
  timeout  = "30s"
  max_response_bytes = 1048576
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MCPServers) != 2 {
		t.Fatalf("%+v", c.MCPServers)
	}
	g, l := c.MCPServers[0], c.MCPServers[1]
	if g.Protocol != "2026-07-28" || g.Timeout != time.Minute || g.MaxResponseBytes != 8<<20 || g.TokenFile == "" {
		t.Fatalf("%+v", g)
	}
	if l.Protocol != "2025-11-25" || l.Timeout != 30*time.Second || l.MaxResponseBytes != 1<<20 {
		t.Fatalf("%+v", l)
	}
	if !slices.Contains(c.SecretRefs(), "/etc/llmflow/mcp-github.token") {
		t.Fatal("token_file is not a secret reference")
	}
	for name, block := range map[string]string{
		"dot in name":       "mcp_server \"a.b\" {\n  url = \"https://x/mcp\"\n}\n",
		"duplicate":         "mcp_server \"a\" {\n  url = \"https://x/mcp\"\n}\nmcp_server \"a\" {\n  url = \"https://y/mcp\"\n}\n",
		"credentials":       "mcp_server \"a\" {\n  url = \"https://u:p@x/mcp\"\n}\n",
		"query":             "mcp_server \"a\" {\n  url = \"https://x/mcp?k=v\"\n}\n",
		"scheme":            "mcp_server \"a\" {\n  url = \"file:///etc/passwd\"\n}\n",
		"relative":          "mcp_server \"a\" {\n  url = \"/mcp\"\n}\n",
		"token over http":   "mcp_server \"a\" {\n  url = \"http://x/mcp\"\n  token_file = \"/t\"\n}\n",
		"protocol":          "mcp_server \"a\" {\n  url = \"https://x/mcp\"\n  protocol = \"2024-11-05\"\n}\n",
		"timeout":           "mcp_server \"a\" {\n  url = \"https://x/mcp\"\n  timeout = \"1h\"\n}\n",
		"response limit":    "mcp_server \"a\" {\n  url = \"https://x/mcp\"\n  max_response_bytes = 10\n}\n",
		"unknown attribute": "mcp_server \"a\" {\n  url = \"https://x/mcp\"\n  headers = {}\n}\n",
	} {
		if _, err := parse(valid + block); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parse(valid + "mcp_server \"a\" {\n  url = \"http://127.0.0.1:9/mcp\"\n  token_file = \"/t\"\n}\n"); err != nil {
		t.Fatalf("loopback http with a token: %v", err)
	}
}
