package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// uEscape writes every rune of s as \uXXXX (ASCII only here).
func uEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

// TestEscapedSecretsNeverReachVLLM: the v0.7.0 bypass. A secret written
// with \u escapes inside JSON-in-a-string tool arguments, or as literal
// escapes in a tool result, reached the model decoded. Every layer is now
// decoded before the finders run.
func TestEscapedSecretsNeverReachVLLM(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "secrets" {
  type  = "hide_secrets"
  teams = ["etabA"]
}
guardrail "pii" {
  type  = "pii"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	iban, mail := "FR14 2004 1010 0505 0001 3M02 606", "jean.dupont@example.fr"

	args := `{"token":"` + uEscape(ghToken) + `"}`
	chatBody := `{"model":"llama","messages":[` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":` + quoted(args) + `}}]},` +
		`{"role":"tool","tool_call_id":"c","content":` + quoted(`{"owner":"`+uEscape(mail)+`","iban":"`+uEscape(iban)+`"}`) + `}]}`
	// The control: the request as sent does hold every secret, once
	// decoded — and none in clear text.
	for _, secret := range []string{ghToken, iban, mail} {
		if !holds([]byte(chatBody), secret) || strings.Contains(chatBody, secret) {
			t.Fatalf("control: %q", secret)
		}
	}
	if code, out := e.chatText(key, chatBody); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	sent, _ := json.Marshal(up.last().Body)
	for _, secret := range []string{ghToken, iban, mail} {
		if holds(sent, secret) {
			t.Fatalf("%q reached vLLM: %s", secret, sent)
		}
	}

	msgBody := `{"model":"llama","max_tokens":8,"messages":[` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"owner ` + uEscape(mail) + `"}]}]}`
	// The tool_result content above is a JSON string whose escapes the
	// request's own parsing decodes; a doubly escaped one stays escaped
	// text until Deep decodes it.
	msgBody2 := `{"model":"llama","max_tokens":8,"messages":[` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":` + quoted("iban "+uEscape(iban)) + `}]}]}`
	for _, b := range []string{msgBody, msgBody2} {
		if !holds([]byte(b), mail) && !holds([]byte(b), iban) {
			t.Fatalf("control: %s", b)
		}
		if code, out := e.messages(key, b); code != 200 {
			t.Fatalf("%d %s", code, out)
		}
		sent, _ := json.Marshal(up.last().Body)
		if holds(sent, mail) || holds(sent, iban) {
			t.Fatalf("reached vLLM: %s", sent)
		}
	}
}

func quoted(s string) string { b, _ := json.Marshal(s); return string(b) }

// holds reports whether secret appears in body once every JSON layer and
// escape is decoded (as the model would read it).
func holds(body []byte, secret string) bool {
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			views := []string{x}
			if strings.Contains(x, `\u`) {
				var d string
				if json.Unmarshal([]byte(`"`+strings.ReplaceAll(x, `"`, `\"`)+`"`), &d) == nil {
					views = append(views, d)
				}
			}
			for _, s := range views {
				if strings.Contains(s, secret) {
					return true
				}
				var inner any
				if json.Unmarshal([]byte(s), &inner) == nil {
					if _, isStr := inner.(string); !isStr && walk(inner) {
						return true
					}
				}
			}
		case []any:
			for _, e := range x {
				if walk(e) {
					return true
				}
			}
		case map[string]any:
			for k, e := range x {
				if walk(k) || walk(e) {
					return true
				}
			}
		}
		return false
	}
	var v any
	_ = json.Unmarshal(body, &v)
	return walk(v)
}
