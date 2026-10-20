package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Every path that reaches one state folder names one endpoint: another
// case, a trailing dot and the 8.3 short name.
func TestEndpointKeyFollowsTheFolderIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Lemonade State Folder")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want, err := endpointKey("S-1-5-21-x", dir)
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{strings.ToUpper(dir), dir + ".", dir + ". ."}
	long, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	if n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf))); err == nil && n > 0 && n < uint32(len(buf)) {
		if short := windows.UTF16ToString(buf[:n]); !strings.EqualFold(short, dir) {
			aliases = append(aliases, short)
		} else {
			t.Log("8.3 names are off on this volume; the short-name alias is not checked")
		}
	}
	for _, alias := range aliases {
		got, err := endpointKey("S-1-5-21-x", alias)
		if err != nil || got != want {
			t.Errorf("%s: %s %v, want %s", alias, got, err, want)
		}
	}
}

// An existing parent owned by another account refuses the folder; this
// account, SYSTEM and Administrators are admitted, and the volume root is
// not checked.
func TestEnsurePrivateFolderChecksParentOwners(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "parent", "state")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkParents(dir); err != nil {
		t.Fatalf("this account's parents: %v", err)
	}
	guests, err := windows.CreateWellKnownSid(windows.WinBuiltinGuestsSid)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	real := folderOwner
	t.Cleanup(func() { folderOwner = real })
	var seen []string
	owner := guests
	folderOwner = func(path string) (*windows.SID, error) {
		seen = append(seen, path)
		if path == filepath.Dir(dir) {
			return owner, nil
		}
		return real(path)
	}
	if err := EnsurePrivateFolder(dir); !errors.Is(err, ErrSelection) || !strings.Contains(err.Error(), filepath.Dir(dir)) {
		t.Fatalf("a parent owned by Guests: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused folder was created")
	}
	owner, seen = admins, nil
	if err := checkParents(dir); err != nil {
		t.Fatalf("a parent owned by Administrators: %v", err)
	}
	for _, path := range seen {
		if path == filepath.VolumeName(dir)+`\` {
			t.Fatal("the volume root was checked")
		}
	}
}
