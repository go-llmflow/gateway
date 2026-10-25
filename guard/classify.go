package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Verdict is what a classifying guard concluded.
type Verdict struct {
	// Flagged: the text is unsafe (Llama Guard), an injection (Prompt
	// Guard), or the service said so.
	Flagged bool
	// Categories it falls under (S1…, "injection", a service's own).
	Categories []string
	// Action a service asked for ("block" or "log"); empty: the guard's.
	Action string
}

// MaxInput bounds the text sent to a guard model or service: the newest
// part of the conversation is kept.
const MaxInput = 32 << 10

// ---- Llama Guard ----
//
// Llama Guard 3 is served by vLLM as a chat model; its chat template builds
// the classification prompt (task, the S1… categories, the conversation as
// "User:"/"Agent:" turns) and asks it to assess the last turn. The answer's
// first line is "safe" or "unsafe"; when unsafe, the second line lists the
// violated categories, comma-separated ("S1,S10"). The template wants the
// turns to alternate user/assistant, starting with the user.

// LlamaGuardConversation turns a request's messages, and the completion for
// a post-call check, into alternating user/assistant turns (system prompts
// and tool messages are not judged; consecutive turns of one role are
// joined), keeping the newest MaxInput bytes.
func LlamaGuardConversation(msgs []Message, completion string, post bool) []Message {
	var turns []Message
	add := func(role, text string) {
		if text == "" {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].Role == role {
			turns[n-1].Content += "\n\n" + text
			return
		}
		turns = append(turns, Message{role, text})
	}
	for _, m := range msgs {
		switch m.Role {
		case "user", "prompt", "input":
			add("user", m.Content)
		case "assistant":
			add("assistant", m.Content)
		}
	}
	if post {
		add("assistant", completion)
	}
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	// Keep the newest turns within MaxInput, cutting the oldest kept one
	// from its start if needed; the first must stay a user turn.
	total := 0
	for i := len(turns) - 1; i >= 0; i-- {
		total += len(turns[i].Content)
		if total > MaxInput {
			over := total - MaxInput
			turns[i].Content = tail(turns[i].Content, len(turns[i].Content)-over)
			turns = turns[i:]
			break
		}
	}
	for len(turns) > 0 && turns[0].Role != "user" {
		turns = turns[1:]
	}
	return turns
}

// tail keeps the last n bytes of s, on a rune boundary.
func tail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// LlamaGuardRequest is the chat completion request asking model to judge
// the conversation: deterministic, a few tokens.
func LlamaGuardRequest(model string, turns []Message) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": model, "messages": turns, "max_tokens": 32, "temperature": 0, "stream": false,
	})
	return b
}

var llamaCategory = regexp.MustCompile(`^S[1-9][0-9]?$`)

// ParseLlamaGuard reads Llama Guard's answer.
func ParseLlamaGuard(answer string) (Verdict, error) {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	switch strings.TrimSpace(lines[0]) {
	case "safe":
		return Verdict{}, nil
	case "unsafe":
	default:
		// The answer is not quoted: model output is content, and errors
		// reach logs and the audit log.
		return Verdict{}, errors.New("guard: Llama Guard answered neither safe nor unsafe")
	}
	v := Verdict{Flagged: true}
	if len(lines) > 1 {
		for _, c := range strings.Split(lines[1], ",") {
			c = strings.TrimSpace(c)
			if !llamaCategory.MatchString(c) {
				return Verdict{}, errors.New("guard: Llama Guard answered an unknown category")
			}
			v.Categories = append(v.Categories, c)
		}
	}
	if len(v.Categories) == 0 {
		// Unsafe without a category still blocks under the default action.
		v.Categories = []string{"unsafe"}
	}
	return v, nil
}

// ChatAnswer returns choices[0].message.content of a chat completion.
func ChatAnswer(body []byte) (string, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", errors.New("guard: the guard model's answer is not a chat completion")
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == nil {
		return "", errors.New("guard: the guard model answered no content")
	}
	return *r.Choices[0].Message.Content, nil
}

// ---- Prompt Guard ----
//
// Llama Prompt Guard 2 is a sequence classifier (mDeBERTa) labelling a text
// benign or malicious (an attempt to override prior instructions); its
// context is 512 tokens, and longer texts are to be split into segments
// scanned in parallel. vLLM serves it on /classify: {"model", "input": [..]}
// answers {"data": [{"index", "label", "probs", "num_classes"}]}.

// SegmentBytes is the size of a Prompt Guard segment: 512 tokens are at
// least this many bytes of text in practice.
const SegmentBytes = 1500

// MaxSegments bounds the segments of one request.
const MaxSegments = 64

// PromptGuardInput splits the user and tool texts (direct and indirect
// injections) into segments, newest last; it reports false when they do not
// fit in MaxSegments.
func PromptGuardInput(msgs []Message) ([]string, bool) {
	var segs []string
	for _, m := range msgs {
		switch m.Role {
		case "user", "tool", "prompt", "input":
		default:
			continue
		}
		s := m.Content
		for len(s) > 0 {
			n := min(len(s), SegmentBytes)
			for n < len(s) && n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
			if n == 0 {
				n = len(s)
			}
			segs = append(segs, s[:n])
			s = s[n:]
		}
	}
	return segs, len(segs) <= MaxSegments
}

// PromptGuardRequest is the /classify request.
func PromptGuardRequest(model string, segments []string) []byte {
	b, _ := json.Marshal(map[string]any{"model": model, "input": segments})
	return b
}

// ParsePromptGuard reads a /classify answer: a segment is an injection when
// its label is not benign and its top probability reaches threshold.
func ParsePromptGuard(body []byte, segments int, threshold float64) (Verdict, error) {
	var r struct {
		Data []struct {
			Index int       `json:"index"`
			Label string    `json:"label"`
			Probs []float64 `json:"probs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Verdict{}, errors.New("guard: the classifier's answer is not a classification")
	}
	if len(r.Data) != segments {
		return Verdict{}, fmt.Errorf("guard: the classifier judged %d segments of %d", len(r.Data), segments)
	}
	for _, d := range r.Data {
		if strings.EqualFold(d.Label, "benign") || strings.EqualFold(d.Label, "label_0") {
			continue
		}
		top := 0.0
		for _, p := range d.Probs {
			if !(p >= 0 && p <= 1) {
				return Verdict{}, errors.New("guard: the classifier returned a probability outside [0, 1]")
			}
			top = max(top, p)
		}
		if top >= threshold {
			return Verdict{Flagged: true, Categories: []string{"injection"}}, nil
		}
	}
	return Verdict{}, nil
}

// ---- generic HTTP guardrail ----
//
// The gateway POSTs {"guardrail", "stage", "messages", "completion"} and
// expects {"action": "allow" | "block" | "log", "categories": [...]}.

// HTTPRequest is the body sent to an HTTP guardrail.
func HTTPRequest(name, stage string, msgs []Message, completion string) []byte {
	b, _ := json.Marshal(map[string]any{"guardrail": name, "stage": stage, "messages": msgs, "completion": completion})
	return b
}

var categoryName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// ParseHTTP reads an HTTP guardrail's answer.
func ParseHTTP(body []byte) (Verdict, error) {
	var r struct {
		Action     string   `json:"action"`
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Verdict{}, errors.New("guard: the guardrail service's answer is not JSON")
	}
	if len(r.Categories) > 16 {
		return Verdict{}, errors.New("guard: the guardrail service returned more than 16 categories")
	}
	for _, c := range r.Categories {
		if !categoryName.MatchString(c) {
			return Verdict{}, errors.New("guard: the guardrail service returned an invalid category name")
		}
	}
	switch r.Action {
	case "allow":
		return Verdict{}, nil
	case "block", "log":
		cats := r.Categories
		if len(cats) == 0 {
			cats = []string{"flagged"}
		}
		return Verdict{Flagged: true, Categories: cats, Action: r.Action}, nil
	}
	return Verdict{}, errors.New("guard: the guardrail service answered an unknown action")
}
