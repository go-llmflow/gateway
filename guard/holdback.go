package guard

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Span is a finding of a masking guard: text[Start:End] is of Category
// (a PII kind, a secret kind) and the guard's configured Action for it is
// "mask", "block" or "log".
type Span struct {
	Start, End int
	Guard      string
	Category   string
	Action     string
}

// Finder finds spans in a text.
type Finder func(text string) []Span

// Combine runs every finder and returns their spans ordered and without
// overlap: of overlapping spans the one starting first wins, extended to
// cover the others, and the strongest action (block, then mask, then log)
// is kept.
func Combine(text string, finders []Finder) []Span {
	var all []Span
	for _, f := range finders {
		all = append(all, f(text)...)
	}
	if len(all) < 2 {
		return all
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Start != all[j].Start {
			return all[i].Start < all[j].Start
		}
		return all[i].End > all[j].End
	})
	out := all[:1]
	for _, s := range all[1:] {
		last := &out[len(out)-1]
		if s.Start < last.End {
			if s.End > last.End {
				last.End = s.End
			}
			if strength(s.Action) > strength(last.Action) {
				last.Action, last.Guard, last.Category = s.Action, s.Guard, s.Category
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func strength(a string) int {
	switch a {
	case "block":
		return 3
	case "mask":
		return 2
	case "log":
		return 1
	}
	return 0
}

// Apply returns text with every "mask" span replaced by repl(span); spans
// of other actions are left in place.
func Apply(text string, spans []Span, repl func(Span) string) string {
	var b strings.Builder
	last := 0
	for _, s := range spans {
		if s.Action != "mask" {
			continue
		}
		b.WriteString(text[last:s.Start])
		b.WriteString(repl(s))
		last = s.End
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// Placeholder is the default replacement: "[CATEGORY]".
func Placeholder(s Span) string { return "[" + strings.ToUpper(s.Category) + "]" }

// MaxHold bounds what a Holdback keeps when one span grows without end (a
// "token" with no maximum length): past it, the span is released masked up
// to where it has reached.
const MaxHold = 64 << 10

// Holdback masks a stream of text without ever buffering all of it. It
// keeps the last Window bytes, and further back the start of any span that
// reaches into them, since a match may still grow with the next chunk;
// (Window must be at least the longest text that can begin a match without
// being one yet — a token before its minimum length, an address before its
// domain: a beginning that left the window before it matched is not seen.
// FuzzHoldback found it with a window of 2 against a 10-byte minimum.)
// everything before is final and is released, masked.
//
// The latency it adds is the time to generate Window bytes of text (about
// Window/4 tokens) at the start of a stream, and nothing after: each chunk
// releases as much as it adds, except while a span is open.
//
// Window counts bytes, not runes: the finders work on bytes and the bounds
// it must cover (pii.MaxLen, a token's minimum length) are byte lengths. A
// release never splits a rune, so up to 3 more bytes may be held.
//
// Known residual risk: a span still open after MaxHold bytes is released
// masked as far as it has reached; if what follows no longer matches on its
// own, that tail goes out unmasked.
type Holdback struct {
	Window  int
	Finders []Finder
	Repl    func(Span) string
	buf     string
}

// Push adds text and returns what can be released, masked, with the spans
// found in it (positions relative to the released text before masking).
func (h *Holdback) Push(s string) (string, []Span) {
	h.buf += s
	return h.release(false)
}

// Flush releases everything held: the stream has ended.
func (h *Holdback) Flush() (string, []Span) { return h.release(true) }

// Held is the number of bytes waiting.
func (h *Holdback) Held() int { return len(h.buf) }

func (h *Holdback) release(final bool) (string, []Span) {
	if h.buf == "" {
		return "", nil
	}
	spans := Combine(h.buf, h.Finders)
	cut := len(h.buf)
	if !final {
		cut = cutDown(h.buf, max(len(h.buf)-h.Window, 0), spans)
		if cut == 0 && len(h.buf) > MaxHold {
			// One span open for MaxHold bytes: release it masked as far as
			// it goes now.
			for _, s := range spans {
				if s.Start < len(h.buf)-h.Window {
					cut = s.End
				}
			}
			cut = cutUp(h.buf, cut, spans)
		}
	}
	// Every step that moves the cut is checked here: a cut inside a span
	// would release the span's head unmasked (a stray continuation byte
	// after a span once moved the cut into it).
	if !cutIsSafe(cut, spans) {
		panic("guard: holdback cut inside a span")
	}
	if cut == 0 {
		return "", nil
	}
	var done []Span
	for _, s := range spans {
		if s.End <= cut {
			done = append(done, s)
		}
	}
	out := Apply(h.buf[:cut], done, h.Repl)
	h.buf = h.buf[cut:]
	return out, done
}

// cutIsSafe: no span has Start < cut < End.
func cutIsSafe(cut int, spans []Span) bool {
	for _, s := range spans {
		if s.Start < cut && cut < s.End {
			return false
		}
	}
	return true
}

// cutDown moves cut back to a rune start that no span straddles, repeating
// until both hold (each step only moves it back, so it ends, at 0 at
// worst).
func cutDown(buf string, cut int, spans []Span) int {
	for {
		for cut > 0 && cut < len(buf) && !utf8.RuneStart(buf[cut]) {
			cut--
		}
		moved := false
		for _, s := range spans {
			if s.Start < cut && cut < s.End {
				cut, moved = s.Start, true
			}
		}
		if !moved {
			return cut
		}
	}
}

// cutUp moves cut forward to a rune start that no span straddles (each step
// only moves it forward, so it ends, at len(buf) at worst).
func cutUp(buf string, cut int, spans []Span) int {
	for {
		for cut > 0 && cut < len(buf) && !utf8.RuneStart(buf[cut]) {
			cut++
		}
		moved := false
		for _, s := range spans {
			if s.Start < cut && cut < s.End {
				cut, moved = s.End, true
			}
		}
		if !moved {
			return cut
		}
	}
}
