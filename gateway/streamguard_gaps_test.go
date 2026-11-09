package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGuardedStreamInspectsToolCallsReasoningAndLongUserinfo covers the three
// things a guarded stream used to refuse, strip or miss.
func TestGuardedStreamInspectsToolCallsReasoningAndLongUserinfo(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "secrets" {
  type  = "hide_secrets"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	chunk := func(delta, finish string) string {
		if finish == "" {
			finish = "null"
		}
		return `{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + `}]}`
	}

	// Streamed tool calls: the arguments are masked, in one chunk, before the end.
	args1, args2 := `{\"token\":\"`+ghToken[:10], ghToken[10:]+`\",\"n\":1}`
	up.setRawStream(
		chunk(`{"role":"assistant"}`, ""),
		chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run","arguments":""}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"`+args1+`"}}]}`, ""),
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"`+args2+`"}}]}`, ""),
		chunk(`{}`, `"tool_calls"`),
	)
	code, out := e.chatText(key, chatBody("call it", true))
	if code != 200 || strings.Contains(out, ghToken) || strings.Contains(out, ghToken[10:]) {
		t.Fatalf("%d %s", code, out)
	}
	var got strings.Builder
	chunks := 0
	for _, ev := range strings.Split(out, "\n\n") {
		data := strings.TrimPrefix(ev, "data: ")
		if !strings.HasPrefix(ev, "data: {") {
			continue
		}
		var c struct {
			Error   any
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if c.Error != nil {
			t.Fatalf("the stream was stopped: %s", out)
		}
		for _, ch := range c.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				chunks++
				got.WriteString(tc.ID + "|" + tc.Function.Name + "|" + tc.Function.Arguments)
			}
		}
	}
	if chunks != 1 || got.String() != `call_1|run|{"n":1,"token":"[TOKEN]"}` {
		t.Fatalf("%d tool chunks, got %q\n%s", chunks, got.String(), out)
	}

	// Reasoning text is masked, not stripped.
	up.setRawStream(
		chunk(`{"reasoning_content":"my key is `+ghToken[:10]+`"}`, ""),
		chunk(`{"reasoning_content":"`+ghToken[10:]+` ok"}`, ""),
		chunk(`{"content":"done"}`, `"stop"`),
	)
	code, out = e.chatText(key, chatBody("think", true))
	if code != 200 || strings.Contains(out, ghToken[10:]) || !strings.Contains(out, "[TOKEN]") || !strings.Contains(out, "reasoning_content") ||
		!strings.Contains(out, "done") {
		t.Fatalf("%d %s", code, out)
	}

	// A URL whose userinfo starts far before the `@` that makes it a secret.
	up.setRawStream()
	userinfo := "https://" + strings.Repeat("u", 600) + ":s3cr3tpassw0rd@host.example/x"
	var parts []string
	for s := "see " + userinfo + " and on"; s != ""; {
		k := min(40, len(s))
		parts, s = append(parts, s[:k]), s[k:]
	}
	up.setRawStream()
	up.rawStream = nil
	up.setReply(parts...)
	code, out = e.chatText(key, chatBody("url", true))
	if code != 200 || strings.Contains(out, "uuuuuuuuuu") || strings.Contains(out, "s3cr3tpassw0rd") || !strings.Contains(out, "host.example/x and on") {
		t.Fatalf("%d %s", code, out)
	}
}
