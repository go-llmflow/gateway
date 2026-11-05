package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime/debug"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// Billing: what the reports compute live, and what a closed month freezes.
//
// A month is billed from cells (energy, network, storage, fixed costs, one
// per consumer and day) and usage lines (GPU-hours and provider spend). The
// reports compute them live from the stores, so a corrected PUE, a new price
// or late data changes a month that was already invoiced. POST
// /admin/billing/close?month=YYYY-MM freezes a month: it stores the cells and
// the usage lines WITH the parameters that produced them (the method), enters
// a SHA-256 of the snapshot in the audit log, and from then on a report that
// covers the whole month reads the snapshot. A later difference between the
// live computation and the snapshot is not a rewrite: it is booked as an
// adjustment line on the next open month (a credit note when negative). A
// month cannot be reopened.

// BillingMethodVersion numbers the computation: 1 was the energy-only fluids
// report of v0.26.0 and before; 2 adds fixed costs, metered network and
// storage, markups, credits and closed months. Bump it when a rule changes.
const BillingMethodVersion = 2

func gatewayVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "(devel)"
}

var billingMonthRE = regexp.MustCompile(`^20[0-9]{2}-(0[1-9]|1[0-2])$`)

// ParseBillingMonth parses "YYYY-MM" (2000 to 2099) into the first instant of
// the month, UTC.
func ParseBillingMonth(v string) (time.Time, error) {
	if !billingMonthRE.MatchString(v) {
		return time.Time{}, fmt.Errorf("month %q: want YYYY-MM", v)
	}
	return time.Parse("2006-01", v)
}

// ---- the method ----

type periodMethod struct {
	PUE             float64 `json:"pue,omitempty"`
	PricePerKWh     float64 `json:"price_per_kwh,omitempty"`
	PricePerGB      float64 `json:"price_per_gb,omitempty"`
	PricePerGBMonth float64 `json:"price_per_gb_month,omitempty"`
}

type fixedMethod struct {
	Name    string             `json:"name"`
	Pool    string             `json:"pool,omitempty"`
	Monthly float64            `json:"monthly"`
	Charge  string             `json:"charge"`
	Basis   string             `json:"basis"`
	Periods map[string]float64 `json:"periods,omitempty"`
}

// billingMethod is the parameters a report was computed with, stamped on
// every report and stored with every closed month.
type billingMethod struct {
	MethodVersion    int                     `json:"method_version"`
	GatewayVersion   string                  `json:"gateway_version,omitempty"`
	Currency         string                  `json:"currency"`
	CurrencyDecimals int                     `json:"currency_decimals"`
	PUE              float64                 `json:"pue"`
	PricePerKWh      float64                 `json:"price_per_kwh"`
	PricePerGB       float64                 `json:"price_per_gb"`
	PricePerGBMonth  float64                 `json:"price_per_gb_month"`
	IdleEnergy       string                  `json:"idle_energy"`
	WPrefill         float64                 `json:"w_prefill"`
	WDecode          float64                 `json:"w_decode"`
	IdleWatts        float64                 `json:"idle_watts"`
	Periods          map[string]periodMethod `json:"periods,omitempty"`
	FixedCosts       []fixedMethod           `json:"fixed_costs,omitempty"`
	Markups          map[string]float64      `json:"markups,omitempty"`
}

func (s *Server) billingMethod() billingMethod {
	f := s.cfg.Fluids
	m := billingMethod{MethodVersion: BillingMethodVersion, GatewayVersion: gatewayVersion()}
	if f == nil {
		return m
	}
	m.Currency, m.CurrencyDecimals, m.PUE, m.PricePerKWh = f.Currency, f.CurrencyDecimals, f.PUE, f.PricePerKWh
	m.PricePerGB, m.PricePerGBMonth, m.IdleEnergy = f.PricePerGB, f.PricePerGBMonth, f.IdleEnergy
	m.WPrefill, m.WDecode, m.IdleWatts = f.WPrefill, f.WDecode, f.IdleWatts
	if len(f.Periods) > 0 {
		m.Periods = map[string]periodMethod{}
		for k, p := range f.Periods {
			m.Periods[k] = periodMethod{p.PUE, p.PricePerKWh, p.PricePerGB, p.PricePerGBMonth}
		}
	}
	for _, c := range f.FixedCosts {
		m.FixedCosts = append(m.FixedCosts, fixedMethod{c.Name, c.Pool, c.Monthly, c.Charge, c.Basis, c.Periods})
	}
	for _, o := range s.cfg.Orgs {
		if o.Markup != 0 {
			if m.Markups == nil {
				m.Markups = map[string]float64{}
			}
			m.Markups[o.Name] = o.Markup
		}
	}
	return m
}

// ---- the snapshot ----

type cellSnap struct {
	Day             string `json:"day,omitempty"`
	Org             string `json:"org,omitempty"`
	Lab             string `json:"lab,omitempty"`
	Team            string `json:"team,omitempty"`
	Project         string `json:"project,omitempty"`
	User            string `json:"user,omitempty"`
	Model           string `json:"model,omitempty"`
	Pool            string `json:"pool,omitempty"`
	ActiveMJ        int64  `json:"active_mj,omitempty"`
	IdleMJ          int64  `json:"idle_mj,omitempty"`
	FixedMinor      int64  `json:"fixed_minor,omitempty"`
	CreditMinor     int64  `json:"credit_minor,omitempty"`
	NetIn           int64  `json:"net_in,omitempty"`
	NetOut          int64  `json:"net_out,omitempty"`
	StorageByteDays int64  `json:"storage_byte_days,omitempty"`
}

func toSnap(c fluidsCell) cellSnap {
	return cellSnap{c.Day, c.Org, c.Lab, c.Team, c.Project, c.User, c.Model, c.Pool, c.ActiveMJ, c.IdleMJ, c.FixedMinor, c.CreditMinor, c.NetIn, c.NetOut, c.StorageByteDays}
}

// terms rebuilds the price terms a closed month was billed with.
func (m billingMethod) terms() *cellTerms {
	f := &config.Fluids{PUE: m.PUE, PricePerKWh: m.PricePerKWh, Currency: m.Currency, CurrencyDecimals: m.CurrencyDecimals,
		PricePerGB: m.PricePerGB, PricePerGBMonth: m.PricePerGBMonth, Periods: map[string]config.FluidsPeriod{}}
	for k, p := range m.Periods {
		f.Periods[k] = config.FluidsPeriod{PUE: p.PUE, PricePerKWh: p.PricePerKWh, PricePerGB: p.PricePerGB, PricePerGBMonth: p.PricePerGBMonth}
	}
	markups := m.Markups
	return &cellTerms{f: f, markup: func(org string) float64 { return markups[org] }}
}

func (c cellSnap) cell(t *cellTerms) fluidsCell {
	return fluidsCell{terms: t, Day: c.Day, Consumer: energy.Consumer{Org: c.Org, Lab: c.Lab, Team: c.Team, Project: c.Project, User: c.User, Model: c.Model, Pool: c.Pool},
		ActiveMJ: c.ActiveMJ, IdleMJ: c.IdleMJ, FixedMinor: c.FixedMinor, CreditMinor: c.CreditMinor, NetIn: c.NetIn, NetOut: c.NetOut, StorageByteDays: c.StorageByteDays}
}

// usageSnap is a line of GPU-hours (Unit "") or provider spend (Unit a
// currency) of a month.
type usageSnap struct {
	Org     string  `json:"org,omitempty"`
	Lab     string  `json:"lab,omitempty"`
	Team    string  `json:"team,omitempty"`
	Project string  `json:"project,omitempty"`
	User    string  `json:"user,omitempty"`
	Model   string  `json:"model,omitempty"`
	Pool    string  `json:"pool,omitempty"`
	Day     string  `json:"day,omitempty"`
	Unit    string  `json:"unit,omitempty"`
	Cost    float64 `json:"cost"`
}

// adjLine is the difference between what a closed month was billed and what
// the live computation gives now, in the fluids currency's minor units, booked
// on the next open month for a cost centre (a credit note when negative).
type adjLine struct {
	FromMonth  string `json:"from_month"`
	Org        string `json:"org"`
	CostCenter string `json:"cost_center"`
	Fluids     int64  `json:"fluids"`
	Fixed      int64  `json:"fixed"`
	Network    int64  `json:"network"`
	Storage    int64  `json:"storage"`
	Markup     int64  `json:"markup"`
	Credit     int64  `json:"credit"`
}

func (a adjLine) total() int64 {
	return a.Fluids + a.Fixed + a.Network + a.Storage + a.Markup - a.Credit
}
func (a adjLine) zero() bool {
	return a.Fluids == 0 && a.Fixed == 0 && a.Network == 0 && a.Storage == 0 && a.Markup == 0 && a.Credit == 0
}

type closedPayload struct {
	Method      billingMethod `json:"method"`
	Cells       []cellSnap    `json:"cells"`
	Usage       []usageSnap   `json:"usage"`
	Adjustments []adjLine     `json:"adjustments,omitempty"`
}

// closedRef says a report read a closed month.
type closedRef struct {
	Month          string    `json:"month"`
	ClosedAt       time.Time `json:"closed_at"`
	Checksum       string    `json:"checksum"`
	MethodVersion  int       `json:"method_version"`
	GatewayVersion string    `json:"gateway_version"`
}

// billingView is the cells and usage of a period, closed months read from
// their snapshot and the others computed live.
type billingView struct {
	cells  []fluidsCell
	usage  []usageSnap
	closed []closedRef
	snaps  map[string]*closedPayload
	spans  [][2]time.Time
}

func monthKey(t time.Time) string { return t.UTC().Format("2006-01") }

func wholeMonth(sp [2]time.Time) bool {
	ms := time.Date(sp[0].Year(), sp[0].Month(), 1, 0, 0, 0, 0, time.UTC)
	return sp[0].Equal(ms) && sp[1].Equal(ms.AddDate(0, 1, 0))
}

func (s *Server) billingStore() (store.BillingStore, bool) {
	bs, ok := s.st.(store.BillingStore)
	return bs, ok
}

// snapshot loads a closed month (nil when open).
func (s *Server) snapshotOf(ctx context.Context, month string) (*closedPayload, *store.ClosedMonth, error) {
	bs, ok := s.billingStore()
	if !ok {
		return nil, nil, nil
	}
	c, found, err := bs.ClosedMonth(ctx, month)
	if err != nil || !found {
		return nil, nil, err
	}
	var p closedPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		return nil, nil, fmt.Errorf("closed month %s: its snapshot cannot be read: %w", month, err)
	}
	return &p, &c, nil
}

func wantsStorage(f *config.Fluids) bool {
	if f.PricePerGBMonth > 0 {
		return true
	}
	for _, p := range f.Periods {
		if p.PricePerGBMonth > 0 {
			return true
		}
	}
	for _, c := range f.FixedCosts {
		if c.Basis == config.BasisStorageGBMonth {
			return true
		}
	}
	return false
}

// addMetered adds the network and storage cells of [from, to).
func (s *Server) addMetered(ctx context.Context, cells []fluidsCell, from, to time.Time) ([]fluidsCell, error) {
	ms, ok := s.st.(store.MeterStore)
	if !ok {
		return cells, nil
	}
	nc, err := ms.NetworkBetween(ctx, from, to)
	if err != nil {
		return nil, err
	}
	for _, n := range nc {
		cells = append(cells, fluidsCell{Day: n.Day, Consumer: energy.Consumer{Org: n.Org, Lab: n.Lab, Team: n.Team, Project: n.Project, User: n.User, Model: n.Model, Pool: n.Pool},
			NetIn: n.In, NetOut: n.Out})
	}
	if wantsStorage(s.cfg.Fluids) {
		sc, err := ms.StorageBetween(ctx, from, to)
		if err != nil {
			return nil, err
		}
		for _, c := range sc {
			cells = append(cells, fluidsCell{Day: c.Day, Consumer: energy.Consumer{Org: c.Org}, StorageByteDays: c.Bytes})
		}
	}
	return cells, nil
}

// liveBase is the energy, network and storage cells of [from, to), before the
// fixed costs.
func (s *Server) liveBase(ctx context.Context, es store.EnergyStore, from, to time.Time) ([]fluidsCell, error) {
	cells, _, err := s.allocatedEnergyBasis(ctx, es, from, to)
	if err != nil {
		return nil, err
	}
	return s.addMetered(ctx, cells, from, to)
}

// fixedEnvFor wires the allocation of the fixed costs to the stores. The
// first error of a lookup is left in *errp.
func (s *Server) fixedEnvFor(ctx context.Context, es store.EnergyStore, errp *error) fixedEnv {
	cache := map[string][]fluidsCell{}
	fail := func(err error) {
		if err != nil && *errp == nil {
			*errp = err
		}
	}
	return fixedEnv{
		sharesOf: s.poolShares,
		shareOf:  s.cfg.PoolShare,
		monthCells: func(month string) []fluidsCell {
			if c, ok := cache[month]; ok {
				return c
			}
			t, err := ParseBillingMonth(month)
			if err != nil {
				fail(err)
				return nil
			}
			c, err := s.liveBase(ctx, es, t, t.AddDate(0, 1, 0))
			fail(err)
			cache[month] = c
			return c
		},
		lent: func(pool string, lo, hi time.Time) map[string]map[string]float64 {
			ls, ok := s.st.(store.LendingStore)
			if !ok {
				return nil
			}
			res, err := s.lendingCtx(ctx, lo, hi, ls)
			if err != nil {
				fail(err)
				return nil
			}
			if pool != "" {
				for _, p := range res.Pools {
					if p.Pool == pool {
						return p.Matrix
					}
				}
			}
			return res.Matrix
		},
	}
}

// liveCells computes the cells of [from, to) from the stores, fixed costs
// included, whatever has been closed.
func (s *Server) liveCells(ctx context.Context, es store.EnergyStore, from, to time.Time) ([]fluidsCell, error) {
	cells, err := s.liveBase(ctx, es, from, to)
	if err != nil {
		return nil, err
	}
	var envErr error
	cells = allocateFixedCosts(cells, s.cfg.Fluids, from, to, s.fixedEnvFor(ctx, es, &envErr))
	return cells, envErr
}

var usageDims = []string{"org", "lab", "team", "project", "user", "model", "pool", "day"}

func (s *Server) liveUsage(ctx context.Context, from, to time.Time) ([]usageSnap, error) {
	rows, err := s.st.Report(ctx, from, to, usageDims)
	if err != nil {
		return nil, err
	}
	out := make([]usageSnap, len(rows))
	for i, r := range rows {
		out[i] = usageSnap{r.Org, r.Lab, r.Team, r.Project, r.User, r.Model, r.Pool, r.Day, r.Unit, r.Cost}
	}
	return out, nil
}

// billing reads a period, month by month: a whole closed month from its
// snapshot, anything else computed live. Every organisation is in it; the
// caller filters what a principal may see.
func (s *Server) billing(ctx context.Context, from, to time.Time) (*billingView, error) {
	v := &billingView{snaps: map[string]*closedPayload{}, spans: monthSpans(from, to)}
	es, haveEnergy := s.st.(store.EnergyStore)
	for _, sp := range v.spans {
		month := monthKey(sp[0])
		if wholeMonth(sp) {
			snap, c, err := s.snapshotOf(ctx, month)
			if err != nil {
				return nil, err
			}
			if snap != nil {
				v.snaps[month] = snap
				v.closed = append(v.closed, closedRef{month, c.ClosedAt, c.Checksum, snap.Method.MethodVersion, snap.Method.GatewayVersion})
				terms := snap.Method.terms()
				for _, c := range snap.Cells {
					v.cells = append(v.cells, c.cell(terms))
				}
				v.usage = append(v.usage, snap.Usage...)
				continue
			}
		}
		if haveEnergy && s.cfg.Fluids != nil {
			cells, err := s.liveCells(ctx, es, sp[0], sp[1])
			if err != nil {
				return nil, err
			}
			v.cells = append(v.cells, cells...)
		}
		u, err := s.liveUsage(ctx, sp[0], sp[1])
		if err != nil {
			return nil, err
		}
		v.usage = append(v.usage, u...)
	}
	return v, nil
}

// ---- adjustments ----

type adjKey struct{ org, cc string }

// moneyLines prices cells and sums them per organisation and cost centre, in
// minor units of the fluids currency.
func (s *Server) moneyLines(cells []fluidsCell) map[adjKey]adjLine {
	f := s.cfg.Fluids
	type acc struct {
		fluids, network, storage, markup float64
		fixed, credit                    int64
	}
	sums := map[adjKey]*acc{}
	for _, c := range cells {
		k := adjKey{c.Org, s.costCenterOf(c.Org, c.Lab, c.Team, c.Project)}
		a := sums[k]
		if a == nil {
			a = &acc{}
			sums[k] = a
		}
		m := priceCell(c, f, s.cfg.MarkupOf)
		a.fluids += m.fluids
		a.network += m.network
		a.storage += m.storage
		a.markup += m.markup
		a.fixed += c.FixedMinor
		a.credit += c.CreditMinor
	}
	dec := f.CurrencyDecimals
	out := make(map[adjKey]adjLine, len(sums))
	for k, a := range sums {
		out[k] = adjLine{Org: k.org, CostCenter: k.cc, Fluids: minorUnits(a.fluids, dec), Fixed: a.fixed, Network: minorUnits(a.network, dec),
			Storage: minorUnits(a.storage, dec), Markup: minorUnits(a.markup, dec), Credit: a.credit}
	}
	return out
}

func (s *Server) costCenterOf(org, lab, team, project string) string {
	if org == Unallocated {
		return CostCenterUnallocated
	}
	if cc := s.cfg.CostCenterOf(org, lab, team, project); cc != "" {
		return cc
	}
	return CostCenterUnmapped
}

// adjustmentsFor returns the adjustments booked on a month: for each closed
// month whose next open month it is, what the live computation gives beyond
// what was billed and beyond what later closed months already booked.
func (s *Server) adjustmentsFor(ctx context.Context, target string) ([]adjLine, error) {
	bs, ok := s.billingStore()
	es, haveEnergy := s.st.(store.EnergyStore)
	if !ok || !haveEnergy || s.cfg.Fluids == nil {
		return nil, nil
	}
	months, err := bs.ClosedMonths(ctx)
	if err != nil {
		return nil, err
	}
	closed := map[string]bool{}
	for _, m := range months {
		closed[m] = true
	}
	snaps := map[string]*closedPayload{}
	load := func(m string) (*closedPayload, error) {
		if p := snaps[m]; p != nil {
			return p, nil
		}
		p, _, err := s.snapshotOf(ctx, m)
		if p != nil {
			snaps[m] = p
		}
		return p, err
	}
	var out []adjLine
	for _, m := range months {
		t, err := ParseBillingMonth(m)
		if err != nil {
			continue
		}
		n := t.AddDate(0, 1, 0)
		for closed[monthKey(n)] {
			n = n.AddDate(0, 1, 0)
		}
		if monthKey(n) != target {
			continue
		}
		snap, err := load(m)
		if err != nil || snap == nil {
			return nil, err
		}
		cells := make([]fluidsCell, len(snap.Cells))
		terms := snap.Method.terms()
		for i, c := range snap.Cells {
			cells[i] = c.cell(terms)
		}
		was := s.moneyLines(cells)
		liveCells, err := s.liveCells(ctx, es, t, t.AddDate(0, 1, 0))
		if err != nil {
			return nil, err
		}
		now := s.moneyLines(liveCells)
		booked := map[adjKey]adjLine{}
		for _, k := range months {
			if k <= m {
				continue
			}
			p, err := load(k)
			if err != nil || p == nil {
				return nil, err
			}
			for _, a := range p.Adjustments {
				if a.FromMonth == m {
					b := booked[adjKey{a.Org, a.CostCenter}]
					b.Fluids += a.Fluids
					b.Fixed += a.Fixed
					b.Network += a.Network
					b.Storage += a.Storage
					b.Markup += a.Markup
					b.Credit += a.Credit
					booked[adjKey{a.Org, a.CostCenter}] = b
				}
			}
		}
		keys := map[adjKey]bool{}
		for k := range was {
			keys[k] = true
		}
		for k := range now {
			keys[k] = true
		}
		for k := range booked {
			keys[k] = true
		}
		var lines []adjLine
		for k := range keys {
			a, b, bk := now[k], was[k], booked[k]
			d := adjLine{FromMonth: m, Org: k.org, CostCenter: k.cc,
				Fluids: a.Fluids - b.Fluids - bk.Fluids, Fixed: a.Fixed - b.Fixed - bk.Fixed, Network: a.Network - b.Network - bk.Network,
				Storage: a.Storage - b.Storage - bk.Storage, Markup: a.Markup - b.Markup - bk.Markup, Credit: a.Credit - b.Credit - bk.Credit}
			if !d.zero() {
				lines = append(lines, d)
			}
		}
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].Org != lines[j].Org {
				return lines[i].Org < lines[j].Org
			}
			return lines[i].CostCenter < lines[j].CostCenter
		})
		out = append(out, lines...)
	}
	return out, nil
}

// ---- closing a month ----

// billingRoutes registers the closed-month routes.
func (s *Server) billingRoutes(h func(string, adminHandler)) {
	h("POST /admin/billing/close", s.billingClose)
	h("GET /admin/billing/closed", s.billingClosed)
}

// billingClose answers POST /admin/billing/close?month=YYYY-MM: a proxy admin
// freezes a past month. The current and the future months are refused, and a
// closed month cannot be closed again or reopened.
func (s *Server) billingClose(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.proxyAdmin() {
		forbidden(w)
		return
	}
	bs, ok := s.billingStore()
	_, haveEnergy := s.st.(store.EnergyStore)
	if !ok || !haveEnergy || s.cfg.Fluids == nil {
		apiError(w, http.StatusNotFound, "not_found", "closing months needs the fluids block and the PostgreSQL store")
		return
	}
	t, err := ParseBillingMonth(r.URL.Query().Get("month"))
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	nowM := s.now().UTC()
	if !t.Before(time.Date(nowM.Year(), nowM.Month(), 1, 0, 0, 0, 0, time.UTC)) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "only a month that has ended can be closed; "+monthKey(t)+" is the current or a future month")
		return
	}
	release, ok := heavy(w)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	month := monthKey(t)
	if snap, _, err := s.snapshotOf(ctx, month); err != nil {
		storeError(w, err)
		return
	} else if snap != nil {
		apiError(w, http.StatusConflict, "invalid_request_error", "the month "+month+" is already closed; a closed month cannot be reopened")
		return
	}
	es, _ := s.st.(store.EnergyStore)
	cells, err := s.liveCells(ctx, es, t, t.AddDate(0, 1, 0))
	if err != nil {
		storeError(w, err)
		return
	}
	usage, err := s.liveUsage(ctx, t, t.AddDate(0, 1, 0))
	if err != nil {
		storeError(w, err)
		return
	}
	adj, err := s.adjustmentsFor(ctx, month)
	if err != nil {
		storeError(w, err)
		return
	}
	payload := closedPayload{Method: s.billingMethod(), Usage: usage, Adjustments: adj}
	for _, c := range cells {
		payload.Cells = append(payload.Cells, toSnap(c))
	}
	body, err := json.Marshal(payload)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "server_error", "the snapshot cannot be written")
		return
	}
	sum := sha256.Sum256(body)
	c := store.ClosedMonth{Month: month, ClosedAt: s.now().UTC(), ClosedBy: p.name, Checksum: hex.EncodeToString(sum[:]), Payload: body}
	if err := bs.CloseMonth(ctx, c); err != nil {
		if errors.Is(err, store.ErrConflict) {
			apiError(w, http.StatusConflict, "invalid_request_error", "the month "+month+" is already closed; a closed month cannot be reopened")
			return
		}
		storeError(w, err)
		return
	}
	after, _ := json.Marshal(map[string]any{"month": month, "checksum": c.Checksum, "bytes": len(body), "cells": len(payload.Cells), "usage": len(payload.Usage),
		"adjustments": len(adj), "method_version": payload.Method.MethodVersion, "gateway_version": payload.Method.GatewayVersion})
	s.appendAudit(ctx, store.AuditEntry{Kind: "billing", Actor: p.name, Roles: rolesJSON(p), Action: "billing.close", Target: month,
		Before: "null", After: string(after), Status: http.StatusOK, RequestID: r.Header.Get("X-Request-Id")})
	writeJSON(w, http.StatusOK, map[string]any{"month": month, "closed_at": c.ClosedAt, "closed_by": c.ClosedBy, "checksum": c.Checksum,
		"cells": len(payload.Cells), "usage": len(payload.Usage), "adjustments": len(adj), "method": payload.Method})
}

// billingClosed answers GET /admin/billing/closed: the closed months, or with
// ?month= one snapshot (proxy roles and finance viewers only).
func (s *Server) billingClosed(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.reportView() {
		forbidden(w)
		return
	}
	bs, ok := s.billingStore()
	if !ok {
		apiError(w, http.StatusNotFound, "not_found", "closed months need the PostgreSQL store")
		return
	}
	if m := r.URL.Query().Get("month"); m != "" {
		if _, err := ParseBillingMonth(m); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		c, found, err := bs.ClosedMonth(r.Context(), m)
		if err != nil {
			storeError(w, err)
			return
		}
		if !found {
			apiError(w, http.StatusNotFound, "not_found", "the month "+m+" is open")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Llmflow-Checksum", c.Checksum)
		_, _ = w.Write(c.Payload)
		return
	}
	months, err := bs.ClosedMonths(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	out := []closedRef{}
	for _, m := range months {
		snap, c, err := s.snapshotOf(r.Context(), m)
		if err != nil || snap == nil {
			continue
		}
		out = append(out, closedRef{m, c.ClosedAt, c.Checksum, snap.Method.MethodVersion, snap.Method.GatewayVersion})
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": out})
}

// methodFor is the method as a reader may see it: the markups of the
// organisations it may read only (a markup is the commercial term of one
// partner's agreement), and the gateway version only for the proxy roles.
func (s *Server) methodFor(p *principal) billingMethod {
	m := s.billingMethod()
	if len(m.Markups) > 0 {
		kept := map[string]float64{}
		for o, v := range m.Markups {
			if p.reportView() || p.orgView(o) {
				kept[o] = v
			}
		}
		m.Markups = nil
		if len(kept) > 0 {
			m.Markups = kept
		}
	}
	if !p.proxyView() {
		m.GatewayVersion = ""
	}
	return m
}

// closedFor is the closed months a reader read, without the gateway version
// for a reader that is not a proxy role.
func closedFor(p *principal, in []closedRef) []closedRef {
	out := make([]closedRef, len(in))
	copy(out, in)
	if !p.proxyView() {
		for i := range out {
			out[i].GatewayVersion = ""
		}
	}
	return out
}

// stampMethod writes the method of a report in its headers (a CSV has no room
// for it in the body): the version, and the parameters as compact JSON, as the
// reader may see them (methodFor).
func (s *Server) stampMethod(w http.ResponseWriter, v *billingView, p *principal) {
	m := s.methodFor(p)
	b, _ := json.Marshal(m)
	w.Header().Set("X-Llmflow-Method-Version", fmt.Sprint(m.MethodVersion))
	if m.GatewayVersion != "" {
		w.Header().Set("X-Llmflow-Gateway-Version", m.GatewayVersion)
	}
	w.Header().Set("X-Llmflow-Method", string(b))
	if v != nil && len(v.closed) > 0 {
		cb, _ := json.Marshal(closedFor(p, v.closed))
		w.Header().Set("X-Llmflow-Closed-Months", string(cb))
	}
}
