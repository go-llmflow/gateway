package gateway

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/internal/testpki"
)

// fakeSMTP is a relay for the tests: it records what a client said, offers
// STARTTLS and AUTH as asked, and can speak implicit TLS.
type fakeSMTP struct {
	ln       net.Listener
	starttls bool
	implicit bool
	auth     bool
	silent   bool // accepts and never answers
	cert     tls.Certificate

	mu       sync.Mutex
	commands []string
	data     string
}

func newFakeSMTP(t *testing.T, starttls, implicit, auth bool, silent ...bool) (*fakeSMTP, *x509.CertPool) {
	t.Helper()
	ca := testpki.NewRoot("smtp test ca")
	certPEM, keyPEM := ca.ValidLeaf("localhost", "ecdsa")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, starttls: starttls, implicit: implicit, auth: auth, cert: cert, silent: len(silent) > 0 && silent[0]}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f, pool
}

func (f *fakeSMTP) addr() string {
	return strings.Replace(f.ln.Addr().String(), "127.0.0.1", "localhost", 1)
}

func (f *fakeSMTP) saw(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(strings.ToUpper(c), prefix) {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.session(c)
	}
}

func (f *fakeSMTP) session(c net.Conn) {
	defer c.Close()
	if f.silent {
		time.Sleep(5 * time.Second)
		return
	}
	if f.implicit {
		c = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
	}
	r := bufio.NewReader(c)
	w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	inData := false
	var body strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.data = body.String()
				f.mu.Unlock()
				w("250 queued")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}
		f.mu.Lock()
		f.commands = append(f.commands, line)
		f.mu.Unlock()
		switch cmd := strings.ToUpper(line); {
		case strings.HasPrefix(cmd, "EHLO"):
			c.Write([]byte("250-fake\r\n"))
			if f.starttls {
				c.Write([]byte("250-STARTTLS\r\n"))
			}
			if f.auth {
				c.Write([]byte("250-AUTH PLAIN\r\n"))
			}
			w("250 8BITMIME")
		case cmd == "STARTTLS":
			w("220 go ahead")
			c = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}})
			r = bufio.NewReader(c)
			w = func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		case strings.HasPrefix(cmd, "AUTH"):
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			w("250 ok")
		case cmd == "DATA":
			inData = true
			w("354 go")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("500 what")
		}
	}
}

func withTrust(t *testing.T, pool *x509.CertPool) {
	old := smtpTLSConfig
	smtpTLSConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	t.Cleanup(func() { smtpTLSConfig = old })
}

func cfgFor(f *fakeSMTP, mode string) *config.SMTP {
	return &config.SMTP{Server: f.addr(), From: "gw@example.org", To: []string{"a@example.org"}, TLS: mode, Timeout: 2 * time.Second}
}

// A relay that does not offer STARTTLS is refused: nothing is sent, and no
// credentials either.
func TestSMTPRefusesWithoutSTARTTLS(t *testing.T) {
	f, _ := newFakeSMTP(t, false, false, true)
	c := cfgFor(f, config.SMTPStartTLS)
	c.Username = "user"
	err := sendSMTP(c, "secret", c.From, c.To, []byte("Subject: x\r\n\r\nbody\r\n"))
	if err == nil || err != errNoStartTLS {
		t.Fatalf("error %v, want errNoStartTLS", err)
	}
	if f.saw("MAIL") || f.saw("AUTH") || f.saw("DATA") {
		t.Fatalf("a message or a credential went out in clear text: %v", f.commands)
	}
	// The default (an empty mode) is STARTTLS too.
	c.TLS = ""
	if err := sendSMTP(c, "secret", c.From, c.To, []byte("x")); err != errNoStartTLS {
		t.Fatalf("default mode: %v", err)
	}
}

func TestSMTPSTARTTLSDelivers(t *testing.T) {
	f, pool := newFakeSMTP(t, true, false, true)
	withTrust(t, pool)
	c := cfgFor(f, config.SMTPStartTLS)
	c.Username = "user"
	if err := sendSMTP(c, "secret", c.From, c.To, []byte("Subject: hello\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	if !f.saw("STARTTLS") || !f.saw("AUTH") || !strings.Contains(f.data, "Subject: hello") {
		t.Fatalf("commands %v data %q", f.commands, f.data)
	}
}

// A certificate that does not match the host or the roots is refused: the
// session is not protected by a handshake that proves nothing.
func TestSMTPRefusesAnUntrustedCertificate(t *testing.T) {
	f, _ := newFakeSMTP(t, true, false, false) // the default roots do not know the test CA
	c := cfgFor(f, config.SMTPStartTLS)
	if err := sendSMTP(c, "", c.From, c.To, []byte("x")); err == nil || f.saw("MAIL") {
		t.Fatalf("untrusted certificate accepted: %v %v", err, f.commands)
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	f, pool := newFakeSMTP(t, false, true, false)
	withTrust(t, pool)
	c := cfgFor(f, config.SMTPImplicit)
	if err := sendSMTP(c, "", c.From, c.To, []byte("Subject: implicit\r\n\r\nx\r\n")); err != nil || !strings.Contains(f.data, "implicit") {
		t.Fatalf("%v %q", err, f.data)
	}
}

// tls = "none" keeps the old behaviour for a relay that offers nothing.
func TestSMTPNoneSendsInClear(t *testing.T) {
	f, _ := newFakeSMTP(t, false, false, false)
	c := cfgFor(f, config.SMTPNone)
	if err := sendSMTP(c, "", c.From, c.To, []byte("Subject: clear\r\n\r\nx\r\n")); err != nil || !strings.Contains(f.data, "clear") {
		t.Fatalf("%v %q", err, f.data)
	}
}

// A relay that accepts and never answers ends at the timeout.
func TestSMTPTimesOut(t *testing.T) {
	f, _ := newFakeSMTP(t, true, false, false, true)
	c := cfgFor(f, config.SMTPStartTLS)
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	if err := sendSMTP(c, "", c.From, c.To, []byte("x")); err == nil {
		t.Fatal("a silent relay was waited for")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}
