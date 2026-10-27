package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShapeOf(t *testing.T) {
	for _, c := range []struct {
		name string
		ep   Endpoint
		body string
		want Shape
		err  bool
	}{
		{"chat default", Chat, `{"model":"m","messages":[]}`, Shape{Generates: true, Inputs: 1, Choices: 1}, false},
		{"chat n and max_tokens", Chat, `{"n":3,"max_tokens":50}`, Shape{Generates: true, Inputs: 1, Choices: 3, MaxTokens: 50, HasMaxTokens: true}, false},
		// vLLM: max_completion_tokens wins, even when 0.
		{"chat max_completion_tokens wins", Chat, `{"max_completion_tokens":0,"max_tokens":100}`, Shape{Generates: true, Inputs: 1, Choices: 1, HasMaxTokens: true}, false},
		{"chat null max_completion_tokens", Chat, `{"max_completion_tokens":null,"max_tokens":7}`, Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 7, HasMaxTokens: true}, false},
		{"chat best_of above n", Chat, `{"n":2,"best_of":5}`, Shape{Generates: true, Inputs: 1, Choices: 5}, false},
		{"chat n zero", Chat, `{"n":0}`, Shape{}, true},
		{"chat n string", Chat, `{"n":"4"}`, Shape{}, true},
		{"chat n float", Chat, `{"n":2.0}`, Shape{}, true},
		{"chat n exponent", Chat, `{"n":1e2}`, Shape{}, true},
		{"chat max_tokens negative", Chat, `{"max_tokens":-1}`, Shape{}, true},
		{"chat max_tokens huge", Chat, `{"max_tokens":9223372036854775808}`, Shape{}, true},
		// Completions: max_tokens defaults to 16, null included.
		{"completion default 16", Completions, `{"prompt":"hi"}`, Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 16, HasMaxTokens: true}, false},
		{"completion null 16", Completions, `{"prompt":"hi","max_tokens":null}`, Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 16, HasMaxTokens: true}, false},
		{"completion prompt list", Completions, `{"prompt":["a","b","c"],"n":2,"max_tokens":5}`, Shape{Generates: true, Inputs: 3, Choices: 2, MaxTokens: 5, HasMaxTokens: true}, false},
		{"completion token ids", Completions, `{"prompt":[1,2,3],"max_tokens":5}`, Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 5, HasMaxTokens: true}, false},
		{"completion token lists", Completions, `{"prompt":[[1],[2]],"max_tokens":5}`, Shape{Generates: true, Inputs: 2, Choices: 1, MaxTokens: 5, HasMaxTokens: true}, false},
		{"completion prompt and embeds", Completions, `{"prompt":["a"],"prompt_embeds":["x","y"],"max_tokens":5}`, Shape{Generates: true, Inputs: 3, Choices: 1, MaxTokens: 5, HasMaxTokens: true}, false},
		{"responses", Responses, `{"input":"x","max_output_tokens":9}`, Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 9, HasMaxTokens: true}, false},
		{"responses function tool", Responses, `{"tools":[{"type":"function","name":"f"}]}`, Shape{Generates: true, Inputs: 1, Choices: 1}, false},
		{"messages", Messages, `{"max_tokens":64,"tools":[{"name":"t","input_schema":{}},{"type":"custom","name":"u"},{"type":"bash_20250124","name":"bash"}]}`,
			Shape{Generates: true, Inputs: 1, Choices: 1, MaxTokens: 64, HasMaxTokens: true}, false},
		{"embeddings strings", Embeddings, `{"input":["a","b"]}`, Shape{Inputs: 2, Choices: 1}, false},
		{"embeddings one conversation", Embeddings, `{"input":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, Shape{Inputs: 1, Choices: 1}, false},
		{"embeddings batched conversations", Embeddings, `{"messages":[[{"role":"user","content":"a"}],[{"role":"user","content":"b"}]]}`, Shape{Inputs: 2, Choices: 1}, false},
		{"embeddings input and messages", Embeddings, `{"input":["a","b"],"messages":[{"role":"user","content":"c"}]}`, Shape{Inputs: 3, Choices: 1}, false},
		{"rerank", Rerank, `{"query":"q","documents":["a","b","c"]}`, Shape{Inputs: 4, Choices: 1}, false},
		{"rerank content", Rerank, `{"query":"q","documents":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`, Shape{Inputs: 3, Choices: 1}, false},
		{"tokenize free", Tokenize, `{}`, Shape{Free: true, Inputs: 1, Choices: 1}, false},
		{"not an object", Chat, `[1]`, Shape{}, true},
	} {
		got, err := ShapeOf(c.ep, []byte(c.body))
		if (err != nil) != c.err {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestShapeUnbounded(t *testing.T) {
	for _, c := range []struct {
		ep   Endpoint
		body string
	}{
		{Responses, `{"tools":[{"type":"function","name":"f"},{"type":"web_search_preview"}]}`},
		{Responses, `{"tools":[{"type":"mcp","server_label":"x"}]}`},
		{Responses, `{"tools":[{"name":"no type"}]}`},
		{Responses, `{"tools":"x"}`},
		{Messages, `{"max_tokens":8,"tools":[{"type":"web_search_20250305","name":"web_search"}]}`},
		{Messages, `{"max_tokens":8,"tools":[{"type":"code_execution_20250825","name":"c"}]}`},
		{Messages, `{"max_tokens":8,"mcp_servers":[{"type":"url","url":"https://x"}]}`},
		{Messages, `{"max_tokens":8,"container":"c1"}`},
		{Transcriptions, `{}`},
	} {
		sh, err := ShapeOf(c.ep, []byte(c.body))
		if err != nil || sh.Unbounded == "" {
			t.Errorf("%s: %+v %v, want unbounded", c.body, sh, err)
		}
	}
	// Empty lists and nulls are not tools.
	for _, b := range []string{`{"max_tokens":8,"mcp_servers":[]}`, `{"max_tokens":8,"container":null,"tools":null}`} {
		if sh, err := ShapeOf(Messages, []byte(b)); err != nil || sh.Unbounded != "" {
			t.Errorf("%s: %+v %v", b, sh, err)
		}
	}
}

func TestSetMaxTokens(t *testing.T) {
	for _, c := range []struct {
		ep     Endpoint
		body   string
		fields []string
	}{
		{Chat, `{"model":"m","max_tokens":5,"n":2}`, []string{"max_tokens", "max_completion_tokens"}},
		{Completions, `{"model":"m"}`, []string{"max_tokens"}},
		{Messages, `{"model":"m","max_tokens":5}`, []string{"max_tokens"}},
		{Responses, `{"model":"m"}`, []string{"max_output_tokens"}},
	} {
		out, err := SetMaxTokens(c.ep, []byte(c.body), 42)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatal(err)
		}
		for _, f := range c.fields {
			if m[f] != float64(42) {
				t.Errorf("%s: %s = %v in %s", c.body, f, m[f], out)
			}
		}
		if c.ep == Chat && m["n"] != float64(2) {
			t.Errorf("n lost: %s", out)
		}
	}
	if _, err := SetMaxTokens(Chat, []byte(`{"n":"2"}`), 1); err == nil {
		t.Fatal("a string n was rewritten")
	}
	if _, err := SetMaxTokens(Chat, []byte(`{}`), -1); err == nil {
		t.Fatal("a negative bound was written")
	}
	if out, err := SetMaxTokens(Embeddings, []byte(`{"input":"x"}`), 3); err != nil || strings.Contains(string(out), "max_tokens") {
		t.Fatalf("pooling body changed: %s %v", out, err)
	}
}

// FuzzShapeOf: never panics, and an accepted shape always has at least one
// input and one choice.
func FuzzShapeOf(f *testing.F) {
	for _, s := range []string{`{"n":2,"max_tokens":3}`, `{"prompt":["a",[1,2],"c"]}`, `{"input":[{"role":"user"}]}`,
		`{"documents":{"content":[1,2]}}`, `{"tools":[{"type":"web_search"}]}`, `{"max_completion_tokens":null}`} {
		for ep := Chat; ep <= CountTokens; ep++ {
			f.Add(int(ep), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, ep int, body []byte) {
		e := Endpoint(ep % int(CountTokens+1))
		if ep < 0 {
			e = Chat
		}
		sh, err := ShapeOf(e, body)
		if err != nil {
			return
		}
		if sh.Inputs < 1 || sh.Choices < 1 || sh.MaxTokens < 0 {
			t.Fatalf("%d %q: %+v", e, body, sh)
		}
		if out, err := SetMaxTokens(e, body, 7); err == nil && sh.Generates && e != Transcriptions {
			again, err := ShapeOf(e, out)
			if err != nil || !again.HasMaxTokens || again.MaxTokens != 7 || again.Choices != sh.Choices || again.Inputs != sh.Inputs {
				t.Fatalf("%d %q: after SetMaxTokens %+v %v (before %+v)", e, body, again, err, sh)
			}
		}
	})
}
