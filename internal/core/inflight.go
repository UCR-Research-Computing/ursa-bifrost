package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrBusy means the caller already has MaxInFlight calls running.
var ErrBusy = errors.New("busy")

// MaxInFlight is how many calls one caller (a person, or a program for one
// person: one Service) may have running at once on the hosted server. The 8 SSH
// sessions per person queue anything above 8; past 16 a client is looping
// without waiting, and is refused at once instead of piling up behind itself.
const MaxInFlight = 16

// AcctSlots caps accounting (sacct) runs at once across everyone on the hosted
// server: slurmdbd is one shared daemon, and accounting is the slowest read (7 s
// alone, 10-20 s several at once). Others wait their turn (bounded by the
// command timeout), so the database is not flooded.
const AcctSlots = 4

// Gate is shared by every person's Service on the hosted server.
type Gate struct {
	acct chan struct{}
}

// NewGate returns the server-wide gate.
func NewGate() *Gate { return &Gate{acct: make(chan struct{}, AcctSlots)} }

// acquireAcct waits for an accounting slot or ctx.
func (g *Gate) acquireAcct(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	select {
	case g.acct <- struct{}{}:
		return func() { <-g.acct }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: accounting is busy for everyone right now; try again shortly", ErrBusy)
	}
}

// inFlight counts one Service's running calls.
type inFlight struct {
	mu sync.Mutex
	n  int
}

func (f *inFlight) acquire(max int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n >= max {
		return false
	}
	f.n++
	return true
}

func (f *inFlight) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n > 0 {
		f.n--
	}
}

// Running reports this Service's calls in flight.
func (s *Service) Running() int {
	s.flights.mu.Lock()
	defer s.flights.mu.Unlock()
	return s.flights.n
}
