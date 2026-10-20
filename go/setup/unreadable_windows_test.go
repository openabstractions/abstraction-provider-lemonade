package setup

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
	"golang.org/x/sys/windows"
)

// lockExclusive opens path with no sharing, as an antivirus scan can, and
// returns the function that closes it.
func lockExclusive(t *testing.T, path string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { windows.CloseHandle(h) }
}

// A package file another process holds open without sharing cannot be read.
// That is no verdict on the package: package check fails without the
// definitive refusal (exit 1), so Setup keeps the staged version, and
// passes once the file is free.
func TestPackageCheckOfAnUnreadableFileIsNoRefusal(t *testing.T) {
	root := t.TempDir()
	p := fixturePackageTree(t, root, "vC", map[string]string{"lemond.exe": "server", "resources/defaults.json": `{}`})
	withPackages(t, p)
	for _, name := range []string{"lemond.exe", filepath.Join("resources", "defaults.json")} {
		release := lockExclusive(t, filepath.Join(root, p.Manifest.Name, name))
		_, err := CheckPackage(context.Background(), "vC", root)
		release()
		if err == nil || errors.Is(err, ErrPackageRefused) {
			t.Fatalf("a locked %s: %v", name, err)
		}
		// The program's own read and the kit's tree proof both keep the
		// system error; the kit names its refusal unreadable.
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("the locked %s's error lost its cause: %v", name, err)
		}
		if name != "lemond.exe" && !errors.Is(err, programfiles.ErrUnreadable) {
			t.Fatalf("the locked %s is not unreadable: %v", name, err)
		}
	}
	if _, err := CheckPackage(context.Background(), "vC", root); err != nil {
		t.Fatalf("the freed package was refused: %v", err)
	}
}
