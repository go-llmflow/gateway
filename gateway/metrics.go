package gateway

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// A small Prometheus text-format (version 0.0.4) writer. The format rules
// followed here are from https://prometheus.io/docs/instrumenting/exposition_formats/
// (opened 2026-10-23): one HELP and TYPE line per metric name before its
// samples, label values escaped (backslash, double quote, line feed),
// histograms as _bucket{le} cumulative counts with a +Inf bucket equal to
// _count, plus _sum and _count.

// counterVec is a counter with labels.
type counterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	values     map[string]*atomicFloat
	order      map[string][]string
}

type atomicFloat struct{ bits atomic.Uint64 }

func (f *atomicFloat) add(v float64) {
	for {
		old := f.bits.Load()
		if f.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

func (f *atomicFloat) load() float64 { return math.Float64frombits(f.bits.Load()) }

func newCounterVec(name, help string, labels ...string) *counterVec {
	return &counterVec{name: name, help: help, labels: labels, values: map[string]*atomicFloat{}, order: map[string][]string{}}
}

func (c *counterVec) add(v float64, values ...string) {
	key := strings.Join(values, "\x00")
	c.mu.Lock()
	f := c.values[key]
	if f == nil {
		f = &atomicFloat{}
		c.values[key] = f
		c.order[key] = append([]string(nil), values...)
	}
	c.mu.Unlock()
	f.add(v)
}

func (c *counterVec) write(w io.Writer) {
	writeHeader(w, c.name, c.help, "counter")
	c.mu.Lock()
	keys := make([]string, 0, len(c.values))
	for k := range c.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeSample(w, c.name, c.labels, c.order[k], c.values[k].load())
	}
	c.mu.Unlock()
}

// histogram is a fixed-bucket histogram without labels.
type histogram struct {
	name, help string
	bounds     []float64
	mu         sync.Mutex
	counts     []uint64
	sum        float64
	count      uint64
}

func newHistogram(name, help string, bounds []float64) *histogram {
	return &histogram{name: name, help: help, bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
			break
		}
	}
	h.sum += v
	h.count++
}

func (h *histogram) write(w io.Writer) {
	writeHeader(w, h.name, h.help, "histogram")
	h.mu.Lock()
	defer h.mu.Unlock()
	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i]
		fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", h.name, formatFloat(b), cum)
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, h.count)
	fmt.Fprintf(w, "%s_sum %s\n", h.name, formatFloat(h.sum))
	fmt.Fprintf(w, "%s_count %d\n", h.name, h.count)
}

func writeHeader(w io.Writer, name, help, typ string) {
	help = strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help)
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func writeSample(w io.Writer, name string, labels, values []string, v float64) {
	bw, ok := w.(*bufio.Writer)
	if !ok {
		bw = bufio.NewWriter(w)
		defer bw.Flush()
	}
	bw.WriteString(name)
	if len(labels) > 0 {
		bw.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				bw.WriteByte(',')
			}
			bw.WriteString(l)
			bw.WriteString(`="`)
			bw.WriteString(labelEscaper.Replace(values[i]))
			bw.WriteByte('"')
		}
		bw.WriteByte('}')
	}
	bw.WriteByte(' ')
	bw.WriteString(formatFloat(v))
	bw.WriteByte('\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// metrics groups the gateway's instruments.
type metrics struct {
	requests   *counterVec // team, model, code
	tokens     *counterVec // team, model, kind
	gpuHours   *counterVec // team, pool
	cost       *counterVec // team, pool, unit: costs in a currency (external providers)
	denied     *counterVec // team, reason
	retries    *counterVec // group
	fallbacks  *counterVec // from, to
	estimated  *counterVec // team, model
	softAlerts *counterVec // kind
	// auditFailures counts audit entries or checkpoints that could not be
	// written.
	auditFailures *counterVec // kind
	// guardDecisions counts guardrail findings by guard, stage and action
	// (allow is not counted: it is every request); guardErrors the calls
	// that failed, by guard and whether the request went on (fail-open).
	guardDecisions *counterVec // guard, stage, action
	guardErrors    *counterVec // guard, outcome
	guardSeconds   *histogram
	// toolCalls counts MCP tool calls by team, server and outcome.
	toolCalls *counterVec // team, server, status
	overhead  *histogram
	upstreamT *histogram
}

// charge counts a cost in its unit's counter: GPU-hours and currencies are
// never one series.
func (m *metrics) charge(cost float64, team, pool, unit string) {
	if unit == "" {
		m.gpuHours.add(cost, team, pool)
		return
	}
	m.cost.add(cost, team, pool, unit)
}

func newMetrics() *metrics {
	return &metrics{
		requests: newCounterVec("llmflow_requests_total", "Requests handled, by team, model and HTTP status.", "team", "model", "code"),
		tokens:   newCounterVec("llmflow_tokens_total", "Tokens reported by vLLM usage, by kind (prompt, completion, cached).", "team", "model", "kind"),
		gpuHours: newCounterVec("llmflow_gpu_hours_total", "Internal cost charged, in GPU-hours.", "team", "pool"),
		cost:     newCounterVec("llmflow_cost_total", "Cost charged in a currency (external providers), by unit; GPU-hours are in llmflow_gpu_hours_total.", "team", "pool", "unit"),
		denied:   newCounterVec("llmflow_denied_total", "Requests refused before reaching vLLM, by reason.", "team", "reason"),
		retries:  newCounterVec("llmflow_upstream_retries_total", "Retries to another upstream before the first byte.", "group"),
		fallbacks: newCounterVec("llmflow_fallbacks_total",
			"Requests moved to a fallback model group before the first byte, by group left, group used and reason (unavailable, saturated).",
			"from", "to", "reason"),
		softAlerts: newCounterVec("llmflow_soft_budget_alerts_total", "Soft-budget limits reached (one per budget and period).", "kind"),
		auditFailures: newCounterVec("llmflow_audit_failures_total",
			"Audit entries (by kind) or checkpoints that could not be written.", "kind"),
		guardDecisions: newCounterVec("llmflow_guardrail_decisions_total",
			"Guardrail findings, by guardrail, stage (pre, post) and action (mask, block, log).", "guard", "stage", "action"),
		guardErrors: newCounterVec("llmflow_guardrail_errors_total",
			"Guardrail calls that failed or timed out, by guardrail and outcome (blocked: fail-closed; allowed: fail-open).", "guard", "outcome"),
		guardSeconds: newHistogram("llmflow_guardrail_seconds", "Time spent in guardrails per request and stage (concurrent classifiers count once).",
			[]float64{0.0001, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}),
		toolCalls: newCounterVec("llmflow_tool_calls_total",
			"MCP tool calls by team, tool server and outcome (ok, tool_error, input_required, server_error, unavailable, malformed, denied, audit_failed, list_failed).",
			"team", "server", "status"),
		estimated: newCounterVec("llmflow_usage_estimated_total",
			"Requests charged with estimated tokens because vLLM's usage never reached the gateway (client gone first).", "team", "model"),
		overhead: newHistogram("llmflow_overhead_seconds", "Time spent in the gateway itself (request handling before the upstream call plus response handling after it).",
			[]float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1}),
		upstreamT: newHistogram("llmflow_upstream_seconds", "Time from sending the upstream request to the end of its response.",
			[]float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}),
	}
}
