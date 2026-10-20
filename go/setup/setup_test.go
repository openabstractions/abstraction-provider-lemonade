package setup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-provider-lemonade/go/provider"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	res "github.com/openabstractions/abstraction-resource/go/client"
)

// standInCache points the person's cache folder (os.UserCacheDir:
// LOCALAPPDATA on Windows, XDG_CACHE_HOME on Linux, $HOME/Library/Caches on
// macOS) at a temporary folder and returns it, so no test reads or writes
// this account's provider state.
func standInCache(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("HOME", dir)
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

// idleLeases is leases@1 for a provider that starts nothing in the test: no
// call reaches it.
type idleLeases struct{}

func (idleLeases) Acquire(string, int64, time.Duration) (res.AcquireResult, error) {
	return res.AcquireResult{}, errors.New("no lease in this test")
}
func (idleLeases) BindProcess(string, int) (res.ProcessBindResult, error) {
	return res.ProcessBindResult{}, nil
}
func (idleLeases) Renew(string) (res.LeaseChange, error)   { return res.LeaseChange{}, nil }
func (idleLeases) Release(string) (res.LeaseChange, error) { return res.LeaseChange{}, nil }
func (idleLeases) Observe(ctx context.Context, _ string, _ time.Duration) (res.YieldPage, error) {
	<-ctx.Done()
	return res.YieldPage{}, ctx.Err()
}
func (idleLeases) Answer(context.Context, string, res.YieldAnswer, string) (res.AnswerResult, error) {
	return res.AnswerResult{}, nil
}

// The staging pins and the server's reviewed manifest describe one archive.
func TestStagingPinsMatchReviewedManifest(t *testing.T) {
	var expanded int64
	for _, f := range server.SDCppVulkan.Files {
		expanded += f.Size
		if f.Name == "sd-server.exe" && f.SHA256 != VulkanServerSHA256 {
			t.Fatal("sd-server.exe pin differs from the manifest")
		}
	}
	if expanded != vulkanExpandedBytes || len(vulkanEntries) != 21 || server.SDCppVulkan.Version != VulkanBackendVersion {
		t.Fatalf("manifest expands to %d bytes in %d files; staging pins %d", expanded, len(vulkanEntries), vulkanExpandedBytes)
	}
}

// The setup gate is the server's manifest check: a folder with every reviewed
// name and unreviewed bytes is refused with the manifest error.
func TestApprovedVulkanBackendHashesEveryFile(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "engine-cache")
	dir := backendDir(cache)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte(VulkanBackendVersion+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, f := range server.SDCppVulkan.Files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), []byte("unreviewed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyApprovedVulkanBackend(cache); !errors.Is(err, ErrSelection) || !errors.Is(err, server.ErrBackendManifest) {
		t.Fatalf("unreviewed libraries accepted: %v", err)
	}
}

func TestPackagedSelectionDerivesPrivatePathsAndRuntimeIdentity(t *testing.T) {
	root, err := os.MkdirTemp("", "oa-lemonade-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	tools := filepath.Join(root, "installed", "tools")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	providerProgram := filepath.Join(tools, "lemonaded.exe")
	runtimeProgram := filepath.Join(tools, "openabstractions.exe")
	engineProgram := filepath.Join(root, "selected-lemonade", "lemond.exe")
	if err := os.MkdirAll(filepath.Dir(engineProgram), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{providerProgram, runtimeProgram, engineProgram} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(root, "state")
	if err := Save(SelectionPath(state), selectionFor(testPackage, []string{ApprovedModel})); err != nil {
		t.Fatal(err)
	}
	read, err := Read(SelectionPath(state))
	if err != nil {
		t.Fatal(err)
	}
	steps := resolveSteps{
		program:       func(server.Package) (string, error) { return engineProgram, nil },
		verifyPackage: func(server.Package) (string, error) { return engineProgram, nil },
		verifyBackend: func(string, server.Package) error { return nil },
	}
	cfg, err := resolve(read, providerProgram, state, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.RequireVulkanBackend || cfg.Server.Bind == nil ||
		len(cfg.RuntimePrograms) != 1 || cfg.RuntimePrograms[0] != runtimeProgram || cfg.Name != ProviderID ||
		cfg.Server.Port < 1 || cfg.Endpoint == "" || cfg.Server.Models[0] != ApprovedModel ||
		// The installed declaration names both audio profiles, staged or not.
		!cfg.Server.Transcription || !cfg.Server.Speech {
		t.Fatalf("invalid installed setup: %+v", cfg)
	}
	b, err := cfg.Server.Bind(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Program != engineProgram || b.ProgramSHA256 != testPackage.ProgramSHA256 ||
		b.SDBackend.Version != VulkanBackendVersion || len(b.SDBackend.Files) != len(vulkanEntries) ||
		b.LlamaBackend.Version != server.LlamaCppVulkanB10825.Version || b.Package.Name != testPackage.Manifest.Name {
		t.Fatalf("invalid binding: %+v", b)
	}
	for _, path := range []string{cfg.StoreRoot, cfg.RecordPath, cfg.Server.TempDir, cfg.Server.ModelsDir, b.CacheDir, b.ConfigDir, b.StateDir} {
		if rel, err := filepath.Rel(state, path); err != nil || !filepath.IsAbs(path) || strings.HasPrefix(rel, "..") {
			t.Fatalf("path escaped private state: %s", path)
		}
	}
	if b.CacheDir != CacheDirFor(state, testPackage.Version) || b.ConfigDir != ConfigDirFor(state, testPackage.Version) {
		t.Fatalf("the binding names shared folders: %s, %s", b.CacheDir, b.ConfigDir)
	}
	if _, err := resolve(Selection{Version: SelectionVersion, Package: testPackage.ID, Activated: true, Models: []string{"user.A", "user.A"}}, providerProgram, state, steps); !errors.Is(err, ErrSelection) {
		t.Fatalf("duplicate model accepted: %v", err)
	}
	// A speech-only selection serves no picture model and needs no image
	// backend; its launch proves only the speech folders present.
	speechSteps := steps
	speechSteps.verifyBackend = func(string, server.Package) error { return errors.New("the image backend check ran") }
	speech, err := resolve(speechSelectionFor(testPackage), providerProgram, state, speechSteps)
	if err != nil {
		t.Fatal(err)
	}
	if speech.Server.RequireVulkanBackend || len(speech.Server.Models) != 0 || !speech.Server.Transcription || !speech.Server.Speech {
		t.Fatalf("speech-only setup: %+v", speech.Server)
	}
	// The speech-only configuration serves: the provider starts with no
	// image model, and no image model name is allowed. lemonaded serve adds
	// the lease service; this test starts nothing and takes no lease.
	speech.Leases = idleLeases{}
	// The provider admits only its designated runtime by Program proof, so
	// the rest is skipped wherever the socket endpoint cannot carry that
	// proof (macOS today). Lemonade's provider is packaged for Windows x64
	// alone.
	if err := listen.CanEver(speech.Endpoint, listen.Program); err != nil {
		t.Skipf("this platform's socket endpoint cannot carry Program proof: %v", err)
	}
	served, err := provider.New(speech)
	if err != nil {
		t.Fatalf("a speech-only selection does not serve: %v", err)
	}
	if served.Server.AllowsModel(ApprovedModel) || served.Server.AllowsModel("") {
		t.Fatal("a speech-only provider allows an image model")
	}
	if err := served.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(runtimeProgram); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(read, providerProgram, state, steps); !errors.Is(err, ErrSelection) {
		t.Fatalf("missing packaged runtime accepted: %v", err)
	}
}

// A selection naming each known package resolves to that version's program,
// cache, config and records; an unknown package is ErrSelection.
func TestSelectionResolvesEachKnownPackage(t *testing.T) {
	standInCache(t)
	second := testPackage
	second.ID, second.Version = "lemonade-embeddable-v2026.40.0-windows-amd64", "v2026.40.0"
	withPackages(t, testPackage, second)
	root := t.TempDir()
	for _, p := range server.Packages {
		if err := Save(SelectionPath(root), selectionFor(p, []string{ApprovedModel})); err != nil {
			t.Fatal(err)
		}
		b, err := bindSelected(context.Background(), root, PackageProgramFor)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := PackageProgramFor(p)
		if b.Program != want || !strings.Contains(b.Program, string(filepath.Separator)+p.Version+string(filepath.Separator)) ||
			b.CacheDir != filepath.Join(root, "engine-cache", p.Version) || b.ConfigDir != filepath.Join(root, "engine-config", p.Version) ||
			b.StateDir != filepath.Join(root, "engine-files", p.Version) {
			t.Fatalf("%s binds %+v", p.Version, b)
		}
	}
	for _, id := range []string{"lemonade-embeddable-v2026.38.0-windows-amd64", "custom-exe", ""} {
		if err := (Selection{Version: SelectionVersion, Package: id, Activated: true, Models: []string{ApprovedModel}}).Validate(); !errors.Is(err, ErrSelection) {
			t.Fatalf("unknown package %q accepted: %v", id, err)
		}
	}
}

// withPackages replaces the provider's package table for one test.
func withPackages(t *testing.T, packages ...server.Package) {
	t.Helper()
	saved := server.Packages
	server.Packages = packages
	t.Cleanup(func() { server.Packages = saved })
}

func TestPackageHashAndActivation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lemond.exe")
	if err := os.WriteFile(path, []byte("reviewed fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("reviewed fixture")))
	if err := verifyProgram(path, hash); err != nil {
		t.Fatal(err)
	}
	if err := verifyProgram(path, testPackage.ProgramSHA256); !errors.Is(err, ErrSelection) {
		t.Fatalf("replacement accepted: %v", err)
	}
	if err := (Selection{Version: SelectionVersion, Package: testPackage.ID, Models: []string{ApprovedModel}}).Validate(); !errors.Is(err, ErrSelection) {
		t.Fatal("missing activation accepted")
	}
	if err := (Selection{Version: SelectionVersion, Package: "custom-exe", Activated: true, Models: []string{ApprovedModel}}).Validate(); !errors.Is(err, ErrSelection) {
		t.Fatal("custom executable accepted in guided selection")
	}
}

func TestSelectionReplacementRetainsPriorValueOnRefusal(t *testing.T) {
	path := SelectionPath(t.TempDir())
	old := selectionFor(testPackage, []string{ApprovedModel})
	if err := Save(path, old); err != nil {
		t.Fatal(err)
	}
	invalid := old
	invalid.Package = "unreviewed"
	if err := Save(path, invalid); !errors.Is(err, ErrSelection) {
		t.Fatalf("invalid replacement accepted: %v", err)
	}
	got, err := Read(path)
	if err != nil || got.Package != old.Package || !got.Activated {
		t.Fatalf("prior activation changed after failed replacement: %+v, %v", got, err)
	}
}

func TestModelPathLengthBoundary(t *testing.T) {
	// A short temporary root: the test's own folder name can already use up
	// the Windows limit under a long temporary folder (macOS /var/folders).
	root, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := CheckModelPath(root, approvedRevision); err != nil {
		if len(root) > 80 {
			t.Skipf("temporary folder %s leaves no room below Lemonade's path limit", root)
		}
		t.Fatal(err)
	}
	for len(filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b", "snapshots", approvedRevision, "split_files", "diffusion_models", "flux-2-klein-4b.safetensors")) < 260 {
		root = filepath.Join(root, "long")
	}
	if err := CheckModelPath(root, approvedRevision); !errors.Is(err, ErrSelection) {
		t.Fatalf("long path accepted: %v", err)
	}
}
