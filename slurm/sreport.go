// Package slurm reads the output of
//
//	sreport cluster AccountUtilizationByUser -T gres/gpu [-t Hours] [-P|-p]
//
// (https://slurm.schedmd.com/sreport.html) to reconcile batch GPU-hours
// with inference GPU-hours. The input is untrusted: it is bounded in size,
// lines and fields, and every refusal is an error, never a guess.
//
// Both layouts sreport prints are read:
//
//   - delimited (-p, -P): '|' between fields (a trailing '|' with -p); a
//     comma-separated file with the same header is read too;
//   - the default fixed-width table, whose columns are cut at the dashes of
//     the rule line under the header.
//
// The header must name an Account and a Used column; Cluster, Login and
// TRES Name are used when present. The unit comes from the "Usage reported
// in … Minutes|Hours|Seconds" line (sreport's default is minutes); a
// report in percent is refused, since it says nothing about hours.
// Rows with a TRES Name other than gres/gpu are skipped.
package slurm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds on an input.
const (
	MaxBytes  = 1 << 20
	MaxLines  = 50_000
	MaxFields = 32
	maxLine   = 4096
)

// Row is one line of the report: an account total (Login empty) or one
// user's usage in an account.
type Row struct {
	Cluster, Account, Login string
	GPUHours                float64
}

// Report is a parsed sreport output.
type Report struct {
	// Period is the period line sreport printed ("2026-10-01T00:00:00 -
	// 2026-10-31T23:59:59"), empty when the header was suppressed.
	Period string
	// Unit is what Used was in: "minutes", "hours" or "seconds".
	Unit string
	Rows []Row
}

var (
	periodRE = regexp.MustCompile(`^\s*Cluster/Account/User Utilization (\S+ - \S+)`)
	unitRE   = regexp.MustCompile(`(?i)^\s*Usage reported in .*?\b(percent|minutes|hours|seconds)`)
	ruleRE   = regexp.MustCompile(`^[- ]+$`)
)

// ErrTooLarge is wrapped by every refusal about size.
var ErrTooLarge = errors.New("slurm: input too large")

// Parse reads an AccountUtilizationByUser report.
func Parse(r io.Reader) (*Report, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("slurm: the input is not UTF-8 text")
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, maxLine), maxLine)
	var lines []string
	for sc.Scan() {
		if len(lines) == MaxLines {
			return nil, fmt.Errorf("%w: more than %d lines", ErrTooLarge, MaxLines)
		}
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: a line longer than %d bytes", ErrTooLarge, maxLine)
	}
	rep := &Report{Unit: "minutes"}
	head := -1
	for i, l := range lines {
		if m := periodRE.FindStringSubmatch(l); m != nil {
			rep.Period = m[1]
			continue
		}
		if m := unitRE.FindStringSubmatch(l); m != nil {
			u := strings.ToLower(m[1])
			if strings.HasPrefix(u, "percent") {
				return nil, errors.New("slurm: a report in percent gives no hours: run sreport with -t Hours or -t Minutes")
			}
			rep.Unit = u
			continue
		}
		if isHeader(l) {
			head = i
			break
		}
	}
	if head < 0 {
		return nil, errors.New("slurm: no header with Account and Used columns (sreport cluster AccountUtilizationByUser, without -n)")
	}
	var split func(string) []string
	first := head + 1 // the first data line
	switch hl := lines[head]; {
	case strings.Contains(hl, "|"):
		split = func(s string) []string { return strings.Split(strings.TrimSuffix(s, "|"), "|") }
	case strings.Contains(hl, ","):
		split = func(s string) []string { return strings.Split(s, ",") }
	default:
		if head+1 >= len(lines) || !ruleRE.MatchString(lines[head+1]) || !strings.Contains(lines[head+1], "-") {
			return nil, errors.New("slurm: a fixed-width report needs its rule line under the header")
		}
		cols := columns(lines[head+1])
		split = func(s string) []string { return cut(s, cols) }
		first++ // the data starts after the rule
	}
	names := split(lines[head])
	if len(names) > MaxFields {
		return nil, fmt.Errorf("%w: more than %d columns", ErrTooLarge, MaxFields)
	}
	idx := map[string]int{}
	for i, n := range names {
		idx[normal(n)] = i
	}
	acc, ok1 := idx["account"]
	used, ok2 := idx["used"]
	if !ok1 || !ok2 {
		return nil, errors.New("slurm: the header has no Account or no Used column")
	}
	cluster, hasCluster := idx["cluster"]
	login, hasLogin := idx["login"]
	tres, hasTRES := idx["tresname"]
	div := map[string]float64{"minutes": 60, "hours": 1, "seconds": 3600}[rep.Unit]
	for i, l := range lines[first:] {
		n := first + i + 1 // the line number, from 1
		if strings.TrimSpace(l) == "" || ruleRE.MatchString(l) {
			continue
		}
		f := split(l)
		if len(f) != len(names) {
			return nil, fmt.Errorf("slurm: line %d has %d fields, the header %d", n, len(f), len(names))
		}
		if hasTRES && !strings.EqualFold(strings.TrimSpace(f[tres]), "gres/gpu") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(f[used]), 64)
		if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) || v > 1e15 {
			return nil, fmt.Errorf("slurm: line %d: Used %q is not a usage", n, strings.TrimSpace(f[used]))
		}
		row := Row{Account: strings.TrimSpace(f[acc]), GPUHours: v / div}
		if hasCluster {
			row.Cluster = strings.TrimSpace(f[cluster])
		}
		if hasLogin {
			row.Login = strings.TrimSpace(f[login])
		}
		if row.Account == "" {
			return nil, fmt.Errorf("slurm: line %d has no account", n)
		}
		rep.Rows = append(rep.Rows, row)
	}
	return rep, nil
}

// normal lowercases a column name and drops what is not a letter, so
// "TRES Name", "TRESName" and "tres_name" are one name.
func normal(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isHeader(l string) bool {
	has := map[string]bool{}
	for _, f := range strings.FieldsFunc(l, func(r rune) bool { return r == '|' || r == ',' || r == ' ' || r == '\t' }) {
		has[normal(f)] = true
	}
	return has["account"] && has["used"]
}

// columns returns the [start, end) of each run of dashes.
func columns(rule string) [][2]int {
	var cols [][2]int
	start := -1
	for i := 0; i <= len(rule); i++ {
		dash := i < len(rule) && rule[i] == '-'
		if dash && start < 0 {
			start = i
		}
		if !dash && start >= 0 {
			cols = append(cols, [2]int{start, i})
			start = -1
		}
		if len(cols) > MaxFields {
			break
		}
	}
	return cols
}

// cut slices a fixed-width line at the rule's columns (byte offsets; the
// last column takes the rest of the line).
func cut(l string, cols [][2]int) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		s, e := c[0], c[1]
		if i == len(cols)-1 {
			e = len(l)
		}
		if s >= len(l) {
			continue
		}
		if e > len(l) {
			e = len(l)
		}
		// A cut inside a multi-byte character drops its pieces.
		out[i] = strings.ToValidUTF8(l[s:e], "")
	}
	return out
}

// ByAccount sums the account totals (rows without a login) per account,
// over clusters. An account listed twice on one cluster (the tree option
// repeats nothing, but a pasted file can) counts once.
func (r *Report) ByAccount() map[string]float64 {
	seen := map[[2]string]bool{}
	out := map[string]float64{}
	for _, x := range r.Rows {
		if x.Login != "" {
			continue
		}
		k := [2]string{x.Cluster, x.Account}
		if seen[k] {
			continue
		}
		seen[k] = true
		out[x.Account] += x.GPUHours
	}
	return out
}
