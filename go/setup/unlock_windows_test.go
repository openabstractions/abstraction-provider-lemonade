package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// lemonaded engine unlock gives the package folder and the backend folders of
// every known version back to the account, so an uninstaller or a person can
// remove them; the private model and the selection stay.
func TestUnlockEngineFilesMakesEveryVersionsPackageAndBackendsRemovable(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	second := testPackage
	second.ID, second.Version = "lemonade-embeddable-v2026.40.0-windows-amd64", "v2026.40.0"
	withPackages(t, testPackage, second)
	root := filepath.Join(t.TempDir(), "lemonade")
	model := filepath.Join(root, "engine-models", "weights.safetensors")
	files := []string{model}
	var locked []string
	for _, p := range server.Packages {
		program, err := PackageProgramFor(p)
		if err != nil {
			t.Fatal(err)
		}
		backend := p.SDBackend.Dir(CacheDirFor(root, p.Version))
		files = append(files, program, filepath.Join(backend, "sd-server.exe"))
		locked = append(locked, filepath.Dir(program), backend)
	}
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sid, err := enginefiles.AccountSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range locked {
		if err := enginefiles.SetFolderACL(folder, sid, enginefiles.ReadOnly); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, folder := range locked {
			//unchecked: cleanup of a folder the test may already have removed
			_ = enginefiles.UnlockWithin(folder)
		}
	})
	if _, err := UnlockEngineFiles(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	for _, folder := range locked {
		if err := os.RemoveAll(folder); err != nil {
			t.Fatalf("%s is not removable after engine unlock: %v", folder, err)
		}
	}
	if _, err := os.Stat(model); err != nil {
		t.Fatalf("engine unlock touched the private model: %v", err)
	}
	if _, err := UnlockEngineFiles(context.Background(), filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("an absent install has nothing to unlock: %v", err)
	}
}
