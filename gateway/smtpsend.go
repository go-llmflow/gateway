package gateway

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// Mail goes out over a protected session or not at all (#109). net/smtp's
// SendMail upgrades to TLS "when the server offers it": a relay that does not
// advertise STARTTLS (a misconfiguration, or someone in the path stripping the
// extension) made the message, and an unauthenticated session has no password
// to refuse, go in clear text; it also has no timeout and cannot speak
// implicit TLS. sendSMTP:
//
//   - tls = "starttls" (the default): the server must offer STARTTLS and the
//     handshake must succeed (certificate verified against the host name,
//     TLS 1.2 or later), else the session is refused and nothing is sent;
//   - tls = "implicit": TLS from the first byte (port 465);
//   - tls = "none": clear text, for a relay on this host or when
//     allow_cleartext is set (config refuses it otherwise);
//   - the dial and the whole session end at the configured timeout.
//
// Credentials follow the session: PlainAuth is only attempted on a protected
// session, or to this host in clear text.

// errNoStartTLS is why a session was refused.
var errNoStartTLS = errors.New("smtp: the server does not offer STARTTLS; refusing to send in clear text (set tls = \"none\" only for a relay on this host)")

// smtpDial opens the connection; tests replace it to reach a fake server.
var smtpDial = func(network, addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}

// smtpTLSConfig is the client's TLS configuration; tests add their CA.
var smtpTLSConfig = func(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// sendSMTP sends one message through the relay s describes.
func sendSMTP(s *config.SMTP, password, from string, to []string, msg []byte) error {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = config.DefaultSMTPTimeout
	}
	host, _, err := net.SplitHostPort(s.Server)
	if err != nil {
		return err
	}
	mode := s.TLS
	if mode == "" {
		mode = config.SMTPStartTLS
	}
	conn, err := smtpDial("tcp", s.Server, timeout)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout)) // the dial and the session share the bound
	protected := false
	if mode == config.SMTPImplicit {
		tc := tls.Client(conn, smtpTLSConfig(host))
		if err := tc.Handshake(); err != nil {
			return fmt.Errorf("smtp: TLS: %w", err)
		}
		conn, protected = tc, true
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()
	if mode == config.SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errNoStartTLS
		}
		if err := c.StartTLS(smtpTLSConfig(host)); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
		protected = true
	}
	if s.Username != "" {
		if !protected && !loopbackAddr(host) {
			return errors.New("smtp: refusing to send a password in clear text")
		}
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: the server does not offer AUTH")
		}
		if err := c.Auth(plainAuthAnyTLS(s.Username, password, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func loopbackAddr(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// plainAuthAnyTLS is PlainAuth without its own transport check: sendSMTP has
// already decided the session may carry a password (it is protected, or it is
// this host). net/smtp's PlainAuth refuses a clear-text session to anything but
// localhost, and a test relay on 127.0.0.1 over TLS has its own certificate.
func plainAuthAnyTLS(user, pass, host string) smtp.Auth { return anyTLSAuth{user, pass, host} }

type anyTLSAuth struct{ user, pass, host string }

func (a anyTLSAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if server.Name != a.host {
		return "", nil, errors.New("smtp: wrong host name")
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a anyTLSAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("smtp: unexpected server challenge")
	}
	return nil, nil
}

// mailAttachment is a file of a report mail.
type mailAttachment struct {
	Name, Type string
	Data       []byte
}

// buildReportMail writes a multipart/mixed message: a plain-text part and the
// attachments in base64 (76-column lines). Every header value comes from the
// configuration or from the gateway, and passes headerSafe.
func buildReportMail(from string, to []string, subject, text string, atts []mailAttachment) []byte {
	var b bytes.Buffer
	boundary := "llmflow-" + hex.EncodeToString(randomBytes(12))
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n", headerSafe(from), headerSafe(strings.Join(to, ", ")), headerSafe(subject))
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", boundary, text)
	for _, a := range atts {
		name := headerSafe(a.Name)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; name=%q\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=%q\r\n\r\n", boundary, headerSafe(a.Type), name, name)
		enc := base64.StdEncoding.EncodeToString(a.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

func randomBytes(n int) []byte {
	out := make([]byte, n)
	_, _ = rand.Read(out)
	return out
}
