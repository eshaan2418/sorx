package main

import (
	"flag"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/eshaan2418/sorx/internal/mm/capture"
	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/pipeline"
	"github.com/eshaan2418/sorx/internal/mm/venue"
)

type venueStat struct {
	Venue        string  `json:"venue"`
	LiveFrac     float64 `json:"live_fraction"`
	SpreadBps    float64 `json:"median_spread_bps"`
	DepthBid10   float64 `json:"median_bid_depth_10bps"` // base units within 10 bps of the consolidated mid
	DepthAsk10   float64 `json:"median_ask_depth_10bps"`
	AtBestBid    float64 `json:"at_best_bid_frac"`
	AtBestAsk    float64 `json:"at_best_ask_frac"`
	AloneBestBid float64 `json:"alone_at_best_bid_frac"`
	AloneBestAsk float64 `json:"alone_at_best_ask_frac"`
}

// cmdReplay rebuilds the books from a capture and samples them once per second: venue spreads,
// depth near the mid, and how often each venue holds the consolidated best price.
func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	dir := fs.String("dir", "capture", "capture directory")
	symbols := fs.String("symbols", "BTC-USD,ETH-USD", "symbols")
	out := fs.String("out", "-", "result JSON")
	_ = fs.Parse(args)
	syms := strings.Split(*symbols, ",")
	table, err := venue.NewSymbolTable(syms)
	if err != nil {
		return err
	}
	frames, err := capture.ReadAll(*dir)
	if err != nil {
		return err
	}
	eng := pipeline.New(table, pipeline.Options{})
	vs := []model.Venue{model.Kraken, model.Coinbase, model.Bitstamp, model.Gemini}
	type acc struct {
		samples, live, bb, ba, aloneB, aloneA int
		spread, dB, dA                        []float64
	}
	accs := map[string]map[model.Venue]*acc{}
	for _, s := range syms {
		accs[s] = map[model.Venue]*acc{}
		for _, v := range vs {
			accs[s][v] = &acc{}
		}
	}
	type crossAcc struct {
		samples, n int
		bps        []float64
	}
	crossed := map[string]*crossAcc{}
	for _, s := range syms {
		crossed[s] = &crossAcc{}
	}
	var next int64
	t0 := time.Now()
	for i := range frames {
		f := &frames[i]
		eng.Handle(f)
		if f.RecvNs < next {
			continue
		}
		next = f.RecvNs + 1e9
		for _, s := range syms {
			var bb, ba model.Fixed
			for _, v := range vs {
				if b, live := eng.Book(v, s); live && len(b.Bids) > 0 && len(b.Asks) > 0 && !b.Crossed() {
					if bb == 0 || b.Bids[0].Price > bb {
						bb = b.Bids[0].Price
					}
					if ba == 0 || b.Asks[0].Price < ba {
						ba = b.Asks[0].Price
					}
				}
			}
			if bb == 0 || ba == 0 {
				continue
			}
			mid := (bb.Float64() + ba.Float64()) / 2
			cx := crossed[s]
			cx.samples++
			if ba <= bb {
				cx.n++
				cx.bps = append(cx.bps, (bb.Float64()/ba.Float64()-1)*1e4)
			}
			nb, na := 0, 0
			for _, v := range vs {
				if b, live := eng.Book(v, s); live && len(b.Bids) > 0 && len(b.Asks) > 0 && !b.Crossed() {
					if b.Bids[0].Price == bb {
						nb++
					}
					if b.Asks[0].Price == ba {
						na++
					}
				}
			}
			for _, v := range vs {
				a := accs[s][v]
				a.samples++
				b, live := eng.Book(v, s)
				if !live || len(b.Bids) == 0 || len(b.Asks) == 0 || b.Crossed() {
					continue
				}
				a.live++
				a.spread = append(a.spread, (b.Asks[0].Price.Float64()/b.Bids[0].Price.Float64()-1)*1e4)
				var db, da float64
				for _, l := range b.Bids {
					if l.Price.Float64() < mid*(1-10e-4) {
						break
					}
					db += l.Qty.Float64()
				}
				for _, l := range b.Asks {
					if l.Price.Float64() > mid*(1+10e-4) {
						break
					}
					da += l.Qty.Float64()
				}
				a.dB, a.dA = append(a.dB, db), append(a.dA, da)
				if b.Bids[0].Price == bb {
					a.bb++
					if nb == 1 {
						a.aloneB++
					}
				}
				if b.Asks[0].Price == ba {
					a.ba++
					if na == 1 {
						a.aloneA++
					}
				}
			}
		}
	}
	med := func(x []float64) float64 {
		if len(x) == 0 {
			return math.NaN()
		}
		sort.Float64s(x)
		return x[len(x)/2]
	}
	res := map[string][]venueStat{}
	for _, s := range syms {
		for _, v := range vs {
			a := accs[s][v]
			if a.samples == 0 {
				continue
			}
			n := float64(a.samples)
			res[s] = append(res[s], venueStat{Venue: v.String(), LiveFrac: float64(a.live) / n, SpreadBps: med(a.spread), DepthBid10: med(a.dB), DepthAsk10: med(a.dA),
				AtBestBid: float64(a.bb) / n, AtBestAsk: float64(a.ba) / n, AloneBestBid: float64(a.aloneB) / n, AloneBestAsk: float64(a.aloneA) / n})
		}
	}
	cross := map[string]any{}
	for _, s := range syms {
		c := crossed[s]
		p90 := math.NaN()
		if len(c.bps) > 0 {
			sort.Float64s(c.bps)
			p90 = c.bps[int(0.9*float64(len(c.bps)))]
		}
		cross[s] = map[string]any{"samples": c.samples, "crossed_fraction": float64(c.n) / math.Max(1, float64(c.samples)), "median_cross_bps": med(c.bps), "p90_cross_bps": p90}
	}
	st := eng.Stats()
	return writeJSON(*out, map[string]any{
		"consolidated_crossed": cross,
		"frames":               len(frames), "duration_s": float64(frames[len(frames)-1].RecvNs-frames[0].RecvNs) / 1e9, "replay_s": time.Since(t0).Seconds(),
		"engine": map[string]any{"checksum_ok": st.ChecksumOK, "checksum_fail": st.ChecksumFail, "resyncs": st.Resyncs, "resync_by_reason": st.ResyncByReason,
			"decode_errors": st.DecodeErrors, "snapshots": st.Snapshots, "deltas": st.Deltas},
		"venues_sampled_1hz": res,
	})
}
