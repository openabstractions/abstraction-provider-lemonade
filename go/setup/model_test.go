package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

func TestStageBF16VerifiesBeforePrivatePublication(t *testing.T) {
	root, err := modelFixtureRoot(t, "oa-klein-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if ready, err := ProbeApprovedBF16(context.Background(), root); err != nil || ready {
		t.Fatalf("absent private model reported ready: %v, %v", ready, err)
	}
	files := []weightPin{}
	for _, rel := range []string{"diffusion_models/main.safetensors", "text_encoders/encoder.safetensors", "vae/vae.safetensors"} {
		source := filepath.Join(root, filepath.Base(rel))
		data := []byte(rel)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, weightPin{source: source, rel: filepath.FromSlash(rel), digest: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	wrong := append([]weightPin(nil), files...)
	wrong[2].digest = testPackage.ProgramSHA256
	if err := stageBF16(context.Background(), root, testPackage, wrong, nil, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("wrong weight accepted: %v", err)
	}
	modelRoot := filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b")
	if _, err := os.Stat(filepath.Join(modelRoot, "refs", "main")); !os.IsNotExist(err) {
		t.Fatal("unverified model published")
	}
	var progressCalls int
	if err := stageBF16(context.Background(), root, testPackage, files, func(done, total int64) error {
		progressCalls++
		if done > total {
			t.Fatal("progress overflow")
		}
		return nil
	}, false); err != nil {
		t.Fatal(err)
	}
	if progressCalls == 0 {
		t.Fatal("no copy progress")
	}
	if err := os.WriteFile(files[0].source, []byte("source replaced after publication"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkDigest(filepath.Join(modelRoot, "snapshots", approvedRevision, "split_files", files[0].rel), files[0].digest); err != nil {
		t.Fatalf("published bytes changed with source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(modelRoot, "refs", "main")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ConfigDirFor(root, testPackage.Version), "user_models.json")); err != nil {
		t.Fatal(err)
	}
	// A re-run over the staged model that verifies keeps it: the replaced
	// source is not copied.
	if err := stageBF16(context.Background(), root, testPackage, files, nil, false); err != nil {
		t.Fatalf("re-run over a verified staged model: %v", err)
	}
	if err := checkDigest(filepath.Join(modelRoot, "snapshots", approvedRevision, "split_files", files[0].rel), files[0].digest); err != nil {
		t.Fatalf("a re-run without replacement intent copied the source: %v", err)
	}
	writeSources(t, files)
	if err := stageBF16(context.Background(), root, testPackage, files, nil, true); err != nil {
		t.Fatalf("idempotent staging: %v", err)
	}
	ready, err := probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files)
	if err != nil || !ready {
		t.Fatalf("verified model not recovered: %v, %v", ready, err)
	}
	privateMain := filepath.Join(modelRoot, "snapshots", approvedRevision, "split_files", files[0].rel)
	if err := os.WriteFile(privateMain, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	ready, err = probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files)
	if ready || !errors.Is(err, ErrSelection) {
		t.Fatalf("damaged model reported ready: %v, %v", ready, err)
	}
	writeSources(t, files)
	if err := stageBF16(context.Background(), root, testPackage, files, nil, true); err != nil {
		t.Fatalf("explicit replacement failed: %v", err)
	}
	ready, err = probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files)
	if err != nil || !ready {
		t.Fatalf("replacement not verified: %v, %v", ready, err)
	}
	if err := os.WriteFile(filepath.Join(ConfigDirFor(root, testPackage.Version), "config.json"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	ready, err = probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files)
	if ready || !errors.Is(err, ErrSelection) {
		t.Fatalf("changed private config reported ready: %v, %v", ready, err)
	}
	stale := filepath.Join(modelRoot, "snapshots", ".stage-interrupted")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	ready, err = probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files)
	if ready || !errors.Is(err, ErrSelection) {
		t.Fatalf("stale stage reported ready: %v, %v", ready, err)
	}
	writeSources(t, files)
	if err := stageBF16(context.Background(), root, testPackage, files, nil, true); err != nil {
		t.Fatalf("explicit recovery failed: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("owned stale stage not cleaned")
	}
}

// A pinned digest does not make a pickle format acceptable.
// Review F2: a plain configure re-run over a staged model that verifies
// copies nothing and reads no source, so it can end in InstallFiles without
// --replace-private-model. A staged model that does not verify still needs
// replacement intent.
func TestStageBF16KeepsAVerifiedStagedModelWithoutCopying(t *testing.T) {
	root, err := modelFixtureRoot(t, "oa-klein-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	var files []weightPin
	for _, rel := range []string{"diffusion_models/main.safetensors", "text_encoders/encoder.safetensors", "vae/vae.safetensors"} {
		source := filepath.Join(root, filepath.Base(rel))
		data := []byte(rel)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, weightPin{source: source, rel: filepath.FromSlash(rel), digest: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	if err := stageBF16(context.Background(), root, testPackage, files, nil, false); err != nil {
		t.Fatal(err)
	}
	requireMoved(t, files) // the downloads are gone
	copied := false
	if err := stageBF16(context.Background(), root, testPackage, files, func(int64, int64) error { copied = true; return nil }, false); err != nil {
		t.Fatalf("plain re-run over a verified staged model refused: %v", err)
	}
	if copied {
		t.Fatal("the re-run copied or re-verified bytes through the staging path")
	}
	if ready, err := probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, files); err != nil || !ready {
		t.Fatalf("staged model lost: %v %v", ready, err)
	}
	snapshot := filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b", "snapshots", approvedRevision, "split_files")
	if err := os.WriteFile(filepath.Join(snapshot, files[2].rel), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stageBF16(context.Background(), root, testPackage, files, nil, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("a damaged staged model was kept without replacement intent: %v", err)
	}
}

func TestStageBF16RefusesNonSafetensorsWeights(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"main.ckpt", "main.pt", "main.pth", "main.bin", "main"} {
		source := filepath.Join(root, "source-"+rel)
		data := []byte(rel)
		if err := os.WriteFile(source, data, 0600); err != nil {
			t.Fatal(err)
		}
		pin := weightPin{source: source, rel: filepath.Join("diffusion_models", rel), digest: fmt.Sprintf("%x", sha256.Sum256(data))}
		if err := stageBF16(context.Background(), root, testPackage, []weightPin{pin}, nil, false); !errors.Is(err, ErrSelection) || !errors.Is(err, server.ErrWeightFormat) {
			t.Fatalf("%s staged: %v", rel, err)
		}
		if _, err := probeBF16(context.Background(), root, ConfigDirFor(root, testPackage.Version), testPackage.Config, []weightPin{pin}); !errors.Is(err, server.ErrWeightFormat) {
			t.Fatalf("%s probed: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "engine-models")); !os.IsNotExist(err) {
		t.Fatalf("refused weight left private state: %v", err)
	}
	for _, pin := range approvedBF16Pins(BF16Files{}) {
		if !server.WeightFileAllowed(pin.rel) {
			t.Fatalf("approved pin %s is not safetensors or GGUF", pin.rel)
		}
	}
}

// The Klein registry and pins come from the provider's model catalogue and
// stay byte for byte what earlier releases staged, so an installed model
// still probes as complete.
func TestKleinRegistryAndPinsFollowTheCatalogue(t *testing.T) {
	_, registry, err := privateConfigBytes(t.TempDir(), server.LockedConfig)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"LocalApprovedKlein":{"checkpoints":{"main":"Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/diffusion_models/flux-2-klein-4b.safetensors",` +
		`"text_encoder":"Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/text_encoders/qwen_3_4b.safetensors",` +
		`"vae":"Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/vae/flux2-vae.safetensors"},` +
		`"image_defaults":{"cfg_scale":1,"height":512,"steps":4,"width":512},"recipe":"sd-cpp","size":16.1,"source":"huggingface"}}`
	if string(registry) != want {
		t.Fatalf("registry = %s", registry)
	}
	klein, ok := server.FindImageModel(ApprovedModel)
	if !ok || klein.RegistryName() != "LocalApprovedKlein" || klein.Files[0].Revision != approvedRevision {
		t.Fatalf("catalogue Klein = %+v", klein)
	}
	pins := approvedBF16Pins(BF16Files{})
	for i, f := range klein.Files {
		if pins[i].digest != f.SHA256 || filepath.ToSlash(filepath.Join("split_files", pins[i].rel)) != f.Path {
			t.Fatalf("pin %d = %+v, catalogue %+v", i, pins[i], f)
		}
	}
}

// Setup writes exactly the bytes the server compares before each launch, and
// upgrades only the exact pre-lock file.
func TestPrivateConfigIsTheServerLock(t *testing.T) {
	root := t.TempDir()
	config, _, err := privateConfigBytes(root, server.LockedConfig)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := server.LockedConfig(filepath.Join(root, "engine-models"))
	if err != nil || !bytes.Equal(config, locked) {
		t.Fatalf("setup config differs from the server lock: %v", err)
	}
	path := filepath.Join(ConfigDirFor(root, testPackage.Version), "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyConfigBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := upgradeLegacyConfig(root, filepath.Dir(path), testPackage); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, locked) {
		t.Fatalf("pre-lock config not upgraded: %v", err)
	}
	tampered := bytes.Replace(legacy, []byte(`"auto_check_model_updates":false`), []byte(`"auto_check_model_updates":true`), 1)
	if err := os.WriteFile(path, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if err := upgradeLegacyConfig(root, filepath.Dir(path), testPackage); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, tampered) {
		t.Fatal("upgrade replaced a config it did not write")
	}
}

func TestStageBF16CancellationLeavesNoPublishedModel(t *testing.T) {
	root, err := modelFixtureRoot(t, "oa-klein-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	file := filepath.Join(root, "source")
	data := []byte("copy me")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	pin := weightPin{source: file, rel: "main.safetensors", digest: fmt.Sprintf("%x", sha256.Sum256(data))}
	ctx, cancel := context.WithCancel(context.Background())
	err = stageBF16(ctx, root, testPackage, []weightPin{pin}, func(done, total int64) error { cancel(); return ctx.Err() }, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	modelRoot := filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b")
	if _, err := os.Stat(filepath.Join(modelRoot, "refs", "main")); !os.IsNotExist(err) {
		t.Fatal("canceled model published")
	}
}
