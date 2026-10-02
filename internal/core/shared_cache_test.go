package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UCR-Research-Computing/ursa-bifrost/internal/backend"
	"github.com/UCR-Research-Computing/ursa-bifrost/internal/config"
)

// countingBackend counts runs per command and can be slowed down.
type countingBackend struct {
	mu    sync.Mutex
	runs  map[string]int
	delay time.Duration
	fail  bool
	who   string
}

func (b *countingBackend) Run(_ context.Context, c backend.Command) ([]byte, error) {
	time.Sleep(b.delay)
	b.mu.Lock()
	b.runs[c.String()]++
	b.mu.Unlock()
	if b.fail {
		return nil, errors.New("login node down")
	}
	return []byte(b.who + ":" + c.String()), nil
}

func (b *countingBackend) Name() string { return "counting" }

func (b *countingBackend) n(c backend.Command) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs[c.String()]
}

func svcFor(t *testing.T, who string, shared *SharedCache, delay time.Duration) (*Service, *countingBackend) {
	t.Helper()
	cfg := config.Default()
	be := &countingBackend{runs: map[string]int{}, delay: delay, who: who}
	s := NewService(cfg, be, nil)
	s.Principal = who
	s.Shared = shared
	return s, be
}

// TestPublicOutputIsSharedAcrossPeople: node state asked by two people runs
// once; each person's own jobs run once per person and are never shared.
func TestPublicOutputIsSharedAcrossPeople(t *testing.T) {
	shared := NewSharedCache()
	a, ba := svcFor(t, "alice", shared, 0)
	b, bb := svcFor(t, "bob", shared, 0)
	ctx := context.Background()
	if _, err := a.run(ctx, backend.Nodes()); err != nil {
		t.Fatal(err)
	}
	out, err := b.run(ctx, backend.Nodes())
	if err != nil {
		t.Fatal(err)
	}
	if ba.n(backend.Nodes())+bb.n(backend.Nodes()) != 1 {
		t.Errorf("public command ran %d times for two people", ba.n(backend.Nodes())+bb.n(backend.Nodes()))
	}
	if string(out) != "alice:"+backend.Nodes().String() {
		t.Errorf("bob did not get the shared answer: %s", out)
	}
	qa, _ := backend.SqueueUser("alice_ucr_edu")
	qb, _ := backend.SqueueUser("alice_ucr_edu") // same command text, different person
	ra, _ := a.run(ctx, qa)
	rb, _ := b.run(ctx, qb)
	if string(ra) == string(rb) {
		t.Error("a per-person command was answered from another person's run")
	}
	if ba.n(qa) != 1 || bb.n(qb) != 1 {
		t.Errorf("per-person runs: alice %d bob %d", ba.n(qa), bb.n(qb))
	}
}

// TestSingleFlight: 20 concurrent identical requests run the command once,
// for public output across people and for a person's own output.
func TestSingleFlight(t *testing.T) {
	shared := NewSharedCache()
	var svcs []*Service
	var bes []*countingBackend
	for _, who := range []string{"a", "b", "c", "d"} {
		s, be := svcFor(t, who, shared, 50*time.Millisecond)
		svcs = append(svcs, s)
		bes = append(bes, be)
	}
	var wg sync.WaitGroup
	var errs atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svcs[i%4].run(context.Background(), backend.SqueueAll()); err != nil {
				errs.Add(1)
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, be := range bes {
		total += be.n(backend.SqueueAll())
	}
	if total != 1 || errs.Load() != 0 {
		t.Errorf("20 concurrent public requests ran %d times (%d errors)", total, errs.Load())
	}
	own, _ := backend.SqueueUser("a_ucr_edu")
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = svcs[0].run(context.Background(), own) }()
	}
	wg.Wait()
	if bes[0].n(own) != 1 {
		t.Errorf("10 concurrent identical own requests ran %d times", bes[0].n(own))
	}
}

// TestErrorsAreNotShared: a failure reaches the waiters but is not stored, so
// the next request tries again.
func TestErrorsAreNotShared(t *testing.T) {
	shared := NewSharedCache()
	a, ba := svcFor(t, "alice", shared, 0)
	ba.fail = true
	if _, err := a.run(context.Background(), backend.Nodes()); err == nil {
		t.Fatal("expected error")
	}
	ba.fail = false
	b, bb := svcFor(t, "bob", shared, 0)
	if _, err := b.run(context.Background(), backend.Nodes()); err != nil {
		t.Fatalf("error was cached: %v", err)
	}
	if bb.n(backend.Nodes()) != 1 {
		t.Error("bob's request did not run after alice's failure")
	}
}

// TestWritesAreNeverShared: a write command (A1) always runs, never stored
// and never coalesced with a concurrent identical write (a double click must
// still reach the scheduler twice, where Slurm decides).
func TestWritesAreNeverShared(t *testing.T) {
	shared := NewSharedCache()
	a, ba := svcFor(t, "alice", shared, 50*time.Millisecond)
	c, err := backend.Scancel("123")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = a.run(context.Background(), c)
	_, _ = a.run(context.Background(), c)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = a.run(context.Background(), c) }()
	}
	wg.Wait()
	if ba.n(c) != 5 {
		t.Errorf("write ran %d times, want 5", ba.n(c))
	}
	if shared.Runs != 0 {
		t.Errorf("a write went through the shared cache (%d runs)", shared.Runs)
	}
}

// TestPublicSharedOnlyWhenFresh: a shared entry older than the TTL is refetched.
func TestPublicSharedOnlyWhenFresh(t *testing.T) {
	shared := NewSharedCache()
	now := time.Now()
	shared.now = func() time.Time { return now }
	a, ba := svcFor(t, "alice", shared, 0)
	b, bb := svcFor(t, "bob", shared, 0)
	_, _ = a.run(context.Background(), backend.Nodes())
	now = now.Add(time.Hour)
	b.Now = func() time.Time { return now }
	_, _ = b.run(context.Background(), backend.Nodes())
	if ba.n(backend.Nodes())+bb.n(backend.Nodes()) != 2 {
		t.Error("stale shared entry was served")
	}
}

// TestConcurrentPrivateRunsStaySeparate: two people running the same
// per-person command at the same moment each get their own run (their own
// identity), never the other's answer through single-flight.
func TestConcurrentPrivateRunsStaySeparate(t *testing.T) {
	shared := NewSharedCache()
	a, ba := svcFor(t, "alice", shared, 50*time.Millisecond)
	b, bb := svcFor(t, "bob", shared, 50*time.Millisecond)
	c, _ := backend.SqueueUser("same_user")
	var wg sync.WaitGroup
	var ra, rb []byte
	wg.Add(2)
	go func() { defer wg.Done(); ra, _ = a.run(context.Background(), c) }()
	go func() { defer wg.Done(); rb, _ = b.run(context.Background(), c) }()
	wg.Wait()
	if string(ra) != "alice:"+c.String() || string(rb) != "bob:"+c.String() {
		t.Errorf("private answers crossed: alice=%q bob=%q", ra, rb)
	}
	if ba.n(c) != 1 || bb.n(c) != 1 {
		t.Errorf("runs: alice %d bob %d", ba.n(c), bb.n(c))
	}
}
