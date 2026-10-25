package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// audioForm builds a transcription form: fields in order, then a file.
func audioForm(t testing.TB, filename string, audio []byte, fields ...string) (string, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		if err := mw.WriteField(fields[i], fields[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if audio != nil {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
		h.Set("Content-Type", "audio/wav")
		pw, _ := mw.CreatePart(h)
		_, _ = pw.Write(audio)
	}
	_ = mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

func (e *env) transcribe(key, ctype string, body []byte) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/audio/transcriptions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.StatusCode, readAll(e.t, resp), resp.Header
}

func TestTranscriptionProxied(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.ModelGroups[0].Cost.AudioPerHour = 2 // GPU-hours per hour of audio
	cfg.Limits.MaxAudioBytes = 1000
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")

	ct, form := audioForm(t, `..\..\etc/pass wd.wav`, bytes.Repeat([]byte{1}, 500), "model", "llama", "language", "fr", "priority", "-9", "request_id", "x")
	code, out, _ := e.transcribe(key, ct, form)
	if code != 200 || !strings.Contains(out, `"text":"hello world"`) {
		t.Fatalf("%d %s", code, out)
	}
	got := up.last()
	if got.Body["model"] != "meta/llama" || got.Body["language"] != "fr" || got.Body["priority"] != nil || got.Body["request_id"] != nil ||
		got.Body["file_name"] != "pass_wd.wav" || got.Body["file_type"] != "audio/wav" || got.Body["file_bytes"] != 500 {
		t.Fatalf("upstream got %v", got.Body)
	}
	// 90 s of audio at 2 GPU-hours per hour.
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status == 200 })
	if r.Cost < 0.0499 || r.Cost > 0.0501 || r.Estimated {
		t.Fatalf("%+v", r)
	}

	// Plain text: vLLM is asked for JSON, the client gets the text.
	ct, form = audioForm(t, "a.wav", []byte{1, 2, 3}, "model", "llama", "response_format", "text")
	code, out, hdr := e.transcribe(key, ct, form)
	if code != 200 || out != "hello world" || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") || up.last().Body["response_format"] != "json" {
		t.Fatalf("%d %q %v", code, out, up.last().Body)
	}

	// Streamed: token usage counted, the usage chunk the client did not ask
	// for dropped.
	ct, form = audioForm(t, "a.wav", []byte{1, 2, 3}, "model", "llama", "stream", "true")
	code, out, _ = e.transcribe(key, ct, form)
	if code != 200 || !strings.Contains(out, "transcription.chunk") || strings.Contains(out, `"usage"`) || up.last().Body["stream_include_usage"] != "true" {
		t.Fatalf("%d %s", code, out)
	}
	r = waitRecord(t, e.st, func(r store.Record) bool { return r.Stream })
	if r.PromptTokens != 50 || r.CompletionTokens != 5 || r.Estimated {
		t.Fatalf("%+v", r)
	}

	for name, c := range map[string]struct {
		ct   string
		body []byte
		want int
	}{
		"too large": func() struct {
			ct   string
			body []byte
			want int
		} {
			ct, b := audioForm(t, "a.wav", bytes.Repeat([]byte{1}, 1001), "model", "llama")
			return struct {
				ct   string
				body []byte
				want int
			}{ct, b, 413}
		}(),
		"no file": func() struct {
			ct   string
			body []byte
			want int
		} {
			ct, b := audioForm(t, "", nil, "model", "llama")
			return struct {
				ct   string
				body []byte
				want int
			}{ct, b, 400}
		}(),
		"json body": {"application/json", []byte(`{"model":"llama"}`), 400},
		"bad format": func() struct {
			ct   string
			body []byte
			want int
		} {
			ct, b := audioForm(t, "a.wav", []byte{1}, "model", "llama", "response_format", "srt")
			return struct {
				ct   string
				body []byte
				want int
			}{ct, b, 400}
		}(),
	} {
		if code, out, _ := e.transcribe(key, c.ct, c.body); code != c.want {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
}

func TestTranscriptionGuarded(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "pii" {
  type  = "pii"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	up.setReply("call me at jean@example.fr")
	ct, form := audioForm(t, "a.wav", []byte{1, 2}, "model", "llama", "prompt", "names: jean@example.fr")
	code, out, _ := e.transcribe(key, ct, form)
	if code != 200 || strings.Contains(out, "jean@") || !strings.Contains(out, "[EMAIL]") || strings.Contains(out, "segments") {
		t.Fatalf("%d %s", code, out)
	}
	if p := up.last().Body["prompt"]; p != "names: [EMAIL]" {
		t.Fatalf("vLLM got prompt %v", p)
	}
}

func FuzzReadAudioForm(f *testing.F) {
	_, form := audioForm(f, "a.wav", []byte("RIFF"), "model", "m", "timestamp_granularities[]", "word", "timestamp_granularities[]", "segment")
	f.Add(form)
	f.Add([]byte("--B\r\nContent-Disposition: form-data; name=\"file\"; filename=\"../x\"\r\n\r\nabc\r\n--B--\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		boundary := "B"
		if i := bytes.Index(in, []byte("\r\n")); i > 2 && bytes.HasPrefix(in, []byte("--")) {
			boundary = string(in[2:i])
		}
		view, file, err := readAudioForm(bytes.NewReader(in), boundary, 64)
		if err != nil {
			return
		}
		if file == nil || len(file.data) == 0 || len(file.data) > 64 || strings.ContainsAny(file.name, `/\ `) || file.name == ".." {
			t.Fatalf("bad file %+v", file)
		}
		var m map[string]any
		if json.Unmarshal(view, &m) != nil {
			t.Fatalf("view %q", view)
		}
		// The rebuilt form parses back to the same file.
		out, ct, err := buildAudioForm(view, file)
		if err != nil {
			return
		}
		got := parseFakeForm(ct, out)
		if got["file_bytes"] != len(file.data) || got["file_name"] != file.name {
			t.Fatalf("rebuilt %v from %+v", got, file)
		}
	})
}
