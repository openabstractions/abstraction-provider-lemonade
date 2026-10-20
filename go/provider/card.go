package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	"github.com/openabstractions/abstraction-resource/go/gpufault"
	"github.com/openabstractions/abstraction-resource/go/holder"
)

// CardResource is the card the Lemonade tree's engines load into: the
// resource of its lease and the card:0 its installed declaration names, so
// the runtime writes this program's hold rules through its one card-rules
// writer (facade CONTRACT.md FAC-R3).
const CardResource = "card:0"

// treeComponent is the holder's one component: the Lemonade process with
// every engine it starts beneath it (llama-server, sd-server, whisper-server,
// koko). Lemonade loads and unloads those engines itself, so the lease
// covers the tree and ends when the tree stops.
const treeComponent = "lemonade"

// reasonLeaseUnavailable is a lease the resource service did not grant for
// a reason the kit's lease words do not name, such as no service answering.
const reasonLeaseUnavailable = "lease:unavailable"

// engine is the owned Lemonade tree the card lease covers; *server.Server
// is the one this provider runs.
type engine interface {
	CheckBackend() error
	Start(ctx context.Context) error
	PID() int
	Running() bool
	ExitUnconfirmed() bool
	Close() error
}

// cardOptions is how the tree holds its card: the lease service, the card,
// the admission estimate, the holder's timings and the GPU fault handling.
type cardOptions struct {
	Leases            holder.Leases
	Resource          string
	Amount            int64
	Idle, Renew, Wait time.Duration
	GPU               *gpufault.Guard
	OnError           func(error)
}

// card loads the Lemonade tree under a lease of the card through the
// resource kit's holder, the owner the model host and TheNoise use: the
// lease is taken before the tree starts, renewed while it runs, given back
// when the tree stops, and answered on a yield (resource CONTRACT.md
// RES-L1..RES-L4, VISION 2026-10-01). The GPU fault handling refuses a load
// during a reset or Modern Standby and stops the tree on a reset
// (RES-G1..RES-G5).
type card struct {
	holder *holder.Holder
	engine engine
	amount int64
	report func(error)
	// active is the provider's own work, bound once the inference provider
	// exists (bindActive).
	mu     sync.Mutex
	active func() bool
}

func newCard(e engine, opts cardOptions) (*card, error) {
	if opts.Leases == nil {
		return nil, errors.New("lemonade: the lease service is required")
	}
	if opts.Amount <= 0 {
		return nil, errors.New("lemonade: the card lease needs a positive admission estimate")
	}
	if opts.Resource == "" {
		opts.Resource = CardResource
	}
	c := &card{engine: e, amount: opts.Amount, report: opts.OnError}
	var admit func(context.Context, string) error
	if gpu := opts.GPU; gpu != nil {
		admit = func(ctx context.Context, _ string) error { return gpu.AdmitLoad(ctx) }
	}
	h, err := holder.New(holder.Options{Leases: opts.Leases, Resource: opts.Resource, Idle: opts.Idle, Renew: opts.Renew, Wait: opts.Wait,
		Admit: admit, OnError: opts.OnError,
		// The tree is in use while any declared model has an admitted,
		// running or retained operation (inference Provider.ActiveForModel).
		Active: func(string) bool {
			c.mu.Lock()
			active := c.active
			c.mu.Unlock()
			return active != nil && active()
		}})
	if err != nil {
		return nil, err
	}
	c.holder = h
	if opts.GPU != nil {
		// A driver reset stops the tree and gives its lease back.
		opts.GPU.Attach(h)
	}
	return c, nil
}

// bindActive gives the card the provider's view of its own work.
func (c *card) bindActive(active func() bool) {
	c.mu.Lock()
	c.active = active
	c.mu.Unlock()
}

// ensure runs the tree under the card lease: the backend proof before any
// lease, then the lease, then the launch and its readiness. A running tree
// is used as it is. A lease the service ends while the tree readies stops
// the tree, which loads once more under a new lease; a second end refuses
// the load (holder.ErrLeaseEnded).
func (c *card) ensure(ctx context.Context) error {
	return c.holder.Ensure(ctx, treeComponent, func() (holder.Component, error) {
		if err := c.engine.CheckBackend(); err != nil {
			return holder.Component{}, err
		}
		return holder.Component{Amount: c.amount, Start: func() (holder.Owned, error) {
			if err := c.engine.Start(ctx); err != nil {
				return nil, err
			}
			pid := c.engine.PID()
			if pid == 0 {
				return nil, fmt.Errorf("%w: the tree exited once it was ready", server.ErrUnavailable)
			}
			return &ownedTree{engine: c.engine, pid: pid, report: c.report}, nil
		}}, nil
	})
}

// run renews the lease, unloads an idle or exited tree and answers yields
// until ctx ends.
func (c *card) run(ctx context.Context) { c.holder.Run(ctx) }

// close stops the tree and gives its lease back.
func (c *card) close() { c.holder.Close() }

// ownedTree is one launch of the tree, named by its Lemonade process: a
// later launch is another instance, which Stop leaves alone.
type ownedTree struct {
	engine engine
	pid    int
	report func(error)
}

func (o *ownedTree) PID() int { return o.pid }

// Ready confirms the launch still runs: Start already waited for its
// health and its loopback check.
func (o *ownedTree) Ready(context.Context) error {
	if !o.Alive() {
		return fmt.Errorf("%w: the tree exited before its lease covered it", server.ErrUnavailable)
	}
	return nil
}

func (o *ownedTree) Alive() bool { return o.engine.PID() == o.pid && o.engine.Running() }

// Stop stops this launch's whole tree and reports true once its exit is
// confirmed; a launch already gone reads stopped. A tree whose exit was seen
// but not confirmed reads not stopped, and its lease stays held
// (holder.ExitConfirmer).
func (o *ownedTree) Stop() bool {
	if o.engine.PID() == o.pid {
		if err := o.engine.Close(); err != nil && o.report != nil {
			o.report(err)
		}
	}
	return !o.Alive() && o.ExitConfirmed()
}

// ExitConfirmed reports whether this launch's tree is confirmed gone, or was
// never seen to end unconfirmed.
func (o *ownedTree) ExitConfirmed() bool {
	return o.engine.PID() != o.pid || !o.engine.ExitUnconfirmed()
}

// loadReason is the admission reason of a load the card refused: the GPU
// fault handling's word, the lease outcome's, or the launch's own.
func loadReason(err error) string {
	var refusal *gpufault.Refusal
	if errors.As(err, &refusal) {
		return refusal.Word
	}
	var failure *holder.Failure
	if !errors.As(err, &failure) {
		return server.RefusalReason(err)
	}
	if failure.Stage != holder.StageAcquire {
		return server.RefusalReason(failure.Err)
	}
	// The lease refusal words are the resource kit's (holder.Failure.Word).
	if word := failure.Word(); word != "" {
		return word
	}
	return reasonLeaseUnavailable
}
