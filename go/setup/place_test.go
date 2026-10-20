package setup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// requireMoved fails unless every pin's download has left its source.
func requireMoved(t *testing.T, pins []weightPin) {
	t.Helper()
	for _, pin := range pins {
		if _, err := os.Lstat(pin.source); !os.IsNotExist(err) {
			t.Fatalf("the download %s stayed after staging: %v", pin.source, err)
		}
	}
}

// writeSources writes each fixture download again: its bytes are its rel.
func writeSources(t *testing.T, pins []weightPin) {
	t.Helper()
	for _, pin := range pins {
		if err := os.WriteFile(pin.source, []byte(filepath.ToSlash(pin.rel)), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// digestOf is the SHA-256 of text in hex.
func digestOf(text string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(text)))
}

// identities are the files at paths, for os.SameFile after a move.
func identities(t *testing.T, paths ...string) []os.FileInfo {
	t.Helper()
	var infos []os.FileInfo
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// On Windows the file id is read from the path on first comparison;
		// comparing now records it before the path moves.
		if !os.SameFile(info, info) {
			t.Fatalf("%s has no file identity", path)
		}
		infos = append(infos, info)
	}
	return infos
}

// OA never makes a copy of a model for an engine (VISION 2026-10-01): each
// download of the starter model becomes the staged file by rename, so the
// disk holds one file per download, and the download is gone afterwards.
func TestStageBF16PublishesEachDownloadByRename(t *testing.T) {
	// A short root keeps the model path within Lemonade's Windows limit.
	root, err := modelFixtureRoot(t, "oa-klein-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	var pins []weightPin
	var sources []string
	for _, rel := range []string{"diffusion_models/main.safetensors", "text_encoders/encoder.safetensors", "vae/vae.safetensors"} {
		source := filepath.Join(DownloadDir(root), filepath.Base(rel))
		pins = append(pins, weightPin{source: source, rel: filepath.FromSlash(rel), digest: digestOf(rel)})
		sources = append(sources, source)
	}
	if err := os.MkdirAll(DownloadDir(root), 0700); err != nil {
		t.Fatal(err)
	}
	writeSources(t, pins)
	before := identities(t, sources...)
	if err := stageBF16(context.Background(), root, testPackage, pins, nil, false); err != nil {
		t.Fatal(err)
	}
	requireMoved(t, pins)
	snapshot := filepath.Join(root, "engine-models", modelRepositoryFolder, "snapshots", approvedRevision, "split_files")
	for i, pin := range pins {
		after := identities(t, filepath.Join(snapshot, pin.rel))
		if !os.SameFile(before[i], after[0]) {
			t.Fatalf("%s was copied, not moved", pin.rel)
		}
	}
}

// Each download of an added model becomes its staged file by rename.
func TestStageModelPublishesEachDownloadByRename(t *testing.T) {
	f := newStageModelFixture(t)
	before := identities(t, f.sources["main"], f.sources["vae"])
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil); err != nil {
		t.Fatal(err)
	}
	for i, role := range []string{"main", "vae"} {
		if _, err := os.Lstat(f.sources[role]); !os.IsNotExist(err) {
			t.Fatalf("the %s download stayed after staging: %v", role, err)
		}
		if after := identities(t, f.path(f.z, role)); !os.SameFile(before[i], after[0]) {
			t.Fatalf("the %s download was copied, not moved", role)
		}
	}
}

// A download is the job service's result under a second name (job
// CONTRACT.md JOB-A17). The read-only attribute belongs to the file, shared
// by both names, so staging never sets it: the published model stays
// writable by attribute, and the service's own name stays removable when its
// retention ends. The provider folder's ACL protects the model.
func TestStagingLeavesTheLinkedFileAttributeClear(t *testing.T) {
	f := newStageModelFixture(t)
	service := filepath.Join(t.TempDir(), "results")
	if err := os.MkdirAll(service, 0700); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for role, source := range f.sources {
		names[role] = filepath.Join(service, role)
		if err := os.Link(source, names[role]); err != nil {
			t.Fatal(err)
		}
	}
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil); err != nil {
		t.Fatal(err)
	}
	for role, name := range names {
		published, err := os.Stat(f.path(f.z, role))
		if err != nil {
			t.Fatal(err)
		}
		if published.Mode().Perm()&0200 == 0 {
			t.Fatalf("staging set the %s file read-only", role)
		}
		if err := os.Remove(name); err != nil {
			t.Fatalf("the service's own %s name is not removable after staging: %v", role, err)
		}
		if _, err := os.Stat(f.path(f.z, role)); err != nil {
			t.Fatalf("removing the service's name removed the published %s: %v", role, err)
		}
	}
}

// A download that cannot be renamed into the models folder, such as one on
// another volume, is refused with its remedy and stays where it was; no
// model is published and no copy is made.
func TestADownloadThatCannotBeRenamedIsRefusedAndKept(t *testing.T) {
	f := newStageModelFixture(t)
	saved := renameFile
	renameFile = func(string, string) error {
		return errors.New("The system cannot move the file to a different disk drive.")
	}
	t.Cleanup(func() { renameFile = saved })
	before := identities(t, f.sources["main"], f.sources["vae"])
	err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil)
	if !errors.Is(err, ErrSelection) || !strings.Contains(err.Error(), "volume") || !strings.Contains(err.Error(), "download it again in Setup") {
		t.Fatalf("a download that cannot be renamed was not refused with its remedy: %v", err)
	}
	for i, role := range []string{"main", "vae"} {
		if after := identities(t, f.sources[role]); !os.SameFile(before[i], after[0]) {
			t.Fatalf("the %s download moved or changed", role)
		}
	}
	for _, role := range []string{"main", "text_encoder", "vae"} {
		if _, err := os.Lstat(f.path(f.z, role)); !os.IsNotExist(err) {
			t.Fatalf("a refused stage published %s: %v", role, err)
		}
	}
	if selected, _ := Read(SelectionPath(f.root)); !slices.Equal(selected.Models, []string{ApprovedModel}) {
		t.Fatalf("a refused stage changed the selection: %v", selected.Models)
	}
}
