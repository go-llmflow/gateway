package config

import (
	"strings"
	"testing"
	"time"
)

func smtpCfg(attrs string) string {
	return valid + "\nalerts {\n smtp {\n  server = \"" + "mail.example.org:25" + "\"\n  from = \"a@b.c\"\n  to = [\"d@e.f\"]\n" + attrs + "\n }\n}\n"
}

func TestSMTPTLSModes(t *testing.T) {
	c, err := parse(smtpCfg(""))
	if err != nil || c.Alerts.SMTP.TLS != SMTPStartTLS || c.Alerts.SMTP.Timeout != DefaultSMTPTimeout {
		t.Fatalf("default must require STARTTLS: %+v %v", c.Alerts.SMTP, err)
	}
	if c, err = parse(smtpCfg("  tls = \"implicit\"\n  timeout = \"5s\"")); err != nil || c.Alerts.SMTP.TLS != SMTPImplicit || c.Alerts.SMTP.Timeout != 5*time.Second {
		t.Fatalf("implicit: %+v %v", c.Alerts.SMTP, err)
	}
	// Clear text: this host only, or explicitly accepted.
	if _, err = parse(smtpCfg("  tls = \"none\"")); err == nil || !strings.Contains(err.Error(), "Clear-text SMTP") {
		t.Fatalf("none to a remote relay: %v", err)
	}
	if c, err = parse(smtpCfg("  tls = \"none\"\n  allow_cleartext = true")); err != nil || c.Alerts.SMTP.TLS != SMTPNone {
		t.Fatalf("none with the flag: %v", err)
	}
	local := strings.Replace(smtpCfg("  tls = \"none\""), "mail.example.org:25", "127.0.0.1:25", 1)
	if c, err = parse(local); err != nil || c.Alerts.SMTP.TLS != SMTPNone {
		t.Fatalf("none to localhost: %v", err)
	}
	for _, bad := range []string{"  tls = \"ssl\"", "  timeout = \"0s\"", "  timeout = \"1h\""} {
		if _, err := parse(smtpCfg(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecisionReportMailTo(t *testing.T) {
	exports := "\nexport \"reports\" {\n kind = \"file\"\n directory = \"/tmp/x\"\n}\n"
	// mail_to needs the relay.
	if _, err := parse(valid + exports + "decision_report {\n destination = \"reports\"\n mail_to = [\"f@g.h\"]\n}\n"); err == nil || !strings.Contains(err.Error(), "No mail relay") {
		t.Fatalf("without smtp: %v", err)
	}
	src := smtpCfg("") + exports + "decision_report {\n destination = \"reports\"\n mail_to = [\"f@g.h\"]\n}\n"
	c, err := parse(src)
	if err != nil || len(c.DecisionReport.MailTo) != 1 {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := parse(smtpCfg("") + exports + "decision_report {\n destination = \"reports\"\n mail_to = [\"not an address\"]\n}\n"); err == nil {
		t.Fatal("a bad address accepted")
	}
}
