// Command sorx is an order management system and smart order router evaluated against
// recorded public multi-venue L2 books. It simulates fills; it never connects to an order API.
package main

import (
	"fmt"
	"os"
)

const usage = `usage: sorx <command> [flags]
  record    record public L2 depth from Kraken, Coinbase, Bitstamp, Gemini
  replay    rebuild books from a capture and report book statistics
  sim       run parent orders through the OMS + router strategies over a capture; TCA report
  cut       copy the first part of a capture (samples)
  bench     router and OMS micro-benchmarks`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "record":
		err = cmdRecord(os.Args[2:])
	default:
		err = dispatch(os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sorx:", err)
		os.Exit(1)
	}
}
