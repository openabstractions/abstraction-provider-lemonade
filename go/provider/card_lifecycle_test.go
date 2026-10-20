package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	res "github.com/openabstractions/abstraction-resource/go/client"
)

// RES-L4 through the holder's rule: a tree whose exit was seen but not
// confirmed keeps its lease through the liveness pass and a yield.
func TestAnUnconfirmedTreeExitKeepsTheLease(t *testing.T) {
	leases := &fakeLeases{}
	tree := &fakeTree{leases: leases}
	c := newTestCard(t, leases, tree, nil)
	if err := c.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	tree.exitUnconfirmed()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, renewed := leases.snapshot(); renewed >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if answer, _ := c.holder.Yield(res.YieldRequest{Resource: CardResource, Lease: "lease-1", Amount: c.amount}); answer == res.YieldAnswerYielded {
		t.Fatal("an unconfirmed tree yielded")
	}
	cancel()
	<-done
	if _, released, _ := leases.snapshot(); slices.Contains(released, "lease-1") {
		t.Fatalf("the lease of an unconfirmed tree was released: %q", released)
	}
}

// servedProviders numbers the served test providers' endpoints.
var servedProviders atomic.Int64

// newServedProvider is a provider whose card runs fakeTree in place of
// Lemonade.
func newServedProvider(t *testing.T, leases *fakeLeases) (*Provider, *fakeTree) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{Endpoint: providerEndpoint(t, fmt.Sprintf("lemonade-card-serve-%d-%d", os.Getpid(), servedProviders.Add(1))),
		RuntimePrograms: []string{os.Args[0]}, StoreRoot: filepath.Join(dir, "store"), RecordPath: filepath.Join(dir, "records.json"),
		Leases: leases, Renew: 5 * time.Millisecond, Wait: time.Second,
		Server: server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234,
			Models: []string{"user.LocalApprovedKlein"}}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tree := &fakeTree{leases: leases}
	p.card.engine = tree
	return p, tree
}

// Serve, once its context ends, stops the tree and gives the lease back; so
// does Close.
func TestTheProviderReleasesTheLeaseWhenItStops(t *testing.T) {
	t.Run("serve", func(t *testing.T) {
		leases := &fakeLeases{}
		p, tree := newServedProvider(t, leases)
		if err := p.card.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan error, 1)
		go func() { served <- p.Serve(ctx) }()
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Fatal("Serve did not return")
		}
		if _, released, _ := leases.snapshot(); !slices.Equal(released, []string{"lease-1"}) {
			t.Fatalf("released %q after Serve", released)
		}
		if _, stops := tree.counts(); stops != 1 {
			t.Fatalf("the tree stopped %d time(s)", stops)
		}
	})
	t.Run("close", func(t *testing.T) {
		leases := &fakeLeases{}
		p, _ := newServedProvider(t, leases)
		if err := p.card.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		//unchecked: the provider's close result is not what this test reads
		p.Close()
		if _, released, _ := leases.snapshot(); !slices.Equal(released, []string{"lease-1"}) {
			t.Fatalf("released %q after Close", released)
		}
	})
}

// A yield is refused while an admitted image operation of the provider is
// held: the card's yield reads the inference provider's own work.
func TestAYieldIsRefusedWhileAnImageOperationIsHeld(t *testing.T) {
	leases := &fakeLeases{}
	p, _ := newServedProvider(t, leases)
	defer p.Close()
	ctx := inference.WithDesignatedRuntime(inference.WithRoute(context.Background(), inference.RouteNative, os.Args[0]))
	caller := inference.Subject{Account: "test", Program: os.Args[0]}
	a := p.Host.Provider.StartImage(ctx, caller, wire.ImageRequest{Model: "user.LocalApprovedKlein", Prompt: "a lighthouse",
		Mode: wire.ImageModeGenerate, Count: 1, Size: "512x512"})
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if lease, loaded := p.card.holder.Snapshot(treeComponent); !loaded || lease != "lease-1" {
		t.Fatalf("the admission loaded no tree under lease-1: %q %v", lease, loaded)
	}
	if answer, reason := p.card.holder.Yield(res.YieldRequest{Resource: CardResource, Lease: "lease-1", Amount: p.card.amount}); answer == res.YieldAnswerYielded {
		t.Fatalf("a yield with an image operation held: %s %s", answer, reason)
	}
	if _, released, _ := leases.snapshot(); len(released) != 0 {
		t.Fatalf("released %q", released)
	}
}
