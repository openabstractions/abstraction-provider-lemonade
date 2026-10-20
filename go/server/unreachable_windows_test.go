package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginehttp"
)

// A Lemonade whose address every connect fails to reach, with an error a
// starting server never gives, ends readiness after
// enginehttp.UnreachableFor, not after its whole budget, and its child is
// reaped. Port 0 stands in: a Windows connect to it reads
// WSAEADDRNOTAVAIL every time.
func TestUnreachableAddressEndsReadinessEarly(t *testing.T) {
	cfg := fakeConfig(t, "helper-stall")
	cfg.Ready = time.Minute
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.base = "http://127.0.0.1:0"
	began := time.Now()
	err = s.Start(context.Background())
	took := time.Since(began)
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, enginehttp.ErrEngineUnreachable) {
		t.Fatalf("got %v, want ErrUnavailable wrapping enginehttp.ErrEngineUnreachable", err)
	}
	if took > enginehttp.UnreachableFor+10*time.Second {
		t.Fatalf("readiness ended after %s, want about %s", took, enginehttp.UnreachableFor)
	}
	if s.proc != nil {
		t.Fatal("the child was not reaped")
	}
}
