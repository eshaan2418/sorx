package main

import (
	"flag"
	"math"
	"time"

	"github.com/eshaan2418/sorx/internal/mm/capture"
	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/venue"
	"github.com/eshaan2418/sorx/internal/oms"
	"github.com/eshaan2418/sorx/internal/sim"
)

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dir := fs.String("dir", "capture", "capture directory")
	out := fs.String("out", "-", "result JSON")
	_ = fs.Parse(args)
	table, err := venue.NewSymbolTable([]string{"BTC-USD", "ETH-USD"})
	if err != nil {
		return err
	}
	frames, err := capture.ReadAll(*dir)
	if err != nil {
		return err
	}
	// Book replay throughput (decode + book maintenance + checksums), no orders.
	s := sim.New(table, sim.Config{Strategy: sim.StratSOR, Venues: defaultVenues})
	t0 := time.Now()
	for i := range frames {
		s.Handle(&frames[i])
	}
	replay := time.Since(t0).Seconds()
	// SOR planning on the final books: consolidate, fee-adjust, sort, allocate.
	mid, _ := s.Mid("BTC-USD")
	res := map[string]any{"frames": len(frames), "replay_frames_per_sec": float64(len(frames)) / replay}
	plan, sorted := map[string]float64{}, map[string]float64{}
	for _, qty := range []float64{0.1, 2, 10} {
		q := model.Fixed(qty * model.Scale)
		const n = 20000
		t := time.Now()
		for i := 0; i < n; i++ {
			_ = s.Plan("BTC-USD", oms.Buy, q, mid*1.005)
		}
		plan[ftoa(qty)] = float64(time.Since(t).Nanoseconds()) / n
		t = time.Now()
		for i := 0; i < n/10; i++ {
			_ = s.PlanSorted("BTC-USD", oms.Buy, q, mid*1.005)
		}
		sorted[ftoa(qty)] = float64(time.Since(t).Nanoseconds()) / (n / 10)
	}
	res["sor_plan_ns_by_btc_qty"] = plan
	res["sor_plan_collect_and_sort_ns_by_btc_qty"] = sorted
	// OMS: parent + child + fill + done cycle.
	m := oms.New(oms.Limits{CollarBps: 100})
	const n = 500000
	t := time.Now()
	for i := 0; i < n; i++ {
		ts := int64(i)
		p := m.NewParent(ts, "BTC-USD", oms.Buy, model.Scale, 0, 78000)
		c, _ := m.NewChild(ts, p.ID, model.Coinbase, model.Scale, model.Fixed(78010*model.Scale), 78000)
		_ = m.Fill(ts, c.ID, model.Scale/2, model.Fixed(78005*model.Scale), 1)
		_ = m.Done(ts, c.ID, "ioc")
	}
	res["oms_order_cycles_per_sec"] = n / time.Since(t).Seconds()
	res["note"] = "cycle = parent accept + risk checks + child + partial fill + IOC done; journal kept in memory"
	return writeJSON(*out, res)
}

func ftoa(x float64) string {
	if x == math.Trunc(x) {
		return itoa(int(x))
	}
	return itoa(int(x)) + "." + itoa(int(math.Round((x-math.Trunc(x))*10)))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
