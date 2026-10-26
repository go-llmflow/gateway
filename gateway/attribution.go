package gateway

import (
	"errors"
	"net/http"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Project attribution (#68). Coding clients do not say which project a
// request is for: Claude Code sends its session, agent and prompt ids but
// no project, and sends any header set in ANTHROPIC_CUSTOM_HEADERS
// (code.claude.com/docs/en/llm-gateway-connect.md); Continue sends the
// headers of requestOptions.headers. So a request may name its project in
// X-Llmflow-Project (attribution.project_header).
//
// The value is a project NAME, looked up in the key's own team only: a
// project of another team, let alone of another organisation, cannot be
// named at all. It must exist and be enabled; a key pinned to a project
// accepts only that one; for a key with an owner, the owner must still hold
// a stored member, team_admin, org_admin or proxy_admin role on the team
// (a JWT's team comes from its verified map, which is its membership).
// Every refusal is the same 403. Without the header: the key's project,
// else attribution.default_project when the team has it, else none.

var errProjectRefused = errors.New("project not allowed for this key")

// resolveProject returns the project a request is charged to.
func (s *Server) resolveProject(r *http.Request, k store.Key) (string, error) {
	name := r.Header.Get(s.cfg.Attribution.ProjectHeader)
	if len(r.Header.Values(s.cfg.Attribution.ProjectHeader)) > 1 {
		return "", errProjectRefused // two answers to one question
	}
	if name == "" {
		if k.Project != "" {
			return k.Project, nil
		}
		if d := s.cfg.Attribution.DefaultProject; d != "" && s.budget.hasProject(k.Team, d) {
			return d, nil
		}
		return "", nil
	}
	if !config.ValidName(name) {
		return "", errProjectRefused
	}
	if k.Project != "" && name != k.Project {
		return "", errProjectRefused
	}
	if !s.budget.hasProject(k.Team, name) {
		return "", errProjectRefused
	}
	if k.Owner != "" && !isJWTKey(k) && !s.budget.member(k.Owner, k.Team, k.Org) {
		return "", errProjectRefused
	}
	return name, nil
}

func isJWTKey(k store.Key) bool { return len(k.ID) > 4 && k.ID[:4] == "jwt-" }

// recordUser is the user a record is attributed to: a gateway user name,
// never a token. A key's owner is one; a JWT's username is kept when it is
// a valid gateway user name, else the synthetic key id (stable per issuer
// and subject) stands for the person.
func recordUser(k store.Key) string {
	if k.Owner == "" {
		return ""
	}
	if config.ValidUserName(k.Owner) {
		return k.Owner
	}
	if isJWTKey(k) {
		return k.ID
	}
	return ""
}
