package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	res "github.com/openabstractions/abstraction-resource/go/client"
	"github.com/openabstractions/abstraction-resource/go/gpufault"
	"github.com/openabstractions/abstraction-resource/go/holder"
)

// fakeLeases is leases@1 in memory: it records every call in order, so a
// test reads whether the lease came before the launch.
type fakeLeases struct {
	mu       sync.Mutex
	calls    []string
	refused  bool // every acquire reads not permitted
	renewed  int
	refuse   bool // every renewal reads the lease ended
	released []string
	next     int
}

func (f *fakeLeases) log(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeLeases) Acquire(resource string, amount int64, _ time.Duration) (res.AcquireResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(fmt.Sprintf("acquire %s %d", resource, amount))
	if f.refused {
		return res.AcquireResult{Outcome: res.AcquireOutcomeNotPermitted}, nil
	}
	f.next++
	id := fmt.Sprintf("lease-%d", f.next)
	return res.AcquireResult{Outcome: res.AcquireOutcomeAcquired, Lease: &res.Lease{ID: id, Resource: resource, Amount: amount}}, nil
}

func (f *fakeLeases) BindProcess(lease string, pid int) (res.ProcessBindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(fmt.Sprintf("bind %s %d", lease, pid))
	return res.ProcessBindResult{Outcome: res.ProcessBindOutcomeBound}, nil
}

func (f *fakeLeases) Renew(string) (res.LeaseChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed++
	return res.LeaseChange{Applied: !f.refuse}, nil
}

func (f *fakeLeases) Release(lease string) (res.LeaseChange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("release " + lease)
	f.released = append(f.released, lease)
	return res.LeaseChange{Applied: true}, nil
}

func (f *fakeLeases) Observe(ctx context.Context, _ string, _ time.Duration) (res.YieldPage, error) {
	<-ctx.Done()
	return res.YieldPage{}, ctx.Err()
}

func (f *fakeLeases) Answer(context.Context, string, res.YieldAnswer, string) (res.AnswerResult, error) {
	return res.AnswerResult{}, nil
}

func (f *fakeLeases) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls), slices.Clone(f.released), f.renewed
}

// fakeTree stands for the Lemonade tree: Start logs into the lease log, so
// the order of lease and launch is one sequence.
type fakeTree struct {
	leases  *fakeLeases
	mu      sync.Mutex
	pid     int
	starts  int
	stops   int
	ready   func(start int) // runs inside Start, after the launch, before it returns
	backend error
	// unconfirmed is a tree whose exit was seen and not confirmed: it
	// keeps its process id, reads not running, and no stop confirms it.
	unconfirmed bool
}

func (e *fakeTree) CheckBackend() error { return e.backend }

func (e *fakeTree) Start(ctx context.Context) error {
	e.leases.mu.Lock()
	e.leases.log("start")
	e.leases.mu.Unlock()
	e.mu.Lock()
	e.starts++
	e.pid = 4000 + e.starts
	start, ready := e.starts, e.ready
	e.mu.Unlock()
	if ready != nil {
		ready(start)
	}
	return ctx.Err()
}

func (e *fakeTree) PID() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid
}

func (e *fakeTree) Running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid != 0 && !e.unconfirmed
}

func (e *fakeTree) ExitUnconfirmed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pid != 0 && e.unconfirmed
}

// exitUnconfirmed makes the running tree one whose exit was seen and not
// confirmed.
func (e *fakeTree) exitUnconfirmed() {
	e.mu.Lock()
	e.unconfirmed = true
	e.mu.Unlock()
}

func (e *fakeTree) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.unconfirmed {
		return fmt.Errorf("%w: contained tree exit unconfirmed", server.ErrUnavailable)
	}
	if e.pid != 0 {
		e.stops++
	}
	e.pid = 0
	return nil
}

func (e *fakeTree) counts() (int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts, e.stops
}

func newTestCard(t *testing.T, leases *fakeLeases, tree *fakeTree, gpu *gpufault.Guard) *card {
	t.Helper()
	c, err := newCard(tree, cardOptions{Leases: leases, Amount: 1 << 30, Renew: 5 * time.Millisecond, Wait: time.Second, GPU: gpu,
		OnError: func(err error) { t.Logf("card: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The tree starts only under a lease of card:0 for the admission estimate,
// the lease is bound to Lemonade's process, a second request uses the
// running tree, and closing the provider stops the tree and gives the
// lease back (resource RES-L1, RES-L4; VISION 2026-10-01).
func TestTheTreeStartsUnderACardLeaseAndReleasesItAtExit(t *testing.T) {
	leases := &fakeLeases{}
	tree := &fakeTree{leases: leases}
	c := newTestCard(t, leases, tree, nil)
	for range 2 {
		if err := c.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	calls, _, _ := leases.snapshot()
	if want := []string{"acquire card:0 1073741824", "start", "bind lease-1 4001"}; !slices.Equal(calls, want) {
		t.Fatalf("calls %q, want %q", calls, want)
	}
	c.close()
	calls, released, _ := leases.snapshot()
	if starts, stops := tree.counts(); starts != 1 || stops != 1 || !slices.Equal(released, []string{"lease-1"}) {
		t.Fatalf("starts %d, stops %d, released %q (%q)", starts, stops, released, calls)
	}
	if calls[len(calls)-1] != "release lease-1" {
		t.Fatalf("the lease went back before the tree stopped: %q", calls)
	}
}

// A refused lease starts nothing and reads the model host's lease word; a
// backend that does not prove takes no lease at all.
func TestARefusedLeaseStartsNoTree(t *testing.T) {
	leases := &fakeLeases{refused: true}
	tree := &fakeTree{leases: leases}
	err := newTestCard(t, leases, tree, nil).ensure(context.Background())
	if reason := loadReason(err); reason != holder.WordNotPermitted {
		t.Fatalf("reason %q (%v)", reason, err)
	}
	if starts, _ := tree.counts(); starts != 0 {
		t.Fatal("the tree started without a lease")
	}
	leases = &fakeLeases{}
	tree = &fakeTree{leases: leases, backend: fmt.Errorf("%w: %w", server.ErrUnavailable, server.ErrBackendManifest)}
	err = newTestCard(t, leases, tree, nil).ensure(context.Background())
	if calls, _, _ := leases.snapshot(); len(calls) != 0 || loadReason(err) != "engine:backend-unavailable" {
		t.Fatalf("calls %q, reason %q", calls, loadReason(err))
	}
}

// A lease the service ends while the tree readies stops that tree and loads
// it once more under a new lease; a second end refuses the load lease:ended,
// with both trees stopped and both leases given back (holder.ErrLeaseEnded,
// as the model host reacts).
func TestALeaseEndedDuringReadinessStopsTheTree(t *testing.T) {
	leases := &fakeLeases{refuse: true}
	tree := &fakeTree{leases: leases}
	// Each launch readies only after a renewal the service refused.
	tree.ready = func(int) {
		_, _, before := leases.snapshot()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, _, renewed := leases.snapshot(); renewed > before {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	c := newTestCard(t, leases, tree, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)
	err := c.ensure(context.Background())
	if reason := loadReason(err); reason != holder.WordEnded {
		t.Fatalf("reason %q (%v)", reason, err)
	}
	starts, stops := tree.counts()
	_, released, _ := leases.snapshot()
	if starts != 2 || stops != 2 || !slices.Equal(released, []string{"lease-1", "lease-2"}) {
		t.Fatalf("starts %d, stops %d, released %q", starts, stops, released)
	}
}

// A driver reset stops the running tree and gives its lease back through
// the gpufault Guard the tree's holder is attached to; the fault is
// recorded with the tree's name, and a load during the reset's settle is
// refused gpu:resetting without a lease (resource RES-G2, RES-G4, RES-G6).
func TestADriverResetStopsTheTreeAndIsRecorded(t *testing.T) {
	faults := make(chan gpufault.Fault, 1)
	guard, err := gpufault.New(gpufault.Options{Settle: time.Hour, Window: 20 * time.Millisecond,
		Probe: func(context.Context) error { return nil }, OnFault: func(f gpufault.Fault) { faults <- f }})
	if err != nil {
		t.Fatal(err)
	}
	leases := &fakeLeases{}
	tree := &fakeTree{leases: leases}
	c := newTestCard(t, leases, tree, guard)
	if err := c.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	guard.Report(gpufault.Event{Kind: gpufault.Reset, At: time.Now(), Source: "WerKernel 1001 WATCHDOG"})
	if starts, stops := tree.counts(); starts != 1 || stops != 1 {
		t.Fatalf("starts %d, stops %d after the reset", starts, stops)
	}
	if _, released, _ := leases.snapshot(); !slices.Equal(released, []string{"lease-1"}) {
		t.Fatalf("released %q", released)
	}
	select {
	case f := <-faults:
		if !slices.Equal(f.Engines, []string{treeComponent}) || len(f.Unreleased) != 0 || f.Line == "" {
			t.Fatalf("fault %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no fault was recorded")
	}
	err = c.ensure(context.Background())
	if reason := loadReason(err); reason != gpufault.WordResetting {
		t.Fatalf("reason %q (%v)", reason, err)
	}
	calls, _, _ := leases.snapshot()
	acquires := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "acquire ") {
			acquires++
		}
	}
	if acquires != 1 {
		t.Fatalf("a load during the settle took a lease: %q", calls)
	}
}

// providerEndpoint is the local endpoint a test provider serves on. The
// provider admits only its designated runtime by Program proof, so a test
// that serves is skipped wherever the socket endpoint cannot carry that proof
// (macOS today). Lemonade's provider is packaged for Windows x64 alone
// (packaging/windows-amd64.json).
func providerEndpoint(t *testing.T, name string) string {
	t.Helper()
	endpoint := listen.Endpoint(name)
	if err := listen.CanEver(endpoint, listen.Program); err != nil {
		t.Skipf("this platform's socket endpoint cannot carry Program proof: %v", err)
	}
	return endpoint
}

// The provider refuses to run without the lease service, and holds card:0
// for the declared models' pinned sizes.
func TestTheProviderNeedsTheLeaseService(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Endpoint: providerEndpoint(t, fmt.Sprintf("lemonade-card-test-%d", os.Getpid())), RuntimePrograms: []string{os.Args[0]}, StoreRoot: filepath.Join(dir, "store"),
		RecordPath: filepath.Join(dir, "records.json"), Server: server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234,
			Models: []string{"user.LocalApprovedKlein"}, Transcription: true}}
	if _, err := New(cfg); err == nil {
		t.Fatal("a provider without the lease service was made")
	}
	cfg.Leases = &fakeLeases{}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	klein, _ := server.FindImageModel("user.LocalApprovedKlein")
	if want := klein.Bytes() + server.WhisperLargeV3TurboModel.Files[0].Size; p.card.amount != want {
		t.Fatalf("amount %d, want %d", p.card.amount, want)
	}
	cfg.Server.Models = []string{"user.Unpinned"}
	if _, err := New(cfg); !errors.Is(err, server.ErrConfiguration) || !strings.Contains(err.Error(), "pinned size") {
		t.Fatalf("an unpinned model without an amount: %v", err)
	}
}
