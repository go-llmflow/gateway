package energy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds on a sacct input.
const (
	SacctMaxBytes = 16 << 20
	SacctMaxLines = 500_000
	sacctMaxLine  = 8192
)

// SacctTimeLayout is sacct's default timestamp, "year-month-dateThour:minute:second"
// (sacct.html, SLURM_TIME_FORMAT). The input is read as UTC: run sacct with
// TZ=UTC, or the instants are the cluster's local time read as UTC.
const SacctTimeLayout = "2006-01-02T15:04:05"

// SacctJob is the energy of one job allocation.
type SacctJob struct {
	Account    string
	Start, End time.Time
	// Running is true when End is "Unknown" or "None": the job had not ended
	// when sacct ran.
	Running bool
	Joules  float64
}

// SacctReport is a parsed sacct output.
type SacctReport struct {
	Jobs []SacctJob
	// Earliest and Latest are the first and last instants the file mentions
	// (starts and known ends); zero without any job.
	Earliest, Latest time.Time
	// Steps, NoEnergy and NotStarted count the rows left out: job steps (a
	// JobID with a '.'), rows without energy (the plugin did not collect it:
	// counted as 0, and said), and jobs that never started (Start Unknown).
	Steps, NoEnergy, NotStarted int
}

// ParseSacct reads
//
//	sacct -a -X -P -o JobID,Account,Start,End,ConsumedEnergyRaw -S <from> -E <to>
//
// run with TZ=UTC ('|'-delimited with a header). Sources (sacct.html, opened
// 2026-10-30): ConsumedEnergyRaw is "in joules"; "only in the case of an
// exclusive job allocation does this value reflect the job's real energy
// consumption"; Start and End are the job's "initiation time" and "termination
// time" in "year-month-dateThour:minute:second"; End is "Unknown" for a running
// job (the page's truncate example); -S/-E "select jobs in any state after/
// before the specified time": jobs ACTIVE in the window, not jobs that ended
// in it; -X shows "the job allocation itself, not taking steps into
// consideration".
//
// The header must name JobID, Account, Start, End and ConsumedEnergyRaw (or
// ConsumedEnergy): without the instants a job's energy cannot be placed in a
// period, and without JobID steps cannot be told from allocations and are
// counted twice. A ConsumedEnergy value with a unit prefix (K, M, ...) is
// refused: the page says a prefix may be there but not its base. Rows of job
// steps (a JobID with a '.') are skipped even without -X.
func ParseSacct(r io.Reader) (*SacctReport, error) {
	lr := &io.LimitedReader{R: r, N: SacctMaxBytes + 1}
	sc := bufio.NewScanner(lr)
	sc.Buffer(make([]byte, 0, 4096), sacctMaxLine)
	rep := &SacctReport{}
	acct, en, job, st, end := -1, -1, -1, -1, -1
	header := false
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
		if !header {
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
				case "Start":
					st = i
				case "End":
					end = i
				}
			}
			if acct < 0 || en < 0 || job < 0 || st < 0 || end < 0 {
				return nil, errors.New("energy: the sacct header must name JobID, Account, Start, End and ConsumedEnergyRaw (sacct -a -X -P -o JobID,Account,Start,End,ConsumedEnergyRaw -S <from> -E <to>)")
			}
			header = true
			continue
		}
		if len(f) <= max(acct, en, job, st, end) {
			return nil, fmt.Errorf("energy: sacct line %d has %d fields", lines, len(f))
		}
		if strings.Contains(f[job], ".") {
			rep.Steps++
			continue
		}
		a := strings.TrimSpace(f[acct])
		if a == "" {
			continue
		}
		start, startKnown, err := parseSacctTime(f[st])
		if err != nil {
			return nil, fmt.Errorf("energy: sacct line %d: Start: %w", lines, err)
		}
		if !startKnown {
			rep.NotStarted++
			continue
		}
		endT, endKnown, err := parseSacctTime(f[end])
		if err != nil {
			return nil, fmt.Errorf("energy: sacct line %d: End: %w", lines, err)
		}
		if endKnown && endT.Before(start) {
			return nil, fmt.Errorf("energy: sacct line %d: the job ends before it starts", lines)
		}
		v := strings.TrimSpace(f[en])
		var joules float64
		if v == "" {
			rep.NoEnergy++
		} else {
			if joules, err = strconv.ParseFloat(v, 64); err != nil {
				return nil, fmt.Errorf("energy: sacct line %d: energy %q is not a plain number of joules (use ConsumedEnergyRaw)", lines, v)
			}
			if math.IsNaN(joules) || math.IsInf(joules, 0) || joules < 0 {
				return nil, fmt.Errorf("energy: sacct line %d: energy %q out of range", lines, v)
			}
		}
		rep.Jobs = append(rep.Jobs, SacctJob{Account: a, Start: start, End: endT, Running: !endKnown, Joules: joules})
		rep.mention(start)
		if endKnown {
			rep.mention(endT)
		}
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
	if !header {
		return nil, errors.New("energy: empty sacct input")
	}
	return rep, nil
}

func (r *SacctReport) mention(t time.Time) {
	if r.Earliest.IsZero() || t.Before(r.Earliest) {
		r.Earliest = t
	}
	if t.After(r.Latest) {
		r.Latest = t
	}
}

// parseSacctTime reads a timestamp; "Unknown", "None" and "" are not an
// instant (known false), anything else that does not parse is an error.
func parseSacctTime(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", "Unknown", "None", "N/A":
		return time.Time{}, false, nil
	}
	t, err := time.ParseInLocation(SacctTimeLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%q is not a sacct timestamp (%s; run sacct with TZ=UTC)", s, SacctTimeLayout)
	}
	return t, true, nil
}

// Coverage says what a period got out of a sacct file.
type Coverage struct {
	// Jobs is the number of job allocations in the file; Used those with energy
	// in the period, Prorated those only partly inside it, Outside those
	// entirely outside (counted as 0), Running those still running (their end
	// is the period's end, or the file's latest instant if earlier).
	Jobs, Used, Prorated, Outside, Running int
	// Earliest and Latest are the instants the file mentions.
	Earliest, Latest time.Time
	// Covers is false when the file's span [Earliest, Latest] reaches less than
	// half of the period, or no job falls in it: the file is for another
	// period, and no difference should be printed.
	Covers bool
}

// Prorate attributes each job's energy to [from, to) in proportion to how much
// of its run falls inside, assuming constant power; it returns joules per
// account. A job with no extent (End = Start) counts fully when its instant is
// in the period. A running job (End Unknown) has accrued its energy up to the
// file's latest instant: it counts up to the period's end, or up to that
// instant if it is earlier.
func (r *SacctReport) Prorate(from, to time.Time) (map[string]float64, Coverage) {
	out := map[string]float64{}
	c := Coverage{Jobs: len(r.Jobs), Earliest: r.Earliest, Latest: r.Latest}
	for _, j := range r.Jobs {
		end := j.End
		if j.Running {
			// Its energy so far was read when the file was made: the latest
			// instant the file mentions. It accrued over [Start, that], of
			// which the part inside the period counts.
			end = r.Latest
			if end.Before(j.Start) {
				end = j.Start
			}
			c.Running++
		}
		var frac float64
		switch {
		case !end.After(j.Start):
			if !j.Start.Before(from) && j.Start.Before(to) {
				frac = 1
			}
		default:
			lo, hi := j.Start, end
			if from.After(lo) {
				lo = from
			}
			if to.Before(hi) {
				hi = to
			}
			if hi.After(lo) {
				frac = float64(hi.Sub(lo)) / float64(end.Sub(j.Start))
			}
		}
		if frac <= 0 {
			c.Outside++
			continue
		}
		c.Used++
		if frac < 1 {
			c.Prorated++
		}
		out[j.Account] += j.Joules * frac
	}
	if !r.Earliest.IsZero() && to.After(from) {
		lo, hi := r.Earliest, r.Latest
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		c.Covers = hi.After(lo) && 2*hi.Sub(lo) >= to.Sub(from) && c.Used > 0
	}
	return out, c
}
