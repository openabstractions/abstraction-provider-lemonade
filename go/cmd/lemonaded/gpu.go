package main

import (
	"io"
	"log/slog"

	"github.com/openabstractions/abstraction-resource/go/gpufault"
)

// newGPUGuard is serve's GPU fault handling: the resource kit's provider
// composition (gpufault.ForProvider, resource CONTRACT.md RES-G1..RES-G7)
// with OA logging. It takes no wake lease: the provider's wake use stays as
// it was until the owner decides D2 (TODO.md), and the runtime's rules for a
// card provider are unchanged. state "" keeps the quarantine in memory.
func newGPUGuard(state string, stderr io.Writer, report func(error), logger *slog.Logger) (*gpufault.Guard, error) {
	return gpufault.ForProvider(gpufault.ProviderOptions{Name: "lemonaded", State: state,
		Logger: logger, Stderr: stderr, Report: report})
}
