package setup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// stageModelFixture is a configured provider (the starter model staged and
// selected) with a fake catalogue: the starter model's entry carries the
// fixture's pins, and Z-Image-Turbo's entry carries fake main and VAE bytes
// and the starter model's text encoder digest, the file both share.
type stageModelFixture struct {
	*configureFixture
	klein, z  server.ImageModel
	catalogue []server.ImageModel
	sources   map[string]string
}

func newStageModelFixture(t *testing.T) *stageModelFixture {
	t.Helper()
	f := &stageModelFixture{configureFixture: newConfigureFixture(t)}
	if err := Save(SelectionPath(f.root), selectionFor(testPackage, []string{ApprovedModel})); err != nil {
		t.Fatal(err)
	}
	klein, _ := server.FindImageModel(ApprovedModel)
	klein.Files = nil
	for i, role := range []string{"main", "text_encoder", "vae"} {
		pin := f.pins[i]
		klein.Files = append(klein.Files, server.ModelFile{Role: role, Repository: "Comfy-Org/vae-text-encorder-for-flux-klein-4b",
			Revision: approvedRevision, Path: "split_files/" + filepath.ToSlash(pin.rel), Bytes: int64(len(pin.rel)), SHA256: pin.digest})
	}
	f.klein = klein
	z, _ := server.FindImageModel("user.Z-Image-Turbo")
	z.Files = append([]server.ModelFile(nil), z.Files...)
	f.sources = map[string]string{}
	for i := range z.Files {
		if z.Files[i].Role == "text_encoder" {
			z.Files[i].SHA256, z.Files[i].Bytes = klein.Files[1].SHA256, klein.Files[1].Bytes
			continue
		}
		data := []byte("z-image " + z.Files[i].Role)
		source := filepath.Join(f.root, "download-"+z.Files[i].Role)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		z.Files[i].SHA256, z.Files[i].Bytes = fmt.Sprintf("%x", sha256.Sum256(data)), int64(len(data))
		f.sources[z.Files[i].Role] = source
	}
	f.z = z
	f.catalogue = []server.ImageModel{klein, z}
	return f
}

func (f *stageModelFixture) path(m server.ImageModel, role string) string {
	for _, file := range m.Files {
		if file.Role == role {
			return filepath.Join(f.root, "engine-models", file.CachePath())
		}
	}
	return ""
}

// A second model whose text encoder is the starter model's file stages that
// file once: the new path is a hard link to the staged copy, no source is
// needed for it, the selection serves both models and the registry lists both.
func TestStageModelSharesAStagedFileByDigest(t *testing.T) {
	f := newStageModelFixture(t)
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil); err != nil {
		t.Fatal(err)
	}
	shared, err := os.Stat(f.path(f.z, "text_encoder"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(f.path(f.klein, "text_encoder"))
	if err != nil || !os.SameFile(shared, original) {
		t.Fatalf("the shared text encoder was copied, not linked: %v", err)
	}
	for _, role := range []string{"main", "vae"} {
		if err := checkDigest(f.path(f.z, role), fileDigest(f.z, role)); err != nil {
			t.Fatalf("%s: %v", role, err)
		}
	}
	refs, err := os.ReadFile(filepath.Join(f.root, "engine-models", "models--Comfy-Org--z_image_turbo", "refs", "main"))
	if err != nil || string(refs) != f.z.Files[0].Revision {
		t.Fatalf("refs/main %q %v", refs, err)
	}
	selected, err := Read(SelectionPath(f.root))
	if err != nil || !slices.Equal(selected.Models, []string{ApprovedModel, "user.Z-Image-Turbo"}) {
		t.Fatalf("selection %+v %v", selected, err)
	}
	raw, err := os.ReadFile(filepath.Join(ConfigDirFor(f.root, testPackage.Version), "user_models.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &registry); err != nil || len(registry) != 2 || registry["LocalApprovedKlein"] == nil || registry["Z-Image-Turbo"] == nil {
		t.Fatalf("registry %s %v", raw, err)
	}
	// A second run finds every file staged and changes nothing.
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, nil, nil); err != nil {
		t.Fatal(err)
	}
	if again, _ := Read(SelectionPath(f.root)); !slices.Equal(again.Models, selected.Models) {
		t.Fatalf("a second run changed the selection: %v", again.Models)
	}
	// The deep probe hashes the added model's files too.
	if err := verifyAddedModels(context.Background(), f.root, selected.Models, f.catalogue); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path(f.z, "vae"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyAddedModels(context.Background(), f.root, selected.Models, f.catalogue); !errors.Is(err, ErrSelection) {
		t.Fatalf("a changed added model verified: %v", err)
	}
}

// A shared file is reached only by a hard link to the staged object (VISION
// 2026-10-01, OA never makes a copy of a model for an engine): when the link
// fails, the step refuses with what to do and publishes nothing.
func TestStageModelRefusesWhenTheSharedFileCannotBeLinked(t *testing.T) {
	f := newStageModelFixture(t)
	saved := linkFile
	linkFile = func(string, string) error { return errors.New("not the same volume") }
	t.Cleanup(func() { linkFile = saved })
	err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil)
	if !errors.Is(err, ErrSelection) || !strings.Contains(err.Error(), "hard link") || !strings.Contains(err.Error(), "one volume") {
		t.Fatalf("a failed link was not refused with its remedy: %v", err)
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

func fileDigest(m server.ImageModel, role string) string {
	for _, file := range m.Files {
		if file.Role == role {
			return file.SHA256
		}
	}
	return ""
}

// A model is staged only into a configured provider, only from sources that
// verify, and never as the starter model; a refusal publishes no file and
// leaves the selection as it was.
func TestStageModelRefusals(t *testing.T) {
	f := newStageModelFixture(t)
	missing := map[string]string{"main": f.sources["main"]}
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, missing, nil); !errors.Is(err, ErrSelection) {
		t.Fatalf("a missing VAE source was accepted: %v", err)
	}
	wrong := map[string]string{"main": f.sources["vae"], "vae": f.sources["vae"]}
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, wrong, nil); !errors.Is(err, ErrSelection) {
		t.Fatalf("a source of other bytes was accepted: %v", err)
	}
	if err := stageModel(context.Background(), f.root, f.klein, f.catalogue, f.sources, nil); !errors.Is(err, ErrSelection) {
		t.Fatalf("the starter model was staged as an added model: %v", err)
	}
	for _, role := range []string{"main", "vae"} {
		if _, err := os.Lstat(f.path(f.z, role)); !os.IsNotExist(err) {
			t.Fatalf("a refused stage published %s: %v", role, err)
		}
	}
	if selected, _ := Read(SelectionPath(f.root)); !slices.Equal(selected.Models, []string{ApprovedModel}) {
		t.Fatalf("a refused stage changed the selection: %v", selected.Models)
	}
	// Each refusal moved every download it had taken back to its source.
	for role, source := range f.sources {
		if _, err := os.Lstat(source); err != nil {
			t.Fatalf("a refused stage lost the %s download: %v", role, err)
		}
	}
	err := filepath.WalkDir(filepath.Join(f.root, "engine-models"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(entry.Name(), ".stage-") {
			return fmt.Errorf("staging residue %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(SelectionPath(f.root)); err != nil {
		t.Fatal(err)
	}
	if err := stageModel(context.Background(), f.root, f.z, f.catalogue, f.sources, nil); !errors.Is(err, ErrSelection) {
		t.Fatalf("a model was staged before the starter setup: %v", err)
	}
}
