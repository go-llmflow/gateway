package export

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The test parser follows the ABNF of RFC 5424 section 6 closely enough to
// refuse every message the formatter must never produce.

var (
	reTimestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
	reSDName    = regexp.MustCompile(`^[\x21-\x7e]{1,32}$`)
)

type sdParam struct{ name, value string }

type parsedSyslog struct {
	pri                                 int
	timestamp, host, app, procid, msgid string
	sdID                                string
	params                              []sdParam
	msg                                 []byte // after the BOM
	hasMsg                              bool
}

func parseSyslogMsg(b []byte) (p parsedSyslog, err error) {
	if len(b) < 4 || b[0] != '<' {
		return p, errors.New("no PRI")
	}
	end := bytes.IndexByte(b, '>')
	if end < 2 || end > 4 {
		return p, errors.New("bad PRI")
	}
	p.pri, err = strconv.Atoi(string(b[1:end]))
	if err != nil || p.pri > 191 || (end > 2 && b[1] == '0') {
		return p, errors.New("bad PRI value")
	}
	rest := b[end+1:]
	if !bytes.HasPrefix(rest, []byte("1 ")) {
		return p, errors.New("bad VERSION")
	}
	rest = rest[2:]
	var f [5]string
	for i := range f {
		j := bytes.IndexByte(rest, ' ')
		if j < 1 {
			return p, fmt.Errorf("header field %d empty", i)
		}
		f[i], rest = string(rest[:j]), rest[j+1:]
	}
	p.timestamp, p.host, p.app, p.procid, p.msgid = f[0], f[1], f[2], f[3], f[4]
	if !reTimestamp.MatchString(p.timestamp) {
		return p, fmt.Errorf("bad TIMESTAMP %q", p.timestamp)
	}
	for i, lim := range []int{255, 48, 128, 32} {
		s := f[i+1]
		if len(s) > lim {
			return p, fmt.Errorf("header field %d too long", i+1)
		}
		for _, c := range []byte(s) {
			if c < 33 || c > 126 {
				return p, fmt.Errorf("header field %d: byte %d", i+1, c)
			}
		}
	}
	// STRUCTURED-DATA: one element in what we produce.
	if len(rest) == 0 || rest[0] != '[' {
		return p, errors.New("no structured data")
	}
	i := 1
	j := i
	for j < len(rest) && rest[j] != ' ' && rest[j] != ']' {
		j++
	}
	p.sdID = string(rest[i:j])
	name, num, ok := strings.Cut(p.sdID, "@")
	if !ok || !reSDName.MatchString(p.sdID) || strings.ContainsAny(name, "=]\"") || num == "" {
		return p, fmt.Errorf("bad SD-ID %q", p.sdID)
	}
	i = j
	for {
		if i >= len(rest) {
			return p, errors.New("unterminated element")
		}
		if rest[i] == ']' {
			i++
			break
		}
		if rest[i] != ' ' {
			return p, errors.New("expected SP in SD-ELEMENT")
		}
		i++
		k := i
		for k < len(rest) && rest[k] != '=' {
			k++
		}
		pn := string(rest[i:k])
		if !reSDName.MatchString(pn) || strings.ContainsAny(pn, " ]\"") {
			return p, fmt.Errorf("bad PARAM-NAME %q", pn)
		}
		if k+1 >= len(rest) || rest[k+1] != '"' {
			return p, errors.New("PARAM-VALUE not quoted")
		}
		k += 2
		var val []byte
		for {
			if k >= len(rest) {
				return p, errors.New("unterminated PARAM-VALUE")
			}
			c := rest[k]
			if c == '\\' {
				if k+1 >= len(rest) {
					return p, errors.New("dangling escape")
				}
				val = append(val, rest[k+1])
				k += 2
				continue
			}
			if c == '"' {
				k++
				break
			}
			if c == ']' {
				return p, errors.New("unescaped ] in PARAM-VALUE")
			}
			val = append(val, c)
			k++
		}
		if !utf8.Valid(val) {
			return p, errors.New("PARAM-VALUE is not UTF-8")
		}
		p.params = append(p.params, sdParam{pn, string(val)})
		i = k
	}
	rest = rest[i:]
	if len(rest) == 0 {
		return p, nil
	}
	if rest[0] != ' ' {
		return p, errors.New("expected SP before MSG")
	}
	rest = rest[1:]
	p.hasMsg = true
	if bytes.HasPrefix(rest, []byte("\xEF\xBB\xBF")) {
		rest = rest[3:]
	} else {
		return p, errors.New("MSG without BOM")
	}
	if !utf8.Valid(rest) {
		return p, errors.New("MSG is not UTF-8")
	}
	p.msg = rest
	return p, nil
}

func testCfg() SyslogConfig {
	return SyslogConfig{Name: "s", Transport: "tcp", Hostname: "gw1", AppName: "llmflow-gateway", SDID: "llmflow@32473",
		Facility: 16, MaxMessageBytes: 8192}
}

func TestFormatSyslogRFC5424(t *testing.T) {
	ts := time.Date(2026, 11, 9, 14, 30, 5, 123456000, time.UTC)
	line := []byte(`{"team":"lab","tokens":12}`)
	msg, trunc := FormatSyslog(testCfg(), "4242", ts, "logs", "logs/lab/2026-11-09.ndjson.gz", 2, 3, line)
	if trunc {
		t.Fatal("truncated")
	}
	p, err := parseSyslogMsg(msg)
	if err != nil {
		t.Fatalf("%v: %q", err, msg)
	}
	if p.pri != 16*8+6 || p.timestamp != "2026-11-09T14:30:05.123456Z" || p.host != "gw1" || p.app != "llmflow-gateway" ||
		p.procid != "4242" || p.msgid != "logs" || p.sdID != "llmflow@32473" || string(p.msg) != string(line) {
		t.Fatalf("parsed %+v", p)
	}
	want := []sdParam{{"object", "logs/lab/2026-11-09.ndjson.gz"}, {"seq", "2"}, {"of", "3"}}
	if fmt.Sprint(p.params) != fmt.Sprint(want) {
		t.Fatalf("params %v, want %v", p.params, want)
	}
}

func TestFormatSyslogInjection(t *testing.T) {
	// A hostile key and line: every special character of RFC 5424 section
	// 6.3.3 and every control character.
	hostile := "a\"b\\c]d\ne\r\x00\x1b[31m\xff\xfe é "
	c := testCfg()
	c.Hostname, c.AppName = "host name\n<13>1", "app\"x]"
	msg, _ := FormatSyslog(c, "pid\n1", time.Unix(0, 0), "id with space", hostile, 1, 1, []byte(hostile+"\n<13>1 forged"))
	if bytes.ContainsAny(msg, "\r\n\x00\x1b") {
		t.Fatalf("control byte in %q", msg)
	}
	p, err := parseSyslogMsg(msg)
	if err != nil {
		t.Fatalf("%v: %q", err, msg)
	}
	if strings.ContainsAny(p.host+p.app+p.procid+p.msgid, " \n") {
		t.Fatalf("header %+v", p)
	}
	if got := p.params[0].value; strings.ContainsAny(got, "\n\r\x00\x1b") {
		t.Fatalf("object %q", got)
	}
}

func TestFormatSyslogTruncation(t *testing.T) {
	c := testCfg()
	c.MaxMessageBytes = 1024
	big := bytes.Repeat([]byte("x"), 5000)
	msg, trunc := FormatSyslog(c, "1", time.Unix(0, 0), "logs", "logs/k", 1, 1, big)
	if !trunc || len(msg) > 1024 {
		t.Fatalf("trunc %v len %d", trunc, len(msg))
	}
	p, err := parseSyslogMsg(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.msg), `"truncated":true`) || !strings.Contains(string(p.msg), "5000") {
		t.Fatalf("marker %q", p.msg)
	}
	var seen bool
	for _, q := range p.params {
		seen = seen || q.name == "truncated" && q.value == "true"
	}
	if !seen {
		t.Fatalf("no truncated param: %v", p.params)
	}
	// Exactly at the limit is not truncated.
	small := bytes.Repeat([]byte("y"), 100)
	m2, _ := FormatSyslog(c, "1", time.Unix(0, 0), "logs", "logs/k", 1, 1, small)
	m3, tr := FormatSyslog(c, "1", time.Unix(0, 0), "logs", "logs/k", 1, 1, bytes.Repeat([]byte("y"), 100+c.MaxMessageBytes-len(m2)))
	if tr || len(m3) != c.MaxMessageBytes {
		t.Fatalf("boundary: trunc %v len %d", tr, len(m3))
	}
}

func FuzzFormatSyslog(f *testing.F) {
	f.Add("logs/a/b.ndjson", "{\"a\":1}", "host", "app", "7")
	f.Add("k\"]\\", "\x00\xff\n", "h h", "a\"", "p\n")
	f.Fuzz(func(t *testing.T, key, line, host, app, pid string) {
		c := testCfg()
		c.Hostname, c.AppName, c.MaxMessageBytes = host, app, 1024
		msg, _ := FormatSyslog(c, pid, time.Unix(1e9, 0), key, key, 1, 1, []byte(line))
		// Header fields are bounded (255+48+128+32 bytes and the object
		// parameter at 200 characters, each escaped at most four-fold), so the
		// message stays near the limit whatever the input.
		if len(msg) > 1024+2000 {
			t.Fatalf("message of %d bytes", len(msg))
		}
		if bytes.ContainsAny(msg, "\r\n\x00") {
			t.Fatalf("control byte in %q", msg)
		}
		if _, err := parseSyslogMsg(msg); err != nil {
			t.Fatalf("%v: %q", err, msg)
		}
	})
}

func gz(t testing.TB, s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	io.WriteString(w, s)
	w.Close()
	return b.Bytes()
}

func FuzzSplitLines(f *testing.F) {
	f.Add([]byte("a\nb\r\n\n"), "")
	f.Add(gz(f, "x\ny"), "gzip")
	f.Add([]byte("not gzip"), "gzip")
	f.Fuzz(func(t *testing.T, body []byte, enc string) {
		lines, err := splitLines(body, enc)
		if err != nil {
			return
		}
		for _, l := range lines {
			if len(l) == 0 || bytes.IndexByte(l, '\n') >= 0 {
				t.Fatalf("bad line %q", l)
			}
		}
	})
}

func TestSplitLinesBomb(t *testing.T) {
	// 100 MiB of zeros compress to ~100 KiB: refused, not expanded.
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(make([]byte, 100<<20))
	w.Close()
	if _, err := splitLines(b.Bytes(), "gzip"); err == nil {
		t.Fatal("a batch past the bound was accepted")
	}
	if _, err := splitLines([]byte("x"), "br"); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}

// readFrames reads octet-counted frames (RFC 6587 3.4.1) until EOF.
func readFrames(t *testing.T, r io.Reader, n int) [][]byte {
	t.Helper()
	br := bufio.NewReader(r)
	var out [][]byte
	for len(out) < n {
		l, err := br.ReadString(' ')
		if err != nil {
			t.Fatalf("frame %d: %v", len(out), err)
		}
		size, err := strconv.Atoi(strings.TrimSuffix(l, " "))
		if err != nil || size < 1 {
			t.Fatalf("bad MSG-LEN %q", l)
		}
		b := make([]byte, size)
		if _, err := io.ReadFull(br, b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func TestSyslogTCPFraming(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan [][]byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got <- readFrames(t, c, 3)
	}()
	cfg := testCfg()
	cfg.Address, cfg.InsecurePlain = ln.Addr().String(), true
	s, err := newSyslogSink(cfg, func() time.Time { return time.Unix(1e9, 0) })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A line with an embedded newline inside a JSON string is escaped by JSON, so
	// a raw newline never occurs in a line; a space-and-digits prefix inside the
	// message must not confuse the framing.
	body := gz(t, "{\"a\":\"12 <13>1 x\"}\n\n{\"b\":2}\n{\"c\":3}\n")
	if err := s.Put(context.Background(), "logs/lab/1.ndjson.gz", body, PutOptions{ContentType: "application/x-ndjson", ContentEncoding: "gzip"}); err != nil {
		t.Fatal(err)
	}
	frames := <-got
	for i, fr := range frames {
		p, err := parseSyslogMsg(fr)
		if err != nil {
			t.Fatalf("frame %d: %v: %q", i, err, fr)
		}
		if p.msgid != "logs" || p.params[1].value != strconv.Itoa(i+1) || p.params[2].value != "3" {
			t.Fatalf("frame %d: %+v", i, p)
		}
	}
	if !strings.Contains(string(frames[0]), `12 <13>1 x`) {
		t.Fatalf("content altered: %q", frames[0])
	}
	if s.Sent.Load() != 3 {
		t.Fatalf("sent %d", s.Sent.Load())
	}
}

func TestSyslogReconnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conns := make(chan [][]byte, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			conns <- readFrames(t, c, 1)
			c.Close()
		}
	}()
	cfg := testCfg()
	cfg.Address, cfg.InsecurePlain = ln.Addr().String(), true
	s, _ := newSyslogSink(cfg, time.Now)
	defer s.Close()
	for i := 0; i < 2; i++ {
		// The first connection is closed by the peer after one frame: a later Put
		// may write into the closed socket (TCP gives no acknowledgement, RFC 6587
		// section 3); retry as the queue does, until a frame arrives.
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.Put(context.Background(), "logs/k", []byte("{}\n"), PutOptions{ContentType: "application/json"})
			select {
			case <-conns:
			case <-time.After(200 * time.Millisecond):
				if time.Now().After(deadline) {
					t.Fatalf("round %d: no frame", i)
				}
				continue
			}
			break
		}
	}
}

func TestSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	cfg := testCfg()
	cfg.Transport, cfg.Address, cfg.InsecurePlain, cfg.MaxMessageBytes = "udp", pc.LocalAddr().String(), true, 0
	s, err := newSyslogSink(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.cfg.MaxMessageBytes != 2048 {
		t.Fatalf("udp default limit %d", s.cfg.MaxMessageBytes)
	}
	if err := s.Put(context.Background(), "logs/k", []byte("{\"a\":1}\n"), PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSyslogMsg(buf[:n]); err != nil {
		t.Fatalf("%v: %q", err, buf[:n])
	}
}

func TestSyslogTLS(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	srv.Close()
	cert := srv.TLS.Certificates[0]
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan []byte, 1)
	var state tls.ConnectionState
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got <- readFrames(t, c, 1)[0]
		state = c.(*tls.Conn).ConnectionState()
	}()
	cfg := testCfg()
	cfg.Transport, cfg.Address, cfg.CA = "tls", ln.Addr().String(), caPEM
	s, err := newSyslogSink(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Put(context.Background(), "logs/k", []byte("{\"a\":1}\n"), PutOptions{ContentType: "application/x-ndjson"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSyslogMsg(<-got); err != nil {
		t.Fatal(err)
	}
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("TLS version %x", state.Version)
	}

	// Without the CA, the server's certificate is refused: nothing is sent.
	cfg.CA = nil
	s2, err := newSyslogSink(cfg, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	err = s2.Put(context.Background(), "logs/k", []byte("{}\n"), PutOptions{ContentType: "application/json"})
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) {
		t.Fatalf("err = %v, want an unknown authority", err)
	}
	// A wrong server name is refused too.
	cfg.CA, cfg.ServerName = caPEM, "not-the-host.example"
	s3, _ := newSyslogSink(cfg, time.Now)
	defer s3.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	if err := s3.Put(context.Background(), "logs/k", []byte("{}\n"), PutOptions{ContentType: "application/json"}); err == nil {
		t.Fatal("a certificate for another name was accepted")
	}
}

func TestSyslogRefusals(t *testing.T) {
	now := time.Now
	for _, c := range []struct {
		name string
		mod  func(*SyslogConfig)
		want string
	}{
		{"plain without opt-in", func(c *SyslogConfig) { c.InsecurePlain = false; c.Address = "127.0.0.1:514" }, "insecure_plain"},
		{"plain to a public address", func(c *SyslogConfig) { c.InsecurePlain = true; c.Address = "8.8.8.8:514" }, "only loopback and private"},
		{"udp to a public address", func(c *SyslogConfig) { c.Transport, c.InsecurePlain = "udp", true; c.Address = "93.184.216.34:514" }, "only loopback and private"},
		{"no port", func(c *SyslogConfig) { c.Address = "127.0.0.1" }, "host:port"},
		{"port 0", func(c *SyslogConfig) { c.Address = "127.0.0.1:0" }, "bad port"},
		{"transport", func(c *SyslogConfig) { c.Transport, c.Address = "sctp", "127.0.0.1:514" }, "want tls, tcp or udp"},
		{"facility", func(c *SyslogConfig) { c.InsecurePlain, c.Address, c.Facility = true, "127.0.0.1:514", 24 }, "facility"},
		{"small limit", func(c *SyslogConfig) { c.InsecurePlain, c.Address, c.MaxMessageBytes = true, "127.0.0.1:514", 100 }, "max_message_bytes"},
		{"sd id", func(c *SyslogConfig) { c.InsecurePlain, c.Address, c.SDID = true, "127.0.0.1:514", "x=y@1" }, "sd_id"},
		{"bad CA", func(c *SyslogConfig) { c.Transport, c.Address, c.CA = "tls", "h:6514", []byte("junk") }, "PEM"},
	} {
		cfg := SyslogConfig{Name: "s", Transport: "tcp"}
		c.mod(&cfg)
		if _, err := newSyslogSink(cfg, now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// A name that resolves to a public address is refused at dial time:
	// "localhost" is private, a literal public address was refused above; here a
	// private literal is accepted.
	if _, err := newSyslogSink(SyslogConfig{Name: "s", Transport: "tcp", InsecurePlain: true, Address: "10.1.2.3:514"}, now); err != nil {
		t.Fatal(err)
	}
}

func TestSyslogNotReadable(t *testing.T) {
	s, _ := newSyslogSink(SyslogConfig{Name: "s", Transport: "udp", InsecurePlain: true, Address: "127.0.0.1:9"}, time.Now)
	if _, err := s.Get(context.Background(), "k"); !errors.Is(err, ErrWriteOnlyStream) {
		t.Fatal(err)
	}
	if _, err := s.List(context.Background(), "k"); !errors.Is(err, ErrWriteOnlyStream) {
		t.Fatal(err)
	}
	if s.Locked() || s.WriteOnce() {
		t.Fatal("a stream claims retention")
	}
	if err := s.Put(context.Background(), "logs/k", []byte("%PDF"), PutOptions{ContentType: "application/pdf"}); err == nil {
		t.Fatal("a PDF was sent as log lines")
	}
	if err := s.Put(context.Background(), "../k", []byte("{}"), PutOptions{ContentType: "application/json"}); err == nil {
		t.Fatal("invalid key accepted")
	}
}
