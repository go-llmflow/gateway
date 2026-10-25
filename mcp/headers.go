package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Streamable HTTP request headers.
const (
	HeaderProtocolVersion = "MCP-Protocol-Version"
	HeaderMethod          = "Mcp-Method"
	HeaderName            = "Mcp-Name"
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderParamPrefix     = "Mcp-Param-"
)

const (
	sentinelPrefix = "=?base64?"
	sentinelSuffix = "?="
)

// MaxHeaderValue bounds a mirrored value (before encoding).
const MaxHeaderValue = 4096

// EncodeHeaderValue returns v as a header value: as is when it is plain
// visible ASCII (inner spaces allowed), else in the Base64 sentinel form
// "=?base64?…?=" of its UTF-8 bytes. A plain value that looks like a
// sentinel is encoded too, so decoding is unambiguous.
func EncodeHeaderValue(v string) string {
	if plainHeaderValue(v) && !isSentinel(v) {
		return v
	}
	return sentinelPrefix + base64.StdEncoding.EncodeToString([]byte(v)) + sentinelSuffix
}

func plainHeaderValue(v string) bool {
	if v == "" || v[0] == ' ' || v[len(v)-1] == ' ' {
		return v == ""
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

func isSentinel(v string) bool {
	return strings.HasPrefix(v, sentinelPrefix) && strings.HasSuffix(v, sentinelSuffix) && len(v) >= len(sentinelPrefix)+len(sentinelSuffix)
}

// DecodeHeaderValue reverses EncodeHeaderValue. A value with characters a
// header may not carry, or a sentinel that is not valid Base64 of UTF-8,
// is an error.
func DecodeHeaderValue(h string) (string, error) {
	for i := 0; i < len(h); i++ {
		if c := h[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			return "", errors.New("invalid character in header value")
		}
	}
	if !isSentinel(h) {
		return h, nil
	}
	b, err := base64.StdEncoding.Strict().DecodeString(h[len(sentinelPrefix) : len(h)-len(sentinelSuffix)])
	if err != nil || !utf8.Valid(b) {
		return "", errors.New("invalid Base64 header value")
	}
	return string(b), nil
}

// HeaderParam is a tool parameter mirrored into an Mcp-Param-{Name} header
// (the x-mcp-header schema extension).
type HeaderParam struct {
	Name string   // the {Name} part
	Path []string // the chain of properties keys from the schema root
	Type string   // string, integer or boolean
}

// MaxSchemaDepth bounds the walk of a tool's inputSchema.
const MaxSchemaDepth = 64

// HeaderParams reads the x-mcp-header annotations of a tool's inputSchema.
// It returns an error, and the tool must then be left out of tools/list,
// when an annotation breaks a constraint: empty or not an RFC 9110 token,
// duplicated (case-insensitively), on a type other than string, integer or
// boolean, or anywhere but a property statically reachable from the root
// through "properties" keys alone.
func HeaderParams(schema json.RawMessage) ([]HeaderParam, error) {
	// The common case. A key spelled with escapes ("x-mcp-header")
	// holds a backslash, so it still takes the full walk.
	if !bytes.Contains(schema, []byte("x-mcp-header")) && !bytes.Contains(schema, []byte(`\`)) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(schema))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("inputSchema: %v", err)
	}
	var out []HeaderParam
	seen := map[string]bool{}
	var walk func(node any, path []string, reachable bool, depth int) error
	walk = func(node any, path []string, reachable bool, depth int) error {
		if depth > MaxSchemaDepth {
			return errors.New("inputSchema nested too deeply")
		}
		switch n := node.(type) {
		case []any:
			for _, v := range n {
				if err := walk(v, nil, false, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			if h, ok := n["x-mcp-header"]; ok {
				if !reachable || len(path) == 0 {
					return errors.New("x-mcp-header on a property not statically reachable from the root")
				}
				name, ok := h.(string)
				if !ok || !validToken(name) {
					return fmt.Errorf("x-mcp-header %v: not an HTTP token", h)
				}
				if seen[strings.ToLower(name)] {
					return fmt.Errorf("x-mcp-header %q declared twice", name)
				}
				seen[strings.ToLower(name)] = true
				typ, _ := n["type"].(string)
				if typ != "string" && typ != "integer" && typ != "boolean" {
					return fmt.Errorf("x-mcp-header %q on a property of type %v (want string, integer or boolean)", name, n["type"])
				}
				out = append(out, HeaderParam{Name: name, Path: append([]string(nil), path...), Type: typ})
			}
			for k, v := range n {
				if k == "properties" {
					props, ok := v.(map[string]any)
					if !ok {
						if err := walk(v, nil, false, depth+1); err != nil {
							return err
						}
						continue
					}
					for name, sub := range props {
						p := append(append([]string(nil), path...), name)
						if err := walk(sub, p, reachable, depth+2); err != nil {
							return err
						}
					}
					continue
				}
				if err := walk(v, nil, false, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, nil, true, 0); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// validToken: RFC 9110 field-name token (1*tchar), bounded.
func validToken(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// maxSafeInteger is 2^53-1, the bound the specification sets on mirrored
// integers.
const maxSafeInteger = 1<<53 - 1

// ParamHeaders returns the Mcp-Param-{Name} headers of a call: for each
// parameter, the value at its path in the arguments (absent or null: no
// header), converted and encoded. A value of the wrong kind is an error:
// the call would be refused by the server anyway.
func ParamHeaders(params []HeaderParam, args map[string]any) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, p := range params {
		var v any = args
		for _, k := range p.Path {
			obj, ok := v.(map[string]any)
			if !ok {
				v = nil
				break
			}
			v = obj[k]
		}
		if v == nil {
			continue
		}
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case bool:
			s = strconv.FormatBool(x)
		case json.Number:
			i, err := strconv.ParseInt(x.String(), 10, 64)
			if err != nil || i > maxSafeInteger || i < -maxSafeInteger {
				return nil, fmt.Errorf("argument %s: not an integer in the safe range", strings.Join(p.Path, "."))
			}
			s = strconv.FormatInt(i, 10)
		default:
			return nil, fmt.Errorf("argument %s: an object or array cannot be mirrored into a header", strings.Join(p.Path, "."))
		}
		if len(s) > MaxHeaderValue {
			return nil, fmt.Errorf("argument %s: too long for a header", strings.Join(p.Path, "."))
		}
		out[HeaderParamPrefix+p.Name] = EncodeHeaderValue(s)
	}
	return out, nil
}

// DecodeArguments parses tool arguments keeping numbers exact (json.Number)
// and a single value per key: what is forwarded is re-encoded from this, so
// a server never sees duplicate keys the gateway resolved differently.
func DecodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var args map[string]any
	if err := dec.Decode(&args); err != nil || args == nil {
		return nil, errors.New(`"arguments" must be an object`)
	}
	if dec.More() {
		return nil, errors.New(`"arguments": trailing data`)
	}
	return args, nil
}
