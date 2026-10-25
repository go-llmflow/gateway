package config

import (
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// MCPServer is a tool server the gateway exposes at /mcp. It is the only
// kind of address the MCP gateway reaches: clients name tools, never
// servers or URLs.
type MCPServer struct {
	// Name prefixes its tools ("<name>.<tool>"): [A-Za-z0-9_-]{1,32}.
	Name string
	URL  string
	// TokenFile is a secret reference to the bearer token sent to it.
	TokenFile string
	// Protocol is "2026-07-28" (per-request metadata) or "2025-11-25",
	// "2025-06-18", "2025-03-26" (initialize handshake).
	Protocol string
	Timeout  time.Duration
	// MaxResponseBytes bounds one response of the server.
	MaxResponseBytes int64
}

type mcpServerHCL struct {
	Name           string    `hcl:"name,label"`
	NameRange      hcl.Range `hcl:"name,label_range"`
	URL            string    `hcl:"url"`
	URLRange       hcl.Range `hcl:"url,attr_value_range"`
	TokenFile      *string   `hcl:"token_file,optional"`
	TokenRange     hcl.Range `hcl:"token_file,attr_value_range"`
	Protocol       *string   `hcl:"protocol,optional"`
	ProtocolRange  hcl.Range `hcl:"protocol,attr_value_range"`
	Timeout        *string   `hcl:"timeout,optional"`
	TimeoutRange   hcl.Range `hcl:"timeout,attr_value_range"`
	MaxResponse    *int64    `hcl:"max_response_bytes,optional"`
	MaxResponseRng hcl.Range `hcl:"max_response_bytes,attr_value_range"`
}

var mcpServerName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// MCPProtocols are the revisions a tool server may speak.
var MCPProtocols = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

// MaxMCPServers bounds the mcp_server blocks.
const MaxMCPServers = 64

func (v *validator) mcpServers(raw []mcpServerHCL) []MCPServer {
	var out []MCPServer
	names := map[string]bool{}
	if len(raw) > MaxMCPServers {
		v.errf(raw[MaxMCPServers].NameRange, "Too many mcp_server blocks", "at most %d", MaxMCPServers)
		return nil
	}
	for _, m := range raw {
		where := "mcp_server " + strconv.Quote(m.Name)
		if !mcpServerName.MatchString(m.Name) || names[m.Name] {
			v.errf(m.NameRange, "Invalid mcp_server name", "%s: want [A-Za-z0-9_-]{1,32}, declared once (it prefixes tool names with a dot)", where)
		}
		names[m.Name] = true
		s := MCPServer{Name: m.Name, URL: m.URL, TokenFile: strOr(m.TokenFile, ""), Protocol: strOr(m.Protocol, "2026-07-28"),
			Timeout: v.dur(m.Timeout, m.TimeoutRange, "timeout", 60*time.Second), MaxResponseBytes: 8 << 20}
		if m.MaxResponse != nil {
			s.MaxResponseBytes = *m.MaxResponse
			if s.MaxResponseBytes < 1024 || s.MaxResponseBytes > 64<<20 {
				v.errf(m.MaxResponseRng, "Invalid max_response_bytes", "%s: between 1 KiB and 64 MiB", where)
			}
		}
		if s.Timeout < time.Second || s.Timeout > 10*time.Minute {
			v.errf(m.TimeoutRange, "Invalid timeout", "%s: timeout between 1s and 10m", where)
		}
		known := false
		for _, p := range MCPProtocols {
			known = known || p == s.Protocol
		}
		if !known {
			v.errf(m.ProtocolRange, "Invalid protocol", "%s: protocol %q: want one of %v", where, s.Protocol, MCPProtocols)
		}
		if msg := mcpURLProblem(s.URL, s.TokenFile != ""); msg != "" {
			v.errf(m.URLRange, "Invalid url", "%s: %s", where, msg)
		}
		out = append(out, s)
	}
	return out
}

// mcpURLProblem: an absolute http(s) URL without credentials, query or
// fragment. A token is not sent in clear: with token_file the URL must be
// https, or http to a loopback address.
func mcpURLProblem(raw string, token bool) string {
	u, err := url.Parse(raw)
	switch {
	case err != nil || len(raw) > 2048:
		return "not a URL"
	case u.Scheme != "https" && u.Scheme != "http":
		return "want an http or https URL"
	case u.Host == "" || u.Opaque != "":
		return "want an absolute URL with a host"
	case u.User != nil:
		return "credentials in the URL: use token_file"
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return "no query or fragment"
	}
	if token && u.Scheme == "http" {
		if a, err := netip.ParseAddr(u.Hostname()); !(err == nil && a.IsLoopback()) && u.Hostname() != "localhost" {
			return "token_file needs https (http only to a loopback address)"
		}
	}
	return ""
}
