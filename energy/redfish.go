package energy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"time"
)

// Redfish: the energy of a whole node, from its BMC.
//
// DMTF's EnvironmentMetrics schema (v1.3.1, redfish.dmtf.org/schemas/v1/
// EnvironmentMetrics.v1_3_1.json, opened 2026-11-05) has EnergykWh, "the total
// energy, in kilowatt-hour units, for this resource", a Sensor excerpt whose
// Reading is the value; EnergyJoules is the device-level equivalent ("EnergykWh
// is used for large-scale consumption measurements"). Both are counters of the
// energy used, unlike PowerWatts, a sampled power: a counter makes the energy
// between two reads exact. The resource is the Chassis' EnvironmentMetrics
// (/redfish/v1/Chassis/{id}/EnvironmentMetrics).

// RedfishKey names the node in a Reading.
const RedfishKey = "node"

// MaxRedfishBody bounds a BMC's answer.
const MaxRedfishBody = 1 << 20

// ValidateRedfishURL applies the rules of an exporter URL (ValidateURL) to a
// BMC's, and requires https: the BMC is read with a password, which basic
// authentication sends in clear over http.
func ValidateRedfishURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("redfish_url %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("redfish_url %q: scheme must be https (the BMC password is sent with every read)", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("redfish_url %q: host is required", raw)
	}
	if u.User != nil {
		return fmt.Errorf("redfish_url %q: credentials in a URL are refused", u.Redacted())
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return fmt.Errorf("redfish_url %q: query and fragment are not allowed", raw)
	}
	return nil
}

// ParseRedfishEnergy reads the total energy, in millijoules, from an
// EnvironmentMetrics document: EnergykWh.Reading, else EnergyJoules.Reading.
// A missing, null, negative, NaN or absurd reading is an error: nothing is
// invented.
func ParseRedfishEnergy(doc []byte) (uint64, error) {
	var d struct {
		EnergykWh *struct {
			Reading *float64 `json:"Reading"`
		} `json:"EnergykWh"`
		EnergyJoules *struct {
			Reading *float64 `json:"Reading"`
		} `json:"EnergyJoules"`
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&d); err != nil {
		return 0, fmt.Errorf("redfish: %w", err)
	}
	var mj float64
	switch {
	case d.EnergykWh != nil && d.EnergykWh.Reading != nil:
		mj = *d.EnergykWh.Reading * MJPerKWh
	case d.EnergyJoules != nil && d.EnergyJoules.Reading != nil:
		mj = *d.EnergyJoules.Reading * 1000
	default:
		return 0, errors.New("redfish: no EnergykWh or EnergyJoules reading")
	}
	if math.IsNaN(mj) || math.IsInf(mj, 0) || mj < 0 || mj > float64(maxDelta)*1000 {
		return 0, errors.New("redfish: the energy reading is negative or out of range")
	}
	return uint64(math.Round(mj)), nil
}

// RedfishScraper reads the energy counter of one node's BMC.
type RedfishScraper struct {
	URL      string
	User     string
	Password string
	// Client carries the TLS configuration (the BMC's CA); redirects are never
	// followed.
	Client  *http.Client
	Timeout time.Duration
}

// Scrape reads the BMC once.
func (sc *RedfishScraper) Scrape(ctx context.Context, now time.Time) (Reading, error) {
	if err := ValidateRedfishURL(sc.URL); err != nil {
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
	req.SetBasicAuth(sc.User, sc.Password)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	cl := sc.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	nr := *cl
	nr.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := nr.Do(req)
	if err != nil {
		return Reading{}, redactCreds(err, sc.Password)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Reading{}, fmt.Errorf("redfish: the BMC answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxRedfishBody+1))
	if err != nil {
		return Reading{}, err
	}
	if len(body) > MaxRedfishBody {
		return Reading{}, errors.New("redfish: the answer is larger than 1 MiB")
	}
	mj, err := ParseRedfishEnergy(body)
	if err != nil {
		return Reading{}, err
	}
	return Reading{At: now, MJ: map[string]uint64{RedfishKey: mj}}, nil
}

// redactCreds keeps a password out of an error (a proxy or transport may echo
// a request).
func redactCreds(err error, password string) error {
	if password == "" {
		return err
	}
	msg := err.Error()
	for i := 0; i+len(password) <= len(msg); i++ {
		if msg[i:i+len(password)] == password {
			return errors.New("redfish: request failed (the error mentioned the password and was redacted)")
		}
	}
	return err
}
