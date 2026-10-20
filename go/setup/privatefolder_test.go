package setup

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// --state and --dir name local folders private to this account: an absent
// one is created private; a share, a device path, a network drive, a link
// or junction, a file and a folder open to other accounts are refused.
func TestEnsurePrivateFolder(t *testing.T) {
	created := filepath.Join(t.TempDir(), "state")
	if err := EnsurePrivateFolder(created); err != nil {
		t.Fatalf("absent folder: %v", err)
	}
	if err := EnsurePrivateFolder(created); err != nil {
		t.Fatalf("the created private folder: %v", err)
	}
	// The device forms name a folder under the test's own temporary folder,
	// so a missing check creates nothing elsewhere.
	tmp := t.TempDir()
	for _, dir := range []string{`\\127.0.0.1\absent-share\state`, `\\?\` + filepath.Join(tmp, "a"), `\\.\pipe\state`, `\\.\` + filepath.Join(tmp, "b"), "relative"} {
		if runtime.GOOS != "windows" && dir != "relative" {
			continue
		}
		err := EnsurePrivateFolder(dir)
		if !errors.Is(err, ErrSelection) || dir != "relative" && !strings.Contains(err.Error(), "network share or device path") {
			t.Errorf("%s: %v", dir, err)
		}
	}
	remoteDrive = func(string) (bool, error) { return true, nil }
	err := EnsurePrivateFolder(filepath.Join(t.TempDir(), "remote"))
	remoteDrive = isRemoteDrive
	if !errors.Is(err, ErrSelection) {
		t.Fatalf("network drive: %v", err)
	}
	// t.TempDir inherits its parent's entries, which allow more than this
	// account.
	if err := EnsurePrivateFolder(t.TempDir()); !errors.Is(err, ErrSelection) || !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a folder open to others: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateFolder(file); !errors.Is(err, ErrSelection) {
		t.Fatalf("a file: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if runtime.GOOS == "windows" {
		// A junction to the private folder: a reparse point is never one.
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, created).CombinedOutput(); err != nil {
			t.Fatalf("mklink: %v %s", err, out)
		}
	} else if err := os.Symlink(created, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateFolder(link); !errors.Is(err, ErrSelection) {
		t.Fatalf("a junction: %v", err)
	}
}

// The per-user root keeps the account's endpoint; any other state folder
// adds its identity, so two isolated installs get two endpoints.
func TestEndpointKey(t *testing.T) {
	def, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	one, two := filepath.Join(t.TempDir(), "one"), filepath.Join(t.TempDir(), "two")
	for _, dir := range []string{one, two} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := func(account, root string) string {
		t.Helper()
		k, err := endpointKey(account, root)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	a := key("S-1-5-21-x", one)
	if a == key("S-1-5-21-x", two) || a == key("S-1-5-21-y", one) || a != key("S-1-5-21-x", one+string(filepath.Separator)) {
		t.Fatal("state endpoints do not follow the folder and the account")
	}
	if key("S-1-5-21-x", def) != accountOnlyKey("S-1-5-21-x") {
		t.Fatal("the per-user endpoint changed")
	}
	if _, err := endpointKey("S-1-5-21-x", filepath.Join(t.TempDir(), "absent")); !errors.Is(err, ErrSelection) {
		t.Fatalf("an absent state folder: %v", err)
	}
}
