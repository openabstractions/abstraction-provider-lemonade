package server

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
)

// The table holds the reviewed v2026.39.1 entry under the selection ID the
// existing selection.json names, with the triple it was tested with.
func TestPackageTableHoldsTheReviewedEntries(t *testing.T) {
	if err := ValidTable(Packages); err != nil {
		t.Fatal(err)
	}
	p, ok := FindPackage("v2026.39.1")
	if !ok || p.ID != "lemonade-embeddable-v2026.39.1-windows-amd64" {
		t.Fatalf("the v2026.39.1 entry: %+v %v", p, ok)
	}
	if byID, ok := FindPackageID(p.ID); !ok || byID.Version != p.Version {
		t.Fatal("the entry is not found by its selection ID")
	}
	if !sameManifest(p.SDBackend, SDCppVulkan) || !sameManifest(p.LlamaBackend, LlamaCppVulkanB10825) ||
		p.Manifest.Name != Lemonade2026391.Name || p.Program != "lemond.exe" ||
		p.BackendArchiveSHA256 != "b9248856707de2e125538ede8a7dc71a9d800bba67e469e60881dae2a5a1c0a2" ||
		p.ModelRevision != "5f526678002e43af5551dadb73ce2e8c91b43afe" ||
		p.ArchiveSHA256 != "235c2361be3a9729a06b92c97e40a9530ebb8e5088bd3334524472d6da7fbbca" {
		t.Fatalf("the v2026.39.1 entry differs from its reviews: %+v", p)
	}
	models := filepath.Join(t.TempDir(), "engine-models")
	got, err := p.Config(models)
	want, _ := LockedConfig(models)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the entry's config is not the locked config: %v", err)
	}
	if _, ok := FindPackage("v2026.38.0"); ok {
		t.Fatal("an unknown version was found")
	}
	if _, ok := FindPackageID("custom-exe"); ok {
		t.Fatal("an unknown selection ID was found")
	}
}

func TestPackageTableRefusesUnsafeOrRepeatedVersions(t *testing.T) {
	good := Packages[0]
	for _, version := range []string{"", "bin", "BIN", "CON", "com1.2", "1.0.", ".hidden", "v1/2", `v1\2`, "v 1"} {
		if version == "BIN" && !identity.SamePathName("BIN", "bin") {
			continue // another folder than bin where names keep their case
		}
		p := good
		p.Version = version
		if err := p.Valid(); !errors.Is(err, ErrUnknownPackage) {
			t.Fatalf("version %q accepted: %v", version, err)
		}
	}
	second := good
	second.ID = "other-id"
	second.Version = "V2026.39.1"
	if err := ValidTable([]Package{good, second}); !errors.Is(err, ErrUnknownPackage) {
		t.Fatalf("two entries naming one folder accepted: %v", err)
	}
	second.Version = "v2026.40.0"
	if err := ValidTable([]Package{good, second}); err != nil {
		t.Fatalf("two distinct entries refused: %v", err)
	}
	broken := good
	broken.ProgramSHA256 = "0" + good.ProgramSHA256[1:]
	if err := broken.Valid(); !errors.Is(err, ErrUnknownPackage) {
		t.Fatalf("a program digest that differs from the manifest accepted: %v", err)
	}
	broken = good
	broken.LlamaBackend = good.SDBackend
	if err := broken.Valid(); !errors.Is(err, ErrUnknownPackage) {
		t.Fatalf("two backends of one family accepted: %v", err)
	}
}
