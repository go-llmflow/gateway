package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"regexp"
	"strings"

	"github.com/go-llmflow/gateway/openai"
)

// Audio transcription forms (RFC 7578 multipart/form-data). The gateway
// reads the form once, bounded, into the uploaded file and a JSON view of
// the other fields; the view goes through the shared pipeline (model
// resolution, policy, guardrails on the prompt, the content log, which so
// never holds the audio); the form sent to vLLM is rebuilt from the
// rewritten view and the file, so nothing of the client's form reaches
// vLLM unread.

// Bounds of a transcription form beyond the file itself.
const (
	maxFormParts = 32
	maxFormField = 8 << 10
	formOverhead = 64 << 10
)

// audioFile is the uploaded audio.
type audioFile struct {
	name, ctype string
	data        []byte
}

var (
	errNotMultipart = errors.New("the request must be multipart/form-data")
	errAudioTooBig  = errors.New("audio file too large")
	safeFilename    = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	mediaType       = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]{0,63}/[a-z0-9][a-z0-9!#$&^_.+-]{0,63}$`)
)

// parseAudioForm reads a transcription form of at most limit bytes of audio.
func parseAudioForm(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, *audioFile, error) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, nil, errNotMultipart
	}
	return readAudioForm(http.MaxBytesReader(w, r.Body, limit+formOverhead), params["boundary"], limit)
}

func readAudioForm(body io.Reader, boundary string, limit int64) ([]byte, *audioFile, error) {
	mr := multipart.NewReader(body, boundary)
	view := map[string]any{}
	var file *audioFile
	for n := 0; ; n++ {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return nil, nil, errAudioTooBig
			}
			return nil, nil, fmt.Errorf("malformed form: %w", err)
		}
		if n >= maxFormParts {
			return nil, nil, errors.New("too many form fields")
		}
		name := p.FormName()
		if name == "" {
			return nil, nil, errors.New("a form part has no name")
		}
		if name == "file" {
			if file != nil {
				return nil, nil, errors.New("one audio file per request")
			}
			data, err := io.ReadAll(io.LimitReader(p, limit+1))
			if err != nil {
				var mbe *http.MaxBytesError
				if errors.As(err, &mbe) {
					return nil, nil, errAudioTooBig
				}
				return nil, nil, fmt.Errorf("reading the audio: %w", err)
			}
			if int64(len(data)) > limit {
				return nil, nil, errAudioTooBig
			}
			file = &audioFile{name: cleanFilename(p.FileName()), ctype: cleanMediaType(p.Header.Get("Content-Type")), data: data}
			continue
		}
		v, err := io.ReadAll(io.LimitReader(p, maxFormField+1))
		if err != nil || len(v) > maxFormField {
			return nil, nil, fmt.Errorf("form field %q too long or unreadable", truncateName(name))
		}
		if name == "timestamp_granularities[]" {
			list, _ := view[name].([]string)
			view[name] = append(list, string(v))
			continue
		}
		if _, dup := view[name]; dup {
			return nil, nil, fmt.Errorf("form field %q given twice", truncateName(name))
		}
		view[name] = string(v)
	}
	if file == nil || len(file.data) == 0 {
		return nil, nil, errors.New(`the form has no "file"`)
	}
	b, err := json.Marshal(view)
	return b, file, err
}

// cleanFilename keeps the base name, in a safe alphabet: vLLM uses its
// extension to guess the format; nothing else of it is needed (RFC 7578
// §4.2: a receiver must not use the filename blindly).
func cleanFilename(name string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	name = safeFilename.ReplaceAllString(name, "_")
	if len(name) > 64 {
		name = name[len(name)-64:]
	}
	if name == "" || name == "." || name == ".." || strings.Trim(name, "_.") == "" {
		return "audio"
	}
	return name
}

func cleanMediaType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || !mediaType.MatchString(mt) {
		return "application/octet-stream"
	}
	return mt
}

func truncateName(s string) string {
	if len(s) > 32 {
		return s[:32]
	}
	return s
}

// buildAudioForm writes the form for vLLM from the rewritten view and the
// file: only the transcription fields, as strings.
func buildAudioForm(view []byte, f *audioFile) ([]byte, string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(view, &m); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range openai.TranscriptionFields {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var values []string
		var s string
		if json.Unmarshal(raw, &s) == nil {
			values = []string{s}
		} else if json.Unmarshal(raw, &values) != nil {
			return nil, "", fmt.Errorf("form field %q is not text", k)
		}
		for _, v := range values {
			if err := mw.WriteField(k, v); err != nil {
				return nil, "", err
			}
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.name))
	h.Set("Content-Type", f.ctype)
	pw, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := pw.Write(f.data); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}
