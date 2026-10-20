package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// The packaging manifest lists every known package in table order, and every
// entry's backends have a reviewed archive whose SHA-256 the pairing names.
func TestPackagingManifestListsEveryKnownPackage(t *testing.T) {
	if err := server.ValidTable(server.Packages); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows-amd64.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		KnownPackages []struct {
			Version              string `json:"version"`
			SelectionID          string `json:"selection_id"`
			ArchiveSHA256        string `json:"archive_sha256"`
			ArchiveRoot          string `json:"archive_root"`
			Program              string `json:"program"`
			ProgramSHA256        string `json:"program_sha256"`
			BackendArchiveSHA256 string `json:"backend_archive_sha256"`
			SDBackend            string `json:"sd_backend"`
			LlamaBackend         string `json:"llamacpp_backend"`
			ModelRevision        string `json:"model_revision"`
		} `json:"known_packages"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.KnownPackages) != len(server.Packages) {
		t.Fatalf("the packaging manifest lists %d packages, the table %d", len(manifest.KnownPackages), len(server.Packages))
	}
	for i, p := range server.Packages {
		m := manifest.KnownPackages[i]
		name := func(b server.BackendManifest) string { return b.Family + "/" + b.Variant + "/" + b.Version }
		if m.Version != p.Version || m.SelectionID != p.ID || m.ArchiveSHA256 != p.ArchiveSHA256 || m.ArchiveRoot != p.Manifest.Name ||
			m.Program != p.Program || m.ProgramSHA256 != p.ProgramSHA256 || m.BackendArchiveSHA256 != p.BackendArchiveSHA256 ||
			m.SDBackend != name(p.SDBackend) || m.LlamaBackend != name(p.LlamaBackend) || m.ModelRevision != p.ModelRevision {
			t.Fatalf("packaging entry %d differs from the table: %+v", i, m)
		}
		sd, ok := archiveFor(p.SDBackend)
		if !ok || sd.sha256 != p.BackendArchiveSHA256 {
			t.Fatalf("%s pairs an image backend archive no reviewed pin stages", p.Version)
		}
		if _, ok := archiveFor(p.LlamaBackend); !ok {
			t.Fatalf("%s pairs a llama.cpp build no reviewed pin stages", p.Version)
		}
		if p.ModelRevision != approvedRevision {
			t.Fatalf("%s pairs model revision %s; setup stages %s", p.Version, p.ModelRevision, approvedRevision)
		}
	}
}

// fixturePackageTree writes files under root\<name> and returns a table
// entry of version whose manifest pins them, with lemond.exe as its program.
func fixturePackageTree(t *testing.T, root, version string, files map[string]string) server.Package {
	t.Helper()
	p := testPackage
	p.ID, p.Version = "fixture-"+version, version
	p.Manifest = server.PackageManifest{Name: "fixture-" + version}
	for name, data := range files {
		path := filepath.Join(root, p.Manifest.Name, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(data))
		p.Manifest.Files = append(p.Manifest.Files, server.ManifestFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		if name == "lemond.exe" {
			p.ProgramSHA256 = hex.EncodeToString(sum[:])
		}
	}
	return p
}

// package check proves a staged tree against the version's manifest: an
// extra file, a changed file and a link are refused; Setup's receipt beside
// the package folder is not part of it. It sets no ACL and writes no record.
func TestPackageCheckProvesAStagedTreeWithoutInstallingIt(t *testing.T) {
	files := map[string]string{"lemond.exe": "server", "LICENSE": "apache", "resources/defaults.json": `{"offline":true}`}
	fresh := func(t *testing.T) (string, server.Package) {
		root := t.TempDir()
		p := fixturePackageTree(t, root, "vC", files)
		withPackages(t, p)
		if err := os.WriteFile(filepath.Join(root, ".oa-install.json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return root, p
	}
	root, p := fresh(t)
	got, err := CheckPackage(context.Background(), "vC", root)
	if err != nil || got.BackendArchiveSHA256 != p.BackendArchiveSHA256 || got.ModelRevision != p.ModelRevision {
		t.Fatalf("the exact tree was refused or reported another pairing: %+v %v", got, err)
	}
	if runtime.GOOS == "windows" {
		if installed, err := programfiles.Installed(filepath.Join(root, p.Manifest.Name)); err != nil || installed {
			t.Fatalf("package check changed the folder ACL: %v %v", installed, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("package check wrote beside the package: %v %v", entries, err)
	}
	for name, mutate := range map[string]func(t *testing.T, dir string){
		"extra file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "version.dll"), []byte("planted"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"changed file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "resources", "defaults.json"), []byte(`{"offline":fals}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"link": func(t *testing.T, dir string) {
			target := filepath.Join(dir, "LICENSE")
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), target); err != nil {
				t.Skipf("symbolic links are unavailable: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root, p := fresh(t)
			mutate(t, filepath.Join(root, p.Manifest.Name))
			if _, err := CheckPackage(context.Background(), "vC", root); !errors.Is(err, ErrPackageRefused) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		})
	}
	if _, err := CheckPackage(context.Background(), "v1999.1.1", root); !errors.Is(err, ErrPackageRefused) || !errors.Is(err, server.ErrUnknownPackage) {
		t.Fatalf("an unknown version was checked: %v", err)
	}
}

// activationFixture is a configured provider root bound to version vA, with
// a staged private model, vA's image backend and a second entry vB paired
// with the same backend build.
type activationFixture struct {
	root              string
	a, b              server.Package
	pins              []weightPin
	sd                backendArchive
	sdZip             string
	steps             engineSteps
	installs          []server.Binding
	selectedAtInstall []string
}

func newActivationFixture(t *testing.T) *activationFixture {
	t.Helper()
	ctx := context.Background()
	// The model path limit (CheckModelPath) needs a short root.
	root, err := modelFixtureRoot(t, "oa-act-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &activationFixture{root: root}
	for _, rel := range []string{"diffusion_models/main.safetensors", "text_encoders/encoder.safetensors", "vae/vae.safetensors"} {
		source := filepath.Join(t.TempDir(), filepath.Base(rel))
		if err := os.WriteFile(source, []byte(rel), 0o600); err != nil {
			t.Fatal(err)
		}
		f.pins = append(f.pins, weightPin{source: source, rel: filepath.FromSlash(rel), digest: fmt.Sprintf("%x", sha256.Sum256([]byte(rel)))})
	}
	f.sd, f.sdZip = fixtureArchive(t, map[string]string{"sd-server.exe": "sd", "ggml.dll": "ggml"})
	f.sd.manifest.Family = "sd-cpp"
	llama, _ := fixtureArchive(t, map[string]string{"llama-server.exe": "server"})
	f.a = testPackage
	f.a.ID, f.a.Version, f.a.SDBackend, f.a.LlamaBackend = "fixture-a", "vA", f.sd.manifest, llama.manifest
	f.b = f.a
	f.b.ID, f.b.Version = "fixture-b", "vB"
	withPackages(t, f.a, f.b)
	if err := stageBF16(ctx, root, f.a, f.pins, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := stageBackends(ctx, CacheDirFor(root, "vA"), []stagedArchive{{f.sd, f.sdZip}}, false, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Save(SelectionPath(root), selectionFor(f.a, []string{ApprovedModel})); err != nil {
		t.Fatal(err)
	}
	f.steps = engineSteps{
		verifyPackage: func(p server.Package) (string, error) {
			return filepath.Join(root, "apps", p.Version, "lemond.exe"), nil
		},
		verifyBackend: func(cache string, p server.Package) error {
			return server.CheckBackendTree(cache, p.SDBackend, p.LlamaBackend, true)
		},
		installFiles: func(_ context.Context, b server.Binding) error {
			selected, err := Read(SelectionPath(root))
			if err != nil {
				return err
			}
			if err := server.CheckBackendTree(b.CacheDir, b.SDBackend, b.LlamaBackend, true); err != nil {
				return fmt.Errorf("engine files installed before the backend was staged: %w", err)
			}
			f.installs = append(f.installs, b)
			f.selectedAtInstall = append(f.selectedAtInstall, selected.Package)
			return nil
		},
		filesInstalled: func(b server.Binding) (bool, error) {
			for _, installed := range f.installs {
				if installed.Program == b.Program {
					return true, nil
				}
			}
			return false, nil
		},
	}
	return f
}

func (f *activationFixture) selected(t *testing.T) string {
	t.Helper()
	s, err := Read(SelectionPath(f.root))
	if err != nil {
		t.Fatal(err)
	}
	return s.Package
}

// package activate installs vB's engine files with vB's backend staged in its
// own cache, writes vB's config folder, and selects vB last. A second run
// with the selection at vB proves and returns without copying.
func TestPackageActivateSelectsTheVersionLast(t *testing.T) {
	f := newActivationFixture(t)
	ctx := context.Background()
	if err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps); err != nil {
		t.Fatal(err)
	}
	if f.selected(t) != f.b.ID || len(f.installs) != 1 || f.selectedAtInstall[0] != f.a.ID {
		t.Fatalf("selection %s after %d installs; at install %v", f.selected(t), len(f.installs), f.selectedAtInstall)
	}
	b := f.installs[0]
	if b.CacheDir != CacheDirFor(f.root, "vB") || b.ConfigDir != ConfigDirFor(f.root, "vB") || b.StateDir != StateDirFor(f.root, "vB") {
		t.Fatalf("activation installed another version's folders: %+v", b)
	}
	if err := server.VerifyBackendDir(f.sd.manifest.Dir(b.CacheDir), f.sd.manifest); err != nil {
		t.Fatalf("vB's backend is not in vB's cache: %v", err)
	}
	if err := server.VerifyBackendDir(f.sd.manifest.Dir(CacheDirFor(f.root, "vA")), f.sd.manifest); err != nil {
		t.Fatalf("activation changed vA's backend: %v", err)
	}
	locked, _ := server.LockedConfig(filepath.Join(f.root, "engine-models"))
	if got, err := os.ReadFile(filepath.Join(b.ConfigDir, "config.json")); err != nil || !bytes.Equal(got, locked) {
		t.Fatalf("vB's config folder lacks the locked config: %v", err)
	}
	if ready, err := probeBF16(ctx, f.root, b.ConfigDir, f.b.Config, f.pins); err != nil || !ready {
		t.Fatalf("the shared model does not verify for vB: %v %v", ready, err)
	}
	sdServer := filepath.Join(f.sd.manifest.Dir(b.CacheDir), "sd-server.exe")
	before, err := os.Stat(sdServer)
	if err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps); err != nil {
		t.Fatalf("a second activation refused: %v", err)
	}
	after, err := os.Stat(sdServer)
	if err != nil || len(f.installs) != 1 || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("a second activation copied again: %d installs, %v", len(f.installs), err)
	}
}

// A process killed while it copied vA's backend for vB leaves its staging
// folder beside vB's backend folder. vB is not bound, so the next activation
// removes the leftover under the setup lock and completes.
func TestPackageActivateClearsAnInterruptedBackendCopy(t *testing.T) {
	f := newActivationFixture(t)
	parent := filepath.Dir(f.sd.manifest.Dir(CacheDirFor(f.root, "vB")))
	leftover := filepath.Join(parent, ".vulkan-stage-123456789")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "sd-server.exe"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(context.Background(), f.root, "vB", "", "", false, f.steps); err != nil {
		t.Fatalf("a leftover copy blocked the activation: %v", err)
	}
	if f.selected(t) != f.b.ID {
		t.Fatalf("the selection stayed at %s", f.selected(t))
	}
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the leftover copy remains: %v", err)
	}
}

// A source backend changed by one byte is refused before anything is copied;
// the selection stays at vA.
func TestPackageActivateRefusesATamperedSourceBackend(t *testing.T) {
	f := newActivationFixture(t)
	server := filepath.Join(f.sd.manifest.Dir(CacheDirFor(f.root, "vA")), "sd-server.exe")
	if err := os.WriteFile(server, []byte("sD"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(context.Background(), f.root, "vB", "", "", false, f.steps); err == nil {
		t.Fatal("a tampered source backend was copied")
	}
	if f.selected(t) != f.a.ID || len(f.installs) != 0 {
		t.Fatalf("a refused copy moved the selection or installed files: %s", f.selected(t))
	}
	if _, err := os.Lstat(f.sd.manifest.Dir(CacheDirFor(f.root, "vB"))); !os.IsNotExist(err) {
		t.Fatalf("vB holds a backend copied from a tampered source: %v", err)
	}
}

// A package proof that could not read a file (an antivirus lock) is passed on
// as it is: activation fails without the definitive refusal, and Setup keeps
// the version checked for another attempt.
func TestPackageActivateOfAnUnreadablePackageIsNoRefusal(t *testing.T) {
	f := newActivationFixture(t)
	unreadable := f.steps
	unreadable.verifyPackage = func(server.Package) (string, error) {
		return "", &os.PathError{Op: "open", Path: "lemond.exe", Err: errors.New("the file is in use by another process")}
	}
	err := activatePackage(context.Background(), f.root, "vB", "", "", false, unreadable)
	if err == nil || errors.Is(err, ErrPackageRefused) || f.selected(t) != f.a.ID {
		t.Fatalf("an unreadable package was refused or moved the selection: %v", err)
	}
}

// An archive named for vB is staged from the archive, not from vA.
func TestPackageActivateStagesANamedArchive(t *testing.T) {
	f := newActivationFixture(t)
	saved := knownArchives
	knownArchives = []backendArchive{f.sd}
	t.Cleanup(func() { knownArchives = saved })
	if err := os.RemoveAll(CacheDirFor(f.root, "vA")); err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(context.Background(), f.root, "vB", f.sdZip, "", false, f.steps); err != nil {
		t.Fatal(err)
	}
	if err := server.VerifyBackendDir(f.sd.manifest.Dir(CacheDirFor(f.root, "vB")), f.sd.manifest); err != nil || f.selected(t) != f.b.ID {
		t.Fatalf("the named archive was not staged: %v", err)
	}
}

// A refusal leaves the selection where it was: a failed engine-file install,
// a private model of another revision (with the named remedy), and an
// unknown version.
func TestPackageActivateRefusalKeepsTheSelection(t *testing.T) {
	f := newActivationFixture(t)
	ctx := context.Background()
	failing := f.steps
	failing.installFiles = func(context.Context, server.Binding) error { return errors.New("folder ACL refused") }
	if err := activatePackage(ctx, f.root, "vB", "", "", false, failing); err == nil || f.selected(t) != f.a.ID {
		t.Fatalf("a failed install moved the selection: %v", err)
	}
	other := f.b
	other.ModelRevision = strings.Repeat("1", 40)
	withPackages(t, f.a, other)
	err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps)
	if !errors.Is(err, ErrPackageRefused) || !strings.Contains(err.Error(), "--replace-private-model") || f.selected(t) != f.a.ID || len(f.installs) != 0 {
		t.Fatalf("a pairing with another model revision was activated, is no definitive refusal or names no remedy: %v", err)
	}
	if err := activatePackage(ctx, f.root, "vZ", "", "", false, f.steps); !errors.Is(err, ErrPackageRefused) || f.selected(t) != f.a.ID {
		t.Fatalf("an unknown version was activated: %v", err)
	}
	// The selected version whose engine files are not installed is a
	// definitive refusal too; the engine-files step repairs it.
	if err := activatePackage(ctx, f.root, "vA", "", "", false, f.steps); !errors.Is(err, ErrPackageRefused) || f.selected(t) != f.a.ID {
		t.Fatalf("a selected version without its engine files: %v", err)
	}
	if err := os.Remove(SelectionPath(f.root)); err != nil {
		t.Fatal(err)
	}
	if err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps); !errors.Is(err, ErrSelection) {
		t.Fatalf("activation without a configured install: %v", err)
	}
}

// package retire refuses the selected version and otherwise removes the
// version's cache, config and records; it never touches the private model,
// the content store or the selection.
func TestPackageRetireRemovesOnlyTheVersionsFolders(t *testing.T) {
	f := newActivationFixture(t)
	ctx := context.Background()
	if err := activatePackage(ctx, f.root, "vB", "", "", false, f.steps); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(f.root, "store", "object")
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(StateDirFor(f.root, "vA"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RetirePackage(ctx, f.root, "vB"); !errors.Is(err, ErrPackageRefused) {
		t.Fatalf("the selected version was retired: %v", err)
	}
	if err := RetirePackage(ctx, f.root, "vA"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{CacheDirFor(f.root, "vA"), ConfigDirFor(f.root, "vA"), StateDirFor(f.root, "vA")} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s remains after retire: %v", gone, err)
		}
	}
	if ready, err := probeBF16(ctx, f.root, ConfigDirFor(f.root, "vB"), f.b.Config, f.pins); err != nil || !ready {
		t.Fatalf("retire touched the private model or the bound version's config: %v %v", ready, err)
	}
	if _, err := os.Stat(store); err != nil || f.selected(t) != f.b.ID {
		t.Fatalf("retire touched the store or the selection: %v", err)
	}
	if err := server.VerifyBackendDir(f.sd.manifest.Dir(CacheDirFor(f.root, "vB")), f.sd.manifest); err != nil {
		t.Fatalf("retire touched the bound version's backend: %v", err)
	}
	if err := RetirePackage(ctx, f.root, "vA"); err != nil {
		t.Fatalf("retiring a retired version again: %v", err)
	}
	if err := RetirePackage(ctx, f.root, "vZ"); !errors.Is(err, ErrPackageRefused) {
		t.Fatalf("an unknown version was retired: %v", err)
	}
}

// A root from before per-version folders verifies incomplete; the
// engine-files step moves engine-cache\bin and engine-config's files under
// the selected version once, installs the records again, and the root
// verifies afterwards. A second run moves nothing.
func TestLegacyLayoutMovesUnderTheSelectedVersionOnce(t *testing.T) {
	f := newActivationFixture(t)
	ctx := context.Background()
	root := f.root
	if err := os.Rename(CacheDirFor(root, "vA"), filepath.Join(root, "engine-cache-v")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "engine-cache")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "engine-cache-v"), filepath.Join(root, "engine-cache")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.json", "user_models.json"} {
		if err := os.Rename(filepath.Join(ConfigDirFor(root, "vA"), name), filepath.Join(root, "engine-config", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(ConfigDirFor(root, "vA")); err != nil {
		t.Fatal(err)
	}
	legacyRecord := filepath.Join(EngineFilesDir(root), "package.files")
	if err := os.WriteFile(legacyRecord, []byte("bound to the old path"), 0o600); err != nil {
		t.Fatal(err)
	}
	if complete, err := probeReviewedSetup(ctx, root, f.pins, nil, f.steps); err != nil || complete {
		t.Fatalf("a legacy layout verified: %v %v", complete, err)
	}
	weight := filepath.Join(root, "engine-models", modelRepositoryFolder, "snapshots", approvedRevision, "split_files", f.pins[0].rel)
	before, err := os.Stat(weight)
	if err != nil {
		t.Fatal(err)
	}
	if err := installEngineFiles(ctx, root, nil, false, f.steps); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(weight); err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("the move touched the private model: %v", err)
	}
	for _, gone := range []string{filepath.Join(root, "engine-cache", "bin"), filepath.Join(root, "engine-config", "config.json"), legacyRecord} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s remains after the move: %v", gone, err)
		}
	}
	if err := server.VerifyBackendDir(f.sd.manifest.Dir(CacheDirFor(root, "vA")), f.sd.manifest); err != nil {
		t.Fatalf("the backend did not move under vA: %v", err)
	}
	if len(f.installs) != 1 || f.installs[0].CacheDir != CacheDirFor(root, "vA") {
		t.Fatalf("the records were not installed again for the moved trees: %+v", f.installs)
	}
	if complete, err := probeReviewedSetup(ctx, root, f.pins, nil, f.steps); err != nil || !complete {
		t.Fatalf("the moved layout does not verify: %v %v", complete, err)
	}
	if moved, err := migrateLegacyLayout(root, f.a); err != nil || moved {
		t.Fatalf("a second run moved something: %v %v", moved, err)
	}
}
