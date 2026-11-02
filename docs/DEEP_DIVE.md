# sorx: deep-dive interview questions

### 1. What does an OMS guarantee that a router alone doesn't?
- **Bookkeeping.** It is the source of truth for what was asked, what is working and what is
  done. Every order has a state machine with legal transitions only, and quantities that always
  reconcile: parent cum = Σ child cum, working = Σ open child remainders, position = Σ signed
  fills.
- **Containment.** A router bug can then at worst send a bad child, never corrupt the books:
  overfills, fills through the limit and fills after cancel are rejected.
- **Checks.** `Check()` verifies the invariants after every simulation run, under a
  200k-operation property test, and under fuzzing.

### 2. Which pre-trade risk checks did you implement, and why each?
| check | why |
|---|---|
| max order qty / notional | fat-finger protection |
| max position incl. working parents | a burst of orders must not exceed limits if all of them fill |
| price collar on child limits vs arrival mid | stops a router walking a thin book far through the market |
| per-venue child rate (rolling 1 s) | venue throttles, and protection against runaway loops |
| kill switch | stop everything now |

They are ordered cheapest first and run synchronously before any child is acknowledged.

### 3. How does the SOR decide where to send?
1. **Merge.** It merges each live venue's ladder into one sequence ordered by fee-adjusted
   price, px·(1 + fee) for buys and px·(1 − fee) for sells.
2. **Take greedily** within the collar until the quantity is covered.
3. **Send.** For each venue it sends one IOC for its share, with limit = the worst price it
   intends to take there.

A venue whose book is not trustworthy (resyncing, disconnected, own book crossed) is excluded.
Ties break by venue id, deterministically.

### 4. Why is the merge a k-way merge and not a sort?
Each venue's side is already sorted, and the fee is constant per venue, so fee-adjusted prices
are monotone within each venue. The global order is a merge of k = 4 sorted lists, and it can
stop as soon as the order is filled. The first version collected every level within the collar
(Coinbase alone has ~20k) and sorted them: 45–49 µs per decision regardless of size. The merge
takes 1–3 µs, and an oracle test checks that it returns exactly the sorted ladder.

### 5. How do you simulate fills from a recording without fooling yourself?
- **Arrival time.** A child executes when it *arrives* (decision time + venue latency),
  against the book as it stood then, not at decision time.
- **Consumption.** Liquidity it takes is marked as consumed at that (venue, side, price) until
  the feed changes the level. Otherwise a second order would re-take the same liquidity that
  the recording, unaware of us, still shows.
- **Not modelled, and documented:** others racing us for the same liquidity, hidden liquidity,
  and other participants' reaction to our trades.

The simulation is therefore optimistic for latency-sensitive takers, and it is reasonable for
order sizes that are small relative to replenishment.

### 6. What is implementation shortfall and how do you handle partial fills?
- **Definition.** IS = side × (average fill − arrival mid) / arrival mid, plus fees.
- **Partial fills.** A strategy that fills less can look cheaper simply by not trading, so the
  unfilled part is charged its **opportunity cost**: it is marked at the mid at the horizon.
  Total IS = filled fraction × (slippage + fees) + unfilled fraction × the mid's adverse move.
- **Example.** Kraken-only at 10 BTC filled 92.8%, and its total IS reflects the mark on the
  rest.

### 7. Why paired comparisons?
Market moves during an order's life add noise much larger than the differences between
routers. Running every strategy on *the same parents over the same recording* and
differencing per parent cancels the common moves. Example: TWAP vs SOR at 250 ETH is −1.29 ±
0.33 bp paired, while the unpaired standard errors of each mean are 0.36 and 0.17 bp.

### 8. Why is the consolidated book crossed 100% of the time, and what does it do to TCA?
Gemini quotes at a persistent premium: it held the best bid 88% of the time for BTC, while
Coinbase held the best ask 75% of the time. The median cross is 2.6 bp, below two taker fees,
so it cannot be arbitraged.
- **For TCA:** the midpoint benchmark sits inside the cross. A buyer routed to the cheap venue
  can execute below the mid, giving negative shortfall: −0.56 bp for 0.5 BTC via SOR. That is
  an artefact of the benchmark, which is why the arrival book cost is reported too. The SOR
  realises it almost exactly.

### 9. Why does TWAP beat the immediate sweep, even at equal fees?
The sweep takes everything near the touch at once, so its cost is the book's depth profile.
TWAP takes one sixth at a time, and the books refill in between. Over 30 seconds the books
replenish faster than prices drift on average, so TWAP pays less: −0.47 to −1.29 bp vs SOR in
five of six size buckets (+0.23 ± 0.34 bp at 50 ETH). The cost is timing risk, visible in its larger standard error, and a more exposed order:
others see six prints.

### 10. And with unequal fees the greedy SOR loses. Why?
- **What greedy does.** The fee-adjusted sweep exhausts the cheapest-fee venue's visible
  liquidity, then pays a 5–15 bp fee premium elsewhere immediately.
- **What patience does.** Waiting a second lets the cheap venue refill. Kraken-only (with
  retries) gets 26.1 bp vs the sweep's 30.7 bp at 250 ETH under the hypothetical schedule.
- **Lesson.** Routing is a scheduling problem: urgency vs fee vs replenishment rate. A
  one-shot sort ignores time.

### 11. Latency barely mattered here. Is latency irrelevant for routing?
No. It is irrelevant to *this* decision at *this* time scale. Zero latency changed mean
shortfall by at most 0.13 bp. These parents are seconds long, and the recorded liquidity near
the touch mostly survives 5–80 ms.
- **Where latency does matter:** when you race other takers for the same quotes (latency
  arbitrage of stale venues, e.g. the persistent Gemini premium), and for passive orders'
  queue position. Neither can be measured from a recording that doesn't contain the other
  racers.

### 12. Why IOC children instead of resting orders?
IOCs make the simulation honest: their outcome depends only on the book at arrival. Resting
orders need a queue-position model (queuesim does this for L3 equities). On L2 crypto feeds
the queue position is not observable, so any passive-fill model would be an assumption piled
on an assumption.

### 13. How do you know the books are right?
They come from marketmux's engine, vendored unchanged:
- **Kraken:** books are validated against the venue's CRC32 checksum on every update, 185,328
  of 185,328 OK in this capture.
- **Bitstamp:** REST snapshot + diff synchronisation by microtimestamp.
- **Coinbase:** batched level2.
- **Gemini:** l2.
- **Failure handling:** a crossed own book or a disconnect marks the venue untrusted. The
  router never routes to an untrusted book, and Bitstamp's one reconnect during the capture
  shows as 99.83% live time.

### 14. Why fixed-point quantities but float notionals?
- **Quantities and prices.** These must reconcile exactly (cum = Σ fills, leaves = qty − cum −
  working). In int64 at 1e-8 that holds with no rounding.
- **Notionals.** price × qty at 1e-8 scale overflows int64 (78,000 × 10 BTC ≈ 7.8e21 in
  units), so notionals and fees are float64. They are only used for averages, risk limits and
  TCA, never for reconciliation.

### 15. What would you add for production?
- A FIX or venue-API gateway with acks, rejects and cancel/replace. The OMS state machine
  already has the states.
- Persistence of the journal and recovery.
- Per-venue fill-probability models and markouts.
- Passive and post-only routing with rebates.
- Urgency-aware scheduling: Almgren-Chriss-style trajectories with replenishment models.
- Cross-venue inventory constraints.
- A real-time TCA feed into the router.
