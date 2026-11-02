# sorx design

## Pieces
```
public WS (Kraken, Coinbase, Bitstamp, Gemini) ──► capture (raw frames, gzip)      [record]
                                                       │
capture ──► book engine (vendored marketmux pipeline: snapshots, deltas, Kraken CRC32,
            Bitstamp snapshot/diff sync, resyncs) ──► per-venue L2 books
                                                       │
parent schedule ──► action queue (time-ordered) ──► router ──► OMS (state machine, risk) ──► child IOCs
                                                       ▲                                       │ + venue latency
                                                       └──── fills ◄── fill simulator ◄────────┘
                                                                                        TCA per parent
```

## OMS (`internal/oms`)
- **Orders.** Parents are client orders; children are venue IOC orders with a limit price.
- **State machine.** `PendingNew → Open | Rejected`, then `Open → PartiallyFilled → Filled`, or
  `Canceled` (the IOC remainder) or `Expired` (the parent horizon). Illegal transitions are
  errors: a fill after cancel, overfill, a fill through the limit, a child on a terminal parent.
- **Accounting.** Quantities are exact fixed point (1e-8). A parent tracks `CumQty`, `Working`
  (quantity at venues) and `Leaves = Qty − CumQty − Working`. A child can never exceed the
  parent's leaves. `Check()` verifies every invariant: cum ≤ qty, parent cum = Σ children cum,
  parent working = Σ open children's remainder, and positions = Σ signed fills. It is
  property-tested over 200k random operations and fuzzed.
- **Pre-trade risk.**
  - maximum order quantity and notional;
  - maximum position, including the worst case of other working parents filling;
  - a price collar on child limits against the arrival mid;
  - a per-venue child rate limit over a rolling 1 s;
  - a kill switch.
- **Journal.** Every state change is journaled.

## Router strategies (`internal/sim`)
All strategies send marketable limit IOC children. The limit is the worst price they intend to
take, never beyond the collar: 50 bp through the arrival mid by default. Unfilled leaves are
re-routed every second until the horizon (30 s), then the parent expires. The strategies:

| name | decision |
|---|---|
| `single:<venue>` | the whole order to one venue, walking its book |
| `best` | the whole order to the venue whose best fee-adjusted price is best |
| `sor` | merge the live books of all venues into one ladder of (venue, price, available qty), sorted by fee-adjusted price px·(1 ± fee). Take greedily up to the order size, then send one IOC per venue for its share, limit = the worst price taken there |
| `twap` | split into N slices (6 by default) released evenly over the horizon; each slice is routed with `sor`. The last slice releases everything left |

A venue whose book is not live (resync, disconnect, own book crossed) is excluded.

## Fill simulation
- **Arrival.** A child decided at time t reaches its venue at t + latency(venue), and executes
  against that venue's recorded book as it stood then. The book may have moved since the
  decision. If the child's limit is no longer available it fills less, or nothing.
- **Liquidity consumption.** Liquidity taken at (venue, side, price) is remembered and not
  offered again until the feed changes that level's quantity. The feed cannot know about our
  trade, so without this a second order would re-take the same liquidity. When the level
  changes, the venue has refreshed it.
- **Fees.** The taker fee is charged as a fraction of the fill's notional.
- **Not modelled.** Market impact beyond consumed levels (others' reaction to our prints),
  hidden or iceberg liquidity, passive orders, and venue-side rejects.

## TCA
For each parent:
- **Arrival mid:** the consolidated mid when the parent arrives.
- **Shortfall:** side × (avg fill − arrival mid) / arrival mid, in bp, over the filled part.
- **Fees:** in bp of the filled notional.
- **Opportunity cost:** the unfilled remainder is marked at the consolidated mid at the
  horizon.
- **Total IS** = filled fraction × (shortfall + fees) + unfilled fraction × side × (end mid /
  arrival mid − 1).
- **Arrival book cost:** what sweeping the consolidated book at decision time would cost (no
  fees), the market's own estimate of the order's cost.

**Paired comparison.** Every strategy runs on the same parents, so each parent's total IS is
compared against `sor` and the mean of the per-parent differences is reported with its
standard error. Pairing cancels market moves common to both strategies, which dominate the
noise.

## Time and determinism
- **Time base.** Time is the capture's receive clock. Actions (parent arrival, child arrival,
  retry, TWAP slice, expiry) live in a min-heap ordered by (time, sequence).
- **Ordering rule.** Actions due at or before a frame's receive time run before that frame
  updates the books.
- **Determinism.** Venue iteration is sorted wherever it affects decisions, so a run is fully
  deterministic (tested).

## Why vendor marketmux's book engine
Correct multi-venue L2 books are most of the work, and marketmux already solved and tested it:
- Kraken's CRC32 book checksums were 259k/259k on its live session.
- Bitstamp's REST snapshot + diff synchronisation and Coinbase's batched level2 are handled.
- A crossed own book triggers a resync, and disconnect markers invalidate the venue.

sorx vendors `model`, `book`, `venue`, `pipeline`, `capture` and `fakevenue` unchanged except for
import paths (`internal/mm`, MIT, same author). Go's `internal` rule prevents importing them
across modules.
