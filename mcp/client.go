package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client talks to one tool server over Streamable HTTP. Its URL comes from
// the configuration only: no method takes a URL, and redirects are refused,
// so a server cannot send the gateway (and its token) anywhere else.
type Client struct {
	URL string
	// Token is sent as a bearer token when set.
	Token string
	// Protocol is Version (modern, per-request metadata) or a legacy
	// revision (initialize handshake, optional session).
	Protocol string
	Timeout  time.Duration
	// MaxResponse bounds a response body.
	MaxResponse int64
	Info        Implementation
	HTTP        *http.Client

	ids atomic.Uint64

	mu      sync.Mutex
	inited  bool
	session string
	version string // negotiated legacy version
}

// NoRedirects is the CheckRedirect of a client that follows none.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ErrServer wraps an error the tool server returned (a JSON-RPC error).
type ErrServer struct{ E *Error }

func (e *ErrServer) Error() string { return "tool server: " + e.E.Message }

func (c *Client) legacy() bool { return c.Protocol != Version }

func (c *Client) nextID() string { return "llmflow-" + strconv.FormatUint(c.ids.Add(1), 10) }

// call sends one request and returns its result. caps are the client
// capabilities to declare (modern only; "{}" when nil), extra the
// Mcp-Param-* headers.
func (c *Client) call(ctx context.Context, method string, params map[string]any, name string, caps json.RawMessage, extra map[string]string) (json.RawMessage, error) {
	if c.legacy() {
		if err := c.initialize(ctx); err != nil {
			return nil, err
		}
	}
	res, status, err := c.post(ctx, method, params, name, caps, extra)
	if c.legacy() && status == http.StatusNotFound {
		// Legacy: 404 on a request with a session means it expired; start
		// a new one, once.
		c.mu.Lock()
		hadSession := c.session != ""
		c.inited, c.session = false, ""
		c.mu.Unlock()
		if hadSession {
			if err := c.initialize(ctx); err != nil {
				return nil, err
			}
			res, _, err = c.post(ctx, method, params, name, caps, extra)
		}
	}
	return res, err
}

// post sends one request; status is the HTTP status (0 when none).
func (c *Client) post(ctx context.Context, method string, params map[string]any, name string, caps json.RawMessage, extra map[string]string) (json.RawMessage, int, error) {
	if params == nil {
		params = map[string]any{}
	}
	if !c.legacy() {
		if caps == nil {
			caps = json.RawMessage("{}")
		}
		meta := map[string]any{MetaProtocolVersion: Version, MetaClientInfo: c.Info, MetaClientCapabilities: caps}
		if m, ok := params["_meta"].(map[string]json.RawMessage); ok {
			for k, v := range m {
				if _, set := meta[k]; !set {
					meta[k] = v
				}
			}
		}
		params["_meta"] = meta
	}
	id := c.nextID()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.send(ctx, body, method, name, extra)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	r, err := ReadResponse(resp.Header.Get("Content-Type"), resp.Body, id, c.MaxResponse)
	switch {
	case err == nil && r.Error != nil:
		return nil, resp.StatusCode, &ErrServer{E: r.Error}
	case err == nil && resp.StatusCode == http.StatusOK:
		return r.Result, resp.StatusCode, nil
	case err == nil:
		return nil, resp.StatusCode, fmt.Errorf("tool server: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, resp.StatusCode, fmt.Errorf("tool server: HTTP %d", resp.StatusCode)
	default:
		return nil, resp.StatusCode, err
	}
}

// send POSTs a message with the binding's headers.
func (c *Client) send(ctx context.Context, body []byte, method, name string, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		h.Set("Authorization", "Bearer "+c.Token)
	}
	if c.legacy() {
		c.mu.Lock()
		v, sess := c.version, c.session
		c.mu.Unlock()
		if v != "" {
			h.Set(HeaderProtocolVersion, v)
		}
		if sess != "" {
			h.Set(HeaderSessionID, sess)
		}
	} else {
		h.Set(HeaderProtocolVersion, Version)
		h.Set(HeaderMethod, method)
		if name != "" {
			h.Set(HeaderName, EncodeHeaderValue(name))
		}
		for k, v := range extra {
			h.Set(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("tool server: HTTP %d (redirects are not followed)", resp.StatusCode)
	}
	return resp, nil
}

// initialize runs the legacy handshake once (again after a session
// expired).
func (c *Client) initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inited {
		return nil
	}
	c.version, c.session = "", ""
	id := c.nextID()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{"protocolVersion": c.Protocol, "capabilities": map[string]any{}, "clientInfo": c.Info}})
	resp, err := c.sendLocked(ctx, body, "")
	if err != nil {
		return err
	}
	r, rerr := ReadResponse(resp.Header.Get("Content-Type"), resp.Body, id, c.MaxResponse)
	resp.Body.Close()
	if rerr != nil {
		return fmt.Errorf("tool server: initialize: HTTP %d: %v", resp.StatusCode, rerr)
	}
	if r.Error != nil {
		return &ErrServer{E: r.Error}
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || !IsLegacy(res.ProtocolVersion) {
		return fmt.Errorf("tool server: initialize: protocol version %q not supported", res.ProtocolVersion)
	}
	sess := resp.Header.Get(HeaderSessionID)
	for i := 0; i < len(sess); i++ {
		if sess[i] < 0x21 || sess[i] > 0x7e {
			return errors.New("tool server: invalid session id")
		}
	}
	if len(sess) > 1024 {
		return errors.New("tool server: session id too long")
	}
	c.version, c.session = res.ProtocolVersion, sess
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	resp, err = c.sendLocked(ctx, note, sess)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tool server: notifications/initialized: HTTP %d", resp.StatusCode)
	}
	c.inited = true
	return nil
}

// sendLocked is send for the handshake (c.mu held).
func (c *Client) sendLocked(ctx context.Context, body []byte, session string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.version != "" {
		req.Header.Set(HeaderProtocolVersion, c.version)
	}
	if session != "" {
		req.Header.Set(HeaderSessionID, session)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("tool server: HTTP %d (redirects are not followed)", resp.StatusCode)
	}
	return resp, nil
}

// Tool is one tool of a server, as listed.
type Tool struct {
	Name string
	// Raw is the tool's definition as the server sent it.
	Raw json.RawMessage
	// Headers are its x-mcp-header parameters.
	Headers []HeaderParam
}

// Limits on a listing.
const (
	MaxToolPages = 20
	MaxTools     = 1000
)

// Rejected is a tool left out of a listing, and why.
type Rejected struct {
	Name   string
	Reason string
}

// ListTools reads every page of tools/list. Tools with an unsafe name or an
// invalid x-mcp-header annotation are left out (and returned in rejected).
func (c *Client) ListTools(ctx context.Context) (tools []Tool, rejected []Rejected, err error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cursor := ""
	for page := 0; page < MaxToolPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx, "tools/list", params, "", nil, nil)
		if err != nil {
			return nil, nil, err
		}
		var lr struct {
			ResultType string            `json:"resultType"`
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &lr); err != nil {
			return nil, nil, errors.New("tool server: malformed tools/list result")
		}
		if lr.ResultType != "" && lr.ResultType != "complete" {
			return nil, nil, fmt.Errorf("tool server: tools/list result type %q", lr.ResultType)
		}
		for _, raw := range lr.Tools {
			var t struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				rejected = append(rejected, Rejected{Reason: "malformed tool definition"})
				continue
			}
			if !SafeToolName(t.Name) {
				rejected = append(rejected, Rejected{Name: t.Name, Reason: "name outside [A-Za-z0-9_.-]{1,128}"})
				continue
			}
			hp, err := HeaderParams(t.InputSchema)
			if err != nil {
				rejected = append(rejected, Rejected{Name: t.Name, Reason: err.Error()})
				continue
			}
			if len(tools) == MaxTools {
				return nil, nil, fmt.Errorf("tool server: more than %d tools", MaxTools)
			}
			tools = append(tools, Tool{Name: t.Name, Raw: raw, Headers: hp})
		}
		if lr.NextCursor == "" {
			return tools, rejected, nil
		}
		cursor = lr.NextCursor
	}
	return nil, nil, fmt.Errorf("tool server: more than %d pages of tools", MaxToolPages)
}

// SafeToolName: the characters and length the specification recommends.
// Names outside them are not exposed (they could not travel in Mcp-Name
// without encoding, and a gateway prefix keeps them unambiguous only if
// "." separates).
func SafeToolName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// CallParams are the parts of a tools/call the gateway forwards.
type CallParams struct {
	Name      string
	Arguments map[string]any
	// Headers are the tool's x-mcp-header parameters.
	Headers []HeaderParam
	// InputResponses and RequestState continue a multi round-trip request
	// (modern only).
	InputResponses json.RawMessage
	RequestState   json.RawMessage
	// Capabilities are the calling client's (modern only).
	Capabilities json.RawMessage
	// Meta are other _meta members to pass on (progressToken is not: the
	// gateway answers in one JSON object, without progress).
	Meta map[string]json.RawMessage
}

// CallTool calls a tool and returns its result object.
func (c *Client) CallTool(ctx context.Context, p CallParams) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	params := map[string]any{"name": p.Name, "arguments": p.Arguments}
	var extra map[string]string
	if !c.legacy() {
		var err error
		if extra, err = ParamHeaders(p.Headers, p.Arguments); err != nil {
			return nil, &ErrServer{E: Errorf(CodeInvalidParams, "%v", err)}
		}
		if p.InputResponses != nil {
			params["inputResponses"] = p.InputResponses
		}
		if p.RequestState != nil {
			params["requestState"] = p.RequestState
		}
		if len(p.Meta) > 0 {
			params["_meta"] = p.Meta
		}
	}
	return c.call(ctx, "tools/call", params, p.Name, p.Capabilities, extra)
}
