package oms

import (
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/eshaan2418/sorx/internal/mm/model"
)

const btc = model.Fixed(model.Scale)

func px(x float64) model.Fixed { return model.Fixed(x * model.Scale) }

func TestLifecycle(t *testing.T) {
	m := New(Limits{})
	p := m.NewParent(1, "BTC-USD", Buy, 2*btc, 0, 78000)
	if p.State != Open {
		t.Fatal(p.State)
	}
	c, err := m.NewChild(2, p.ID, model.Kraken, btc+btc/2, px(78010), 78000)
	if err != nil || c.State != Open || p.Working != btc+btc/2 || p.Leaves() != btc/2 {
		t.Fatalf("%v %+v %+v", err, c, p)
	}
	if err := m.Fill(3, c.ID, btc, px(78005), 31.2); err != nil {
		t.Fatal(err)
	}
	if c.State != PartiallyFilled || p.State != PartiallyFilled || p.CumQty != btc {
		t.Fatal(c.State, p.State)
	}
	if err := m.Fill(4, c.ID, btc, px(78005), 0); !errors.Is(err, ErrOverfill) {
		t.Fatal("overfill accepted", err)
	}
	if err := m.Fill(4, c.ID, btc/4, px(78011), 0); err == nil {
		t.Fatal("fill through limit accepted")
	}
	if err := m.Done(5, c.ID, "ioc"); err != nil || c.State != Canceled || p.Working != 0 {
		t.Fatal(err, c.State, p.Working)
	}
	if err := m.Fill(6, c.ID, btc/4, px(78000), 0); !errors.Is(err, ErrTransition) {
		t.Fatal("fill after cancel accepted", err)
	}
	c2, _ := m.NewChild(7, p.ID, model.Coinbase, btc, px(78020), 78000)
	if err := m.Fill(8, c2.ID, btc, px(78020), 0); err != nil || p.State != Filled || c2.State != Filled {
		t.Fatal(err, p.State)
	}
	if m.Position("BTC-USD") != 2*btc {
		t.Fatal(m.Position("BTC-USD"))
	}
	if _, err := m.NewChild(9, p.ID, model.Kraken, btc, px(78000), 78000); !errors.Is(err, ErrTransition) {
		t.Fatal("child on filled parent")
	}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	if got := p.AvgPx(); got < 78005 || got > 78020 {
		t.Fatal(got)
	}
}

func TestRiskChecks(t *testing.T) {
	m := New(Limits{MaxOrderQty: 10 * btc, MaxOrderNotional: 500_000, MaxPosition: 8 * btc, CollarBps: 50, MaxChildPerSec: 2})
	if p := m.NewParent(1, "BTC-USD", Buy, 11*btc, 0, 78000); p.Reason != "max_order_qty" {
		t.Fatal(p.Reason)
	}
	if p := m.NewParent(1, "BTC-USD", Buy, 7*btc, 0, 78000); p.Reason != "max_order_notional" {
		t.Fatal(p.Reason)
	}
	a := m.NewParent(1, "BTC-USD", Buy, 5*btc, 0, 78000)
	if a.State != Open {
		t.Fatal(a.Reason)
	}
	if p := m.NewParent(1, "BTC-USD", Buy, 4*btc, 0, 78000); p.Reason != "max_position" { // 5 working + 4 > 8
		t.Fatal(p.Reason)
	}
	if p := m.NewParent(1, "BTC-USD", Sell, 4*btc, 0, 78000); p.State != Open { // reduces exposure
		t.Fatal(p.Reason)
	}
	if c, _ := m.NewChild(2, a.ID, model.Kraken, btc, px(78500), 78000); c.Reason != "price_collar" { // 64 bps
		t.Fatal(c.Reason)
	}
	for i := 0; i < 2; i++ {
		if c, _ := m.NewChild(int64(3+i), a.ID, model.Kraken, btc/10, px(78010), 78000); c.State != Open {
			t.Fatal(c.Reason)
		}
	}
	if c, _ := m.NewChild(5, a.ID, model.Kraken, btc/10, px(78010), 78000); c.Reason != "rate_limit" {
		t.Fatal(c.Reason)
	}
	if c, _ := m.NewChild(5, a.ID, model.Coinbase, btc/10, px(78010), 78000); c.State != Open { // other venue
		t.Fatal(c.Reason)
	}
	if c, _ := m.NewChild(1_000_000_010, a.ID, model.Kraken, btc/10, px(78010), 78000); c.State != Open { // window rolled
		t.Fatal(c.Reason)
	}
	m.Kill()
	if p := m.NewParent(2, "ETH-USD", Buy, btc, 0, 2500); p.Reason != "kill_switch" {
		t.Fatal(p.Reason)
	}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
}

// Random sequences of children, fills (some illegal) and cancels never break the invariants.
func TestRandomSequencesKeepInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	m := New(Limits{})
	var parents []*Order
	var live []ID
	for step := 0; step < 200000; step++ {
		ts := int64(step)
		switch k := r.IntN(10); {
		case k == 0 || len(parents) == 0:
			s := Buy
			if r.IntN(2) == 0 {
				s = Sell
			}
			parents = append(parents, m.NewParent(ts, "BTC-USD", s, model.Fixed(1+r.IntN(1000))*btc/100, 0, 78000))
		case k <= 3:
			p := parents[r.IntN(len(parents))]
			q := model.Fixed(r.Int64N(int64(p.Qty))) + 1
			c, err := m.NewChild(ts, p.ID, model.Venue(1+r.IntN(4)), q, px(78000+float64(p.Side)*float64(r.IntN(100))), 0)
			if err == nil && c.State == Open {
				live = append(live, c.ID)
			}
		case k <= 7 && len(live) > 0:
			i := r.IntN(len(live))
			c := m.Get(live[i])
			q := model.Fixed(r.Int64N(int64(c.Qty))) + 1 // may overfill: must be rejected
			_ = m.Fill(ts, c.ID, q, c.Limit-model.Fixed(c.Side)*px(float64(r.IntN(5))), 0.1)
			if c.State.Terminal() {
				live = append(live[:i], live[i+1:]...)
			}
		case len(live) > 0:
			i := r.IntN(len(live))
			_ = m.Done(ts, live[i], "ioc")
			live = append(live[:i], live[i+1:]...)
		}
		if step%5000 == 0 {
			if err := m.Check(); err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
	}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
}

// Arbitrary operation sequences (including illegal ones) never break the invariants.
func FuzzOMS(f *testing.F) {
	f.Add([]byte{0, 10, 1, 3, 2, 5, 3, 1, 2, 9})
	f.Fuzz(func(t *testing.T, ops []byte) {
		m := New(Limits{MaxPosition: 50 * btc, CollarBps: 200, MaxChildPerSec: 20})
		var ids []ID
		for i := 0; i+1 < len(ops); i += 2 {
			op, arg := ops[i]%5, int64(ops[i+1])
			ts := int64(i) * 1_000_000
			switch op {
			case 0:
				p := m.NewParent(ts, "BTC-USD", Side(1-2*(arg%2)), model.Fixed(1+arg)*btc/10, 0, 78000)
				ids = append(ids, p.ID)
			case 1:
				if len(ids) > 0 {
					if c, err := m.NewChild(ts, ids[int(arg)%len(ids)], model.Venue(1+arg%4), model.Fixed(1+arg)*btc/20, px(78000+float64(arg-128)), 78000); err == nil {
						ids = append(ids, c.ID)
					}
				}
			case 2:
				if len(ids) > 0 {
					_ = m.Fill(ts, ids[int(arg)%len(ids)], model.Fixed(1+arg)*btc/40, px(78000+float64(arg%7)), 0.5)
				}
			case 3:
				if len(ids) > 0 {
					_ = m.Done(ts, ids[int(arg)%len(ids)], "x")
				}
			case 4:
				if len(ids) > 0 {
					_ = m.Expire(ts, ids[int(arg)%len(ids)], "x")
				}
			}
		}
		if err := m.Check(); err != nil {
			t.Fatal(err)
		}
	})
}
