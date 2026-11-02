// Package oms is a small order management system: order state machine, parent/child
// accounting, pre-trade risk checks and a journal of every state change.
//
// Quantities are fixed point (model.Fixed, 1e-8) so cumulative fills are exact; notionals are
// float64 (price x qty overflows int64 at 1e-8 scale) and used only for risk limits.
package oms

import (
	"errors"
	"fmt"

	"github.com/eshaan2418/sorx/internal/mm/model"
)

// Side of an order.
type Side int8

const (
	Buy  Side = 1
	Sell Side = -1
)

func (s Side) String() string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

// State of an order. Transitions (anything else is rejected):
//
//	PendingNew -> Open | Rejected
//	Open       -> PartiallyFilled | Filled | Canceled | Expired
//	PartiallyFilled -> PartiallyFilled | Filled | Canceled | Expired
//
// Canceled/Expired on a child means "the rest was not filled" (IOC remainder).
type State uint8

const (
	PendingNew State = iota
	Open
	PartiallyFilled
	Filled
	Canceled
	Expired
	Rejected
)

var stateNames = [...]string{"pending_new", "open", "partially_filled", "filled", "canceled", "expired", "rejected"}

func (s State) String() string { return stateNames[s] }

// Terminal reports whether no further transitions are possible.
func (s State) Terminal() bool { return s >= Filled }

// ID identifies an order.
type ID uint64

// Order is a parent (client order) or a child (venue order).
type Order struct {
	ID       ID
	Parent   ID // 0 for parents
	Symbol   string
	Side     Side
	Qty      model.Fixed
	Limit    model.Fixed // 0 = market (parents only); children are always limit IOC
	Venue    model.Venue // children only
	State    State
	CumQty   model.Fixed
	Notional float64 // sum of fill qty x price (quote currency)
	Fees     float64 // quote currency
	Created  int64   // ns
	Updated  int64
	Reason   string // reject / cancel reason
	// Parent bookkeeping: quantity currently working at venues (sent, not yet terminal).
	Working model.Fixed
	Kids    []ID
}

// Leaves is the quantity neither filled nor working (parents) / not filled (children).
func (o *Order) Leaves() model.Fixed { return o.Qty - o.CumQty - o.Working }

// AvgPx is the average fill price.
func (o *Order) AvgPx() float64 {
	if o.CumQty == 0 {
		return 0
	}
	return o.Notional / o.CumQty.Float64()
}

// Limits are pre-trade risk limits. Zero disables a check.
type Limits struct {
	MaxOrderQty      model.Fixed
	MaxOrderNotional float64
	MaxPosition      model.Fixed // absolute net position per symbol, including working orders
	CollarBps        float64     // child limit price must be within this of the reference mid
	MaxChildPerSec   int         // per venue, rolling one second
}

// Event is one journal entry.
type Event struct {
	TS     int64
	Order  ID
	Kind   string // "new", "ack", "reject", "fill", "cancel", "expire"
	Qty    model.Fixed
	Price  model.Fixed
	Fee    float64
	State  State
	Reason string
}

// OMS owns all orders. Not safe for concurrent use (single-writer, like the router).
type OMS struct {
	Limits  Limits
	orders  map[ID]*Order
	next    ID
	pos     map[string]model.Fixed // filled net position per symbol
	recent  map[model.Venue][]int64
	Journal []Event
	killed  bool
}

// New returns an empty OMS.
func New(l Limits) *OMS {
	return &OMS{Limits: l, orders: map[ID]*Order{}, pos: map[string]model.Fixed{}, recent: map[model.Venue][]int64{}}
}

// Errors.
var (
	ErrUnknown    = errors.New("unknown order")
	ErrTransition = errors.New("illegal state transition")
	ErrOverfill   = errors.New("fill exceeds order quantity")
	ErrKilled     = errors.New("kill switch engaged")
)

// Get returns an order (nil if unknown).
func (m *OMS) Get(id ID) *Order { return m.orders[id] }

// Position returns the filled net position for a symbol.
func (m *OMS) Position(sym string) model.Fixed { return m.pos[sym] }

// Kill engages the kill switch: new orders are rejected.
func (m *OMS) Kill() { m.killed = true }

func (m *OMS) log(e Event) { m.Journal = append(m.Journal, e) }

// NewParent accepts a client order after risk checks. Rejected orders are kept (state Rejected).
func (m *OMS) NewParent(ts int64, sym string, side Side, qty, limit model.Fixed, refPx float64) *Order {
	m.next++
	o := &Order{ID: m.next, Symbol: sym, Side: side, Qty: qty, Limit: limit, Created: ts, Updated: ts}
	m.orders[o.ID] = o
	m.log(Event{TS: ts, Order: o.ID, Kind: "new", Qty: qty, Price: limit, State: PendingNew})
	if r := m.checkParent(o, refPx); r != "" {
		o.State, o.Reason = Rejected, r
		m.log(Event{TS: ts, Order: o.ID, Kind: "reject", State: Rejected, Reason: r})
		return o
	}
	o.State = Open
	m.log(Event{TS: ts, Order: o.ID, Kind: "ack", State: Open})
	return o
}

func (m *OMS) checkParent(o *Order, refPx float64) string {
	l := m.Limits
	switch {
	case m.killed:
		return "kill_switch"
	case o.Qty <= 0:
		return "bad_qty"
	case l.MaxOrderQty > 0 && o.Qty > l.MaxOrderQty:
		return "max_order_qty"
	case l.MaxOrderNotional > 0 && o.Qty.Float64()*refPx > l.MaxOrderNotional:
		return "max_order_notional"
	}
	if l.MaxPosition > 0 {
		p := m.pos[o.Symbol] + model.Fixed(o.Side)*o.Qty
		// Include other working parents on the same symbol (worst case all fill).
		for _, x := range m.orders {
			if x.Parent == 0 && x.ID != o.ID && x.Symbol == o.Symbol && !x.State.Terminal() && x.State != PendingNew {
				p += model.Fixed(x.Side) * (x.Qty - x.CumQty)
			}
		}
		if p > l.MaxPosition || -p > l.MaxPosition {
			return "max_position"
		}
	}
	return ""
}

// NewChild creates a venue IOC child for part of a parent's leaves quantity. It is rejected
// (and returns an order in state Rejected) if it fails the price collar, the venue rate limit,
// or exceeds the parent's leaves.
func (m *OMS) NewChild(ts int64, parent ID, v model.Venue, qty, limit model.Fixed, refMid float64) (*Order, error) {
	p := m.orders[parent]
	if p == nil || p.Parent != 0 {
		return nil, ErrUnknown
	}
	if p.State.Terminal() || p.State == PendingNew {
		return nil, ErrTransition
	}
	m.next++
	c := &Order{ID: m.next, Parent: parent, Symbol: p.Symbol, Side: p.Side, Qty: qty, Limit: limit, Venue: v, Created: ts, Updated: ts}
	m.orders[c.ID] = c
	m.log(Event{TS: ts, Order: c.ID, Kind: "new", Qty: qty, Price: limit, State: PendingNew})
	reason := ""
	l := m.Limits
	switch {
	case m.killed:
		reason = "kill_switch"
	case qty <= 0 || qty > p.Leaves():
		reason = "exceeds_parent_leaves"
	case p.Limit > 0 && ((p.Side == Buy && limit > p.Limit) || (p.Side == Sell && limit < p.Limit)):
		reason = "through_parent_limit"
	case l.CollarBps > 0 && refMid > 0 && abs(limit.Float64()/refMid-1)*1e4 > l.CollarBps:
		reason = "price_collar"
	case l.MaxChildPerSec > 0 && m.rate(v, ts) >= l.MaxChildPerSec:
		reason = "rate_limit"
	}
	if reason != "" {
		c.State, c.Reason = Rejected, reason
		m.log(Event{TS: ts, Order: c.ID, Kind: "reject", State: Rejected, Reason: reason})
		return c, nil
	}
	m.recent[v] = append(m.recent[v], ts)
	c.State = Open
	p.Working += qty
	p.Kids = append(p.Kids, c.ID)
	m.log(Event{TS: ts, Order: c.ID, Kind: "ack", State: Open})
	return c, nil
}

func (m *OMS) rate(v model.Venue, ts int64) int {
	r := m.recent[v]
	i := 0
	for i < len(r) && r[i] <= ts-1_000_000_000 {
		i++
	}
	m.recent[v] = r[i:]
	return len(r) - i
}

// Fill applies an execution to a child (and its parent).
func (m *OMS) Fill(ts int64, child ID, qty, px model.Fixed, fee float64) error {
	c := m.orders[child]
	if c == nil || c.Parent == 0 {
		return ErrUnknown
	}
	if c.State != Open && c.State != PartiallyFilled {
		return fmt.Errorf("%w: fill in state %s", ErrTransition, c.State)
	}
	if qty <= 0 || c.CumQty+qty > c.Qty {
		return ErrOverfill
	}
	if (c.Side == Buy && px > c.Limit) || (c.Side == Sell && px < c.Limit) {
		return fmt.Errorf("fill at %s through limit %s", px, c.Limit)
	}
	p := m.orders[c.Parent]
	n := qty.Float64() * px.Float64()
	c.CumQty += qty
	c.Notional += n
	c.Fees += fee
	c.Updated = ts
	p.CumQty += qty
	p.Working -= qty
	p.Notional += n
	p.Fees += fee
	p.Updated = ts
	m.pos[c.Symbol] += model.Fixed(c.Side) * qty
	if c.CumQty == c.Qty {
		c.State = Filled
	} else {
		c.State = PartiallyFilled
	}
	m.log(Event{TS: ts, Order: c.ID, Kind: "fill", Qty: qty, Price: px, Fee: fee, State: c.State})
	m.parentState(ts, p)
	return nil
}

// Done ends a child: its unfilled remainder is canceled (IOC remainder / venue cancel).
func (m *OMS) Done(ts int64, child ID, reason string) error {
	c := m.orders[child]
	if c == nil || c.Parent == 0 {
		return ErrUnknown
	}
	if c.State != Open && c.State != PartiallyFilled {
		return fmt.Errorf("%w: cancel in state %s", ErrTransition, c.State)
	}
	p := m.orders[c.Parent]
	p.Working -= c.Qty - c.CumQty
	c.State, c.Reason, c.Updated = Canceled, reason, ts
	m.log(Event{TS: ts, Order: c.ID, Kind: "cancel", State: Canceled, Reason: reason})
	m.parentState(ts, p)
	return nil
}

func (m *OMS) parentState(ts int64, p *Order) {
	switch {
	case p.CumQty == p.Qty:
		p.State = Filled
	case p.CumQty > 0:
		p.State = PartiallyFilled
	}
	p.Updated = ts
}

// Expire ends a parent with unfilled quantity (horizon reached). Working children must be done.
func (m *OMS) Expire(ts int64, parent ID, reason string) error {
	p := m.orders[parent]
	if p == nil || p.Parent != 0 {
		return ErrUnknown
	}
	if p.State.Terminal() {
		return nil
	}
	if p.Working != 0 {
		return fmt.Errorf("%w: parent has working children", ErrTransition)
	}
	p.State, p.Reason, p.Updated = Expired, reason, ts
	m.log(Event{TS: ts, Order: p.ID, Kind: "expire", State: Expired, Reason: reason})
	return nil
}

// Check verifies the accounting invariants of every order; it returns the first violation.
func (m *OMS) Check() error {
	pos := map[string]model.Fixed{}
	for _, o := range m.orders {
		if o.CumQty < 0 || o.CumQty > o.Qty {
			return fmt.Errorf("order %d: cum %s qty %s", o.ID, o.CumQty, o.Qty)
		}
		if o.Parent != 0 {
			pos[o.Symbol] += model.Fixed(o.Side) * o.CumQty
			continue
		}
		var cum, working model.Fixed
		for _, k := range o.Kids {
			c := m.orders[k]
			cum += c.CumQty
			if !c.State.Terminal() {
				working += c.Qty - c.CumQty
			}
		}
		if cum != o.CumQty || working != o.Working || o.Working < 0 || o.CumQty+o.Working > o.Qty {
			return fmt.Errorf("parent %d: cum %s (kids %s) working %s (kids %s) qty %s", o.ID, o.CumQty, cum, o.Working, working, o.Qty)
		}
		if o.State == Filled && o.CumQty != o.Qty {
			return fmt.Errorf("parent %d filled with cum %s of %s", o.ID, o.CumQty, o.Qty)
		}
	}
	for s, p := range pos {
		if p != m.pos[s] {
			return fmt.Errorf("position %s: %s vs fills %s", s, m.pos[s], p)
		}
	}
	return nil
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
