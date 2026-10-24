package gateway

import (
	"encoding/json"
	"strings"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Request policies: fields an inference request must carry (a `user` for
// attribution, a project in `metadata`, request tags) and size limits that
// replace the global ones for a team or a key. A key's own policy wins over
// its team's.

// policyFor returns the policy that applies to k, or nil.
func (s *Server) policyFor(k store.Key) *config.Policy {
	name := k.Policy
	if name == "" {
		name = s.teamPolicy[k.Team]
	}
	if name == "" {
		return nil
	}
	return s.policies[name]
}

// maxBody is the request body limit for k.
func (s *Server) maxBody(p *config.Policy) int64 {
	if p != nil && p.MaxBodyBytes > 0 {
		return p.MaxBodyBytes
	}
	return s.cfg.Limits.MaxBodyBytes
}

// maxResponse is the response body limit for k.
func (s *Server) maxResponse(p *config.Policy) int64 {
	if p != nil && p.MaxResponseBytes > 0 {
		return p.MaxResponseBytes
	}
	return s.cfg.Limits.MaxResponseBytes
}

// missingFields lists the policy's required fields the body lacks: absent,
// null, or an empty string, list or object. A body that is not a JSON
// object lacks them all.
func missingFields(body []byte, require []string) []string {
	if len(require) == 0 {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return require
	}
	var missing []string
	for _, path := range require {
		if !present(doc, path) {
			missing = append(missing, path)
		}
	}
	return missing
}

func present(doc map[string]any, path string) bool {
	var cur any = doc
	for _, name := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[name]; !ok {
			return false
		}
	}
	switch v := cur.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// policyRefusal is the message of a request refused by a policy, or "".
func policyRefusal(p *config.Policy, body []byte, tags []string) string {
	if p == nil {
		return ""
	}
	missing := missingFields(body, p.Require)
	if p.RequireTags && len(tags) == 0 {
		missing = append(missing, "a request tag")
	}
	if len(missing) == 0 {
		return ""
	}
	return "policy " + p.Name + " requires: " + strings.Join(missing, ", ")
}
