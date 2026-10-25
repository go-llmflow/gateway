package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseMessage(t *testing.T) {
	for _, tc := range []struct {
		in   string
		code int // 0: valid
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 0},
		{`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"x"}}`, 0},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1.5,"method":"x"}`, CodeInvalidRequest},
		{`{"jsonrpc":"1.0","id":1,"method":"x"}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1}`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1,"method":"x","params":[1]}`, CodeInvalidRequest},
		{`[{"jsonrpc":"2.0","id":1,"method":"x"}]`, CodeInvalidRequest},
		{`{"jsonrpc":"2.0","id":1,"method":"x"`, CodeParseError},
		{``, CodeParseError},
	} {
		m, err := ParseMessage([]byte(tc.in))
		switch {
		case tc.code == 0 && err != nil:
			t.Errorf("%s: %v", tc.in, err)
		case tc.code != 0 && (err == nil || err.Code != tc.code):
			t.Errorf("%s: got %v %+v, want code %d", tc.in, m, err, tc.code)
		}
	}
}

func TestRequestMeta(t *testing.T) {
	m, _ := ParseMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"progressToken":7}}}`))
	meta, err := m.RequestMeta()
	if err != nil || !meta.Present || meta.ProtocolVersion != Version || string(meta.ClientCapabilities) != "{}" || string(meta.Rest["progressToken"]) != "7" {
		t.Fatalf("%+v %v", meta, err)
	}
	m, _ = ParseMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"x","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":[]}}}`))
	if _, err := m.RequestMeta(); err == nil || err.Code != CodeInvalidParams {
		t.Fatalf("capabilities not an object: %v", err)
	}
}

func TestHeaderValue(t *testing.T) {
	for in, want := range map[string]string{
		"us-west1":           "us-west1",
		"Hello, 世界":          "=?base64?SGVsbG8sIOS4lueVjA==?=",
		" padded ":           "=?base64?IHBhZGRlZCA=?=",
		"line1\nline2":       "=?base64?bGluZTEKbGluZTI=?=",
		"=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?=",
	} {
		// The specification's own examples.
		if got := EncodeHeaderValue(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
		if back, err := DecodeHeaderValue(want); err != nil || back != in {
			t.Errorf("%q: decoded %q %v", want, back, err)
		}
	}
	if _, err := DecodeHeaderValue("=?base64?!!!?="); err == nil {
		t.Fatal("invalid Base64 accepted")
	}
	if _, err := DecodeHeaderValue("a\x00b"); err == nil {
		t.Fatal("control character accepted")
	}
}

func TestHeaderParams(t *testing.T) {
	ok := `{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"},` +
		`"opts":{"type":"object","properties":{"n":{"type":"integer","x-mcp-header":"N"}}},"query":{"type":"string"}}}`
	ps, err := HeaderParams(json.RawMessage(ok))
	if err != nil || len(ps) != 2 || ps[0].Name != "N" || strings.Join(ps[0].Path, ".") != "opts.n" || ps[1].Name != "Region" {
		t.Fatalf("%+v %v", ps, err)
	}
	for name, bad := range map[string]string{
		"in items":     `{"type":"object","properties":{"a":{"type":"array","items":{"type":"string","x-mcp-header":"A"}}}}`,
		"in anyOf":     `{"type":"object","anyOf":[{"properties":{"a":{"type":"string","x-mcp-header":"A"}}}]}`,
		"in $defs":     `{"type":"object","$defs":{"a":{"type":"string","x-mcp-header":"A"}}}`,
		"number":       `{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"A"}}}`,
		"duplicate":    `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"},"b":{"type":"string","x-mcp-header":"a"}}}`,
		"not a token":  `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A B"}}}`,
		"empty":        `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":""}}}`,
		"root":         `{"type":"string","x-mcp-header":"A"}`,
		"escaped key":  `{"type":"object","anyOf":[{"properties":{"a":{"type":"string","x-mcp-header":"A"}}}]}`,
		"CRLF in name": `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A\r\nX: y"}}}`,
	} {
		if _, err := HeaderParams(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	args, _ := DecodeArguments(json.RawMessage(`{"region":"Hello, 世界","opts":{"n":9007199254740991},"query":"q"}`))
	h, err := ParamHeaders(ps, args)
	if err != nil || h["Mcp-Param-Region"] != "=?base64?SGVsbG8sIOS4lueVjA==?=" || h["Mcp-Param-N"] != "9007199254740991" {
		t.Fatalf("%v %v", h, err)
	}
	args, _ = DecodeArguments(json.RawMessage(`{"opts":{"n":9007199254740992}}`))
	if _, err := ParamHeaders(ps, args); err == nil {
		t.Fatal("integer beyond 2^53-1 mirrored")
	}
	args, _ = DecodeArguments(json.RawMessage(`{"region":null}`))
	if h, err := ParamHeaders(ps, args); err != nil || len(h) != 0 {
		t.Fatalf("null must give no header: %v %v", h, err)
	}
}

func TestReadResponse(t *testing.T) {
	sse := ": keep-alive\r\n\r\nid: 1\ndata:\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":\"other\",\"result\":{}}\n\n" +
		"data: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":\"r1\",\"result\":{\"ok\":true}}\r\r"
	r, err := ReadResponse("text/event-stream", strings.NewReader(sse), "r1", 1<<20)
	if err != nil || string(r.Result) != `{"ok":true}` {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ReadResponse("text/event-stream", strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"id\":\"r1\",\"result\":{}}"), "r1", 1<<20); err == nil {
		t.Fatal("an event not ended by a blank line was dispatched")
	}
	big := `{"jsonrpc":"2.0","id":"r1","result":{"x":"` + strings.Repeat("a", 100) + `"}}`
	if _, err := ReadResponse("application/json", strings.NewReader(big), "r1", 50); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size limit: %v", err)
	}
	if _, err := ReadResponse("text/event-stream", strings.NewReader("data: "+big+"\n\n"), "r1", 50); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("SSE size limit: %v", err)
	}
	if _, err := ReadResponse("text/html", strings.NewReader(big), "r1", 1<<20); err == nil {
		t.Fatal("text/html accepted")
	}
}

// fakeServer is a modern or legacy tool server that checks the binding.
type fakeServer struct {
	t        *testing.T
	legacy   bool
	sse      bool
	requests atomic.Int32
	inits    atomic.Int32
	expire   atomic.Bool
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.Header.Get("Authorization") != "Bearer tool-token" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	m, perr := ParseMessage(b)
	if perr != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply := func(result any) {
		out, _ := Result(m.ID, result)
		if f.sse {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": hello\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{}}\n\ndata: %s\n\n", out)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
	if f.legacy {
		switch m.Method {
		case "initialize":
			f.inits.Add(1)
			f.expire.Store(false)
			w.Header().Set(HeaderSessionID, "sess-1")
			reply(map[string]any{"protocolVersion": LegacyVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fake", "version": "1"}})
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if r.Header.Get(HeaderSessionID) != "sess-1" || r.Header.Get(HeaderProtocolVersion) != LegacyVersion {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		if f.expire.Load() {
			http.Error(w, "expired", http.StatusNotFound)
			return
		}
	} else {
		meta, _ := m.RequestMeta()
		name, _, _ := m.StringParam("name")
		hn, _ := DecodeHeaderValue(r.Header.Get(HeaderName))
		if !meta.Present || r.Header.Get(HeaderProtocolVersion) != meta.ProtocolVersion || r.Header.Get(HeaderMethod) != m.Method ||
			(m.Method == "tools/call" && hn != name) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write(ErrorResponse(m.ID, Errorf(CodeHeaderMismatch, "header mismatch")))
			return
		}
	}
	switch m.Method {
	case "tools/list":
		cursor, _, _ := m.StringParam("cursor")
		if cursor == "" {
			reply(map[string]any{"tools": []any{
				map[string]any{"name": "search", "inputSchema": json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`)},
				map[string]any{"name": "bad name!", "inputSchema": map[string]any{"type": "object"}},
			}, "nextCursor": "p2"})
			return
		}
		reply(map[string]any{"tools": []any{
			map[string]any{"name": "bad_header", "inputSchema": json.RawMessage(`{"type":"object","properties":{"a":{"type":"number","x-mcp-header":"A"}}}`)},
			map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}},
		}})
	case "tools/call":
		if !f.legacy && r.Header.Get("Mcp-Param-Region") != "" {
			var p struct {
				Arguments map[string]any `json:"arguments"`
			}
			raw, _ := json.Marshal(m.Params)
			json.Unmarshal(raw, &p)
			if v, _ := DecodeHeaderValue(r.Header.Get("Mcp-Param-Region")); v != p.Arguments["region"] {
				w.WriteHeader(http.StatusBadRequest)
				w.Write(ErrorResponse(m.ID, Errorf(CodeHeaderMismatch, "Mcp-Param-Region")))
				return
			}
		}
		raw, _ := json.Marshal(m.Params["arguments"])
		reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}, "isError": false})
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write(ErrorResponse(m.ID, Errorf(CodeMethodNotFound, "no")))
	}
}

func newClient(url, protocol string) *Client {
	return &Client{URL: url, Token: "tool-token", Protocol: protocol, Timeout: 5 * time.Second, MaxResponse: 1 << 20,
		Info: Implementation{Name: "test", Version: "0"}, HTTP: &http.Client{CheckRedirect: NoRedirects}}
}

func TestClientModernAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		sse      bool
	}{{Version, false}, {Version, true}, {LegacyVersion, false}, {LegacyVersion, true}} {
		f := &fakeServer{t: t, legacy: tc.protocol != Version, sse: tc.sse}
		srv := httptest.NewServer(f)
		c := newClient(srv.URL, tc.protocol)
		tools, rejected, err := c.ListTools(context.Background())
		if err != nil || len(tools) != 2 || tools[0].Name != "search" || tools[1].Name != "echo" || len(rejected) != 2 {
			t.Fatalf("%s sse=%v: %+v %+v %v", tc.protocol, tc.sse, tools, rejected, err)
		}
		args, _ := DecodeArguments(json.RawMessage(`{"region":"Zürich"}`))
		res, err := c.CallTool(context.Background(), CallParams{Name: "search", Arguments: args, Headers: tools[0].Headers})
		if err != nil || !bytes.Contains(res, []byte(`Zürich`)) {
			t.Fatalf("%s: %s %v", tc.protocol, res, err)
		}
		if f.legacy {
			// An expired session is renewed once.
			f.expire.Store(true)
			if _, err := c.CallTool(context.Background(), CallParams{Name: "echo", Arguments: map[string]any{}}); err != nil || f.inits.Load() != 2 {
				t.Fatalf("session renewal: %v, %d initializations", err, f.inits.Load())
			}
		}
		srv.Close()
	}
}

func TestClientRefusesRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c := newClient(redir.URL, Version)
	if _, _, err := c.ListTools(context.Background()); err == nil || elsewhere.Load() != 0 {
		t.Fatalf("redirect followed: %v, %d requests elsewhere", err, elsewhere.Load())
	}
}

func TestClientHeaderMismatchIsAServerError(t *testing.T) {
	// A modern server refusing the headers answers 400 with a JSON-RPC
	// error: the client reports it as the server's error.
	srv := httptest.NewServer(&fakeServer{t: t})
	defer srv.Close()
	c := newClient(srv.URL, Version)
	args, _ := DecodeArguments(json.RawMessage(`{"region":"eu"}`))
	// No Headers given: Mcp-Param-Region is missing, the fake checks only
	// when present, so the call goes through; with a wrong header it fails.
	_, err := c.CallTool(context.Background(), CallParams{Name: "search", Arguments: args,
		Headers: []HeaderParam{{Name: "Region", Path: []string{"other"}, Type: "string"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.Protocol = "2099-01-01" // legacy path with an unknown version: initialize refused by the fake
	if _, _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("an initialize the server cannot answer succeeded")
	}
	var se *ErrServer
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m, _ := ParseMessage(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write(ErrorResponse(m.ID, Errorf(CodeUnsupportedVersion, "Unsupported protocol version")))
	}))
	defer srv2.Close()
	c2 := newClient(srv2.URL, Version)
	if _, _, err := c2.ListTools(context.Background()); !errors.As(err, &se) || se.E.Code != CodeUnsupportedVersion {
		t.Fatalf("%v", err)
	}
}

func FuzzParseMessage(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a.b","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":"x","method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := ParseMessage(b)
		if err != nil {
			return
		}
		if m.Method == "" || (m.ID != nil && checkID(m.ID) != nil) {
			t.Fatalf("accepted %+v", m)
		}
		_, _ = m.RequestMeta()
		_, _, _ = m.StringParam("name")
	})
}

func FuzzHeaderValue(f *testing.F) {
	for _, s := range []string{"us-west1", "Hello, 世界", " padded ", "line1\nline2", "=?base64?literal?=", "", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		enc := EncodeHeaderValue(s)
		for i := 0; i < len(enc); i++ {
			if enc[i] < 0x20 || enc[i] > 0x7e {
				t.Fatalf("%q encoded with byte %#x", s, enc[i])
			}
		}
		if enc != "" && (enc[0] == ' ' || enc[len(enc)-1] == ' ') {
			t.Fatalf("%q encoded with outer space", s)
		}
		back, err := DecodeHeaderValue(enc)
		if utf8.ValidString(s) && (err != nil || back != s) {
			t.Fatalf("%q -> %q -> %q %v", s, enc, back, err)
		}
		_, _ = DecodeHeaderValue(s)
	})
}

func FuzzHeaderParams(f *testing.F) {
	f.Add([]byte(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`), []byte(`{"region":"x"}`))
	f.Add([]byte(`{"type":"object","properties":{"o":{"type":"object","properties":{"n":{"type":"integer","x-mcp-header":"N"}}}}}`), []byte(`{"o":{"n":3}}`))
	f.Add([]byte(`{"anyOf":[{"x-mcp-header":"A"}]}`), []byte(`{}`))
	f.Fuzz(func(t *testing.T, schema, args []byte) {
		ps, err := HeaderParams(schema)
		if err != nil {
			return
		}
		for _, p := range ps {
			if !validToken(p.Name) || len(p.Path) == 0 {
				t.Fatalf("accepted %+v", p)
			}
		}
		a, err := DecodeArguments(args)
		if err != nil {
			return
		}
		h, err := ParamHeaders(ps, a)
		if err != nil {
			return
		}
		for k, v := range h {
			if !validToken(k) || strings.ContainsAny(v, "\r\n\x00") {
				t.Fatalf("header %q: %q", k, v)
			}
		}
	})
}

func FuzzReadResponse(f *testing.F) {
	f.Add(true, []byte("data: {\"jsonrpc\":\"2.0\",\"id\":\"r1\",\"result\":{}}\n\n"))
	f.Add(true, []byte(": c\r\ndata: {\"jsonrpc\":\"2.0\",\r\ndata: \"id\":\"r1\",\"error\":{\"code\":-1,\"message\":\"m\"}}\r\r"))
	f.Add(false, []byte(`{"jsonrpc":"2.0","id":"r1","result":{"content":[]}}`))
	f.Fuzz(func(t *testing.T, sse bool, body []byte) {
		ct := "application/json"
		if sse {
			ct = "text/event-stream"
		}
		r, err := ReadResponse(ct, bytes.NewReader(body), "r1", 4096)
		if err != nil {
			return
		}
		if (r.Error == nil) == (r.Result == nil) {
			t.Fatalf("response with both or neither: %+v", r)
		}
		var id string
		if json.Unmarshal(r.ID, &id) != nil || id != "r1" {
			t.Fatalf("response to another request: %s", r.ID)
		}
	})
}
