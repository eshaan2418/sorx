package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eshaan2418/sorx/internal/mm/capture"
	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/venue"
	"github.com/eshaan2418/sorx/internal/oms"
	"github.com/eshaan2418/sorx/internal/sim"
)

// Default execution assumptions (illustrative, not current venue schedules): equal taker fees so
// routing is driven by displayed prices and depth, and one-way latencies typical of a host in
// US-East (Coinbase and Gemini nearby, Kraken further, Bitstamp in Europe).
var defaultVenues = map[model.Venue]sim.VenueParams{
	model.Kraken:   {TakerFeeBps: 10, LatencyMs: 30},
	model.Coinbase: {TakerFeeBps: 10, LatencyMs: 5},
	model.Bitstamp: {TakerFeeBps: 10, LatencyMs: 80},
	model.Gemini:   {TakerFeeBps: 10, LatencyMs: 10},
}

type summary struct {
	Strategy string  `json:"strategy"`
	Symbol   string  `json:"symbol"`
	Size     float64 `json:"size"`
	N        int     `json:"n"`
	FillRate float64 `json:"mean_filled_frac"`
	ISMean   float64 `json:"mean_total_is_bps"`
	ISMedian float64 `json:"median_total_is_bps"`
	ISP90    float64 `json:"p90_total_is_bps"`
	ISSE     float64 `json:"se_mean_total_is_bps"`
	Slippage float64 `json:"mean_shortfall_bps_filled"`
	BookCost float64 `json:"mean_arrival_book_cost_bps"`
	Fees     float64 `json:"mean_fees_bps"`
	Venues   float64 `json:"mean_venues_used"`
	DoneMs   float64 `json:"median_complete_ms"`
	Unfilled int     `json:"orders_not_fully_filled"`
}

func parseVenueParams(s string, base map[model.Venue]sim.VenueParams, fee bool) error {
	if s == "" {
		return nil
	}
	for _, kv := range strings.Split(s, ",") {
		p := strings.SplitN(kv, "=", 2)
		if len(p) != 2 {
			return fmt.Errorf("want venue=value, got %q", kv)
		}
		v := model.ParseVenue(p[0])
		x, err := strconv.ParseFloat(p[1], 64)
		if err != nil {
			return err
		}
		vp, ok := base[v]
		if !ok {
			return fmt.Errorf("unknown venue %q", p[0])
		}
		if fee {
			vp.TakerFeeBps = x
		} else {
			vp.LatencyMs = x
		}
		base[v] = vp
	}
	return nil
}

func cmdSim(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	dir := fs.String("dir", "capture", "capture directory")
	symbols := fs.String("symbols", "BTC-USD,ETH-USD", "symbols")
	strats := fs.String("strategies", "single:kraken,single:coinbase,single:bitstamp,single:gemini,best,sor,twap", "strategies")
	sizes := fs.String("sizes", "BTC-USD=0.5:2:10,ETH-USD=10:50:250", "parent sizes per symbol")
	every := fs.Duration("every", 15*time.Second, "time between parent orders (symbols alternate, so same-symbol parents are 2x apart)")
	warm := fs.Duration("warmup", 60*time.Second, "no parents in the first part of the capture")
	horizon := fs.Duration("horizon", 30*time.Second, "parent horizon")
	slices := fs.Int("slices", 6, "TWAP slices")
	collar := fs.Float64("collar-bps", 50, "child limit price collar vs arrival mid")
	fees := fs.String("fees", "", "override taker fees in bps, e.g. kraken=26,coinbase=40")
	lats := fs.String("latency", "", "override latencies in ms, e.g. bitstamp=0")
	zeroLat := fs.Bool("zero-latency", false, "set every venue latency to 0")
	seed := fs.Uint64("seed", 1, "parent order seed")
	out := fs.String("out", "-", "result JSON")
	detail := fs.Bool("detail", false, "include per-parent rows")
	_ = fs.Parse(args)
	vp := map[model.Venue]sim.VenueParams{}
	for k, v := range defaultVenues {
		vp[k] = v
	}
	if err := parseVenueParams(*fees, vp, true); err != nil {
		return err
	}
	if err := parseVenueParams(*lats, vp, false); err != nil {
		return err
	}
	if *zeroLat {
		for k, v := range vp {
			v.LatencyMs = 0
			vp[k] = v
		}
	}
	syms := strings.Split(*symbols, ",")
	table, err := venue.NewSymbolTable(syms)
	if err != nil {
		return err
	}
	t0 := time.Now()
	frames, err := capture.ReadAll(*dir)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("empty capture")
	}
	loadS := time.Since(t0).Seconds()
	first, last := frames[0].RecvNs, frames[len(frames)-1].RecvNs
	sz := map[string][]float64{}
	for _, kv := range strings.Split(*sizes, ",") {
		p := strings.SplitN(kv, "=", 2)
		for _, x := range strings.Split(p[1], ":") {
			f, _ := strconv.ParseFloat(x, 64)
			sz[p[0]] = append(sz[p[0]], f)
		}
	}
	// Deterministic parent schedule shared by every strategy (paired comparison).
	r := rand.New(rand.NewPCG(*seed, 99))
	var parents []sim.ParentSpec
	k := 0
	for ts := first + warm.Nanoseconds(); ts+horizon.Nanoseconds() < last; ts += every.Nanoseconds() {
		sym := syms[k%len(syms)]
		size := sz[sym][(k/len(syms))%len(sz[sym])]
		side := oms.Buy
		if r.IntN(2) == 0 {
			side = oms.Sell
		}
		parents = append(parents, sim.ParentSpec{TS: ts, Symbol: sym, Side: side, Qty: model.Fixed(math.Round(size * model.Scale))})
		k++
	}
	type runOut struct {
		Strategy string             `json:"strategy"`
		Seconds  float64            `json:"sim_seconds"`
		Children int                `json:"children"`
		Fills    int                `json:"fills"`
		OMS      string             `json:"oms_invariants"`
		Parents  []sim.ParentResult `json:"parents,omitempty"`
	}
	var runs []runOut
	var sums []summary
	all := map[string][]sim.ParentResult{}
	for _, st := range strings.Split(*strats, ",") {
		cfg := sim.Config{Strategy: st, Venues: vp, Slices: *slices, HorizonNs: horizon.Nanoseconds(), CollarBps: *collar, Parents: parents}
		if v, ok := strings.CutPrefix(st, "single:"); ok {
			cfg.Strategy, cfg.Venue = sim.StratSingle, model.ParseVenue(v)
			if _, known := vp[cfg.Venue]; !known {
				return fmt.Errorf("unknown venue in %q", st)
			}
		}
		s := sim.New(table, cfg)
		ts := time.Now()
		for i := range frames {
			s.Handle(&frames[i])
		}
		res := s.Finish()
		ro := runOut{Strategy: st, Seconds: time.Since(ts).Seconds(), Children: res.Children, Fills: res.Fills, OMS: res.OMSCheck}
		if *detail {
			ro.Parents = res.Parents
		}
		runs = append(runs, ro)
		sums = append(sums, summarise(st, res.Parents)...)
		all[st] = res.Parents
	}
	paired := pairedVs(all, "sor")
	vpOut := map[string]sim.VenueParams{}
	for k, v := range vp {
		vpOut[k.String()] = v
	}
	return writeJSON(*out, map[string]any{
		"capture": map[string]any{"frames": len(frames), "duration_s": float64(last-first) / 1e9, "load_s": loadS},
		"parents": len(parents), "horizon_s": horizon.Seconds(), "every_s": every.Seconds(), "twap_slices": *slices, "collar_bps": *collar,
		"venue_assumptions": vpOut, "runs": runs, "summary": sums, "paired_vs_sor": paired,
	})
}

func summarise(st string, ps []sim.ParentResult) []summary {
	type key struct {
		sym  string
		size float64
	}
	groups := map[key][]sim.ParentResult{}
	var keys []key
	for _, p := range ps {
		k := key{p.Spec.Symbol, p.Spec.Qty.Float64()}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sym != keys[j].sym {
			return keys[i].sym < keys[j].sym
		}
		return keys[i].size < keys[j].size
	})
	var out []summary
	for _, k := range keys {
		g := groups[k]
		s := summary{Strategy: st, Symbol: k.sym, Size: k.size}
		var is, done []float64
		var nf int
		for _, p := range g {
			if p.ArrivalMid == 0 {
				continue // no market at arrival (all books down): excluded for every strategy alike
			}
			s.N++
			s.FillRate += p.FilledFrac
			is = append(is, p.TotalISBps)
			if p.FilledFrac > 0 {
				s.Slippage += p.ShortfallBp
				s.Fees += p.FeesBps
				done = append(done, p.CompleteMs)
				nf++
			}
			if !math.IsNaN(p.ArrivalBook) {
				s.BookCost += p.ArrivalBook
			}
			s.Venues += float64(p.Venues)
			if p.FilledFrac < 1 {
				s.Unfilled++
			}
		}
		if s.N == 0 {
			continue
		}
		n := float64(s.N)
		s.FillRate /= n
		s.BookCost /= n
		s.Venues /= n
		if nf > 0 {
			s.Slippage /= float64(nf)
			s.Fees /= float64(nf)
		}
		sort.Float64s(is)
		for _, x := range is {
			s.ISMean += x
		}
		s.ISMean /= n
		var v float64
		for _, x := range is {
			v += (x - s.ISMean) * (x - s.ISMean)
		}
		if s.N > 1 {
			s.ISSE = math.Sqrt(v / (n - 1) / n)
		}
		s.ISMedian = is[len(is)/2]
		s.ISP90 = is[min(len(is)-1, int(0.9*n))]
		if len(done) > 0 {
			sort.Float64s(done)
			s.DoneMs = done[len(done)/2]
		}
		out = append(out, s)
	}
	return out
}

func writeJSON(path string, v any) error {
	w := os.Stdout
	if path != "-" && path != "" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type pairedRow struct {
	Strategy string  `json:"strategy"`
	Symbol   string  `json:"symbol"`
	Size     float64 `json:"size"`
	N        int     `json:"n"`
	MeanDiff float64 `json:"mean_is_minus_sor_bps"`
	SE       float64 `json:"se_bps"`
}

// pairedVs compares each strategy with the reference on the same parents: the mean of
// per-parent differences in total implementation shortfall, with its standard error.
func pairedVs(all map[string][]sim.ParentResult, ref string) []pairedRow {
	base, ok := all[ref]
	if !ok {
		return nil
	}
	var names []string
	for k := range all {
		if k != ref {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var out []pairedRow
	for _, st := range names {
		type key struct {
			sym  string
			size float64
		}
		d := map[key][]float64{}
		var keys []key
		for i, p := range all[st] {
			b := base[i]
			if p.ArrivalMid == 0 || b.ArrivalMid == 0 {
				continue
			}
			k := key{p.Spec.Symbol, p.Spec.Qty.Float64()}
			if _, seen := d[k]; !seen {
				keys = append(keys, k)
			}
			d[k] = append(d[k], p.TotalISBps-b.TotalISBps)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].sym != keys[j].sym {
				return keys[i].sym < keys[j].sym
			}
			return keys[i].size < keys[j].size
		})
		for _, k := range keys {
			x := d[k]
			n := float64(len(x))
			var m, v float64
			for _, y := range x {
				m += y
			}
			m /= n
			for _, y := range x {
				v += (y - m) * (y - m)
			}
			se := math.NaN()
			if len(x) > 1 {
				se = math.Sqrt(v / (n - 1) / n)
			}
			out = append(out, pairedRow{st, k.sym, k.size, len(x), m, se})
		}
	}
	return out
}
