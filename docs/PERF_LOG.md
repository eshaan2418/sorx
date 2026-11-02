# sorx performance and correctness log

Machine: Apple M5 (18 CPUs), Go 1.27.3, shared with an unrelated CPU-heavy workload (load
average 15–16 during the final benchmark; see `bench/results/ENV.txt`).

## Benchmarks (`bench/results/bench.json`)
| what | result |
|---|---|
| Book replay (decode + 4 venue books + Kraken CRC32), 463,653 frames | 71,751 frames/s |
| SOR plan: consolidate 4 live BTC books, fee-adjust, allocate | **1.06 µs** (0.1 BTC), 2.63 µs (2 BTC), 2.95 µs (10 BTC) |
| Same with collect-and-sort (previous implementation) | 48.5 / 48.9 / 45.7 µs |
| OMS cycle: parent accept + risk + child + partial fill + IOC done | 564k cycles/s |
| Full simulation of one strategy over the 40-min capture (154 parents) | 4–9 s (`sim_*.json`, `sim_seconds`) |

## The routing hot path: collect-and-sort → k-way merge (16–46×)
- **The bug in the first version.** `candidates()` collected every level of every venue within
  the collar (Coinbase alone holds ~20k BTC levels) into a slice, then sorted it. That cost
  ~45–49 µs whatever the order size, because it did work proportional to book depth, not to
  the order.
- **The observation.** Each venue's side is already sorted best-first, and the fee is constant
  per venue, so fee-adjusted prices are monotone within a venue. The global best-first order is
  therefore a k-way merge of four sorted ladders, and it can stop as soon as the order is
  covered.
- **Verification.**
  - The collect-and-sort version is kept as `candidatesAll` and used as the oracle in a test
    over 300 random book pairs with random fees, on both sides.
  - All three simulation scenarios produced **identical** summaries and paired comparisons
    before and after the change.
- **Result.** 1.1–2.9 µs. The cost now grows with how many levels the order consumes.

## Correctness journey
| issue | symptom | fix |
|---|---|---|
| Consolidated mid undefined | every parent saw "no market": `Mid()` refused a crossed consolidated book, and the real books were crossed 100% of the time | use the midpoint of the consolidated best bid and ask even when crossed; report the cross in `replay` |
| Expiry with children in flight | expiry deferred itself but reset the "expired" flag, so routing could continue past the horizon | the flag stays set and the re-check only waits for in-flight children |
| State constant `New` shadowed the constructor `New` | compile error in tests | state renamed `Open` |
| Parents overlapping on the same symbol | they share liquidity and couple the measurements | parents every 15 s with symbols alternating: same-symbol parents are 30 s apart, equal to the horizon |
| Strategy comparisons at n ≈ 25 per bucket | means alone overstate differences | paired differences vs SOR with standard errors (common market moves cancel) |

## Invariants checked on every run
`oms.Check()` must hold at the end of every simulation run: cum ≤ qty, parent = Σ children,
working = Σ open children, positions = Σ fills. It is `"ok"` in all 21 runs (3 scenarios × 7
strategies), which is reported in each `sim_*.json`.

On top of that:
- the OMS property test runs 200k random operations;
- the OMS fuzz target ran 1.8M executions locally;
- the sim property test runs random books × all strategies.
