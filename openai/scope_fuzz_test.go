package openai

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// uuidFields are the members whose content parts vLLM parses, per endpoint
// (security audit 2026-10-26, A1): chat and embeddings messages and chat
// "input", Responses input, rerank query and documents, tokenize messages.
var uuidFields = map[Endpoint][]string{
	Chat:      {"messages", "input"},
	Responses: {"input"},
	Rerank:    {"query", "documents"},
	Tokenize:  {"messages"},
}

// FuzzScopeEveryRoute: whatever the body, every content-part uuid vLLM
// would read in the rewritten request is one the scope function produced.
// The judge decodes the output generically (not with the rewrite's own
// RawMessage walk) and follows every shape vLLM accepts: a value, a list,
// or a list of lists, of objects whose "content" is a list of parts.
func FuzzScopeEveryRoute(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","uuid":"u"}]}]}`,
		`{"model":"m","messages":[[{"role":"user","content":[{"uuid":"u"}]}]]}`,
		`{"model":"m","input":[{"role":"user","content":[{"uuid":"u"}]}]}`,
		`{"model":"m","query":{"content":[{"type":"image_url","uuid":"u"}]},"documents":[{"content":[{"uuid":"v"}]},"t"]}`,
		`{"model":"m","documents":{"content":[{"uuid":"w","uuid":"x"}]}}`,
		`{"model":"m","messages":[{"content":[{"uuid":null},{"uuid":1}]}]}`,
	} {
		f.Add([]byte(s))
	}
	var nb [8]byte
	_, _ = rand.Read(nb[:])
	nonce := "scoped-" + hex.EncodeToString(nb[:]) + ":" // the fuzzer cannot guess it
	iso := Isolation{CacheSalt: "SALT", ScopeUUID: func(s string) string { return nonce + s }}
	f.Fuzz(func(t *testing.T, in []byte) {
		for ep, fields := range uuidFields {
			out, _, err := RewriteFor(ep, in, resolveAll, iso)
			if err != nil {
				continue
			}
			var m map[string]any
			dec := json.NewDecoder(bytes.NewReader(out))
			dec.UseNumber()
			if err := dec.Decode(&m); err != nil {
				t.Fatalf("endpoint %d: output does not decode: %v\n%s", ep, err, out)
			}
			for _, fld := range fields {
				checkScoped(t, ep, m[fld], nonce, 0, out)
			}
		}
	})
}

func checkScoped(t *testing.T, ep Endpoint, v any, nonce string, depth int, out []byte) {
	t.Helper()
	switch x := v.(type) {
	case []any:
		if depth >= maxScopeDepth {
			return
		}
		for _, e := range x {
			checkScoped(t, ep, e, nonce, depth+1, out)
		}
	case map[string]any:
		parts, _ := x["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			if u, ok := part["uuid"].(string); ok && !strings.HasPrefix(u, nonce) {
				t.Fatalf("endpoint %d: unscoped uuid %q reaches vLLM:\n%s", ep, u, out)
			}
		}
	}
}
