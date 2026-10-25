package guard

import (
	"encoding/json"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Deep makes a text function read every JSON layer of a text, not only the
// surface the request carries.
//
// A JSON string is decoded once by the request's own parsing, but a text
// can hold more JSON: tool-call arguments are a JSON document inside a
// string, a tool result often is, and a client can write any character of
// a prompt as a \uXXXX escape inside them. The model reads what vLLM
// decodes, so a finder that runs on the escaped form is bypassed: the
// GitHub token written "ghp_…" reaches the model in clear. Deep
// therefore runs f on the DECODED text of every layer:
//
//   - a text that is a JSON object or array is parsed, every key and
//     string value goes through Deep, and the document is re-encoded when
//     anything changed (masks land on the decoded strings);
//   - a text with JSON escapes (\uXXXX, \", \\, \/, \b \f \n \r \t) is
//     decoded; when f changes the decoded form, that form, masked, replaces
//     the text (the escapes do not come back: the masked text is what the
//     model gets); when it does not, the original text is left as it was;
//   - layers are bounded by maxJSONDepth.
func Deep(f func(role, text string) string) func(role, text string) string {
	var g func(role, text string, depth int) string
	g = func(role, text string, depth int) string {
		if depth < maxJSONDepth {
			if t := strings.TrimSpace(text); len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t)) {
				inner := func(r, s string) string { return g(r, s, depth+1) }
				if nv, _, ch, err := mapJSONStrings(json.RawMessage(t), role, inner, 0); err == nil {
					if ch {
						return string(nv)
					}
					return text
				}
			}
			if strings.IndexByte(text, '\\') >= 0 {
				if dec, ok := unescapeJSON(text); ok {
					// The decoded form is what is judged: a match made of
					// the escape sequences themselves ("u001e@0.AA" read
					// as an address, found by FuzzEscapedFindings) is not
					// content.
					if out := g(role, dec, depth+1); out != dec {
						return out
					}
					return text
				}
			}
		}
		return f(role, text)
	}
	return func(role, text string) string { return g(role, text, 0) }
}

// unescapeJSON decodes the JSON string escapes in s, leaving any other
// backslash as it is; it reports whether something was decoded.
func unescapeJSON(s string) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		switch e := s[i+1]; e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, n := decodeU(s[i:])
			if n == 0 {
				b.WriteByte(c)
				continue
			}
			b.WriteRune(r)
			i += n - 1
			changed = true
			continue
		default:
			b.WriteByte(c)
			continue
		}
		i++
		changed = true
	}
	return b.String(), changed
}

// decodeU decodes \uXXXX (and a following low surrogate) at the start of
// s; it returns the rune and the bytes consumed, 0 when s does not start
// with a valid escape.
func decodeU(s string) (rune, int) {
	hex4 := func(t string) (rune, bool) {
		if len(t) < 6 || t[0] != '\\' || t[1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range []byte(t[2:6]) {
			switch {
			case c >= '0' && c <= '9':
				r = r*16 + rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r*16 + rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r*16 + rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	r, ok := hex4(s)
	if !ok {
		return 0, 0
	}
	if utf16.IsSurrogate(r) {
		if r2, ok := hex4(s[6:]); ok {
			if d := utf16.DecodeRune(r, r2); d != utf8.RuneError {
				return d, 12
			}
		}
		return utf8.RuneError, 6
	}
	return r, 6
}
