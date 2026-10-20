package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// The speech archives are pinned the way the image and chat backends are,
// at the reviews' digests, each with the manifest it stages; the Kokoros
// tree sits under the archive's windows-x86_64/ folder.
func TestSpeechArchivesArePinned(t *testing.T) {
	for _, c := range []struct {
		m      server.BackendManifest
		sha    string
		size   int64
		prefix string
	}{
		{server.WhisperCppVulkan, "e0d20a0f92e31b98adc0faf71172efc810b701e6391a9d858ca045bff26f77cd", 21203995, ""},
		{server.WhisperCppCPU, "2f494218bce1ea61dfcd88c318c4747b44e497789c993f2e7f81feabe94dfd8d", 4327767, ""},
		{server.KokorosB17, "df403e86f13b0f964a5a2d4b8ab4a1a6749c0613bb67e45825a4a8913b29c4ef", 19202614, "windows-x86_64/"},
	} {
		a, ok := archiveFor(c.m)
		if !ok || a.sha256 != c.sha || a.size != c.size || a.prefix != c.prefix {
			t.Fatalf("%s/%s: %+v %v", c.m.Family, c.m.Variant, a, ok)
		}
	}
	for _, p := range server.Packages {
		for _, m := range speechBackendsOf(p) {
			if _, ok := archiveFor(m); !ok {
				t.Fatalf("package %s pairs %s/%s with no reviewed archive", p.Version, m.Family, m.Variant)
			}
		}
	}
}

// A Kokoros-like gzip tar stages into kokoro/cpu with its top folder
// stripped, as Lemonade extracts it, and its nested data folder kept.
func TestStagedKokoroBackendKeepsTheDataFolder(t *testing.T) {
	entries := map[string]string{"koko.exe": "koko", "espeak-ng-data/phondata": "ph", "espeak-ng-data/voices/!v/Mr serious": "mr"}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	m := server.BackendManifest{Family: "kokoro", Variant: "cpu", Version: "b-fixture"}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	tw.WriteHeader(&tar.Header{Name: "./windows-x86_64/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, name := range names {
		tw.WriteHeader(&tar.Header{Name: "./windows-x86_64/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(entries[name]))})
		tw.Write([]byte(entries[name]))
		sum := sha256.Sum256([]byte(entries[name]))
		m.Files = append(m.Files, server.ManifestFile{Name: name, Size: int64(len(entries[name])), SHA256: hex.EncodeToString(sum[:])})
	}
	tw.Close()
	gz.Close()
	archive := filepath.Join(t.TempDir(), "kokoros.tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	a := backendArchive{manifest: m, size: int64(buf.Len()), sha256: hex.EncodeToString(sum[:]), prefix: "windows-x86_64/"}
	cache := filepath.Join(t.TempDir(), "engine-cache", "v1")
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	for name, content := range entries {
		got, err := os.ReadFile(filepath.Join(m.Dir(cache), filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	if err := server.VerifyBackendDir(m.Dir(cache), m); err != nil {
		t.Fatal(err)
	}
}

// whisper.cpp condition 6, Kokoros condition 2: a model is copied into the
// cache layout and checked against its pinned SHA-256; a file of other bytes
// is refused, and a staged folder that proves is kept.
func TestStageSpeechModel(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "engine-files", "v1")
	content := map[string]string{"index.json": `{"model":"m.onnx"}`, "m.onnx": "weights"}
	model := server.SpeechModel{Name: "kokoro-v1", Folder: "models--fixture--tts", Revision: "0123456789abcdef0123456789abcdef01234567"}
	var sources []string
	for _, name := range []string{"index.json", "m.onnx"} {
		sum := sha256.Sum256([]byte(content[name]))
		model.Files = append(model.Files, server.ManifestFile{Name: name, Size: int64(len(content[name])), SHA256: hex.EncodeToString(sum[:])})
		source := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(source, []byte(content[name]), 0600); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	pins, err := speechModelFiles(model, sources...)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageSpeechModel(context.Background(), backendFiles, root, stateDir, model, pins, false); err != nil {
		t.Fatal(err)
	}
	dir := model.Dir(modelsDirFor(root))
	if got, _ := os.ReadFile(filepath.Join(dir, "refs", "main")); string(got) != model.Revision {
		t.Fatalf("refs/main %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "snapshots", model.Revision, "m.onnx")); string(got) != "weights" {
		t.Fatalf("snapshot file %q", got)
	}
	if err := stageSpeechModel(context.Background(), backendFiles, root, stateDir, model, pins, false); err != nil {
		t.Fatalf("a proving folder was not kept: %v", err)
	}
	other := t.TempDir()
	if err := os.WriteFile(sources[1], []byte("WEIGHTS"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stageSpeechModel(context.Background(), backendFiles, other, filepath.Join(other, "state"), model, pins, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("changed model bytes: %v", err)
	}
	if _, err := os.Stat(model.Dir(modelsDirFor(other))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused model left its folder")
	}
}

// Speech staging needs a configured install and a reviewed archive for the
// selected package's pairing.
func TestStageSpeechNeedsAConfiguredInstall(t *testing.T) {
	if err := StageSpeech(context.Background(), t.TempDir(), SpeechFiles{WhisperModel: `C:\x.bin`}, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("speech without a selection: %v", err)
	}
	if _, err := speechModelFiles(server.KokoroV1Model, `C:\a`, `C:\b`); !errors.Is(err, ErrSelection) {
		t.Fatalf("two files for three pins: %v", err)
	}
}

// speechOnlySteps verify a fixture package and record each engine-file
// install.
func speechOnlySteps(root string, installs *[]server.Binding, packageErr error) engineSteps {
	return engineSteps{
		verifyPackage: func(p server.Package) (string, error) {
			if packageErr != nil {
				return "", packageErr
			}
			return filepath.Join(root, "apps", p.Version, "lemond.exe"), nil
		},
		installFiles: func(_ context.Context, b server.Binding) error {
			*installs = append(*installs, b)
			return nil
		},
	}
}

// On a state folder with no selection, --speech-only needs only the
// installed Lemonade package: it installs the package's engine files and
// saves a speech-only selection, with no picture model or image backend.
// The image configure and Setup's probe read that selection as an image
// setup still to run.
func TestSpeechOnlyOnAFreshStateFolder(t *testing.T) {
	withPackages(t, testPackage)
	root := t.TempDir()
	var installs []server.Binding
	if err := stageSpeech(context.Background(), root, SpeechFiles{}, false, backendFiles, speechOnlySteps(root, &installs, nil)); err != nil {
		t.Fatal(err)
	}
	selected, err := Read(SelectionPath(root))
	if err != nil || selected.Image() || len(selected.Models) != 0 || selected.Package != testPackage.ID {
		t.Fatalf("selection %+v: %v", selected, err)
	}
	if len(installs) != 1 || installs[0].CacheDir != CacheDirFor(root, testPackage.Version) || installs[0].Program != filepath.Join(root, "apps", testPackage.Version, "lemond.exe") {
		t.Fatalf("engine-file installs %+v", installs)
	}
	if p, err := configurePackage(root, ""); err != nil || p.ID != testPackage.ID {
		t.Fatalf("the image configure binds %+v: %v", p, err)
	}
	if complete, err := probeReviewedSetup(context.Background(), root, approvedBF16Pins(BF16Files{}), nil, speechOnlySteps(root, &installs, nil)); err != nil || complete {
		t.Fatalf("probe of a speech-only setup: %v %v", complete, err)
	}
	// A second run keeps the selection and stages into the selected version.
	if err := stageSpeech(context.Background(), root, SpeechFiles{}, false, backendFiles, speechOnlySteps(root, &installs, nil)); err != nil || len(installs) != 1 {
		t.Fatalf("second run: %v, %d installs", err, len(installs))
	}
}

// Without the installed package, speech on a fresh state folder names what
// it needs: the package version, its program and the --dir option. A speech
// file that does not prove leaves no selection behind.
func TestSpeechOnlyNamesTheMissingPackage(t *testing.T) {
	withPackages(t, testPackage)
	standInCache(t)
	root := t.TempDir()
	var installs []server.Binding
	err := stageSpeech(context.Background(), root, SpeechFiles{}, false, backendFiles, speechOnlySteps(root, &installs, ErrPackageRefused))
	program, _ := PackageProgramFor(testPackage)
	if !errors.Is(err, ErrSelection) || !errors.Is(err, ErrPackageRefused) || !strings.Contains(err.Error(), program) ||
		!strings.Contains(err.Error(), testPackage.Version) || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("missing package: %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "ggml-large-v3-turbo.bin")
	if err := os.WriteFile(wrong, []byte("not the model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stageSpeech(context.Background(), root, SpeechFiles{WhisperModel: wrong}, false, backendFiles, speechOnlySteps(root, &installs, nil)); !errors.Is(err, ErrSelection) {
		t.Fatalf("wrong model bytes: %v", err)
	}
	if _, err := os.Stat(SelectionPath(root)); !errors.Is(err, os.ErrNotExist) || len(installs) != 0 {
		t.Fatalf("a failed speech configure left a selection (%v) or installs (%d)", err, len(installs))
	}
}

// --dir roots every package lookup; an empty folder restores Setup's
// per-user one.
func TestUsePackagesDir(t *testing.T) {
	standInCache(t)
	dir := t.TempDir()
	if err := UsePackagesDir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { UsePackagesDir("") })
	program, err := PackageProgramFor(testPackage)
	if err != nil || program != filepath.Join(dir, testPackage.Version, testPackage.Manifest.Name, testPackage.Program) {
		t.Fatalf("program %s: %v", program, err)
	}
	if err := UsePackagesDir("relative"); !errors.Is(err, ErrSelection) {
		t.Fatalf("relative folder: %v", err)
	}
	if err := UsePackagesDir(""); err != nil {
		t.Fatal(err)
	}
	if got, _ := PackagesDir(); got != filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAbstractions", "apps", "lemonade") {
		t.Fatalf("per-user folder %s", got)
	}
}

// A selection names no model (speech-only) or the staged picture models,
// the starter model first and each declared model once; Image means at least
// one, the starter first. Any other model list is refused.
func TestSelectionModels(t *testing.T) {
	for models, valid := range map[string]bool{
		"": true, ApprovedModel: true, "user.Other": false, ApprovedModel + ",user.Other": false,
		ApprovedModel + ",user.Z-Image-Turbo": true, ApprovedModel + ",user.Z-Image-Turbo,user.Qwen-Image-2512": true,
		"user.Z-Image-Turbo": false, "user.Z-Image-Turbo," + ApprovedModel: false, ApprovedModel + ",user.Z-Image-Turbo,user.Z-Image-Turbo": false,
	} {
		s := Selection{Version: SelectionVersion, Package: testPackage.ID, Activated: true, Models: []string{}}
		if models != "" {
			s.Models = strings.Split(models, ",")
		}
		withPackages(t, testPackage)
		if err := s.Validate(); (err == nil) != valid {
			t.Fatalf("%q: %v", models, err)
		}
		if valid && s.Image() != (models != "") {
			t.Fatalf("%q: Image()=%v", models, s.Image())
		}
	}
}
