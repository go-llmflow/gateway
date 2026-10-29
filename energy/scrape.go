package energy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// Counter and gauge names of the DCGM exporter (etc/default-counters.csv).
const (
	MetricEnergy = "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION" // counter, mJ since the driver was loaded
	MetricPower  = "DCGM_FI_DEV_POWER_USAGE"              // gauge, W
)

// ValidateURL applies to a DCGM exporter URL the rules of every upstream URL
// of the gateway (config.ValidateUpstream): http or https, a host, no
// credentials, no query or fragment. The gateway only ever reads the URLs an
// administrator configured; nothing a client sends reaches them, and the
// scraper follows no redirect, so a configured host cannot bounce it to
// another one.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("dcgm_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("dcgm_url %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("dcgm_url %q: host is required", raw)
	}
	if u.User != nil {
		return fmt.Errorf("dcgm_url %q: credentials in a URL are refused", u.Redacted())
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("dcgm_url %q: query and fragment are not allowed", raw)
	}
	return nil
}

// Scraper reads the energy counters of one DCGM exporter.
type Scraper struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration
}

// Reading is the energy counters of one scrape, in mJ by GPU.
type Reading struct {
	At time.Time
	MJ map[string]uint64
	// Dropped counts samples that carried an unusable value (NaN, negative).
	Dropped int
}

// GPUKey names a GPU of a sample: its UUID, else its index; "" when the
// sample carries neither.
func GPUKey(labels map[string]string) string {
	if v := labels["UUID"]; v != "" {
		return v
	}
	if v := labels["gpu"]; v != "" {
		return "gpu" + v
	}
	return ""
}

// ReadingOf extracts the energy counters from parsed samples. Samples of
// several metrics are ignored; two samples of one GPU (a MIG slice
// exporter, a duplicated series) are refused, since adding or choosing one
// would invent a number.
func ReadingOf(at time.Time, samples []Sample) (Reading, error) {
	r := Reading{At: at, MJ: map[string]uint64{}}
	for _, s := range samples {
		if s.Name != MetricEnergy {
			continue
		}
		key := GPUKey(s.Labels)
		if key == "" {
			return r, errors.New("energy: a sample without UUID or gpu label")
		}
		mj, ok := ToMillijoules(s.Value)
		if !ok {
			r.Dropped++
			continue
		}
		if _, dup := r.MJ[key]; dup {
			return r, fmt.Errorf("energy: two %s samples for %s", MetricEnergy, key)
		}
		r.MJ[key] = mj
	}
	return r, nil
}

// Scrape reads the exporter once. A non-200 answer, an answer past MaxBytes
// and an answer without any energy counter are errors.
func (sc *Scraper) Scrape(ctx context.Context, now time.Time) (Reading, error) {
	if err := ValidateURL(sc.URL); err != nil {
		return Reading{}, err
	}
	if sc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, sc.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sc.URL, nil)
	if err != nil {
		return Reading{}, err
	}
	cl := sc.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	// A copy that refuses redirects: the configured host is the only host read.
	nr := *cl
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := nr.Do(req)
	if err != nil {
		return Reading{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Reading{}, fmt.Errorf("energy: exporter answered %d", resp.StatusCode)
	}
	samples, _, err := ParseText(resp.Body, MetricEnergy)
	if err != nil {
		return Reading{}, err
	}
	rd, err := ReadingOf(now, samples)
	if err != nil {
		return Reading{}, err
	}
	if len(rd.MJ) == 0 {
		return Reading{}, fmt.Errorf("energy: no %s sample in the answer", MetricEnergy)
	}
	return rd, nil
}

// Keys returns the GPU keys of a reading, sorted.
func (r Reading) Keys() []string {
	ks := make([]string, 0, len(r.MJ))
	for k := range r.MJ {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
