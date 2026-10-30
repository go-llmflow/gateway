package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Bounds: a status line is run constantly, and what it reads is untrusted.
const (
	maxStdin    = 1 << 20
	maxResponse = 64 << 10
	maxCache    = 64 << 10
)

// usage is the part of /v1/me/usage this program reads.
type usage struct {
	WarnAt  []int   `json:"warn_at"`
	Session *window `json:"session"`
	Week    *window `json:"week"`
}

type window struct {
	UsedPct  float64   `json:"used_pct"`
	ResetsAt time.Time `json:"resets_at"`
}

// cached is the cache file: the answer and when it was fetched. Never the
// credential, nor anything derived from it but the file name.
type cached struct {
	FetchedAt time.Time `json:"fetched_at"`
	Usage     usage     `json:"usage"`
}

type options struct {
	url, tokenFile, cacheDir, lang string
	ttl, timeout                   time.Duration
	noColor, verbose, allowHTTP    bool
}

// env is what the program reads from its surroundings (tests replace it).
type env struct {
	getenv func(string) string
	now    func() time.Time
	loc    *time.Location
	cache  func() (string, error)
}

func realEnv() env {
	return env{getenv: os.Getenv, now: time.Now, loc: time.Local, cache: os.UserCacheDir}
}

// RootCmd returns the command.
func RootCmd() *cobra.Command { return rootCmd(realEnv()) }

func rootCmd(e env) *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "llmflow-status",
		Short: "One line of soft-quota usage for Claude Code's statusLine",
		Long: `Reads Claude Code's status line JSON on stdin, calls the gateway's
GET /v1/me/usage with the credential from ANTHROPIC_AUTH_TOKEN (else
ANTHROPIC_API_KEY) at ANTHROPIC_BASE_URL, and prints the session and week.
Prints nothing, and exits 0, on any error.`,
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			// The status line's JSON is read (and bounded) so Claude Code's
			// pipe is drained; this program needs none of it.
			_, _ = io.Copy(io.Discard, io.LimitReader(cmd.InOrStdin(), maxStdin))
			line, err := run(cmd.Context(), e, o)
			if err != nil {
				if o.verbose {
					fmt.Fprintln(cmd.ErrOrStderr(), "llmflow-status:", err)
				}
				return nil
			}
			if line != "" {
				fmt.Fprintln(out, line)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.url, "url", "", "gateway base URL (default: ANTHROPIC_BASE_URL)")
	f.StringVar(&o.tokenFile, "token-file", "", "read the credential from this file instead of the environment")
	f.DurationVar(&o.ttl, "ttl", 30*time.Second, "how long an answer is reused")
	f.DurationVar(&o.timeout, "timeout", 2*time.Second, "give up (and print nothing) after this long")
	f.StringVar(&o.lang, "lang", "", "fr or en (default: from LC_ALL, LC_MESSAGES, LANG)")
	f.StringVar(&o.cacheDir, "cache-dir", "", "cache directory (default: the user cache directory)")
	f.BoolVar(&o.noColor, "no-color", false, "no colours (also: NO_COLOR)")
	f.BoolVar(&o.verbose, "verbose", false, "say on stderr why nothing was printed")
	f.BoolVar(&o.allowHTTP, "allow-http", false, "allow a plain http gateway that is not on this machine (the credential then travels in clear)")
	return cmd
}

// credential: the same one Claude Code sends, in the same order.
func credential(e env, o options) (string, error) {
	if o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return "", errors.New("credential file unreadable")
		}
		if c := strings.TrimSpace(string(b)); c != "" {
			return c, nil
		}
		return "", errors.New("credential file empty")
	}
	for _, n := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if v := strings.TrimSpace(e.getenv(n)); v != "" {
			return v, nil
		}
	}
	return "", errors.New("no credential (ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY)")
}

// baseURL validates the gateway's address: https, or http to this machine
// (or with --allow-http), no user info (it would be a second place for a
// secret), no query.
func baseURL(e env, o options) (*url.URL, error) {
	raw := o.url
	if raw == "" {
		raw = e.getenv("ANTHROPIC_BASE_URL")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	switch {
	case raw == "":
		return nil, errors.New("no gateway URL (ANTHROPIC_BASE_URL, --url)")
	case err != nil || u.Host == "":
		return nil, errors.New("gateway URL unreadable")
	case u.User != nil:
		return nil, errors.New("gateway URL carries user info")
	case u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("gateway URL has a query or fragment")
	case u.Scheme == "https":
	case u.Scheme == "http" && (o.allowHTTP || loopback(u.Hostname())):
	default:
		return nil, errors.New("gateway URL: want https (http only to this machine, or with --allow-http)")
	}
	return u, nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// cachePath is the cache file for one gateway and one credential. The name
// is a hash of both, so that two gateways or two people on one machine do
// not read each other's answer; a hash of a high-entropy key reveals
// nothing of it.
func cachePath(e env, o options, base *url.URL, cred string) (string, error) {
	dir := o.cacheDir
	if dir == "" {
		d, err := e.cache()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(d, "llmflow-status")
	}
	sum := sha256.Sum256([]byte("llmflow-status\x00" + base.String() + "\x00" + cred))
	return filepath.Join(dir, "usage-"+hex.EncodeToString(sum[:8])+".json"), nil
}

func readCache(path string, now time.Time, ttl time.Duration) (usage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return usage{}, false
	}
	defer f.Close()
	var c cached
	if json.NewDecoder(io.LimitReader(f, maxCache)).Decode(&c) != nil {
		return usage{}, false
	}
	// A clock that went back, or an entry from the future, is not fresh.
	if age := now.Sub(c.FetchedAt); age < 0 || age >= ttl {
		return usage{}, false
	}
	return c.Usage, true
}

// writeCache stores an answer: the directory 0700, the file 0600, written
// aside and renamed so that a reader never sees half of one.
func writeCache(path string, now time.Time, u usage) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cached{FetchedAt: now.UTC(), Usage: u})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "usage-*.tmp") // 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fetch asks the gateway. Redirects are refused: a redirect would carry the
// credential to another host.
func fetch(ctx context.Context, base *url.URL, cred string, timeout time.Duration) (usage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := *base
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/me/usage"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return usage{}, errors.New("request")
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error of net/http names the URL, never a header: but say less.
		return usage{}, errors.New("gateway unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return usage{}, fmt.Errorf("gateway answered %d", resp.StatusCode)
	}
	var out usage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return usage{}, errors.New("answer unreadable")
	}
	return out, nil
}

// run is the program: the line to print ("" for nothing) or an error (which
// the caller turns into nothing).
func run(ctx context.Context, e env, o options) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cred, err := credential(e, o)
	if err != nil {
		return "", err
	}
	base, err := baseURL(e, o)
	if err != nil {
		return "", err
	}
	now := e.now()
	path, perr := cachePath(e, o, base, cred)
	var u usage
	hit := false
	if perr == nil && o.ttl > 0 {
		u, hit = readCache(path, now, o.ttl)
	}
	if !hit {
		if u, err = fetch(ctx, base, cred, o.timeout); err != nil {
			return "", err
		}
		if perr == nil {
			_ = writeCache(path, now, u) // a cache that cannot be written costs a request, not the line
		}
	}
	return render(u, o, e), nil
}

// ---- rendering ----

var (
	weekdaysEN = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	weekdaysFR = [...]string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}
)

func langOf(e env, o options) string {
	if o.lang == "fr" || o.lang == "en" {
		return o.lang
	}
	for _, n := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(e.getenv(n)); v != "" {
			if strings.HasPrefix(v, "fr") {
				return "fr"
			}
			return "en"
		}
	}
	return "en"
}

// level is how many thresholds pct has reached.
func level(warn []int, pct float64) int {
	n := 0
	for _, w := range warn {
		if pct >= float64(w) {
			n++
		}
	}
	return n
}

const (
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	reset  = "\x1b[0m"
)

// colour: green below the first threshold, yellow past some, red past the
// last (past the only one, with a single threshold). No thresholds (warnings
// off), no colour.
func colour(warn []int, pct float64) string {
	switch c := level(warn, pct); {
	case len(warn) == 0:
		return ""
	case c == 0:
		return green
	case c >= len(warn):
		return red
	default:
		return yellow
	}
}

// render writes the line, or "" when there is nothing to show (a gateway
// without profiles answers no windows).
func render(u usage, o options, e env) string {
	lang := langOf(e, o)
	loc := e.loc
	if loc == nil {
		loc = time.UTC
	}
	colours := !o.noColor && e.getenv("NO_COLOR") == ""
	part := func(w *window, label string, day bool) string {
		if w == nil {
			return ""
		}
		pct := int(w.UsedPct)
		t := w.ResetsAt.In(loc)
		when := t.Format("15:04")
		if day {
			names := weekdaysEN
			if lang == "fr" {
				names = weekdaysFR
			}
			when = names[t.Weekday()] + " " + when
		}
		s := fmt.Sprintf("%s %d%% · %s", label, pct, when)
		if c := colour(u.WarnAt, w.UsedPct); colours && c != "" {
			s = c + s + reset
		}
		return s
	}
	sess, week := "session", "week"
	if lang == "fr" {
		week = "semaine"
	}
	var parts []string
	for _, p := range []string{part(u.Session, sess, false), part(u.Week, week, true)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " │ ")
}
