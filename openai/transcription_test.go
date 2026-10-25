package openai

import (
	"slices"
	"testing"
)

func TestTranscriptionRewriteAndUsage(t *testing.T) {
	out, req, err := RewriteFor(Transcriptions, []byte(`{"model":"m","response_format":"text","priority":"1","language":"fr"}`), resolveAll, Isolation{})
	b := decode(t, out)
	if err != nil || !req.TextOut || b["response_format"] != "json" || b["priority"] != nil || b["model"] != "up-m" || b["language"] != "fr" {
		t.Fatalf("%s %+v %v", out, req, err)
	}
	if _, _, err := RewriteFor(Transcriptions, []byte(`{"model":"m","response_format":"text","stream":"true"}`), resolveAll, Isolation{}); err == nil {
		t.Fatal("a streamed text transcription was accepted")
	}
	_, req, _ = RewriteFor(Transcriptions, []byte(`{"model":"m","stream":"true","max_completion_tokens":"12"}`), resolveAll, Isolation{})
	if !req.Stream || req.ClientWantsUsage || req.MaxTokens != 12 {
		t.Fatalf("%+v", req)
	}
	if u, ok := ParseUsageFor(Transcriptions, []byte(`{"text":"x","usage":{"type":"duration","seconds":90}}`)); !ok || u.AudioSeconds != 90 {
		t.Fatalf("%+v %v", u, ok)
	}
	if u, ok := ParseUsageFor(Transcriptions, []byte(`{"text":"x","duration":12.5}`)); !ok || u.AudioSeconds != 12.5 {
		t.Fatalf("%+v %v", u, ok)
	}
	if _, ok := ParseUsageFor(Transcriptions, []byte(`{"usage":{"type":"duration","seconds":1e12}}`)); ok {
		t.Fatal("absurd duration accepted")
	}
}

func FuzzTranscriptionRewrite(f *testing.F) {
	f.Add([]byte(`{"model":"m","response_format":"verbose_json","temperature":"0.2","cache_salt":"x"}`))
	f.Add([]byte(`{"model":"m","stream":"1","stream_include_usage":"true","timestamp_granularities[]":["word"]}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		if u, ok := ParseUsageFor(Transcriptions, in); ok && !u.Valid() {
			t.Fatalf("invalid usage accepted: %+v", u)
		}
		out, _, err := RewriteFor(Transcriptions, in, resolveAll, Isolation{})
		if err != nil {
			return
		}
		m, err := decodeObject(out)
		if err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !slices.Contains(TranscriptionFields, k) {
				t.Fatalf("field %q passed: %s", k, out)
			}
		}
	})
}
