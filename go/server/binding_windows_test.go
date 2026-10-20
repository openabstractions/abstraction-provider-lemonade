//go:build windows

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// DECISION 2026-10-01 point 4: one provider process resolves the package at
// each launch. A run in progress keeps the version it proved and holds; the
// next launch proves and holds the version the binding names then, and the
// earlier version's files are free.
func TestEachLaunchRunsThePackageTheBindingNamesThen(t *testing.T) {
	program, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	bind := func(version string) Binding {
		root, m := writePackageFixture(t, map[string][]byte{"lemond.exe": program, "resources/defaults.json": []byte(`{"v":"` + version + `"}`)})
		return Binding{
			Program: filepath.Join(root, "lemond.exe"), Package: m,
			CacheDir: filepath.Join(base, "engine-cache", version), ConfigDir: filepath.Join(base, "engine-config", version),
			StateDir: filepath.Join(base, "engine-files", version),
		}
	}
	a, b := bind("A"), bind("B")
	var mu sync.Mutex
	current := a
	cfg := Config{
		Port: freePort(t), Models: []string{"user.ApprovedKlein"}, Ready: 3 * time.Second,
		TempDir: filepath.Join(base, "engine-temp"), ModelsDir: filepath.Join(base, "engine-models"),
		Bind: func(context.Context) (Binding, error) {
			mu.Lock()
			defer mu.Unlock()
			return current, nil
		},
		helperArgs: []string{"-test.run=^TestLemonadeHelperProcess$", "--", "helper-ready"},
		folderACL:  openACL{},
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	held := func(bnd Binding) bool {
		t.Helper()
		path := filepath.Join(filepath.Dir(bnd.Program), "resources", "defaults.json")
		err := os.Rename(path, path+".probe")
		if err == nil {
			if err := os.Rename(path+".probe", path); err != nil {
				t.Fatal(err)
			}
			return false
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("probe of %s: %v", path, err)
		}
		return true
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !held(a) || held(b) {
		s.Close()
		t.Fatal("the first launch does not hold exactly version A")
	}
	mu.Lock()
	current = b
	mu.Unlock()
	if err := s.Start(context.Background()); err != nil {
		s.Close()
		t.Fatalf("a running launch refused a second Start after the binding moved: %v", err)
	}
	if !held(a) || held(b) {
		s.Close()
		t.Fatal("the run in progress left version A or took version B")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if held(a) {
		t.Fatal("version A stays held after its run exited")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !held(b) || held(a) {
		s.Close()
		t.Fatal("the next launch does not hold exactly version B")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{a.ConfigDir, b.ConfigDir} {
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
			t.Fatalf("a launch did not use its own version's config folder %s: %v", dir, err)
		}
	}
	mu.Lock()
	current = Binding{Program: "relative.exe"}
	mu.Unlock()
	if err := s.Start(context.Background()); !errors.Is(err, ErrConfiguration) || s.proc != nil {
		t.Fatalf("a binding with relative folders launched: %v", err)
	}
}
