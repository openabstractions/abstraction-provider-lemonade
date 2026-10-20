//go:build windows

package server

import (
	"context"
	"errors"
	"testing"
)

// Each launch checks the declared models again: a built-in Omni collection,
// by its own name or its builtin.<name> alias, refuses the launch before any
// process is created.
func TestCollectionModelRefusesLaunch(t *testing.T) {
	for _, model := range []string{"LMX-Omni-5.5B-Lite", "builtin.LMX-Omni-5.5B-Lite", "BUILTIN.lmx-omni-5.5b-lite"} {
		cfg := fakeConfig(t, "helper-ready")
		s, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		// The declaration changed after New, as a reloaded selection would.
		s.cfg.Models = []string{model}
		err = s.Start(context.Background())
		if !errors.Is(err, ErrCollectionModel) || RefusalReason(err) != "engine:collection-model" || s.proc != nil {
			_ = s.Close()
			t.Fatalf("%s launched: %v", model, err)
		}
	}
}
