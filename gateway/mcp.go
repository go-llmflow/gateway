package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/mcp"
	"github.com/go-llmflow/gateway/store"
	"go.opentelemetry.io/otel/attribute"
)

// The MCP gateway: POST /mcp is one MCP server (Streamable HTTP, revision
// 2026-07-28, and the initialize-based 2025-11-25 for older clients) whose
// tools are those of the configured tool servers, named
// "<server>.<tool>". A client authenticates with its virtual key; its
// team's allow-list decides which tools it sees and may call; every call is
// written to the audit log before it is forwarded, and its outcome after.
// The gateway reaches only the URLs of its mcp_server blocks.

const (
	// maxMCPBody bounds a request to /mcp.
	maxMCPBody = 1 << 20
	// mcpToolsTTL is how long a server's tool list is reused.
	mcpToolsTTL = time.Minute
	// toolListsTTL is how long the teams' allow-lists are reused (an
	// administrator's change on this replica applies at once).
	toolListsTTL = 5 * time.Second
	// maxToolPatterns bounds a team's allow-list.
	maxToolPatterns = 256
)

type mcpServer struct {
	cfg    config.MCPServer
	client *mcp.Client

	mu     sync.Mutex
	at     time.Time
	tools  []mcp.Tool
	byName map[string]*mcp.Tool
}

// list returns the server's tools, from the cache when fresh.
func (m *mcpServer) list(ctx context.Context, now time.Time, onReject func(mcp.Rejected)) ([]mcp.Tool, map[string]*mcp.Tool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tools != nil && now.Sub(m.at) < mcpToolsTTL {
		return m.tools, m.byName, nil
	}
	tools, rejected, err := m.client.ListTools(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range rejected {
		onReject(r)
	}
	by := make(map[string]*mcp.Tool, len(tools))
	for i := range tools {
		by[tools[i].Name] = &tools[i]
	}
	if tools == nil {
		tools = []mcp.Tool{}
	}
	m.tools, m.byName, m.at = tools, by, now
	return tools, by, nil
}

type toolListCache struct {
	mu    sync.Mutex
	at    time.Time
	lists map[string][]string
}

// buildMCP creates a client per configured tool server.
func (s *Server) buildMCP() {
	if len(s.cfg.MCPServers) == 0 {
		return
	}
	hc := &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second},
		// A tool server's redirect would take the call, and its token,
		// to an address that is not in the configuration.
		CheckRedirect: mcp.NoRedirects,
	}
	s.mcpServers = map[string]*mcpServer{}
	for _, c := range s.cfg.MCPServers {
		s.mcpServers[c.Name] = &mcpServer{cfg: c, client: &mcp.Client{URL: c.URL, Token: s.secrets.MCPTokens[c.Name],
			Protocol: c.Protocol, Timeout: c.Timeout, MaxResponse: c.MaxResponseBytes, Info: gatewayInfo(), HTTP: hc}}
		s.mcpOrder = append(s.mcpOrder, c.Name)
	}
	s.toolLists = &toolListCache{}
}

func gatewayInfo() mcp.Implementation {
	v := "devel"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = bi.Main.Version
	}
	return mcp.Implementation{Name: "llmflow-gateway", Version: v}
}

// teamTools returns a team's tool patterns.
func (s *Server) teamTools(ctx context.Context, team string) ([]string, error) {
	c := s.toolLists
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lists == nil || s.now().Sub(c.at) >= toolListsTTL {
		ctx, cancel := context.WithTimeout(ctx, storeTimeout)
		defer cancel()
		l, err := s.st.TeamTools(ctx)
		if err != nil {
			return nil, err
		}
		c.lists, c.at = l, s.now()
	}
	return c.lists[team], nil
}

func (s *Server) invalidateToolLists() {
	if s.toolLists != nil {
		s.toolLists.mu.Lock()
		s.toolLists.lists = nil
		s.toolLists.mu.Unlock()
	}
}

// toolAllowed: "*", "<server>.*" or the exact "<server>.<tool>".
func toolAllowed(patterns []string, full string) bool {
	for _, p := range patterns {
		if p == "*" || p == full || strings.HasSuffix(p, ".*") && strings.HasPrefix(full, p[:len(p)-1]) {
			return true
		}
	}
	return false
}

func serverAllowed(patterns []string, server string) bool {
	for _, p := range patterns {
		if p == "*" || strings.HasPrefix(p, server+".") {
			return true
		}
	}
	return false
}

// mcpReq is one request being served.
type mcpReq struct {
	w      http.ResponseWriter
	msg    mcp.Message
	meta   mcp.Meta
	modern bool
}

func (q *mcpReq) write(status int, body []byte) {
	q.w.Header().Set("Content-Type", "application/json")
	q.w.Header().Set("Cache-Control", "no-store")
	q.w.WriteHeader(status)
	_, _ = q.w.Write(body)
}

func (q *mcpReq) fail(status int, e *mcp.Error) { q.write(status, mcp.ErrorResponse(q.msg.ID, e)) }

// result answers with a result object: modern results carry their type and
// the server's identity.
func (q *mcpReq) result(res map[string]any) {
	if q.modern {
		if _, ok := res["resultType"]; !ok {
			res["resultType"] = "complete"
		}
		res["_meta"] = map[string]any{mcp.MetaServerInfo: gatewayInfo()}
	} else {
		delete(res, "resultType")
	}
	b, err := mcp.Result(q.msg.ID, res)
	if err != nil {
		q.fail(http.StatusInternalServerError, mcp.Errorf(mcp.CodeInternal, "could not encode the result"))
		return
	}
	q.write(http.StatusOK, b)
}

// methodNotFound: 404 for a modern client (the binding says so); a legacy
// client reads 404 as "session expired", so it gets the error with 200.
func (q *mcpReq) methodNotFound() {
	st := http.StatusOK
	if q.modern {
		st = http.StatusNotFound
	}
	q.fail(st, mcp.Errorf(mcp.CodeMethodNotFound, "Method not found: %s", q.msg.Method))
}

func (s *Server) mcpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No GET stream and no sessions to DELETE (2026-07-28 removed both;
		// the legacy revisions allow a server without them).
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := &mcpReq{w: w}
	// DNS rebinding: an Origin header comes from a browser page. No
	// browser origin is allowed to drive the tool servers through a key.
	if r.Header.Get("Origin") != "" {
		q.fail(http.StatusForbidden, mcp.Errorf(mcp.CodeInvalidRequest, "requests from a browser origin are not accepted"))
		return
	}
	sctx, span := s.trace.startRequest(r, "/mcp")
	defer span.End()
	r = r.WithContext(sctx)
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	span.SetAttributes(attribute.String("llmflow.org", key.Org), attribute.String("llmflow.team", key.Team),
		attribute.String("llmflow.key", key.ID))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMCPBody))
	if err != nil {
		q.fail(http.StatusRequestEntityTooLarge, mcp.Errorf(mcp.CodeInvalidRequest, "request too large or unreadable"))
		return
	}
	msg, perr := mcp.ParseMessage(body)
	if perr != nil {
		q.fail(http.StatusBadRequest, perr)
		return
	}
	q.msg = msg
	if msg.IsNotification() {
		// notifications/initialized, notifications/cancelled: nothing to
		// do, the gateway keeps no session.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	span.SetAttributes(attribute.String("mcp.method.name", msg.Method))
	meta, merr := msg.RequestMeta()
	if merr != nil {
		q.fail(http.StatusBadRequest, merr)
		return
	}
	q.meta, q.modern = meta, meta.Present
	if !s.mcpCheckVersion(q, r) {
		return
	}
	switch msg.Method {
	case "initialize":
		if q.modern {
			q.methodNotFound()
			return
		}
		requested, _, _ := msg.StringParam("protocolVersion")
		v := mcp.LegacyVersion
		if mcp.IsLegacy(requested) {
			v = requested
		}
		q.result(map[string]any{"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": gatewayInfo(), "instructions": "Tools are named <server>.<tool>; your key's team decides which you may call."})
	case "server/discover":
		if !q.modern {
			q.methodNotFound()
			return
		}
		q.result(map[string]any{"supportedVersions": append([]string{mcp.Version}, mcp.LegacyVersions...),
			"capabilities": map[string]any{"tools": map[string]any{}}})
	case "ping":
		q.result(map[string]any{})
	case "tools/list":
		s.mcpListTools(r.Context(), q, key)
	case "tools/call":
		s.mcpCallTool(r.Context(), q, key, r)
	default:
		q.methodNotFound()
	}
}

// mcpCheckVersion validates the protocol version and, for a modern
// request, the headers mirroring the body.
func (s *Server) mcpCheckVersion(q *mcpReq, r *http.Request) bool {
	hv := r.Header.Get(mcp.HeaderProtocolVersion)
	if !q.modern {
		switch {
		case hv == mcp.Version:
			// A modern version without the required _meta fields.
			q.fail(http.StatusBadRequest, mcp.Errorf(mcp.CodeInvalidParams, "missing _meta %s and %s", mcp.MetaProtocolVersion, mcp.MetaClientCapabilities))
			return false
		case hv != "" && !mcp.IsLegacy(hv):
			q.fail(http.StatusBadRequest, &mcp.Error{Code: mcp.CodeUnsupportedVersion, Message: "Unsupported protocol version",
				Data: map[string]any{"supported": append([]string{mcp.Version}, mcp.LegacyVersions...), "requested": hv}})
			return false
		}
		return true
	}
	mismatch := func(format string, args ...any) bool {
		q.fail(http.StatusBadRequest, mcp.Errorf(mcp.CodeHeaderMismatch, format, args...))
		return false
	}
	if hv == "" || r.Header.Get(mcp.HeaderMethod) == "" {
		return mismatch("Header mismatch: %s and %s are required", mcp.HeaderProtocolVersion, mcp.HeaderMethod)
	}
	if hv != q.meta.ProtocolVersion {
		return mismatch("Header mismatch: %s does not match the body", mcp.HeaderProtocolVersion)
	}
	if r.Header.Get(mcp.HeaderMethod) != q.msg.Method {
		return mismatch("Header mismatch: %s does not match the body", mcp.HeaderMethod)
	}
	if q.meta.ProtocolVersion != mcp.Version {
		q.fail(http.StatusBadRequest, &mcp.Error{Code: mcp.CodeUnsupportedVersion, Message: "Unsupported protocol version",
			Data: map[string]any{"supported": append([]string{mcp.Version}, mcp.LegacyVersions...), "requested": q.meta.ProtocolVersion}})
		return false
	}
	if q.meta.ClientCapabilities == nil {
		q.fail(http.StatusBadRequest, mcp.Errorf(mcp.CodeInvalidParams, "missing _meta %s", mcp.MetaClientCapabilities))
		return false
	}
	var nameField string
	switch q.msg.Method {
	case "tools/call", "prompts/get":
		nameField = "name"
	case "resources/read":
		nameField = "uri"
	}
	if nameField != "" {
		body, _, err := q.msg.StringParam(nameField)
		if err != nil {
			q.fail(http.StatusBadRequest, err)
			return false
		}
		hn, ok := r.Header[http.CanonicalHeaderKey(mcp.HeaderName)]
		if !ok || len(hn) != 1 {
			return mismatch("Header mismatch: %s is required", mcp.HeaderName)
		}
		name, derr := mcp.DecodeHeaderValue(hn[0])
		if derr != nil || name != body {
			return mismatch("Header mismatch: %s does not match the body", mcp.HeaderName)
		}
	}
	return true
}

func (s *Server) mcpListTools(ctx context.Context, q *mcpReq, key store.Key) {
	if _, ok := q.msg.Params["cursor"]; ok {
		// One page is all there is: no cursor was ever handed out.
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInvalidParams, "invalid cursor"))
		return
	}
	patterns, err := s.teamTools(ctx, key.Team)
	if err != nil {
		s.log.Error("mcp: tool lists unavailable", "err", err)
		q.fail(http.StatusServiceUnavailable, mcp.Errorf(mcp.CodeInternal, "tool permissions unavailable"))
		return
	}
	out := []json.RawMessage{}
	for _, name := range s.mcpOrder {
		if !serverAllowed(patterns, name) {
			continue
		}
		srv := s.mcpServers[name]
		tools, _, err := srv.list(ctx, s.now(), s.toolRejected(name))
		if err != nil {
			// The other servers' tools are still listed; this one's are
			// missing until it answers.
			s.log.Warn("mcp: tool server unavailable", "server", name, "err", err)
			s.met.toolCalls.add(1, key.Team, name, "list_failed")
			continue
		}
		for _, t := range tools {
			full := name + "." + t.Name
			if !toolAllowed(patterns, full) {
				continue
			}
			if raw, ok := renameTool(t.Raw, full); ok {
				out = append(out, raw)
			}
		}
	}
	q.result(map[string]any{"tools": out})
}

func (s *Server) toolRejected(server string) func(mcp.Rejected) {
	return func(r mcp.Rejected) {
		s.log.Warn("mcp: tool left out", "server", server, "tool", r.Name, "reason", r.Reason)
	}
}

// renameTool sets a tool definition's name to the gateway's.
func renameTool(raw json.RawMessage, name string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	n, _ := json.Marshal(name)
	obj["name"] = n
	b, err := json.Marshal(obj)
	return b, err == nil
}

// toolCallMeta are the _meta members passed on to a tool server: trace
// context only (progressToken is not, the gateway answers in one JSON
// object).
var toolCallMeta = []string{"traceparent", "tracestate"}

func (s *Server) mcpCallTool(ctx context.Context, q *mcpReq, key store.Key, r *http.Request) {
	name, ok, perr := q.msg.StringParam("name")
	if perr != nil || !ok {
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInvalidParams, `"name" is required`))
		return
	}
	patterns, err := s.teamTools(ctx, key.Team)
	if err != nil {
		s.log.Error("mcp: tool lists unavailable", "err", err)
		q.fail(http.StatusServiceUnavailable, mcp.Errorf(mcp.CodeInternal, "tool permissions unavailable"))
		return
	}
	// A tool the team may not use answers exactly like one that does not
	// exist; the refusal is audited too.
	unknown := func() {
		after, _ := json.Marshal(map[string]any{"org": key.Org, "team": key.Team, "project": key.Project})
		s.appendAudit(ctx, store.AuditEntry{Kind: "tool", Actor: "key:" + key.ID, Roles: "[]", Action: "tool.denied",
			Target: name, Before: "null", After: string(after), Status: http.StatusForbidden})
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInvalidParams, "Unknown tool: %s", name))
	}
	srvName, toolName, found := strings.Cut(name, ".")
	srv := s.mcpServers[srvName]
	if !found || srv == nil || !toolAllowed(patterns, name) {
		s.met.toolCalls.add(1, key.Team, "", "denied")
		unknown()
		return
	}
	_, byName, err := srv.list(ctx, s.now(), s.toolRejected(srvName))
	if err != nil {
		s.log.Warn("mcp: tool server unavailable", "server", srvName, "err", err)
		s.met.toolCalls.add(1, key.Team, srvName, "unavailable")
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInternal, "tool server unavailable"))
		return
	}
	tool := byName[toolName]
	if tool == nil {
		s.met.toolCalls.add(1, key.Team, srvName, "denied")
		unknown()
		return
	}
	args, aerr := mcp.DecodeArguments(q.msg.Params["arguments"])
	if aerr != nil {
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInvalidParams, "%v", aerr))
		return
	}
	if err := s.limiter.admit(key.ID, key.RPM, key.TPM, key.MaxInFlight); err != nil {
		s.met.denied.add(1, key.Team, "rate_limit")
		q.fail(http.StatusTooManyRequests, mcp.Errorf(mcp.CodeInternal, "rate limit: %v", err))
		return
	}
	defer s.limiter.release(key.ID, 0)
	canon, _ := json.Marshal(args)
	sum := sha256.Sum256(canon)
	rid := requestID()
	// The call is recorded before it is made: a call the log cannot hold is
	// not made.
	actor := "key:" + key.ID
	call := map[string]any{"org": key.Org, "team": key.Team, "project": key.Project, "request_id": rid,
		"arguments_sha256": hex.EncodeToString(sum[:]), "arguments_bytes": len(canon)}
	if err := s.appendToolAudit(ctx, actor, "tool.call", name, call); err != nil {
		s.met.toolCalls.add(1, key.Team, srvName, "audit_failed")
		q.fail(http.StatusServiceUnavailable, mcp.Errorf(mcp.CodeInternal, "the audit log is unavailable: the call was not made"))
		return
	}
	cp := mcp.CallParams{Name: toolName, Arguments: args, Headers: tool.Headers}
	if q.modern {
		cp.Capabilities = q.meta.ClientCapabilities
		cp.InputResponses = q.msg.Params["inputResponses"]
		cp.RequestState = q.msg.Params["requestState"]
		for _, k := range toolCallMeta {
			if v, ok := q.meta.Rest[k]; ok {
				if cp.Meta == nil {
					cp.Meta = map[string]json.RawMessage{}
				}
				cp.Meta[k] = v
			}
		}
	}
	t0 := s.now()
	res, err := srv.client.CallTool(ctx, cp)
	outcome := map[string]any{"request_id": rid}
	defer func() {
		outcome["duration_ms"] = s.now().Sub(t0).Milliseconds()
		if err := s.appendToolAudit(context.WithoutCancel(ctx), actor, "tool.result", name, outcome); err != nil {
			s.log.Error("mcp: tool result not audited", "tool", name, "request_id", rid, "err", err)
		}
		st, _ := outcome["status"].(string)
		s.met.toolCalls.add(1, key.Team, srvName, st)
	}()
	if err != nil {
		var se *mcp.ErrServer
		if errors.As(err, &se) {
			outcome["status"], outcome["error"] = "server_error", clip(se.E.Message, 256)
			code := mcp.CodeInternal
			if se.E.Code == mcp.CodeInvalidParams {
				code = mcp.CodeInvalidParams
			}
			q.fail(http.StatusOK, &mcp.Error{Code: code, Message: "tool server: " + clip(se.E.Message, 4096)})
			return
		}
		outcome["status"], outcome["error"] = "unavailable", clip(err.Error(), 256)
		s.log.Warn("mcp: tool call failed", "tool", name, "err", err)
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInternal, "tool server unavailable"))
		return
	}
	outcome["result_bytes"] = len(res)
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(string(res)))
	dec.UseNumber()
	if dec.Decode(&obj) != nil || obj == nil {
		outcome["status"] = "malformed"
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInternal, "tool server: malformed result"))
		return
	}
	rt, _ := obj["resultType"].(string)
	switch rt {
	case "", "complete":
		outcome["status"] = "ok"
		if b, _ := obj["isError"].(bool); b {
			outcome["status"] = "tool_error"
		}
		obj["resultType"] = "complete"
	case "input_required":
		outcome["status"] = "input_required"
		if !q.modern {
			q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInternal, "the tool asks for input this client's protocol revision cannot provide"))
			return
		}
	default:
		outcome["status"] = "malformed"
		q.fail(http.StatusOK, mcp.Errorf(mcp.CodeInternal, "tool server: unknown result type"))
		return
	}
	q.result(obj)
}

// clip bounds a message from a tool server, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// appendToolAudit writes a tool entry and returns the error (the caller
// decides whether the call may go on).
func (s *Server) appendToolAudit(ctx context.Context, actor, action, target string, fields map[string]any) error {
	after, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err = s.auditLog.Append(ctx, store.AuditEntry{Kind: "tool", Actor: actor, Roles: "[]", Action: action, Target: target,
		Before: "null", After: string(after)})
	if err != nil {
		s.met.auditFailures.add(1, "tool")
	}
	return err
}

// ---- admin: team tool allow-lists ----

func (s *Server) getTeamTools(w http.ResponseWriter, r *http.Request, p *principal) {
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.teamView(t) {
		forbidden(w)
		return
	}
	all, err := s.st.TeamTools(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	listed := all[t.Name]
	if listed == nil {
		listed = []string{}
	}
	servers := append([]string{}, s.mcpOrder...)
	writeJSON(w, http.StatusOK, map[string]any{"team": t.Name, "tools": listed, "servers": servers})
}

// setTeamTools: an organisation administrator decides which tools a team
// may call (a team administrator may not widen their own team's reach).
func (s *Server) setTeamTools(w http.ResponseWriter, r *http.Request, p *principal) {
	var in struct {
		Tools []string `json:"tools"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if len(in.Tools) > maxToolPatterns {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "too many tool patterns")
		return
	}
	for _, pat := range in.Tools {
		if msg := s.toolPatternProblem(pat); msg != "" {
			apiError(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
	}
	t, ok := s.team(w, r, r.PathValue("team"))
	if !ok {
		return
	}
	if !p.orgAdmin(t.Org) {
		forbidden(w)
		return
	}
	if err := s.st.SetTeamTools(r.Context(), t.Name, in.Tools); err != nil {
		storeError(w, err)
		return
	}
	s.invalidateToolLists()
	s.log.Info("team tools set", "team", t.Name, "patterns", len(in.Tools), "by", p.name)
	s.getTeamTools(w, r, p)
}

// toolPatternProblem: "*", "<server>.*" or "<server>.<tool>", the server
// configured and the tool name safe.
func (s *Server) toolPatternProblem(pat string) string {
	if pat == "*" {
		return ""
	}
	srv, tool, ok := strings.Cut(pat, ".")
	switch {
	case !ok:
		return "tool pattern " + strconv.Quote(pat) + `: want "*", "<server>.*" or "<server>.<tool>"`
	case s.mcpServers[srv] == nil:
		return "tool pattern " + strconv.Quote(pat) + ": no mcp_server " + strconv.Quote(srv)
	case tool != "*" && !mcp.SafeToolName(tool):
		return "tool pattern " + strconv.Quote(pat) + ": invalid tool name"
	}
	return ""
}
