package energy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/redfish"
)

// Redfish: the energy of a whole node, from its BMC, through package redfish.
//
// DMTF's EnvironmentMetrics schema (v1.3.1, redfish.dmtf.org/schemas/v1/
// EnvironmentMetrics.v1_3_1.json, opened 2026-11-05) has EnergykWh, "the total
// energy, in kilowatt-hour units, for this resource", a Sensor excerpt whose
// Reading is the value: a counter of the energy used, unlike PowerWatts, a
// sampled power, so the energy between two reads is exact. A BMC that reports
// only a power is not metered here (the caller's fallback factor applies):
// integrating a sampled power would be an estimate presented as a measurement.

// RedfishKey names the node in a Reading.
const RedfishKey = "node"

// ErrNoCounter: the BMC answered, but has no energy counter.
var ErrNoCounter = errors.New("redfish: the BMC reports no energy counter")

// ValidateRedfishURL applies the rules of a BMC URL (package redfish): https, a
// host, no credentials, no query or fragment.
func ValidateRedfishURL(raw string) error {
	if err := redfish.ValidateURL(raw); err != nil {
		return fmt.Errorf("redfish_url: %w", err)
	}
	return nil
}

// RedfishScraper reads the energy counter of one node's BMC. The URL is either
// the BMC's address (the service is then discovered: https://bmc.example) or
// the path of the chassis' EnvironmentMetrics resource
// (https://bmc.example/redfish/v1/Chassis/1/EnvironmentMetrics).
type RedfishScraper struct {
	URL      string
	User     string
	Password string
	// CAFile is a PEM file of the authorities that sign the BMC's certificate.
	CAFile string
	// Client carries the transport (tests); redirects are never followed.
	Client  *http.Client
	Timeout time.Duration

	once sync.Once
	rc   *redfish.Client
	err  error
}

func (sc *RedfishScraper) client() (*redfish.Client, error) {
	sc.once.Do(func() {
		u, err := url.Parse(sc.URL)
		if err != nil {
			sc.err = err
			return
		}
		cfg := redfish.Config{Credentials: redfish.Static{User: sc.User, Password: sc.Password}, CAFile: sc.CAFile, HTTP: sc.Client, Timeout: sc.Timeout}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/EnvironmentMetrics") {
			cfg.MetricsPath = u.Path
			u.Path = ""
		}
		cfg.BaseURL = u.String()
		sc.rc, sc.err = redfish.New(cfg)
	})
	return sc.rc, sc.err
}

// Scrape reads the BMC once.
func (sc *RedfishScraper) Scrape(ctx context.Context, now time.Time) (Reading, error) {
	rc, err := sc.client()
	if err != nil {
		return Reading{}, err
	}
	r, err := rc.Read(ctx)
	if err != nil {
		return Reading{}, err
	}
	if r.EnergyKWh == nil {
		return Reading{}, ErrNoCounter
	}
	mj := *r.EnergyKWh * MJPerKWh
	if math.IsNaN(mj) || math.IsInf(mj, 0) || mj < 0 || mj > float64(maxDelta)*1000 {
		return Reading{}, errors.New("redfish: the energy reading is out of range")
	}
	return Reading{At: now, MJ: map[string]uint64{RedfishKey: uint64(math.Round(mj))}}, nil
}
