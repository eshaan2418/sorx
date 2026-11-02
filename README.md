# sorx

**OMS + smart order router + transaction-cost analysis in Go, evaluated on recorded public L2
depth from four crypto venues (Kraken, Coinbase, Bitstamp, Gemini).**
- **OMS:** an order state machine with exact fixed-point accounting, pre-trade risk checks and
  a journal.
- **Strategies:** single-venue, best-venue, a fee-aware consolidated-book SOR, and TWAP slices
  of it.
- **Fill simulation:** per-venue latency and consumed-liquidity tracking.
- **TCA:** implementation shortfall with opportunity cost, plus paired comparisons.

[![CI](https://github.com/eshaan2418/sorx/actions/workflows/ci.yml/badge.svg)](https://github.com/eshaan2418/sorx/actions/workflows/ci.yml)

> Educational/research project. Not investment advice. No live trading.
> Every order is **simulated** against a recording of public market data; nothing here connects
> to an order-entry API. Fees and latencies are explicit assumptions, not current venue
> schedules.

## Headline results

All numbers come from a 40-minute capture of BTC-USD and ETH-USD L2 depth (2026-11-02 09:32 UTC,
463,653 frames). There are 154 parent orders, the same for every strategy, with a 30 s horizon,
a 50 bp collar and 10 bp taker fees everywhere. Sources are in
[`research/results`](research/results) and [`bench/results`](bench/results).

**Execution cost vs the SOR.** Total implementation shortfall difference in bp, paired per
parent, mean ± SE (n ≈ 25 per row). Positive means more expensive than the SOR.

| order | Gemini only | Coinbase only | Bitstamp only | best venue | **SOR** (abs. slippage) | TWAP-SOR |
|---|---|---|---|---|---|---|
| 0.5 BTC | +1.99 ± 0.21 | +1.83 ± 0.22 | +0.76 ± 0.10 | +1.12 ± 0.13 | −0.56 | −0.50 ± 0.24 |
| 10 BTC | **+11.0 ± 1.1** | +3.27 ± 0.20 | +0.93 ± 0.17 | +4.52 ± 0.69 | 0.63 | −0.54 ± 0.24 |
| 10 ETH | +1.93 ± 0.19 | +0.64 ± 0.09 | +0.46 ± 0.07 | +0.85 ± 0.16 | 0.06 | −0.47 ± 0.38 |
| 250 ETH | **+15.7 ± 0.5** | +2.08 ± 0.11 | +1.44 ± 0.09 | +6.89 ± 1.32 | 1.37 | −1.29 ± 0.33 |

Full table, with Kraken-only and all sizes: [`sim_default.json`](research/results/sim_default.json).

| finding | evidence |
|---|---|
| The SOR realises the market's own cost | SOR slippage matches the arrival consolidated-book sweep cost within 0.04 bp in every bucket (e.g. 0.63 vs 0.63 bp at 10 BTC) |
| The consolidated book was **crossed 100% of the time** (2,374/2,374 samples; median 2.6 bp BTC, 2.2 bp ETH) | Gemini holds the best bid 88% / 97% of the time, Coinbase the best ask 75% / 69%. The SOR buys below the crossed mid (negative slippage for small orders) |
| 5–80 ms of venue latency barely matters at this time scale | zero-latency rerun changes mean shortfall by at most 0.13 bp ([`sim_zero_latency.json`](research/results/sim_zero_latency.json)) |
| **Greedy fee-aware routing can lose to patience** | with a hypothetical fee schedule (Kraken 25, Coinbase 40, Bitstamp 30, Gemini 35 bp), the sweep costs 30.7 bp at 250 ETH vs 26.1 for Kraken-only and 26.4 for TWAP ([`sim_fees_hypothetical.json`](research/results/sim_fees_hypothetical.json)) |
| Books are exact | 185,328 / 185,328 Kraken CRC32 checksums OK, 0 decode errors, and a Bitstamp reconnect handled |
| OMS invariants hold | `oms.Check()` "ok" in all 21 strategy runs; 200k-step property test; 1.8M-exec fuzz |
| SOR decision cost | **1.1–2.9 µs** (k-way merge) vs 46–49 µs for collect-and-sort, with identical results; OMS 564k order cycles/s; replay 71.8k frames/s |

## How it works

```
public WS ──► capture ──► book engine (vendored marketmux: snapshots/deltas, Kraken CRC32,
                          Bitstamp snapshot+diff sync, resync on crossed own book)
                               │ per-venue L2 books
parents ──► time-ordered action queue ──► router ──► OMS (state machine, risk checks) ──► child IOCs
                               ▲                                                   │ + venue latency
                               └───────────── fills ◄── fill simulator ◄───────────┘
                                                              TCA per parent, paired vs SOR
```

- **Router.**
  - It merges the live venues' ladders by fee-adjusted price. This is a k-way merge, because each
    ladder is sorted and each venue's fee is constant.
  - It takes greedily within the collar and sends one IOC per venue, with limit = the worst price
    intended there.
  - It re-routes unfilled leaves every second until the horizon.
- **Fills.** A child executes when it **arrives** (decision time + latency), against the book
  as it stood then. Liquidity it takes is not offered again until the feed changes that level.
- **TCA.**
  - Total IS = filled × (slippage + fees) + unfilled × (adverse move of the mid by the
    horizon). Without the opportunity term, not trading would look cheap.
  - The arrival book cost is reported because the crossed consolidated mid is a biased
    benchmark.

## Run it

```bash
make build test race     # go test: OMS lifecycle/risk/property/fuzz, sim fills/fees/latency/consumption/TWAP/determinism, walker oracle
make data                # record 40 min of public L2 depth (Kraken, Coinbase, Bitstamp, Gemini)
make replay sim bench    # book statistics, three strategy scenarios, micro-benchmarks
make demo                # the same on the committed 150-second sample (testdata/sample, 1.9 MB)
bin/sorx sim -dir capture -strategies sor,twap -fees coinbase=40 -latency bitstamp=0 -detail
```

Real output on the committed sample (2 parents per bucket, so the numbers are illustrative;
[`demo_output.txt`](research/results/demo_output.txt)):
```
{"strategy":"single:gemini","symbol":"BTC-USD","size":10,"n":2,"fill":0.92,"is_bps":29.19,"slip_bps":21.51,"venues":1}
{"strategy":"best","symbol":"BTC-USD","size":10,"n":2,"fill":1,"is_bps":12.45,"slip_bps":2.45,"venues":1.5}
{"strategy":"sor","symbol":"BTC-USD","size":10,"n":2,"fill":1,"is_bps":10.58,"slip_bps":0.58,"venues":3}
{"strategy":"twap","symbol":"BTC-USD","size":10,"n":2,"fill":1,"is_bps":9.79,"slip_bps":-0.21,"venues":2.5}
```

## Caveats, stated up front
- **Fees and latencies are illustrative.** The defaults are equal 10 bp taker fees and assumed
  US-East latencies of 5–80 ms. Real schedules are tiered and change.
- **Kraken depth is truncated.** It is subscribed at depth 10, as its checksum scheme requires,
  so its book is truncated and Kraken-only orders fill partially at size.
- **The fill model is optimistic.** There is no race for liquidity with other takers, no hidden
  liquidity and no market impact beyond consumed levels.
- **IOC children only.** Passive routing would need a queue model the L2 feed cannot support.
- **Sample size.** n ≈ 25 parents per bucket. Use the paired standard errors.

## Documentation
- [docs/DESIGN.md](docs/DESIGN.md): OMS states and invariants, strategies, fill model, TCA,
  determinism
- [docs/MICROSTRUCTURE.md](docs/MICROSTRUCTURE.md): the crossed consolidated book, venue depth,
  latency vs fees, what a real SOR adds
- [docs/DEEP_DIVE.md](docs/DEEP_DIVE.md): 15 interview-style questions and answers
- [docs/PERF_LOG.md](docs/PERF_LOG.md): the 16–46× routing speed-up and the bugs found

## Where it fits
sorx is the execution layer of a small research trading stack. It reuses the book engine of
[marketmux](https://github.com/eshaan2418/marketmux), vendored in `internal/mm` (MIT, same
author). Related projects: [riskd](https://github.com/eshaan2418/riskd) (options risk),
[queuesim](https://github.com/eshaan2418/queuesim) (queue-aware passive fills),
[matchbox](https://github.com/eshaan2418/matchbox) (the venue side: a matching engine) and the
rest of the [portfolio](https://github.com/eshaan2418?tab=repositories).

## Data
- The capture holds public market-data frames, recorded without API keys.
- The full capture is not committed. Its SHA-256 is in
  `research/results/capture_sha256.txt`, and `make data` records a new one.
- A 150-second sample is committed in `testdata/sample`.

## License
MIT.
