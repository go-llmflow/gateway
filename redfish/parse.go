package redfish

import (
	"encoding/json"
	"fmt"
	"math"
)

// KWhPerJoule converts joules to kilowatt-hours (1 kWh = 3.6 MJ).
const KWhPerJoule = 1.0 / 3_600_000

// maxEnergyKWh and maxWatts bound what a reading may claim: a counter past a
// terawatt-hour or a power past a gigawatt is a broken BMC, not a node.
const (
	maxEnergyKWh = 1e9
	maxWatts     = 1e9
)

// Metrics is what a document says of a node's consumption.
type Metrics struct {
	EnergyKWh  *float64
	PowerWatts *float64
}

// sensor is a Sensor excerpt: the value is its Reading.
type sensor struct {
	Reading *float64 `json:"Reading"`
}

// ParseEnvironmentMetrics reads an EnvironmentMetrics document (also the
// shape of a Chassis that embeds the same properties): EnergykWh.Reading, else
// EnergyJoules.Reading converted to kWh, and PowerWatts.Reading. A reading
// that is null, negative, not finite or out of range is dropped; a document
// that is not a JSON object is ErrMalformed. Nothing is invented: a document
// without any usable reading yields an empty Metrics and no error.
func ParseEnvironmentMetrics(doc []byte) (Metrics, error) {
	var d struct {
		EnergykWh    *sensor `json:"EnergykWh"`
		EnergyJoules *sensor `json:"EnergyJoules"`
		PowerWatts   *sensor `json:"PowerWatts"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return Metrics{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var m Metrics
	switch {
	case usable(d.EnergykWh, maxEnergyKWh):
		v := *d.EnergykWh.Reading
		m.EnergyKWh = &v
	case usable(d.EnergyJoules, maxEnergyKWh/KWhPerJoule):
		v := *d.EnergyJoules.Reading * KWhPerJoule
		m.EnergyKWh = &v
	}
	if usable(d.PowerWatts, maxWatts) {
		v := *d.PowerWatts.Reading
		m.PowerWatts = &v
	}
	return m, nil
}

func usable(s *sensor, max float64) bool {
	if s == nil || s.Reading == nil {
		return false
	}
	v := *s.Reading
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= max
}

// ParsePowerConsumed sums the PowerConsumedWatts of the PowerControl entries
// of a Power resource (the older way a BMC reports power). ErrNoReading when
// none is usable.
func ParsePowerConsumed(doc []byte) (float64, error) {
	var d struct {
		PowerControl []struct {
			PowerConsumedWatts *float64 `json:"PowerConsumedWatts"`
		} `json:"PowerControl"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var sum float64
	n := 0
	for _, pc := range d.PowerControl {
		if pc.PowerConsumedWatts == nil {
			continue
		}
		v := *pc.PowerConsumedWatts
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > maxWatts {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0, ErrNoReading
	}
	return sum, nil
}
