package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
)

// ErrTooLarge: a response exceeded the size limit.
var ErrTooLarge = errors.New("mcp: response too large")

// errNoResponse: the stream ended without the response to the request.
var errNoResponse = errors.New("mcp: the server's stream ended without a response")

// incoming is any message a server sends.
type incoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *Error          `json:"error"`
}

// ReadResponse reads a server's answer to the request with string id "id":
// one JSON object (application/json) or a request-scoped SSE stream
// (text/event-stream) whose notifications, and the requests a legacy
// server may interleave, are skipped. At most max bytes are read.
func ReadResponse(contentType string, body io.Reader, id string, max int64) (Response, error) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return Response{}, fmt.Errorf("mcp: content type %q", contentType)
	}
	lr := &io.LimitedReader{R: body, N: max + 1}
	switch mt {
	case "application/json":
		b, err := io.ReadAll(lr)
		if err != nil {
			return Response{}, err
		}
		if int64(len(b)) > max {
			return Response{}, ErrTooLarge
		}
		r, ok, err := matchResponse(b, id)
		if err != nil {
			return Response{}, err
		}
		if !ok {
			return Response{}, errors.New("mcp: the response does not answer the request")
		}
		return r, nil
	case "text/event-stream":
		r, err := readSSE(lr, id, max)
		if lr.N <= 0 {
			return Response{}, ErrTooLarge
		}
		return r, err
	default:
		return Response{}, fmt.Errorf("mcp: content type %q", mt)
	}
}

// matchResponse parses one message; ok when it is the response to id.
func matchResponse(b []byte, id string) (Response, bool, error) {
	var m incoming
	if err := json.Unmarshal(b, &m); err != nil {
		return Response{}, false, fmt.Errorf("mcp: invalid JSON from the server")
	}
	if m.Method != "" {
		return Response{}, false, nil // a notification or a server request
	}
	var got string
	if json.Unmarshal(m.ID, &got) != nil || got != id {
		return Response{}, false, nil
	}
	if m.JSONRPC != "2.0" || (m.Error == nil) == (m.Result == nil) {
		return Response{}, false, errors.New("mcp: malformed response from the server")
	}
	return Response{JSONRPC: "2.0", ID: m.ID, Result: m.Result, Error: m.Error}, true, nil
}

// readSSE follows the WHATWG event-stream interpretation: lines end with
// CRLF, LF or CR; "data" lines accumulate (joined by LF); a blank line
// dispatches; comments and other fields are ignored.
func readSSE(r io.Reader, id string, max int64) (Response, error) {
	sc := bufio.NewScanner(r)
	limit := int(min(max, 1<<30)) + 1
	sc.Buffer(make([]byte, 0, min(limit, 64<<10)), limit)
	sc.Split(sseLines)
	var data bytes.Buffer
	dispatch := func() (Response, bool, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return Response{}, false, nil
		}
		return matchResponse(data.Bytes(), id)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if r, ok, err := dispatch(); err != nil || ok {
				return r, err
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		if string(field) == "data" {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Response{}, ErrTooLarge
		}
		return Response{}, err
	}
	// End of stream: an event not followed by a blank line is discarded
	// (WHATWG), so is this one.
	return Response{}, errNoResponse
}

// sseLines splits on CRLF, LF or CR.
func sseLines(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		switch c {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil // a LF may follow
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
