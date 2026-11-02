# Market structure notes: routing across fragmented crypto venues

All figures come from `research/results/replay.json`: a 40-minute capture of public L2 depth
from Kraken, Coinbase, Bitstamp and Gemini, BTC-USD and ETH-USD, 2026-11-02 09:32 UTC,
sampled at 1 Hz.

## The consolidated book is crossed, all the time
In **all 2,374 one-second samples** the best bid across venues was above the best ask across
venues:

| symbol | crossed | median cross | p90 cross |
|---|---|---|---|
| BTC-USD | 100% | 2.6 bp | 4.2 bp |
| ETH-USD | 100% | 2.2 bp | 3.8 bp |

Gemini held the best bid 88% (BTC) and 97% (ETH) of the time. Coinbase held the best ask 75% and
69% of the time.
- **Each book is sane.** Every venue's own book is consistent: zero Kraken checksum failures
  out of 185,328, and no own-book crossings.
- **The premium is persistent.** marketmux found the same Gemini premium on an earlier capture,
  with trades printing at it, so it is not a stale-book artefact.
- **It isn't arbitrage.** The cross is far below the cost of a two-leg taker round trip (two
  taker fees of 10–40 bp each), plus the cost of moving inventory between venues.

**Consequences for routing:**
- **Venue choice by side.** A buyer should buy at Coinbase/Kraken/Bitstamp and a seller should
  sell at Gemini. A smart router does this automatically because it sorts by price, while a
  "home venue" router pays the premium.
- **The midpoint benchmark misleads.** The consolidated midpoint lies inside the cross. A small
  order routed to the cheap side of the cross can execute *better* than the mid, which shows up
  as negative shortfall. That is not alpha. It is what a midpoint benchmark does in a crossed
  market, and it is why the arrival *book cost* (what sweeping the book at decision time costs)
  is reported next to the shortfall.

## Depth is where the venues differ
Median quantity within 10 bp of the consolidated mid:

| | Coinbase | Bitstamp | Gemini | Kraken* |
|---|---|---|---|---|
| BTC bid / ask | 21.0 / 18.0 | 19.7 / 18.4 | 6.0 / 2.5 | 2.8 / 8.6 |
| ETH bid / ask | 414 / 550 | 492 / 549 | 98 / 64 | 30 / 52 |

\* Kraken is subscribed at **depth 10**, which its checksum scheme requires. Its book view is
truncated, which understates its depth here and makes Kraken-only orders fill partially at size.
That is a feed-subscription limit, not a property of the venue.

All four venues quote 1-tick spreads (≈0.001 bp on BTC). The difference between them is how
much size sits near the touch. That is why single-venue execution cost grows so differently
with order size: Gemini-only costs +11 bp vs SOR for 10 BTC, Bitstamp-only +0.9 bp.

## Latency matters less than you'd think, at this time scale
The default one-way latencies are assumptions for a US-East host: 5–80 ms. Setting all of them
to zero changes mean shortfall by at most 0.13 bp in any strategy and size bucket
(`sim_zero_latency.json` vs `sim_default.json`; the largest is best venue, 0.5 BTC: 0.56 → 0.43
bp). Public L2 books for these pairs update every few to tens of
milliseconds, but the liquidity near the touch is mostly replenished at the same prices. For a
taker routing parent orders over seconds, depth and fees dominate. This would not hold for
latency-sensitive strategies racing other takers, which the recording cannot model.

## Fees dominate the total cost, and greedy routing can lose to patience
- **Equal fees.** At the default 10 bp taker fee on every venue, fees are the largest part
  of total implementation shortfall: 10 bp of a 9–11 bp total for small SOR orders. Routing quality is the remaining 0.5–17 bp, which
  is what separates the strategies.
- **Unequal fees.** Under a hypothetical schedule (Kraken 25, Coinbase 40, Bitstamp 30,
  Gemini 35 bp) the greedy fee-adjusted sweep is *worse* than simply waiting on the cheapest
  venue. At 250 ETH it costs 30.7 bp against 26.1 for Kraken-only, and TWAP-SOR gets 26.4.
  - The sweep takes all of the cheap venue's visible liquidity, then pays a 5–15 bp fee premium
    elsewhere *now*.
  - The patient strategies let the cheap book refill.

  A production router should model replenishment (the expected time for the cheap venue to
  refill) and trade urgency against fees. That is a scheduling problem, not just a sort.

## What a real SOR adds
- **Hidden and iceberg liquidity, and venue-specific order types** (post-only, IOC vs FOK,
  self-trade prevention).
- **Fill probability.** Displayed liquidity can disappear before an order arrives: others race
  for it. A real router learns per-venue fill ratios.
- **Passive routing.** Posting on maker-rebate venues while sweeping elsewhere, and the queue
  position that comes with it (see queuesim).
- **Inventory and transfer costs.** Buying on one venue and selling on another needs balances in
  both places.
- **Post-trade feedback.** Venue-level markouts feed back into the venue ranking.

## Conduct
- **Public data only.** The capture holds public market-data frames only.
- **Simulated orders.** Every order in this repository is simulated against the recording, and
  nothing connects to an order-entry API.
- **Fees and latencies are illustrative.** They are assumptions, not current venue schedules.
- Educational/research project. Not investment advice. No live trading.
