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

// Bounds on a sacct input.
const (
	SacctMaxBytes = 16 << 20
	SacctMaxLines = 500_000
	sacctMaxLine  = 8192
)

// SacctReport is the batch energy of Slurm jobs per account.
type SacctReport struct {
	// Joules is the energy per account, summed over its job allocations.
	Joules map[string]float64
	// Jobs counts the allocation rows read, NoEnergy those whose energy
	// field was empty (the plugin did not collect it: counted as 0, and said).
	Jobs, NoEnergy int
}

// ParseSacct reads
//
//	sacct -a -X -P -o Account,ConsumedEnergyRaw
//
// ('|'-delimited with a header; sacct.html: ConsumedEnergyRaw is "in
// joules"). The header must name Account and ConsumedEnergyRaw or
// ConsumedEnergy. A ConsumedEnergy value with a unit prefix (K, M, ...) is
// refused: the page says a prefix may be there but not its base, so a value
// is only trusted in plain joules. Rows of job steps (a JobID with a '.'),
// when a JobID column is present, are skipped, so a job is not counted
// twice. Slurm notes that "only in the case of an exclusive job allocation
// does this value reflect the job's real energy consumption": the report
// says nothing about shared nodes.
func ParseSacct(r io.Reader) (*SacctReport, error) {
	lr := &io.LimitedReader{R: r, N: SacctMaxBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 4096), sacctMaxLine)
	rep := &SacctReport{Joules: map[string]float64{}}
	acct, en, job := -1, -1, -1
	lines := 0
	for sc.Scan() {
		lines++
		if lines > SacctMaxLines {
			return nil, fmt.Errorf("%w: more than %d lines", ErrTooLarge, SacctMaxLines)
		}
		line := strings.TrimRight(sc.Text(), "\r")
		if !utf8.ValidString(line) {
			return nil, errors.New("energy: the sacct input is not UTF-8 text")
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "|")
		if acct < 0 {
			for i, h := range f {
				switch strings.TrimSpace(h) {
				case "Account":
					acct = i
				case "ConsumedEnergyRaw", "ConsumedEnergy":
					if en < 0 || strings.TrimSpace(h) == "ConsumedEnergyRaw" {
						en = i
					}
				case "JobID":
					job = i
				}
			}
			if acct < 0 || en < 0 {
				return nil, errors.New("energy: the sacct header must name Account and ConsumedEnergyRaw (sacct -P -o Account,ConsumedEnergyRaw)")
			}
			continue
		}
		if len(f) <= max(acct, en, job) {
			return nil, fmt.Errorf("energy: sacct line %d has %d fields", lines, len(f))
		}
		if job >= 0 && strings.Contains(f[job], ".") {
			continue
		}
		a := strings.TrimSpace(f[acct])
		if a == "" {
			continue
		}
		rep.Jobs++
		v := strings.TrimSpace(f[en])
		if v == "" {
			rep.NoEnergy++
			continue
		}
		j, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("energy: sacct line %d: energy %q is not a plain number of joules (use ConsumedEnergyRaw)", lines, v)
		}
		if math.IsNaN(j) || math.IsInf(j, 0) || j < 0 {
			return nil, fmt.Errorf("energy: sacct line %d: energy %q out of range", lines, v)
		}
		rep.Joules[a] += j
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, SacctMaxBytes)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("%w: a line longer than %d bytes", ErrTooLarge, sacctMaxLine)
		}
		return nil, err
	}
	if acct < 0 {
		return nil, errors.New("energy: empty sacct input")
	}
	return rep, nil
}
