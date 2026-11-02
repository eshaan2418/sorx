package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/eshaan2418/sorx/internal/mm/capture"
)

// cmdCut copies the first part of a capture into a new capture directory (small committed
// samples for tests and the demo).
func cmdCut(args []string) error {
	fs := flag.NewFlagSet("cut", flag.ExitOnError)
	dir := fs.String("dir", "capture", "source capture")
	out := fs.String("out", "sample", "destination directory")
	dur := fs.Duration("dur", 2*time.Minute, "length to keep from the start")
	_ = fs.Parse(args)
	frames, err := capture.ReadAll(*dir)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("empty capture")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	w, err := capture.NewWriter(*out, 0, 0)
	if err != nil {
		return err
	}
	end := frames[0].RecvNs + dur.Nanoseconds()
	n := 0
	for i := range frames {
		if frames[i].RecvNs > end {
			break
		}
		if err := w.Write(&frames[i]); err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(os.Stderr, "cut: %d frames -> %s\n", n, *out)
	return w.Close()
}
