package sim

import (
	"fmt"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/venue"
	"github.com/eshaan2418/sorx/internal/oms"
)

const ms = int64(1e6)

func q(x float64) model.Fixed { return model.Fixed(math.Round(x * model.Scale)) }

func cbSnap(t int64, bids, asks [][2]float64) venue.Frame {
	return venue.Frame{Kind: venue.FrameWS, Venue: model.Coinbase, RecvNs: t,
		Payload: fmt.Appendf(nil, `{"type":"snapshot","product_id":"BTC-USD","bids":%s,"asks":%s}`, lv(bids), lv(asks))}
}

func cbUpdate(t int64, side string, px, qty float64) venue.Frame {
	return venue.Frame{Kind: venue.FrameWS, Venue: model.Coinbase, RecvNs: t,
		Payload: fmt.Appendf(nil, `{"type":"l2update","product_id":"BTC-USD","changes":[["%s","%v","%v"]]}`, side, px, qty)}
}

var synSeq uint64

func synSnap(t int64, bids, asks [][2]float64) venue.Frame {
	synSeq++
	return venue.Frame{Kind: venue.FrameWS, Venue: model.Synthetic, RecvNs: t,
		Payload: fmt.Appendf(nil, `{"t":"s","s":"BTC-USD","b":%s,"a":%s,"q":%d}`, lv(bids), lv(asks), synSeq)}
}

func lv(ls [][2]float64) string {
	s := "["
	for i, l := range ls {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`["%v","%v"]`, l[0], l[1])
	}
	return s + "]"
}

func table(t *testing.T) *venue.SymbolTable {
	tb, err := venue.NewSymbolTable([]string{"BTC-USD"})
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func venues(cbFee, synFee, cbLat, synLat float64) map[model.Venue]VenueParams {
	return map[model.Venue]VenueParams{
		model.Coinbase:  {TakerFeeBps: cbFee, LatencyMs: cbLat},
		model.Synthetic: {TakerFeeBps: synFee, LatencyMs: synLat},
	}
}

func runFrames(t *testing.T, cfg Config, frames []venue.Frame) Result {
	t.Helper()
	s := New(table(t), cfg)
	for i := range frames {
		s.Handle(&frames[i])
	}
	r := s.Finish()
	if r.OMSCheck != "ok" {
		t.Fatalf("OMS invariants: %s", r.OMSCheck)
	}
	return r
}

func book2() []venue.Frame {
	return []venue.Frame{
		cbSnap(1, [][2]float64{{99, 1}}, [][2]float64{{100, 1}, {101, 1}}),
		synSnap(2, [][2]float64{{99, 1}}, [][2]float64{{100.5, 1}, {102, 5}}),
	}
}

func TestSORSweepsConsolidatedBook(t *testing.T) {
	fr := append(book2(), cbUpdate(10*ms, "buy", 98, 1)) // a later frame to drive time forward
	r := runFrames(t, Config{Strategy: StratSOR, Venues: venues(0, 0, 0, 0), HorizonNs: 5 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(2.5)}}}, fr)
	p := r.Parents[0]
	if p.FilledFrac != 1 || p.Venues != 2 || math.Abs(p.AvgPx-100.4) > 1e-9 {
		t.Fatalf("%+v", p)
	}
	// Arrival mid 99.5: shortfall = (100.4/99.5 - 1) x 1e4.
	if math.Abs(p.ShortfallBp-(100.4/99.5-1)*1e4) > 1e-9 || math.Abs(p.ArrivalBook-p.ShortfallBp) > 1e-9 {
		t.Fatalf("shortfall %g arrival book %g", p.ShortfallBp, p.ArrivalBook)
	}
}

func TestSingleVenueWalksItsOwnBook(t *testing.T) {
	fr := append(book2(), cbUpdate(10*ms, "buy", 98, 1))
	r := runFrames(t, Config{Strategy: StratSingle, Venue: model.Coinbase, Venues: venues(0, 0, 0, 0), HorizonNs: 5 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(2.5)}}}, fr)
	p := r.Parents[0]
	// Only 2 BTC on Coinbase within the collar; the remainder is retried and expires.
	if math.Abs(p.FilledFrac-0.8) > 1e-12 || p.Venues != 1 || math.Abs(p.AvgPx-100.5) > 1e-9 || p.State != "expired" {
		t.Fatalf("%+v", p)
	}
}

func TestFeesChangeTheRoute(t *testing.T) {
	fr := append(book2(), cbUpdate(10*ms, "buy", 98, 1))
	// Synthetic venue charges 100 bps: 100.5 costs 101.505 effective, worse than Coinbase's 101.
	r := runFrames(t, Config{Strategy: StratSOR, Venues: venues(0, 100, 0, 0), HorizonNs: 5 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(2)}}}, fr)
	p := r.Parents[0]
	if p.Venues != 1 || math.Abs(p.AvgPx-100.5) > 1e-9 || p.FeesBps != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestConsumedLiquidityIsNotReused(t *testing.T) {
	fr := append(book2(), cbUpdate(10*ms, "buy", 98, 1))
	r := runFrames(t, Config{Strategy: StratSingle, Venue: model.Coinbase, Venues: venues(0, 0, 0, 0), HorizonNs: 5 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(1)}, {TS: 4, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(1)}}}, fr)
	if r.Parents[0].AvgPx != 100 || r.Parents[1].AvgPx != 101 {
		t.Fatalf("first %g second %g", r.Parents[0].AvgPx, r.Parents[1].AvgPx)
	}
	// Once the feed changes the level, it is offered again (someone replenished it).
	fr2 := append(book2(), cbUpdate(5, "sell", 100, 2), cbUpdate(10*ms, "buy", 98, 1))
	r2 := runFrames(t, Config{Strategy: StratSingle, Venue: model.Coinbase, Venues: venues(0, 0, 0, 0), HorizonNs: 5 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(1)}, {TS: 6, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(1)}}}, fr2)
	if r2.Parents[1].AvgPx != 100 {
		t.Fatalf("after refresh: %g", r2.Parents[1].AvgPx)
	}
}

func TestLatencyMeansTheBookMoves(t *testing.T) {
	// Decision at 3 ns sees 100; Coinbase's 100 level disappears at 2 ms; the child arrives at 5 ms.
	fr := append(book2(), cbUpdate(2*ms, "sell", 100, 0), cbUpdate(20*ms, "buy", 98, 1))
	r := runFrames(t, Config{Strategy: StratSingle, Venue: model.Coinbase, Venues: venues(0, 0, 5, 0), HorizonNs: 10 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 3, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(1)}}}, fr)
	p := r.Parents[0]
	// The child's limit was the decision-time price (100), so it misses; nothing fills.
	if p.FilledFrac != 0 || p.Children == 0 {
		t.Fatalf("%+v", p)
	}
}

func TestTWAPSlicesOverHorizon(t *testing.T) {
	var fr []venue.Frame
	fr = append(fr, book2()...)
	for i := int64(1); i <= 50; i++ { // keep refreshing liquidity
		fr = append(fr, cbSnap(i*100*ms, [][2]float64{{99, 1}}, [][2]float64{{100, 10}}))
	}
	r := runFrames(t, Config{Strategy: StratTWAPSOR, Slices: 4, Venues: venues(0, 0, 0, 0), HorizonNs: 4000 * ms, CollarBps: 500,
		Parents: []ParentSpec{{TS: 50 * ms, Symbol: "BTC-USD", Side: oms.Buy, Qty: q(4)}}}, fr)
	p := r.Parents[0]
	if p.FilledFrac != 1 || p.Children < 4 || p.CompleteMs < 2900 {
		t.Fatalf("%+v", p)
	}
}

// Random books, random parents, all strategies: the OMS invariants always hold, fills never
// exceed what was offered, and filled fractions are in [0, 1].
func TestRandomRunsKeepInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	var fr []venue.Frame
	mid := 100.0
	for i := int64(1); i <= 3000; i++ {
		mid *= 1 + 0.0005*r.NormFloat64()
		lv := func(sgn float64) [][2]float64 {
			var out [][2]float64
			for k := 0; k < 8; k++ {
				out = append(out, [2]float64{math.Round((mid+sgn*(0.05+0.1*float64(k)))*100) / 100, float64(1+r.IntN(30)) / 10})
			}
			return out
		}
		if r.IntN(2) == 0 {
			fr = append(fr, cbSnap(i*10*ms, lv(-1), lv(1)))
		} else {
			fr = append(fr, synSnap(i*10*ms, lv(-1), lv(1)))
		}
	}
	var ps []ParentSpec
	for i := 0; i < 200; i++ {
		s := oms.Buy
		if r.IntN(2) == 0 {
			s = oms.Sell
		}
		ps = append(ps, ParentSpec{TS: int64(50+r.IntN(29000)) * ms, Symbol: "BTC-USD", Side: s, Qty: q(float64(1+r.IntN(80)) / 10)})
	}
	for _, st := range []string{StratSingle, StratBest, StratSOR, StratTWAPSOR} {
		res := runFrames(t, Config{Strategy: st, Venue: model.Coinbase, Slices: 5, Venues: venues(5, 10, 3, 8), HorizonNs: 2000 * ms, Parents: ps}, fr)
		for _, p := range res.Parents {
			if p.FilledFrac < 0 || p.FilledFrac > 1+1e-12 {
				t.Fatalf("%s: %+v", st, p)
			}
		}
	}
}

// Same frames and parents => identical results (map iteration never leaks into routing).
func TestDeterministic(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	var fr []venue.Frame
	for i := int64(1); i <= 500; i++ {
		m := 100 + r.NormFloat64()
		fr = append(fr, cbSnap(i*10*ms, [][2]float64{{m - 0.05, 1}, {m - 0.1, 2}}, [][2]float64{{m + 0.05, 1}, {m + 0.1, 2}}))
		fr = append(fr, synSnap(i*10*ms+1, [][2]float64{{m - 0.04, 1}}, [][2]float64{{m + 0.06, 3}}))
	}
	var ps []ParentSpec
	for i := 0; i < 40; i++ {
		ps = append(ps, ParentSpec{TS: int64(100+100*i) * ms, Symbol: "BTC-USD", Side: oms.Side(1 - 2*(i%2)), Qty: q(1.5)})
	}
	cfg := Config{Strategy: StratSOR, Venues: venues(5, 7, 2, 4), HorizonNs: 50 * ms, Parents: ps}
	a, b := runFrames(t, cfg, fr), runFrames(t, cfg, fr)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatal("results differ between identical runs")
	}
}

// The merge walker returns the same ladder prefix as collect-and-sort on random books.
func TestWalkMatchesCollectAndSort(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 11))
	for trial := 0; trial < 300; trial++ {
		mid := 100 + r.NormFloat64()
		mk := func(sgn float64) [][2]float64 {
			var out [][2]float64
			for k := 0; k < 1+r.IntN(30); k++ {
				out = append(out, [2]float64{math.Round((mid+sgn*(0.01+0.01*float64(k+r.IntN(3))))*100) / 100, float64(1+r.IntN(40)) / 10})
			}
			return out
		}
		synSeq = 0
		fr := []venue.Frame{cbSnap(1, mk(-1), mk(1)), synSnap(2, mk(-1), mk(1))}
		s := New(table(t), Config{Strategy: StratSOR, Venues: venues(float64(r.IntN(20)), float64(r.IntN(20)), 0, 0)})
		for i := range fr {
			s.Handle(&fr[i])
		}
		for _, side := range []oms.Side{oms.Buy, oms.Sell} {
			lim := mid * (1 + float64(side)*0.05)
			all := s.candidatesAll("BTC-USD", side, 0, lim)
			got := s.candidates("BTC-USD", side, 0, lim, 0)
			if len(all) != len(got) {
				t.Fatalf("trial %d: %d vs %d levels", trial, len(got), len(all))
			}
			for i := range all {
				if all[i] != got[i] {
					t.Fatalf("trial %d side %d level %d: walk %+v sort %+v", trial, side, i, got[i], all[i])
				}
			}
		}
	}
}
