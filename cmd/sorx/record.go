package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/eshaan2418/sorx/internal/mm/capture"
	"github.com/eshaan2418/sorx/internal/mm/model"
	"github.com/eshaan2418/sorx/internal/mm/pipeline"
	"github.com/eshaan2418/sorx/internal/mm/venue"
)

var start = time.Now()

func clock() int64 { return time.Since(start).Nanoseconds() }

type resyncer struct {
	sessions map[model.Venue]*venue.Session
}

func (r resyncer) Resync(v model.Venue, sym, _ string) {
	if s, ok := r.sessions[v]; ok {
		s.RequestResync(sym)
	}
}

// cmdRecord records public L2 depth from several venues into a capture directory. A book engine
// runs alongside so that checksum failures and gaps trigger resubscription; the capture then
// contains everything needed to rebuild the same books on replay.
func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	dir := fs.String("dir", "capture", "capture directory")
	venues := fs.String("venues", "kraken,coinbase,bitstamp,gemini", "venues")
	symbols := fs.String("symbols", "BTC-USD,ETH-USD", "canonical symbols")
	dur := fs.Duration("dur", 30*time.Minute, "recording length")
	capMB := fs.Int64("cap-mb", 4096, "capture size cap (uncompressed MB)")
	_ = fs.Parse(args)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	table, err := venue.NewSymbolTable(strings.Split(*symbols, ","))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *dur)
	defer cancel2()
	frames := make(chan venue.Frame, 8192)
	sessions := map[model.Venue]*venue.Session{}
	for _, name := range strings.Split(*venues, ",") {
		v := model.ParseVenue(strings.TrimSpace(name))
		spec, ok := venue.SpecFor(v)
		if !ok {
			return fmt.Errorf("unknown venue %q", name)
		}
		sessions[v] = venue.NewSession(spec, table.Symbols(), frames, clock, log)
	}
	eng := pipeline.New(table, pipeline.Options{Resyncer: resyncer{sessions}, Clock: clock})
	w, err := capture.NewWriter(*dir, 256<<20, *capMB<<20)
	if err != nil {
		return err
	}
	// Wall-clock anchor for the monotonic capture time base.
	meta, _ := json.Marshal(map[string]any{"wall_start_utc": start.UTC().Format(time.RFC3339Nano), "symbols": *symbols, "venues": *venues})
	if err := os.WriteFile(*dir+"/meta.json", meta, 0o644); err != nil {
		return err
	}
	for _, s := range sessions {
		go s.Run(ctx)
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case f := <-frames:
			if err := w.Write(&f); err != nil {
				return err
			}
			eng.Handle(&f)
		case <-tick.C:
			st := eng.Stats()
			fmt.Fprintf(os.Stderr, "record: %s frames=%d checksum_ok=%d fail=%d resyncs=%d\n", time.Since(start).Round(time.Second), st.Frames, st.ChecksumOK, st.ChecksumFail, st.Resyncs)
		case <-ctx.Done():
			// Drain what the sessions already delivered.
			for len(frames) > 0 {
				f := <-frames
				_ = w.Write(&f)
			}
			st := eng.Stats()
			fmt.Fprintf(os.Stderr, "record: done frames=%d records=%d dropped=%d resyncs=%d\n", st.Frames, w.Records, w.Dropped, st.Resyncs)
			return w.Close()
		}
	}
}
