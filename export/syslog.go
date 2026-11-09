package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// A syslog destination: RFC 5424 messages (https://www.rfc-editor.org/rfc/rfc5424)
// over TLS (RFC 5425, TLS 1.2 or later, certificate-validated, port 6514 by
// convention) or over plain TCP or UDP, which are refused unless the
// configuration asks for them and the peer is a loopback or private
// address. TCP and TLS frames are octet-counted (RFC 6587 section 3.4.1:
// MSG-LEN SP SYSLOG-MSG), so no byte of a message can end a frame.
//
// The sink carries log lines only: a Put of an NDJSON batch (gzip or not)
// sends one message per line. Whatever a line holds is what the logger put
// there: a request log has no prompt or completion unless the team's
// logging block says content = true. Nothing of a message is ever written
// to the gateway's own log. A line larger than the message limit is not
// truncated into invalid JSON: a marker message with truncated="true" and
// the original size is sent in its place, and counted.
//
// Syslog has no acknowledgement (RFC 6587 section 3: receipt, reliability
// and flow control are TCP's), no read-back and no overwrite protection: it
// is not write-once and cannot hold audit anchors.

// ErrWriteOnlyStream is what Get and List answer on a syslog destination.
var ErrWriteOnlyStream = errors.New("export: a syslog destination cannot be read back")

// maxSyslogBatch bounds the decompressed size of one batch.
const maxSyslogBatch = 64 << 20

// SyslogConfig describes a syslog destination.
type SyslogConfig struct {
	Name string
	// Transport is "tls", "tcp" or "udp"; Address is host:port.
	Transport, Address string
	// InsecurePlain allows tcp and udp, to loopback and private addresses only.
	InsecurePlain bool
	// TLS (transport tls): CA bundle (nil: system roots), client certificate and key
	// (PEM, optional), expected server name (default: the address's host).
	CA, Cert, Key []byte
	ServerName    string
	// Header fields; empty values get defaults.
	Hostname, AppName, SDID string
	Facility                int // 0..23
	MaxMessageBytes         int
}

type syslogSink struct {
	cfg    SyslogConfig
	tls    *tls.Config
	pid    string
	now    func() time.Time
	dialer net.Dialer

	mu   sync.Mutex
	conn net.Conn

	Truncated atomic.Int64
	Sent      atomic.Int64
}

// DefaultSyslogMax returns the default message limit of a transport: 8192
// octets for the stream transports (RFC 5425 section 4.3.1: receivers
// SHOULD process up to 8192) and 2048 for UDP.
func DefaultSyslogMax(transport string) int {
	if transport == "udp" {
		return 2048
	}
	return 8192
}

// ValidSDName reports whether s is a valid SD-ID for a private element:
// name@enterprise-number, the name 1 to 31 printable ASCII characters
// without '=', ' ', ']', '"' or '@', the number digits (RFC 5424 section 6.3.2).
func ValidSDName(s string) bool {
	name, num, ok := strings.Cut(s, "@")
	if !ok || name == "" || num == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 33 || c > 126 || c == '=' || c == ']' || c == '"' || c == '@' {
			return false
		}
	}
	for i := 0; i < len(num); i++ {
		if num[i] != '.' && (num[i] < '0' || num[i] > '9') {
			return false
		}
	}
	return true
}

// PrivateOrLoopback reports whether ip may be reached over plain syslog.
func PrivateOrLoopback(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate()
}

// newSyslogSink validates cfg and prepares the sink; it does not connect.
func newSyslogSink(cfg SyslogConfig, now func() time.Time) (*syslogSink, error) {
	if cfg.Transport == "" {
		cfg.Transport = "tls"
	}
	host, port, err := net.SplitHostPort(cfg.Address)
	if err != nil || host == "" {
		return nil, fmt.Errorf("export %s: address %q: want host:port", cfg.Name, cfg.Address)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("export %s: address %q: bad port", cfg.Name, cfg.Address)
	}
	s := &syslogSink{cfg: cfg, now: now, pid: strconv.Itoa(os.Getpid())}
	switch cfg.Transport {
	case "tls":
		s.tls = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if cfg.ServerName != "" {
			s.tls.ServerName = cfg.ServerName
		}
		if len(cfg.CA) > 0 {
			pool, err := certPool(cfg.CA)
			if err != nil {
				return nil, fmt.Errorf("export %s: %w", cfg.Name, err)
			}
			s.tls.RootCAs = pool
		}
		if len(cfg.Cert) > 0 || len(cfg.Key) > 0 {
			c, err := tls.X509KeyPair(cfg.Cert, cfg.Key)
			if err != nil {
				return nil, fmt.Errorf("export %s: client certificate: %w", cfg.Name, err)
			}
			s.tls.Certificates = []tls.Certificate{c}
		}
	case "tcp", "udp":
		if !cfg.InsecurePlain {
			return nil, fmt.Errorf("export %s: %s syslog sends log data in clear text: set insecure_plain = true (loopback and private addresses only)", cfg.Name, cfg.Transport)
		}
		if ip := net.ParseIP(host); ip != nil && !PrivateOrLoopback(ip) {
			return nil, fmt.Errorf("export %s: plain syslog to %s: only loopback and private addresses", cfg.Name, host)
		}
		// A name is resolved at dial time, and the address actually
		// dialled is checked then (a name that moves to a public address is
		// refused).
		s.dialer.Control = func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(h); ip == nil || !PrivateOrLoopback(ip) {
				return fmt.Errorf("plain syslog: %s is not a loopback or private address", h)
			}
			return nil
		}
	default:
		return nil, fmt.Errorf("export %s: transport %q: want tls, tcp or udp", cfg.Name, cfg.Transport)
	}
	if s.cfg.MaxMessageBytes == 0 {
		s.cfg.MaxMessageBytes = DefaultSyslogMax(cfg.Transport)
	}
	if s.cfg.MaxMessageBytes < 1024 || s.cfg.MaxMessageBytes > 65000 {
		return nil, fmt.Errorf("export %s: max_message_bytes between 1024 and 65000", cfg.Name)
	}
	if len(cfg.Hostname) > 64 || len(cfg.AppName) > 48 {
		return nil, fmt.Errorf("export %s: hostname at most 64 and app_name at most 48 characters", cfg.Name)
	}
	if s.cfg.Facility < 0 || s.cfg.Facility > 23 {
		return nil, fmt.Errorf("export %s: facility between 0 and 23", cfg.Name)
	}
	if s.cfg.SDID == "" {
		s.cfg.SDID = "llmflow@32473" // 32473: the number IANA reserves for documentation (RFC 5424 section 6.3.2)
	}
	if !ValidSDName(s.cfg.SDID) {
		return nil, fmt.Errorf("export %s: sd_id %q: want name@enterprise-number", cfg.Name, s.cfg.SDID)
	}
	if s.cfg.AppName == "" {
		s.cfg.AppName = "llmflow-gateway"
	}
	if s.cfg.Hostname == "" {
		s.cfg.Hostname, _ = os.Hostname()
	}
	s.dialer.Timeout = 10 * time.Second
	return s, nil
}

func (s *syslogSink) Name() string                                { return s.cfg.Name }
func (s *syslogSink) Locked() bool                                { return false }
func (s *syslogSink) WriteOnce() bool                             { return false }
func (s *syslogSink) Get(context.Context, string) ([]byte, error) { return nil, ErrWriteOnlyStream }
func (s *syslogSink) List(context.Context, string) ([]string, error) {
	return nil, ErrWriteOnlyStream
}

// Close closes the connection.
func (s *syslogSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drop()
}

func (s *syslogSink) drop() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

func (s *syslogSink) connect(ctx context.Context) error {
	if s.conn != nil {
		return nil
	}
	network := "tcp"
	if s.cfg.Transport == "udp" {
		network = "udp"
	}
	c, err := s.dialer.DialContext(ctx, network, s.cfg.Address)
	if err != nil {
		return err
	}
	if s.cfg.Transport == "tls" {
		tc := tls.Client(c, s.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return err
		}
		c = tc
	}
	s.conn = c
	return nil
}

func (s *syslogSink) Put(ctx context.Context, key string, body []byte, o PutOptions) error {
	if !ValidKey(key) {
		return fmt.Errorf("export %s: invalid key %q", s.cfg.Name, key)
	}
	if o.ContentType != "application/x-ndjson" && o.ContentType != "application/json" {
		return fmt.Errorf("export %s: a syslog destination carries log lines (NDJSON), not %q", s.cfg.Name, o.ContentType)
	}
	lines, err := splitLines(body, o.ContentEncoding)
	if err != nil {
		return fmt.Errorf("export %s: %s: %w", s.cfg.Name, key, err)
	}
	msgID := key
	if i := strings.IndexByte(key, '/'); i > 0 {
		msgID = key[:i]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connect(ctx); err != nil {
		return fmt.Errorf("export %s: %w", s.cfg.Name, err)
	}
	deadline := time.Now().Add(time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	s.conn.SetWriteDeadline(deadline)
	var out bytes.Buffer
	flush := func() error {
		if out.Len() == 0 {
			return nil
		}
		_, err := s.conn.Write(out.Bytes())
		out.Reset()
		return err
	}
	for i, line := range lines {
		msg, trunc := FormatSyslog(s.cfg, s.pid, s.now(), msgID, key, i+1, len(lines), line)
		if trunc {
			s.Truncated.Add(1)
		}
		if s.cfg.Transport == "udp" {
			// A datagram per message.
			if _, err := s.conn.Write(msg); err != nil {
				s.drop()
				return fmt.Errorf("export %s: %w", s.cfg.Name, err)
			}
			s.Sent.Add(1)
			continue
		}
		out.WriteString(strconv.Itoa(len(msg)))
		out.WriteByte(' ')
		out.Write(msg)
		if out.Len() >= 64<<10 {
			if err := flush(); err != nil {
				s.drop()
				return fmt.Errorf("export %s: %w", s.cfg.Name, err)
			}
		}
		s.Sent.Add(1)
	}
	if err := flush(); err != nil {
		s.drop()
		return fmt.Errorf("export %s: %w", s.cfg.Name, err)
	}
	return nil
}

// splitLines returns the non-empty lines of an NDJSON body, gunzipped when
// encoding says so, bounded by maxSyslogBatch.
func splitLines(body []byte, encoding string) ([][]byte, error) {
	switch encoding {
	case "":
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(zr, maxSyslogBatch+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxSyslogBatch {
			return nil, fmt.Errorf("batch larger than %d bytes once decompressed", maxSyslogBatch)
		}
		body = b
	default:
		return nil, fmt.Errorf("content encoding %q", encoding)
	}
	var out [][]byte
	for _, l := range bytes.Split(body, []byte{'\n'}) {
		if l = bytes.TrimSuffix(l, []byte{'\r'}); len(l) > 0 {
			out = append(out, l)
		}
	}
	return out, nil
}

// sdParamValue escapes a PARAM-VALUE: '"', '\' and ']' with a backslash
// (RFC 5424 section 6.3.3), control characters as "#" and three digits (the
// substitution the RFC suggests for NUL: "#000"), invalid UTF-8 as U+FFFD.
func sdParamValue(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\' || r == ']':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, "#%03d", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// headerField makes a header field of at most max printable ASCII
// characters (%d33-126); anything else becomes '_', empty becomes "-".
func headerField(s string, max int) string {
	var b []byte
	for i := 0; i < len(s) && len(b) < max; i++ {
		c := s[i]
		if c < 33 || c > 126 {
			c = '_'
		}
		b = append(b, c)
	}
	if len(b) == 0 {
		return "-"
	}
	return string(b)
}

// FormatSyslog builds one RFC 5424 message: priority (facility, severity 6
// "informational"), version 1, a UTC timestamp with microseconds, header
// fields reduced to printable ASCII within their limits, one structured-
// data element [sdID export="key" seq="i" of="n"] and the line as MSG
// (UTF-8, with the byte order mark the RFC requires). A line that does not
// fit MaxMessageBytes is replaced by a marker and trunc is true.
func FormatSyslog(c SyslogConfig, procID string, ts time.Time, msgID, key string, seq, of int, line []byte) (msg []byte, trunc bool) {
	pri := c.Facility*8 + 6
	var b bytes.Buffer
	b.WriteString("<" + strconv.Itoa(pri) + ">1 " + ts.UTC().Format("2006-01-02T15:04:05.000000Z") + " " +
		headerField(c.Hostname, 255) + " " + headerField(c.AppName, 48) + " " + headerField(procID, 128) + " " + headerField(msgID, 32) + " ")
	sd := func(extra string) string {
		return "[" + c.SDID + ` object="` + sdParamValue(key[:min(len(key), 200)]) + `" seq="` + strconv.Itoa(seq) + `" of="` + strconv.Itoa(of) + `"` + extra + "]"
	}
	// MSG: control characters in the line become "#" and three digits.
	body := line
	if needsClean(body) {
		body = []byte(sdMsgClean(string(body)))
	}
	head := b.Len()
	full := sd("") + " \xEF\xBB\xBF"
	if head+len(full)+len(body) <= c.MaxMessageBytes {
		b.WriteString(full)
		b.Write(body)
		return b.Bytes(), false
	}
	b.WriteString(sd(` truncated="true" original_bytes="`+strconv.Itoa(len(line))+`"`) + " \xEF\xBB\xBF")
	b.WriteString(`{"truncated":true,"original_bytes":` + strconv.Itoa(len(line)) + `}`)
	return b.Bytes(), true
}

func needsClean(b []byte) bool {
	if !utf8.Valid(b) {
		return true
	}
	for _, c := range b {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

func certPool(pem []byte) (*x509.CertPool, error) {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		return nil, errors.New("tls_ca_file holds no PEM certificate")
	}
	return p, nil
}

// sdMsgClean replaces control characters and invalid UTF-8 of a MSG.
func sdMsgClean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			fmt.Fprintf(&b, "#%03d", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
