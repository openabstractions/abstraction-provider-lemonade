package setup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// configureFixture is a provider root with a staged private model whose
// downloads are gone, a staged image backend and a llama.cpp folder of
// another build, and steps that stand for the reviewed package.
type configureFixture struct {
	root     string
	pins     []weightPin
	archives []stagedArchive
	steps    engineSteps
	installs int
	model    string
}

func newConfigureFixture(t *testing.T) *configureFixture {
	t.Helper()
	// The model path limit (CheckModelPath) needs a short root.
	root, err := modelFixtureRoot(t, "oa-klein-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &configureFixture{root: root}
	for _, rel := range []string{"diffusion_models/main.safetensors", "text_encoders/encoder.safetensors", "vae/vae.safetensors"} {
		source := filepath.Join(root, filepath.Base(rel))
		data := []byte(rel)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		f.pins = append(f.pins, weightPin{source: source, rel: filepath.FromSlash(rel), digest: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	if err := stageBF16(context.Background(), root, testPackage, f.pins, nil, false); err != nil {
		t.Fatal(err)
	}
	requireMoved(t, f.pins) // the downloads are gone
	f.model = filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b", "snapshots", approvedRevision, "split_files", f.pins[0].rel)

	sd, sdZip := fixtureArchive(t, map[string]string{"sd-server.exe": "sd"})
	sd.manifest.Family = "sd-cpp"
	llama, llamaZip := fixtureArchive(t, map[string]string{"llama-server.exe": "server", "ggml-rpc-server.exe": "rpc"}, "ggml-rpc-server.exe")
	cache := CacheDirFor(root, testPackage.Version)
	verify := func(cache string) error { return server.CheckBackendTree(cache, sd.manifest, llama.manifest, true) }
	if err := stageBackends(context.Background(), cache, []stagedArchive{{sd, sdZip}}, false, verify); err != nil {
		t.Fatal(err)
	}
	stale := llama.manifest.Dir(cache)
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "version.txt"), []byte("b10723\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.archives = []stagedArchive{{sd, sdZip}, {llama, llamaZip}}
	f.steps = engineSteps{
		verifyPackage: func(server.Package) (string, error) { return filepath.Join(root, "app", "lemond.exe"), nil },
		verifyBackend: func(cache string, _ server.Package) error { return verify(cache) },
		installFiles: func(_ context.Context, b server.Binding) error {
			if b.Program != filepath.Join(root, "app", "lemond.exe") || b.CacheDir != cache || b.StateDir != StateDirFor(root, testPackage.Version) {
				return fmt.Errorf("engine files installed from %s, %s, %s", b.Program, b.CacheDir, b.StateDir)
			}
			f.installs++
			return nil
		},
		filesInstalled: func(server.Binding) (bool, error) { return f.installs > 0, nil },
	}
	return f
}

// testPackage is the table entry the fixtures bind.
var testPackage = server.Packages[0]

// modelStamp is the staged model file's identity and modification time.
func (f *configureFixture) modelStamp(t *testing.T) os.FileInfo {
	t.Helper()
	info, err := os.Stat(f.model)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// Replacing a backend of another build leaves a staged model that verifies
// in place: the model is not copied again, so configure succeeds after the
// downloads are gone. The shared flag of earlier releases replaced both.
func TestConfigureReplacesABackendWithoutCopyingTheModel(t *testing.T) {
	f := newConfigureFixture(t)
	before := f.modelStamp(t)
	err := configureReviewed(context.Background(), f.root, testPackage, f.archives, f.pins, Replace{}, f.steps)
	if !errors.Is(err, ErrSelection) || f.installs != 0 {
		t.Fatalf("a backend of another build was replaced without --replace-backend: %v", err)
	}
	if err := configureReviewed(context.Background(), f.root, testPackage, f.archives, f.pins, Replace{Backends: true}, f.steps); err != nil {
		t.Fatalf("backend replacement needed the model again: %v", err)
	}
	if f.installs != 1 {
		t.Fatalf("engine files installed %d times", f.installs)
	}
	if after := f.modelStamp(t); !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("backend replacement touched the private model")
	}
	if _, err := Read(SelectionPath(f.root)); err != nil {
		t.Fatalf("configure saved no selection: %v", err)
	}
}

// The engine-files step installs the package and backend folders of an
// existing install and never reads, copies or replaces the private model.
// It needs a configured install, and backend replacement intent for a
// folder of another build.
func TestEngineFilesStepLeavesThePrivateModelUntouched(t *testing.T) {
	f := newConfigureFixture(t)
	if err := installEngineFiles(context.Background(), f.root, f.archives, true, f.steps); !errors.Is(err, ErrSelection) || f.installs != 0 {
		t.Fatalf("the engine-files step ran without a configured install: %v", err)
	}
	if err := Save(SelectionPath(f.root), selectionFor(testPackage, []string{ApprovedModel})); err != nil {
		t.Fatal(err)
	}
	before := f.modelStamp(t)
	if err := os.WriteFile(f.model, []byte("not the pinned bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := f.modelStamp(t)
	if err := installEngineFiles(context.Background(), f.root, f.archives, false, f.steps); !errors.Is(err, ErrSelection) || f.installs != 0 {
		t.Fatalf("a backend of another build was replaced without --replace-backend: %v", err)
	}
	if err := installEngineFiles(context.Background(), f.root, f.archives, true, f.steps); err != nil {
		t.Fatalf("the engine-files step refused: %v", err)
	}
	if err := installEngineFiles(context.Background(), f.root, nil, false, f.steps); err != nil {
		t.Fatalf("the engine-files step without archives refused staged backends: %v", err)
	}
	if f.installs != 2 {
		t.Fatalf("engine files installed %d times", f.installs)
	}
	after := f.modelStamp(t)
	data, err := os.ReadFile(f.model)
	if err != nil || string(data) != "not the pinned bytes" || !os.SameFile(before, after) || !after.ModTime().Equal(changed.ModTime()) {
		t.Fatalf("the engine-files step touched the private model: %q %v", data, err)
	}
}
