// Package energy measures and attributes the electricity a GPU platform
// uses (#97): it reads NVIDIA DCGM exporter counters, turns them into
// per-interval deltas that survive a driver reload and a missed scrape,
// splits the energy of an interval between the requests that ran in it, and
// prices it. Energy is counted in integer millijoules end to end, so what is
// attributed sums exactly to what was measured.
//
// Sources opened (2026-10-29):
//
//   - NVIDIA dcgm-exporter etc/default-counters.csv: DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION
//     is a "counter, Total energy consumption since the driver was last
//     reloaded (in mJ)", DCGM_FI_DEV_POWER_USAGE a "gauge, Power draw (in W)".
//   - Prometheus text exposition format 0.0.4 (prometheus.io/docs/instrumenting/exposition_formats):
//     lines "name{label=\"v\",...} value [timestamp]", label values escape
//     \\ \" \n, "# HELP"/"# TYPE" comments, NaN/+Inf/-Inf are legal values.
//   - Slurm sacct (slurm.schedmd.com/sacct.html): ConsumedEnergy is "in
//     joules" and "may include a unit prefix (K,M,G,T,P)"; ConsumedEnergyRaw
//     is in joules; "only in the case of an exclusive job allocation does this
//     value reflect the job's real energy consumption".
//
// The exporter's answer is untrusted input: it is bounded in size, line
// length, series and labels, and every refusal is an error or a counted skip,
// never a guess.
package energy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Bounds on an exporter's answer.
const (
	MaxBytes  = 8 << 20 // a node with 8 GPUs answers about 100 KiB
	MaxLine   = 64 << 10
	MaxSeries = 20_000
	MaxLabels = 64
)

// ErrTooLarge is wrapped by every refusal about size.
var ErrTooLarge = errors.New("energy: exporter answer too large")

// Sample is one line of a Prometheus text exposition.
type Sample struct {
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp int64 // milliseconds since the epoch; 0 when the line has none
}

// ParseStats counts what the parser skipped.
type ParseStats struct {
	Lines     int
	Samples   int
	Malformed int // sample lines that did not parse
	Skipped   int // well-formed samples of metrics nobody asked for
}

// ParseText reads a Prometheus text exposition and returns the samples of
// the metrics in want (all metrics when want is empty). A malformed line is
// counted and skipped; a body over MaxBytes, a line over MaxLine bytes or
// more than MaxSeries wanted samples is an error wrapping ErrTooLarge.
func ParseText(r io.Reader, want ...string) ([]Sample, ParseStats, error) {
	var st ParseStats
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	lr := &io.LimitedReader{R: r, N: MaxBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 4096), MaxLine)
	var out []Sample
	for sc.Scan() {
		st.Lines++
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		s, ok := parseSample(line)
		if !ok {
			st.Malformed++
			continue
		}
		if len(wanted) > 0 && !wanted[s.Name] {
			st.Skipped++
			continue
		}
		if len(out) == MaxSeries {
			return nil, st, fmt.Errorf("%w: more than %d series", ErrTooLarge, MaxSeries)
		}
		out = append(out, s)
		st.Samples++
	}
	if lr.N <= 0 {
		return nil, st, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, MaxBytes)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, st, fmt.Errorf("%w: a line longer than %d bytes", ErrTooLarge, MaxLine)
		}
		return nil, st, err
	}
	return out, st, nil
}

func isNameStart(c byte) bool {
	return c == '_' || c == ':' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool { return isNameStart(c) || c >= '0' && c <= '9' }

// parseSample parses `name[{labels}] value [timestamp]`.
func parseSample(line string) (Sample, bool) {
	var s Sample
	if !utf8.ValidString(line) || line == "" || !isNameStart(line[0]) {
		return s, false
	}
	i := 1
	for i < len(line) && isNameChar(line[i]) {
		i++
	}
	s.Name = line[:i]
	rest := line[i:]
	if strings.HasPrefix(rest, "{") {
		labels, after, ok := parseLabels(rest)
		if !ok {
			return s, false
		}
		s.Labels, rest = labels, after
	}
	f := strings.Fields(rest)
	if len(f) < 1 || len(f) > 2 {
		return s, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return s, false
	}
	s.Value = v
	if len(f) == 2 {
		ts, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return s, false
		}
		s.Timestamp = ts
	}
	return s, true
}

// parseLabels reads `{a="b",c="d",}` and returns the text after the '}'.
func parseLabels(s string) (map[string]string, string, bool) {
	labels := map[string]string{}
	i := 1 // after '{'
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return nil, "", false
		}
		if s[i] == '}' {
			return labels, s[i+1:], true
		}
		j := i
		if !isNameStart(s[j]) {
			return nil, "", false
		}
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		name := s[i:j]
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j >= len(s) || s[j] != '=' {
			return nil, "", false
		}
		j++
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j >= len(s) || s[j] != '"' {
			return nil, "", false
		}
		j++
		var b strings.Builder
		for {
			if j >= len(s) {
				return nil, "", false
			}
			c := s[j]
			if c == '"' {
				j++
				break
			}
			if c == '\\' {
				if j+1 >= len(s) {
					return nil, "", false
				}
				switch s[j+1] {
				case '\\':
					b.WriteByte('\\')
				case '"':
					b.WriteByte('"')
				case 'n':
					b.WriteByte('\n')
				default: // the format defines no other escape: keep it literal
					b.WriteByte('\\')
					b.WriteByte(s[j+1])
				}
				j += 2
				continue
			}
			b.WriteByte(c)
			j++
		}
		if len(labels) == MaxLabels {
			return nil, "", false
		}
		labels[name] = b.String()
		i = j
	}
}

// ToMillijoules converts a counter value in mJ to an integer. NaN, infinite,
// negative and absurd (above 2^63) values are refused.
func ToMillijoules(v float64) (uint64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v >= 1<<63 {
		return 0, false
	}
	return uint64(math.Round(v)), true
}
