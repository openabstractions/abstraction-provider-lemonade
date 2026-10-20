package setup

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
)

// package retire is refused while a launch holds the version's files, as a
// running engine holds them (INF-F4), and leaves everything in place; once
// the hold is released it removes the version.
func TestPackageRetireIsRefusedWhileALaunchHoldsTheVersion(t *testing.T) {
	f := newActivationFixture(t)
	ctx := context.Background()
	if err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps); err != nil {
		t.Fatal(err)
	}
	tree, err := f.a.SDBackend.StagedTree(CacheDirFor(f.root, "vA"), StateDirFor(f.root, "vA"))
	if err != nil {
		t.Fatal(err)
	}
	hold, err := programfiles.NewVerifier(programfiles.Options{ACL: openFolders{}}).Launch(ctx, tree)
	if err != nil {
		t.Fatal(err)
	}
	err = RetirePackage(ctx, f.root, "vA")
	if !errors.Is(err, ErrHeld) {
		hold.Release()
		t.Fatalf("a held version was retired: %v", err)
	}
	for _, kept := range []string{CacheDirFor(f.root, "vA"), ConfigDirFor(f.root, "vA")} {
		if _, err := os.Lstat(kept); err != nil {
			hold.Release()
			t.Fatalf("a refused retire removed %s: %v", kept, err)
		}
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	if err := RetirePackage(ctx, f.root, "vA"); err != nil {
		t.Fatalf("retire after the launch exited: %v", err)
	}
	if _, err := os.Lstat(CacheDirFor(f.root, "vA")); !os.IsNotExist(err) {
		t.Fatalf("vA's cache remains: %v", err)
	}
}
