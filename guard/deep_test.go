package guard

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/go-gitsafe/gitsafe/redact"
	"github.com/go-llmflow/pii"
)

// realFinders are the gateway's: gitsafe's redact and go-llmflow/pii.
func realFinders() []Finder {
	r, sc := redact.New(), pii.New()
	return []Finder{
		func(text string) []Span {
			var out []Span
			for _, sp := range r.Find(text) {
				out = append(out, Span{Start: sp.Start, End: sp.End, Category: string(sp.Kind), Action: "mask"})
			}
			return out
		},
		func(text string) []Span {
			var out []Span
			for _, m := range sc.Find(text) {
				out = append(out, Span{Start: m.Start, End: m.End, Category: string(m.Kind), Action: "mask"})
			}
			return out
		},
	}
}

// recorder masks with the real finders and records the categories found.
func recorder(found *[]string) func(role, text string) string {
	fs := realFinders()
	return Deep(func(_, text string) string {
		spans := Combine(text, fs)
		for _, s := range spans {
			*found = append(*found, s.Category)
		}
		return Apply(text, spans, Placeholder)
	})
}

// escapeString writes s as the inside of a JSON string literal, each rune
// escaped as \uXXXX when the matching bit of pick is set (and always for
// the characters JSON requires).
func escapeString(s string, pick []byte) string {
	var b strings.Builder
	for i, r := range s {
		esc := len(pick) > 0 && pick[i%len(pick)]&(1<<(uint(i)%8)) != 0
		if r == '"' || r == '\\' || r < 0x20 || esc {
			if r > 0xFFFF {
				r1, r2 := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func quoted(s string) string { b, _ := json.Marshal(s); return string(b) }

// requestWith builds a chat request carrying text in a user message, in
// tool-call arguments (JSON in a string) and in a tool result; inner is
// how text is written inside the nested layers.
func requestWith(userLit, inner string) string {
	args := `{"v":"` + inner + `"}`
	return `{"model":"m","messages":[` +
		`{"role":"user","content":"` + userLit + `"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":` + quoted(args) + `}}]},` +
		`{"role":"tool","tool_call_id":"c","content":` + quoted(inner) + `}]}`
}

func TestEscapedSecretsAreFound(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1b2", 9)
	for _, secret := range []string{token, "FR14 2004 1010 0505 0001 3M02 606", "jean.dupont@example.fr"} {
		all := []byte{0xff}
		body := requestWith(escapeString(secret, all), escapeString(secret, all))
		// Before the fix, the nested layers reached the model in clear: the
		// control shows the escaped form really hides the secret from a
		// surface read.
		if strings.Contains(body, secret) {
			t.Fatalf("the escaped request holds the secret in clear")
		}
		// The bypass itself: without Deep (the v0.7.0 behaviour) the secret
		// gets through — the check below can see it.
		fs := realFinders()
		surface, _, err := MapRequest([]byte(body), func(_, text string) string { return Apply(text, Combine(text, fs), Placeholder) })
		if err != nil {
			t.Fatal(err)
		}
		if decodedHolds(t, surface, secret) == "" {
			t.Fatalf("%q: the surface read already masks it; the test does not reproduce the bypass", secret)
		}
		var found []string
		out, _, err := MapRequest([]byte(body), recorder(&found))
		if err != nil {
			t.Fatal(err)
		}
		if leak := decodedHolds(t, out, secret); leak != "" {
			t.Fatalf("%q reaches the model through %s: %s", secret, leak, out)
		}
		if len(found) < 3 {
			t.Fatalf("%q: found %v, want it in each of the 3 places", secret, found)
		}
	}
}

// decodedHolds decodes every JSON layer of a body (strings that are JSON
// documents, JSON escapes inside strings) and reports where secret
// appears.
func decodedHolds(t *testing.T, body []byte, secret string) string {
	t.Helper()
	var where string
	var walk func(v any, path string, depth int)
	walk = func(v any, path string, depth int) {
		switch x := v.(type) {
		case string:
			views := []string{x}
			if dec, ok := unescapeJSON(x); ok {
				views = append(views, dec)
			}
			for _, s := range views {
				if strings.Contains(s, secret) && where == "" {
					where = path
				}
				var inner any
				if depth < 8 && json.Unmarshal([]byte(s), &inner) == nil {
					if _, isStr := inner.(string); !isStr {
						walk(inner, path+">json", depth+1)
					}
				}
			}
		case []any:
			for i, e := range x {
				walk(e, fmt.Sprintf("%s[%d]", path, i), depth)
			}
		case map[string]any:
			for k, e := range x {
				walk(k, path+".key", depth)
				walk(e, path+"."+k, depth)
			}
		}
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	walk(v, "$", 0)
	return where
}

// FuzzEscapedFindings: writing the same content with arbitrary \u escapes,
// at the request's level and inside the nested JSON layers, finds at least
// what the plain form finds (each finding is masked where it is found;
// TestEscapedSecretsAreFound checks the masked output layer by layer).
func FuzzEscapedFindings(f *testing.F) {
	f.Add("token ghp_"+strings.Repeat("A1b2", 9), []byte{0x55})
	f.Add("IBAN FR14 2004 1010 0505 0001 3M02 606", []byte{0xff})
	f.Add("mail jean.dupont@example.fr please", []byte{0x0f, 0xf0})
	f.Add("carte 4111 1111 1111 1111 et 06 12 34 56 78", []byte{0x81})
	f.Fuzz(func(t *testing.T, s string, pick []byte) {
		if strings.ContainsAny(s, "\\\"") || !utf8Valid(s) || len(s) > 512 {
			return // keep the plain form a plain JSON string
		}
		var plain, escaped []string
		if _, _, err := MapRequest([]byte(requestWith(escapeString(s, nil), escapeString(s, nil))), recorder(&plain)); err != nil {
			return
		}
		_, _, err := MapRequest([]byte(requestWith(escapeString(s, pick), escapeString(s, pick))), recorder(&escaped))
		if err != nil {
			t.Fatalf("the escaped form was refused: %v", err)
		}
		count := func(l []string) map[string]int {
			m := map[string]int{}
			for _, c := range l {
				m[c]++
			}
			return m
		}
		pc, ec := count(plain), count(escaped)
		for c, n := range pc {
			if ec[c] < n {
				sort.Strings(plain)
				sort.Strings(escaped)
				t.Fatalf("plain found %v, escaped %v", plain, escaped)
			}
		}
	})
}
