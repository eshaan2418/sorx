// Package sim replays a multi-venue L2 capture and executes parent orders through the OMS with a
// routing strategy, simulating venue latency, taker fees and liquidity consumption.
//
// Time is the capture's receive clock (RecvNs). An action scheduled for time t (a parent
// arriving, a child reaching its venue, a retry) is processed before the first frame received
// after t, against the books as they stood at t.
//
// Fill model (taker IOC): a child that reaches its venue walks that venue's recorded book on
// its side up to its limit price. Liquidity it takes is remembered per (venue, side, price) and
// not offered again until the feed changes that level's quantity (the real market would have
// removed it; the recording cannot know). There is no market impact beyond that, and passive
// orders are not modelled.
package sim

import (
	"container/heap"
	"fmt"
	"math"
	"sort"

	"github.com/eshaan2418/sorx/internal/mm/book"
	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/pipeline"
	"github.com/eshaan2418/sorx/internal/mm/venue"
	"github.com/eshaan2418/sorx/internal/oms"
)

// VenueParams are execution assumptions for one venue.
type VenueParams struct {
	TakerFeeBps float64 `json:"taker_fee_bps"`
	LatencyMs   float64 `json:"latency_ms"` // decision to arrival at the matching engine
}

// Strategy names.
const (
	StratSingle   = "single" // whole order to one venue (Config.Venue)
	StratBest     = "best"   // whole order to the venue with the best fee-adjusted touch
	StratSOR      = "sor"    // sweep the fee-adjusted consolidated book, one IOC per venue
	StratTWAPSOR  = "twap"   // Slices equal slices over the horizon, each routed with sor
	defaultRetry  = 1e9
	defaultCollar = 50
)

// Config describes one simulation run.
type Config struct {
	Strategy  string
	Venue     model.Venue // for StratSingle
	Venues    map[model.Venue]VenueParams
	Slices    int     // for StratTWAPSOR
	HorizonNs int64   // parents expire after this
	RetryNs   int64   // re-route unfilled leaves after this (aggressive strategies)
	CollarBps float64 // child limit at most this far through the arrival mid
	Parents   []ParentSpec
}

// ParentSpec is a client order arriving at TS.
type ParentSpec struct {
	TS     int64       `json:"ts"`
	Symbol string      `json:"symbol"`
	Side   oms.Side    `json:"side"`
	Qty    model.Fixed `json:"qty"`
}

// ParentResult is the TCA of one parent order.
type ParentResult struct {
	Spec        ParentSpec `json:"spec"`
	ArrivalMid  float64    `json:"arrival_mid"`
	ArrivalBook float64    `json:"arrival_book_cost_bps"` // cost to sweep the consolidated book at decision time, no fees
	AvgPx       float64    `json:"avg_px"`
	FilledFrac  float64    `json:"filled_frac"`
	ShortfallBp float64    `json:"shortfall_bps"` // side x (avg - arrival mid) / mid, filled part, before fees
	FeesBps     float64    `json:"fees_bps"`
	Children    int        `json:"children"`
	Venues      int        `json:"venues_used"`
	CompleteMs  float64    `json:"complete_ms"` // to last fill
	State       string     `json:"state"`
	Rejects     int        `json:"child_rejects"`
	EndMid      float64    `json:"end_mid"`      // consolidated mid at the horizon (or last fill)
	TotalISBps  float64    `json:"total_is_bps"` // filled cost incl. fees + unfilled marked to EndMid
}

// Result of a run.
type Result struct {
	Strategy string         `json:"strategy"`
	Parents  []ParentResult `json:"parents"`
	Frames   uint64         `json:"frames"`
	Children int            `json:"children"`
	Fills    int            `json:"fills"`
	OMSCheck string         `json:"oms_invariants"`
}

type actKind uint8

const (
	actParent actKind = iota
	actArrive
	actRetry
	actSlice
	actExpire
)

type action struct {
	t     int64
	seq   uint64
	kind  actKind
	pid   oms.ID
	child oms.ID
	spec  int // index into Parents (actParent)
	slice int
}

type actHeap []action

func (h actHeap) Len() int { return len(h) }
func (h actHeap) Less(i, j int) bool {
	if h[i].t != h[j].t {
		return h[i].t < h[j].t
	}
	return h[i].seq < h[j].seq
}
func (h actHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *actHeap) Push(x any)   { *h = append(*h, x.(action)) }
func (h *actHeap) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

type consKey struct {
	v    model.Venue
	sym  string
	side model.Side
	px   model.Fixed
}

type cons struct{ feedQty, used model.Fixed }

type parentState struct {
	spec    ParentSpec
	res     *ParentResult
	venues  map[model.Venue]bool
	lastTS  int64
	sliceQ  model.Fixed
	expired bool
}

// Sim is one run. Not safe for concurrent use.
type Sim struct {
	cfg     Config
	eng     *pipeline.Engine
	m       *oms.OMS
	acts    actHeap
	seq     uint64
	used    map[consKey]*cons
	parents map[oms.ID]*parentState
	res     Result
	now     int64
	vorder  []model.Venue
}

// New creates a simulation over symbols.
func New(t *venue.SymbolTable, cfg Config) *Sim {
	if cfg.RetryNs == 0 {
		cfg.RetryNs = defaultRetry
	}
	if cfg.CollarBps == 0 {
		cfg.CollarBps = defaultCollar
	}
	s := &Sim{cfg: cfg, eng: pipeline.New(t, pipeline.Options{}), m: oms.New(oms.Limits{CollarBps: cfg.CollarBps + 1}),
		used: map[consKey]*cons{}, parents: map[oms.ID]*parentState{}}
	s.res.Strategy = cfg.Strategy
	for i, p := range cfg.Parents {
		s.push(action{t: p.TS, kind: actParent, spec: i})
	}
	return s
}

func (s *Sim) push(a action) {
	s.seq++
	a.seq = s.seq
	heap.Push(&s.acts, a)
}

// Handle advances to frame f: due actions first, then the frame updates the books.
func (s *Sim) Handle(f *venue.Frame) {
	s.runUntil(f.RecvNs)
	s.eng.Handle(f)
	s.res.Frames++
}

func (s *Sim) runUntil(t int64) {
	for len(s.acts) > 0 && s.acts[0].t <= t {
		a := heap.Pop(&s.acts).(action)
		s.now = a.t
		switch a.kind {
		case actParent:
			s.onParent(a)
		case actArrive:
			s.onArrive(a)
		case actRetry, actSlice:
			s.route(a.pid, a.kind == actSlice)
		case actExpire:
			s.onExpire(a)
		}
	}
}

// Finish expires whatever is still open and returns the result.
func (s *Sim) Finish() Result {
	s.runUntil(math.MaxInt64)
	if err := s.m.Check(); err != nil {
		s.res.OMSCheck = err.Error()
	} else {
		s.res.OMSCheck = "ok"
	}
	ids := make([]oms.ID, 0, len(s.parents))
	for id := range s.parents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		ps := s.parents[id]
		p := s.m.Get(id)
		r := ps.res
		r.State = p.State.String()
		r.FilledFrac = p.CumQty.Float64() / p.Qty.Float64()
		r.Children = len(p.Kids)
		r.Venues = len(ps.venues)
		if p.CumQty > 0 {
			r.AvgPx = p.AvgPx()
			r.ShortfallBp = float64(p.Side) * (r.AvgPx/r.ArrivalMid - 1) * 1e4
			r.FeesBps = p.Fees / p.Notional * 1e4
			r.CompleteMs = float64(ps.lastTS-ps.spec.TS) / 1e6
		}
		// Implementation shortfall of the whole order: filled part at its cost plus fees, unfilled
		// part marked at the horizon mid (opportunity cost). Fully filled orders have no unfilled part.
		r.TotalISBps = r.FilledFrac * (r.ShortfallBp + r.FeesBps)
		if r.FilledFrac < 1 && r.EndMid > 0 && r.ArrivalMid > 0 {
			r.TotalISBps += (1 - r.FilledFrac) * float64(p.Side) * (r.EndMid/r.ArrivalMid - 1) * 1e4
		}
		s.res.Parents = append(s.res.Parents, *r)
	}
	return s.res
}

func (s *Sim) books(sym string) map[model.Venue]*book.Book {
	out := map[model.Venue]*book.Book{}
	for v := range s.cfg.Venues {
		if b, live := s.eng.Book(v, sym); live && b != nil && len(b.Bids) > 0 && len(b.Asks) > 0 && !b.Crossed() {
			out[v] = b
		}
	}
	return out
}

// Mid returns the consolidated mid: the midpoint of the best bid and best ask across live
// venues. Fragmented markets are often crossed across venues by less than the round-trip taker
// fee (not arbitrageable); the midpoint is still the natural arrival benchmark then.
func (s *Sim) Mid(sym string) (float64, bool) {
	var bb, ba model.Fixed
	for _, b := range s.books(sym) {
		if bb == 0 || b.Bids[0].Price > bb {
			bb = b.Bids[0].Price
		}
		if ba == 0 || b.Asks[0].Price < ba {
			ba = b.Asks[0].Price
		}
	}
	if bb == 0 || ba == 0 {
		return 0, false
	}
	return (bb.Float64() + ba.Float64()) / 2, true
}

func (s *Sim) onParent(a action) {
	spec := s.cfg.Parents[a.spec]
	mid, ok := s.Mid(spec.Symbol)
	p := s.m.NewParent(s.now, spec.Symbol, spec.Side, spec.Qty, 0, mid)
	ps := &parentState{spec: spec, res: &ParentResult{Spec: spec, ArrivalMid: mid}, venues: map[model.Venue]bool{}}
	s.parents[p.ID] = ps
	if !ok {
		_ = s.m.Expire(s.now, p.ID, "no_market")
		return
	}
	ps.res.ArrivalBook = s.sweepCost(spec, mid)
	s.push(action{t: s.now + s.cfg.HorizonNs, kind: actExpire, pid: p.ID})
	if s.cfg.Strategy == StratTWAPSOR && s.cfg.Slices > 1 {
		ps.sliceQ = spec.Qty / model.Fixed(s.cfg.Slices)
		for i := 1; i < s.cfg.Slices; i++ {
			s.push(action{t: s.now + int64(i)*s.cfg.HorizonNs/int64(s.cfg.Slices), kind: actSlice, pid: p.ID, slice: i})
		}
	}
	s.route(p.ID, true)
}

// level is a candidate liquidity level.
type level struct {
	v     model.Venue
	px    model.Fixed
	qty   model.Fixed
	effPx float64 // fee-adjusted
}

func (s *Sim) available(v model.Venue, sym string, side model.Side, l model.Level) model.Fixed {
	c := s.used[consKey{v, sym, side, l.Price}]
	if c == nil || c.feedQty != l.Qty {
		return l.Qty
	}
	return max(0, l.Qty-c.used)
}

// candidates lists the levels a strategy may take, best fee-adjusted price first.
// walk visits liquidity best fee-adjusted price first, stopping when fn returns false. Each
// venue's side is already sorted best-first and its fee is constant, so a k-way merge of the
// venue ladders yields the global order while touching only the levels actually consumed
// (candidatesAll, the collect-and-sort version, is kept as the test oracle).
func (s *Sim) walk(sym string, side oms.Side, only model.Venue, limit float64, fn func(level) bool) {
	type cursor struct {
		v   model.Venue
		lv  []model.Level
		bs  model.Side
		fee float64
		i   int
	}
	var cs []cursor
	for _, v := range s.venueOrder() {
		if only != 0 && v != only {
			continue
		}
		b, ok := s.liveBook(v, sym)
		if !ok {
			continue
		}
		c := cursor{v: v, lv: b.Asks, bs: model.Ask, fee: s.cfg.Venues[v].TakerFeeBps / 1e4}
		if side == oms.Sell {
			c.lv, c.bs = b.Bids, model.Bid
		}
		cs = append(cs, c)
	}
	sgn := float64(side)
	for {
		best := -1
		var bestEff float64
		for k := range cs {
			c := &cs[k]
			// Skip exhausted (fully consumed) levels at the head of this venue.
			for c.i < len(c.lv) && s.available(c.v, sym, c.bs, c.lv[c.i]) == 0 {
				c.i++
			}
			if c.i >= len(c.lv) {
				continue
			}
			px := c.lv[c.i].Price.Float64()
			if (side == oms.Buy && px > limit) || (side == oms.Sell && px < limit) {
				c.i = len(c.lv)
				continue
			}
			eff := px * (1 + sgn*c.fee)
			if best < 0 || (side == oms.Buy && eff < bestEff) || (side == oms.Sell && eff > bestEff) {
				best, bestEff = k, eff // ties keep the lower venue id (venueOrder is sorted)
			}
		}
		if best < 0 {
			return
		}
		c := &cs[best]
		l := c.lv[c.i]
		c.i++
		if !fn(level{c.v, l.Price, s.available(c.v, sym, c.bs, l), bestEff}) {
			return
		}
	}
}

func (s *Sim) venueOrder() []model.Venue {
	if s.vorder == nil {
		for v := range s.cfg.Venues {
			s.vorder = append(s.vorder, v)
		}
		sort.Slice(s.vorder, func(i, j int) bool { return s.vorder[i] < s.vorder[j] })
	}
	return s.vorder
}

func (s *Sim) liveBook(v model.Venue, sym string) (*book.Book, bool) {
	b, live := s.eng.Book(v, sym)
	if !live || b == nil || len(b.Bids) == 0 || len(b.Asks) == 0 || b.Crossed() {
		return nil, false
	}
	return b, true
}

// candidates collects levels via walk until qty is covered (0 = all within the limit).
func (s *Sim) candidates(sym string, side oms.Side, only model.Venue, limit float64, qty model.Fixed) []level {
	var out []level
	var got model.Fixed
	s.walk(sym, side, only, limit, func(l level) bool {
		out = append(out, l)
		got += l.qty
		return qty == 0 || got < qty
	})
	return out
}

// candidatesAll lists every level a strategy may take, best fee-adjusted price first.
func (s *Sim) candidatesAll(sym string, side oms.Side, only model.Venue, limit float64) []level {
	var out []level
	bs := s.books(sym)
	for v, b := range bs {
		if only != 0 && v != only {
			continue
		}
		fee := s.cfg.Venues[v].TakerFeeBps / 1e4
		lv, bs := b.Asks, model.Ask
		if side == oms.Sell {
			lv, bs = b.Bids, model.Bid
		}
		for _, l := range lv {
			px := l.Price.Float64()
			if (side == oms.Buy && px > limit) || (side == oms.Sell && px < limit) {
				break
			}
			if q := s.available(v, sym, bs, l); q > 0 {
				eff := px * (1 + float64(side)*fee)
				out = append(out, level{v, l.Price, q, eff})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].effPx != out[j].effPx {
			if side == oms.Buy {
				return out[i].effPx < out[j].effPx
			}
			return out[i].effPx > out[j].effPx
		}
		return out[i].v < out[j].v
	})
	return out
}

// sweepCost is the cost (bps vs mid, no fees) of taking the whole parent from the consolidated
// book at decision time: the market's own estimate of what the order costs.
func (s *Sim) sweepCost(spec ParentSpec, mid float64) float64 {
	lim := mid * (1 + float64(spec.Side)*s.cfg.CollarBps/1e4)
	left := spec.Qty
	var n, q float64
	for _, l := range s.candidates(spec.Symbol, spec.Side, 0, lim, spec.Qty) {
		t := min(left, l.qty)
		n += t.Float64() * l.px.Float64()
		q += t.Float64()
		left -= t
		if left == 0 {
			break
		}
	}
	if q == 0 {
		return math.NaN()
	}
	return float64(spec.Side) * (n/q/mid - 1) * 1e4
}

// route sends child IOCs for (part of) a parent's leaves according to the strategy.
func (s *Sim) route(pid oms.ID, scheduled bool) {
	ps := s.parents[pid]
	p := s.m.Get(pid)
	if p.State.Terminal() || ps.expired {
		return
	}
	want := p.Leaves()
	if s.cfg.Strategy == StratTWAPSOR && s.cfg.Slices > 1 {
		// Release slices on schedule: after slice k, at most (k+1)/n of the parent may be done.
		k := int((s.now - ps.spec.TS) * int64(s.cfg.Slices) / s.cfg.HorizonNs)
		allowed := model.Fixed(min(k+1, s.cfg.Slices)) * ps.sliceQ
		if k+1 >= s.cfg.Slices {
			allowed = p.Qty
		}
		want = min(want, allowed-p.CumQty-p.Working)
	}
	if want <= 0 {
		return
	}
	if _, ok := s.Mid(p.Symbol); !ok {
		s.push(action{t: s.now + s.cfg.RetryNs, kind: actRetry, pid: pid})
		return
	}
	ref := ps.res.ArrivalMid
	lim := ref * (1 + float64(p.Side)*s.cfg.CollarBps/1e4)
	var only model.Venue
	switch s.cfg.Strategy {
	case StratSingle:
		only = s.cfg.Venue
	case StratBest:
		c := s.candidates(p.Symbol, p.Side, 0, lim, 1)
		if len(c) == 0 {
			s.push(action{t: s.now + s.cfg.RetryNs, kind: actRetry, pid: pid})
			return
		}
		only = c[0].v
	}
	alloc := map[model.Venue]model.Fixed{}
	worst := map[model.Venue]model.Fixed{}
	left := want
	for _, l := range s.candidates(p.Symbol, p.Side, only, lim, want) {
		t := min(left, l.qty)
		alloc[l.v] += t
		worst[l.v] = l.px
		left -= t
		if left == 0 {
			break
		}
	}
	vs := make([]model.Venue, 0, len(alloc))
	for v := range alloc {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	for _, v := range vs {
		c, err := s.m.NewChild(s.now, pid, v, alloc[v], worst[v], ref)
		if err != nil {
			panic(fmt.Sprintf("route: %v", err)) // a routing bug, never market behaviour
		}
		s.res.Children++
		if c.State == oms.Rejected {
			ps.res.Rejects++
			continue
		}
		ps.venues[v] = true
		lat := int64(s.cfg.Venues[v].LatencyMs * 1e6)
		s.push(action{t: s.now + lat, kind: actArrive, pid: pid, child: c.ID})
	}
	// Whatever could not be placed (or will not fill) is retried while the horizon lasts.
	s.push(action{t: s.now + s.cfg.RetryNs, kind: actRetry, pid: pid})
}

// onArrive executes a child IOC against its venue's current book.
func (s *Sim) onArrive(a action) {
	c := s.m.Get(a.child)
	ps := s.parents[a.pid]
	b, live := s.eng.Book(c.Venue, c.Symbol)
	if live && b != nil {
		lv, bs := b.Asks, model.Ask
		if c.Side == oms.Sell {
			lv, bs = b.Bids, model.Bid
		}
		fee := s.cfg.Venues[c.Venue].TakerFeeBps / 1e4
		for _, l := range lv {
			if c.CumQty == c.Qty || (c.Side == oms.Buy && l.Price > c.Limit) || (c.Side == oms.Sell && l.Price < c.Limit) {
				break
			}
			avail := s.available(c.Venue, c.Symbol, bs, l)
			t := min(avail, c.Qty-c.CumQty)
			if t <= 0 {
				continue
			}
			if err := s.m.Fill(s.now, c.ID, t, l.Price, t.Float64()*l.Price.Float64()*fee); err != nil {
				panic(fmt.Sprintf("fill: %v", err))
			}
			s.res.Fills++
			k := consKey{c.Venue, c.Symbol, bs, l.Price}
			u := s.used[k]
			if u == nil || u.feedQty != l.Qty {
				u = &cons{feedQty: l.Qty}
				s.used[k] = u
			}
			u.used += t
			ps.lastTS = s.now
		}
	}
	if !c.State.Terminal() {
		_ = s.m.Done(s.now, c.ID, "ioc_remainder")
	}
}

func (s *Sim) onExpire(a action) {
	ps := s.parents[a.pid]
	p := s.m.Get(a.pid)
	ps.expired = true
	if p.State.Terminal() {
		return
	}
	if p.Working > 0 {
		// Children still in flight: check again once they have arrived.
		s.push(action{t: s.now + 1e6, kind: actExpire, pid: a.pid})
		return
	}
	if m, ok := s.Mid(p.Symbol); ok {
		ps.res.EndMid = m
	}
	_ = s.m.Expire(s.now, a.pid, "horizon")
}

// OMS exposes the order store (tests).
func (s *Sim) OMS() *oms.OMS { return s.m }

// Plan returns the venue allocation the SOR would send for qty now (benchmarks, diagnostics).
func (s *Sim) Plan(sym string, side oms.Side, qty model.Fixed, limit float64) map[model.Venue]model.Fixed {
	alloc := map[model.Venue]model.Fixed{}
	left := qty
	for _, l := range s.candidates(sym, side, 0, limit, qty) {
		t := min(left, l.qty)
		alloc[l.v] += t
		left -= t
		if left == 0 {
			break
		}
	}
	return alloc
}

// Engine exposes the book engine (diagnostics).
func (s *Sim) Engine() *pipeline.Engine { return s.eng }

// PlanSorted is Plan using the collect-and-sort ladder (benchmark baseline for the merge walk).
func (s *Sim) PlanSorted(sym string, side oms.Side, qty model.Fixed, limit float64) map[model.Venue]model.Fixed {
	alloc := map[model.Venue]model.Fixed{}
	left := qty
	for _, l := range s.candidatesAll(sym, side, 0, limit) {
		t := min(left, l.qty)
		alloc[l.v] += t
		left -= t
		if left == 0 {
			break
		}
	}
	return alloc
}
