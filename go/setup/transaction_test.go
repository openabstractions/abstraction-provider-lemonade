package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFullProbeDistinguishesAbsentFromBackendOnlyInterruption(t *testing.T) {
	root := t.TempDir()
	if complete, err := ProbeReviewedSetup(context.Background(), root); err != nil || complete {
		t.Fatalf("empty setup: %v, %v", complete, err)
	}
	parent := filepath.Dir(backendDir(filepath.Join(root, "engine-cache")))
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(parent, ".vulkan-stage-123456789")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if complete, err := ProbeReviewedSetup(context.Background(), root); complete || !errors.Is(err, ErrSelection) {
		t.Fatalf("backend-only residue accepted as absent: %v, %v", complete, err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backendDir(filepath.Join(root, "engine-cache")), 0700); err != nil {
		t.Fatal(err)
	}
	if complete, err := ProbeReviewedSetup(context.Background(), root); complete || !errors.Is(err, ErrSelection) {
		t.Fatalf("partial backend accepted as absent: %v, %v", complete, err)
	}
}
