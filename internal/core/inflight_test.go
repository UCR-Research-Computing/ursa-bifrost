package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
)

// gateBackend records the most accounting runs it saw at once.
type gateBackend struct {
	cur, peak atomic.Int32
	delay     time.Duration
}

func (b *gateBackend) Run(_ context.Context, _ backend.Command) ([]byte, error) {
	n := b.cur.Add(1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(b.delay)
	b.cur.Add(-1)
	return []byte("[]"), nil
}
func (b *gateBackend) Name() string { return "gate" }

// TestAccountingCappedAcrossPeople: 12 people asking for accounting at once
// run at most AcctSlots sacct at a time, and every one of them is answered.
func TestAccountingCappedAcrossPeople(t *testing.T) {
	gate := NewGate()
	be := &gateBackend{delay: 30 * time.Millisecond}
	var wg sync.WaitGroup
	var fails atomic.Int32
	for i := 0; i < 12; i++ {
		s, _ := svcFor(t, "p"+string(rune('a'+i)), nil, 0)
		s.Backend, s.Gate = be, gate
		c, err := backend.SacctJob("1234")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.run(context.Background(), c); err != nil {
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 {
		t.Errorf("%d accounting calls failed", fails.Load())
	}
	if p := be.peak.Load(); p > AcctSlots || p < 2 {
		t.Errorf("peak concurrent sacct %d, want 2..%d", p, AcctSlots)
	}
}

// TestNonAccountingNotGated: node state is not held behind accounting slots.
func TestNonAccountingNotGated(t *testing.T) {
	gate := NewGate()
	for i := 0; i < AcctSlots; i++ {
		gate.acct <- struct{}{} // accounting fully busy
	}
	s, _ := svcFor(t, "alice", nil, 0)
	s.Gate = gate
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.run(ctx, backend.Nodes()); err != nil {
		t.Fatalf("nodes blocked by accounting: %v", err)
	}
	c, _ := backend.SacctJob("1")
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := s.run(short, c); !errors.Is(err, ErrBusy) {
		t.Errorf("accounting with no free slot: %v, want ErrBusy", err)
	}
}

// TestInFlightCapRefusesAndReleases: past MaxInFlight a caller is refused at
// once (audited busy); slots come back however the call ends; another person
// is unaffected.
func TestInFlightCapRefusesAndReleases(t *testing.T) {
	s, _ := svcFor(t, "alice", nil, 0)
	other, _ := svcFor(t, "bob", nil, 0)
	s.MaxInFlight, other.MaxInFlight = 2, 2
	hold := make(chan struct{})
	started := make(chan struct{}, 2)
	block := func(ctx context.Context) (int, error) { started <- struct{}{}; <-hold; return 1, nil }
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = Call(context.Background(), s, "t", "x", "R1", nil, false, block) }()
	}
	<-started
	<-started
	_, err := Call(context.Background(), s, "t", "x", "R1", nil, false, func(ctx context.Context) (int, error) { return 1, nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("third call: %v, want ErrBusy", err)
	}
	if _, err := Call(context.Background(), other, "t", "x", "R1", nil, false, func(ctx context.Context) (int, error) { return 1, nil }); err != nil {
		t.Errorf("another person refused: %v", err)
	}
	close(hold)
	wg.Wait()
	_, _ = Call(context.Background(), s, "t", "x", "R1", nil, false, func(ctx context.Context) (int, error) { return 0, errors.New("boom") })
	if s.Running() != 0 {
		t.Errorf("slots leaked: %d running", s.Running())
	}
	if _, err := Call(context.Background(), s, "t", "x", "R1", nil, false, func(ctx context.Context) (int, error) { return 1, nil }); err != nil {
		t.Errorf("after release: %v", err)
	}
}
