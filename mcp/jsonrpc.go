// Package mcp implements the parts of the Model Context Protocol the gateway
// needs to sit between MCP clients and the tool servers of its
// configuration: JSON-RPC messages, the per-request metadata of revision
// 2026-07-28 and the initialize handshake of the legacy revisions, the
// Streamable HTTP binding (headers, Base64 sentinel values, x-mcp-header
// parameters, SSE response streams), and a client for tools/list and
// tools/call.
//
// References (opened while writing this package):
//   - https://modelcontextprotocol.io/specification/2026-07-28/basic.md
//   - https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning.md
//   - https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http.md
//   - https://modelcontextprotocol.io/specification/2026-07-28/server/discover.md
//   - https://modelcontextprotocol.io/specification/2026-07-28/server/tools.md
//   - https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle.md
//   - https://modelcontextprotocol.io/specification/2025-11-25/basic/transports.md
//   - https://www.jsonrpc.org/specification
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Protocol versions.
const (
	// Version is the modern revision: version, identity and capabilities
	// travel in every request's _meta.
	Version = "2026-07-28"
	// LegacyVersion is the newest revision with an initialize handshake.
	LegacyVersion = "2025-11-25"
)

// LegacyVersions are the initialize-based revisions served.
var LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// IsLegacy reports whether v is a served legacy revision.
func IsLegacy(v string) bool {
	for _, l := range LegacyVersions {
		if v == l {
			return true
		}
	}
	return false
}

// JSON-RPC and MCP error codes.
const (
	CodeParseError         = -32700
	CodeInvalidRequest     = -32600
	CodeMethodNotFound     = -32601
	CodeInvalidParams      = -32602
	CodeInternal           = -32603
	CodeHeaderMismatch     = -32020
	CodeMissingCapability  = -32021
	CodeUnsupportedVersion = -32022
)

// Reserved _meta keys.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// Errorf builds an Error.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Message is a JSON-RPC request or notification sent by a client.
type Message struct {
	// ID is the request's id as received (a JSON string or integer); nil
	// for a notification.
	ID     json.RawMessage
	Method string
	// Params are the request's params, by member; nil when absent.
	Params map[string]json.RawMessage
}

// IsNotification reports whether the message has no id.
func (m Message) IsNotification() bool { return m.ID == nil }

// MaxIDBytes bounds a request id.
const MaxIDBytes = 256

// ParseMessage parses one client message. Responses (no method) and batches
// are refused: a client sends neither in the revisions served.
func ParseMessage(b []byte) (Message, *Error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		if len(b) > 0 && b[0] == '[' {
			return Message{}, Errorf(CodeInvalidRequest, "batches are not supported")
		}
		return Message{}, Errorf(CodeParseError, "not a JSON object")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Message{}, Errorf(CodeParseError, "invalid JSON")
	}
	var m Message
	var version string
	if err := json.Unmarshal(raw["jsonrpc"], &version); err != nil || version != "2.0" {
		return Message{}, Errorf(CodeInvalidRequest, `"jsonrpc" must be "2.0"`)
	}
	if err := json.Unmarshal(raw["method"], &m.Method); err != nil || m.Method == "" || len(m.Method) > 128 {
		return Message{}, Errorf(CodeInvalidRequest, `"method" must be a non-empty string`)
	}
	if id, ok := raw["id"]; ok {
		if err := checkID(id); err != nil {
			return Message{}, Errorf(CodeInvalidRequest, "%v", err)
		}
		m.ID = append(json.RawMessage(nil), id...)
	}
	if p, ok := raw["params"]; ok {
		if err := json.Unmarshal(p, &m.Params); err != nil || m.Params == nil {
			return Message{}, Errorf(CodeInvalidRequest, `"params" must be an object`)
		}
	}
	return m, nil
}

// checkID accepts a string or an integer, never null (MCP narrows
// JSON-RPC here).
func checkID(id json.RawMessage) error {
	if len(id) > MaxIDBytes {
		return errors.New(`"id" too long`)
	}
	var s string
	// Unmarshal accepts null into a string: look at the token itself.
	if len(id) > 0 && id[0] == '"' && json.Unmarshal(id, &s) == nil {
		return nil
	}
	if _, err := strconv.ParseInt(string(id), 10, 64); err == nil {
		return nil
	}
	return errors.New(`"id" must be a string or an integer`)
}

// Meta is the per-request protocol metadata of a modern request.
type Meta struct {
	// Present: the request carries _meta with a protocol version (it is
	// modern).
	Present            bool
	ProtocolVersion    string
	ClientCapabilities json.RawMessage // a JSON object; nil when absent
	ClientInfo         json.RawMessage
	// Rest is every other _meta member (progressToken, traceparent…).
	Rest map[string]json.RawMessage
}

// RequestMeta reads params._meta. A _meta that is not an object, or a
// protocol version that is not a string, is malformed.
func (m Message) RequestMeta() (Meta, *Error) {
	var meta Meta
	raw, ok := m.Params["_meta"]
	if !ok {
		return meta, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return meta, Errorf(CodeInvalidParams, `"_meta" must be an object`)
	}
	if v, ok := fields[MetaProtocolVersion]; ok {
		if err := json.Unmarshal(v, &meta.ProtocolVersion); err != nil {
			return meta, Errorf(CodeInvalidParams, "%s must be a string", MetaProtocolVersion)
		}
		meta.Present = true
	}
	if c, ok := fields[MetaClientCapabilities]; ok {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(c, &obj); err != nil || obj == nil {
			return meta, Errorf(CodeInvalidParams, "%s must be an object", MetaClientCapabilities)
		}
		meta.ClientCapabilities = append(json.RawMessage(nil), c...)
	}
	meta.ClientInfo = fields[MetaClientInfo]
	for k, v := range fields {
		switch k {
		case MetaProtocolVersion, MetaClientCapabilities, MetaClientInfo:
		default:
			if meta.Rest == nil {
				meta.Rest = map[string]json.RawMessage{}
			}
			meta.Rest[k] = v
		}
	}
	return meta, nil
}

// StringParam reads a string member of params; ok is false when it is
// absent, err set when it is not a string.
func (m Message) StringParam(name string) (v string, ok bool, err *Error) {
	raw, present := m.Params[name]
	if !present {
		return "", false, nil
	}
	if json.Unmarshal(raw, &v) != nil {
		return "", false, Errorf(CodeInvalidParams, "%q must be a string", name)
	}
	return v, true, nil
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Result builds a response with a result.
func Result(id json.RawMessage, result any) ([]byte, error) {
	r, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Response{JSONRPC: "2.0", ID: id, Result: r})
}

// ErrorResponse builds an error response (id may be nil when the request's
// could not be read).
func ErrorResponse(id json.RawMessage, e *Error) []byte {
	b, err := json.Marshal(Response{JSONRPC: "2.0", ID: id, Error: e})
	if err != nil {
		// Data that does not marshal: drop it rather than fail.
		b, _ = json.Marshal(Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: e.Code, Message: e.Message}})
	}
	return b
}

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
