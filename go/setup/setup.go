// Package setup turns one reviewed engine/model selection into private native
// provider paths. The installed provider needs no path, port or identity flags.
package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"time"

	cas "github.com/openabstractions/abstraction-cas/go"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/provider"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

const SelectionVersion = 1

const ProviderID = "lemonade-image"
const approvedRevision = "5f526678002e43af5551dadb73ce2e8c91b43afe"

var ErrSelection = errors.New("lemonade setup: invalid selection")

// Selection records explicit activation of one reviewed portable app, the
// entry of server.Packages its package names, and the chosen models. It does
// not authorize an installed Lemonade service or a model download. Setup
// moves its package through `lemonaded package activate`. A selection with
// no models is speech-only: `configure --speech-only` writes it on a fresh
// state folder, and the image configure adds the picture model to it.
type Selection struct {
	Version   int      `json:"version"`
	Package   string   `json:"package"`
	Activated bool     `json:"activated"`
	Models    []string `json:"models"`
}

// packagesDir is the folder UsePackagesDir names, or "" for Setup's
// per-user folder. One lemonaded process sets it once, before any command.
var packagesDir string

// UsePackagesDir roots every package lookup of this process at dir, an
// absolute folder holding <Version>\<archive root>\<program> as Setup lays
// it out; lemonaded --dir sets it so a test can run an isolated install.
// An empty dir restores Setup's per-user folder.
func UsePackagesDir(dir string) error {
	if dir == "" {
		packagesDir = ""
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: the package folder must be absolute", ErrSelection)
	}
	packagesDir = filepath.Clean(dir)
	return nil
}

// PackagesDir is the folder that holds each installed Lemonade version:
// UsePackagesDir's, or %LOCALAPPDATA%\OpenAbstractions\apps\lemonade, where
// Setup extracts each version.
func PackagesDir() (string, error) {
	if packagesDir != "" {
		return packagesDir, nil
	}
	local := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(local) {
		return "", ErrSelection
	}
	return filepath.Join(local, "OpenAbstractions", "apps", "lemonade"), nil
}

// PackageProgramFor is the location of p's program: Setup extracts each
// version into PackagesDir()\<Version> and preserves the upstream archive
// root and its accompanying license.
func PackageProgramFor(p server.Package) (string, error) {
	dir, err := PackagesDir()
	if err != nil || p.Version == "" || p.Manifest.Name == "" || p.Program == "" {
		return "", ErrSelection
	}
	return filepath.Join(dir, p.Version, p.Manifest.Name, p.Program), nil
}

// verifyProgram hashes the program at path. A missing program, one that is
// no regular file, and a digest mismatch are definitive (ErrPackageRefused);
// a program that could not be opened or read, such as one an antivirus scan
// holds, returns that error for another attempt.
func verifyProgram(path, expected string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %w: reviewed engine program missing", ErrSelection, ErrPackageRefused)
	}
	if err != nil {
		return fmt.Errorf("%w: reviewed engine program: %w", ErrSelection, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: reviewed engine program: %w", ErrSelection, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("%w: reviewed engine digest: %w", ErrSelection, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return fmt.Errorf("%w: %w: reviewed engine digest mismatch", ErrSelection, ErrPackageRefused)
	}
	return nil
}

// verifyPackageAt hashes p's program and every file of the package folder
// that holds it against p's manifest; it changes no ACL and writes no record.
// A file the kit could not open or read (enginefiles.ErrUnreadable), such as
// one an antivirus scan holds, returns that error with its system cause for
// another attempt; every other refusal of the folder is definitive
// (ErrPackageRefused).
func verifyPackageAt(p server.Package, program string) error {
	if err := verifyProgram(program, p.ProgramSHA256); err != nil {
		return err
	}
	if err := server.VerifyPackageDir(filepath.Dir(program), p.Manifest); err != nil {
		if errors.Is(err, enginefiles.ErrUnreadable) {
			return fmt.Errorf("%w: %w", ErrSelection, err)
		}
		return fmt.Errorf("%w: %w: %w", ErrSelection, ErrPackageRefused, err)
	}
	return nil
}

// VerifyPackage rejects replacement or removal of p's reviewed files at the
// folder Setup installs it into, and returns its program.
func VerifyPackage(p server.Package) (string, error) {
	program, err := PackageProgramFor(p)
	if err != nil {
		return "", err
	}
	if err := verifyPackageAt(p, program); err != nil {
		return "", err
	}
	return program, nil
}

// DefaultRoot is per-user state. It is separate from Lemonade's normal
// installation and profile, so the provider cannot change an owner's server.
func DefaultRoot() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "OpenAbstractions", "providers", "lemonade"), nil
}

func SelectionPath(root string) string { return filepath.Join(root, "selection.json") }

// EngineFilesDir holds the metadata records of the package and backend
// folders (inference CONTRACT.md INF-F2), one folder per package version.
func EngineFilesDir(root string) string { return filepath.Join(root, "engine-files") }

// CacheDirFor is version's Lemonade cache, which holds that version's paired
// backends under bin.
func CacheDirFor(root, version string) string { return filepath.Join(root, "engine-cache", version) }

// ConfigDirFor is version's Lemonade config folder: its locked config.json
// and the model registry.
func ConfigDirFor(root, version string) string { return filepath.Join(root, "engine-config", version) }

// StateDirFor holds the engine-file records of version's package and backend
// folders. The records are bound to each tree's path, so each version keeps
// its own and package retire removes them with the version.
func StateDirFor(root, version string) string { return filepath.Join(EngineFilesDir(root), version) }

// CheckModelPath fails before setup when the qualified Lemonade Windows build
// could not find the model of revision through a legacy-length filename
// (>259 chars).
func CheckModelPath(root, revision string) error {
	longest := filepath.Join(root, "engine-models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b", "snapshots", revision,
		"split_files", "diffusion_models", "flux-2-klein-4b.safetensors")
	if len(longest) >= 260 {
		return fmt.Errorf("%w: private model path exceeds Lemonade's Windows path limit (%d characters)", ErrSelection, len(longest))
	}
	return nil
}

func Read(path string) (Selection, error) { return readSelection(context.Background(), path) }

// readSelection is Read bounded by ctx.
func readSelection(ctx context.Context, path string) (Selection, error) {
	data, err := cas.ReadLimitContext(ctx, path, 4096)
	if err != nil {
		return Selection{}, err
	}
	if data == nil {
		return Selection{}, os.ErrNotExist
	}
	var selected Selection
	if err := json.Unmarshal(data, &selected); err != nil {
		return Selection{}, fmt.Errorf("%w: %v", ErrSelection, err)
	}
	if err := selected.Validate(); err != nil {
		return Selection{}, err
	}
	return selected, nil
}

// Image reports whether the selection serves picture models: at least one,
// the starter model first.
func (s Selection) Image() bool { return len(s.Models) > 0 && s.Models[0] == ApprovedModel }

// Validate admits a selection whose package is an entry of server.Packages
// and whose models are none (speech-only) or the staged picture models: the
// starter model first, then each other declared model (server.ImageModels)
// at most once, in the order Setup staged them.
func (s Selection) Validate() error {
	if s.Version != SelectionVersion || !s.Activated || len(s.Models) > len(server.ImageModels) ||
		len(s.Models) > 0 && s.Models[0] != ApprovedModel {
		return ErrSelection
	}
	if _, ok := server.FindPackageID(s.Package); !ok {
		return fmt.Errorf("%w: %w: %q", ErrSelection, server.ErrUnknownPackage, s.Package)
	}
	seen := make(map[string]bool, len(s.Models))
	for _, model := range s.Models {
		if _, declared := server.FindImageModel(model); !declared || seen[model] {
			return fmt.Errorf("%w: model", ErrSelection)
		}
		seen[model] = true
	}
	return nil
}

// selectionFor is the selection that binds p and serves models.
func selectionFor(p server.Package, models []string) Selection {
	return Selection{Version: SelectionVersion, Package: p.ID, Activated: true, Models: append([]string(nil), models...)}
}

// selectedModels are the models the saved selection serves, or the starter
// model alone when nothing is selected yet. A selection that does not read
// keeps the starter model alone; probes report it separately.
func selectedModels(root string) []string {
	if selected, err := Read(SelectionPath(root)); err == nil {
		return append([]string(nil), selected.Models...)
	}
	return []string{ApprovedModel}
}

// speechSelectionFor is the speech-only selection that binds p.
func speechSelectionFor(p server.Package) Selection {
	return Selection{Version: SelectionVersion, Package: p.ID, Activated: true, Models: []string{}}
}

// selectedPackage is the entry a valid selection names.
func selectedPackage(s Selection) server.Package {
	p, _ := server.FindPackageID(s.Package)
	return p
}

// Save records the reviewed package ID and model IDs; paths are derived.
func Save(path string, selected Selection) error {
	if err := selected.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(selected, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return cas.ChangeLimit(path, 4096, func([]byte) ([]byte, error) { return data, nil })
}

// bindingFor names p's program, manifests and per-version folders under
// root.
func bindingFor(root string, p server.Package, program string) server.Binding {
	speech := speechBackendsOf(p)
	return server.Binding{
		Program: program, ProgramSHA256: p.ProgramSHA256, Package: p.Manifest,
		SDBackend: p.SDBackend, LlamaBackend: p.LlamaBackend, WhisperBackend: speech[0], KokoroBackend: speech[1],
		CacheDir: CacheDirFor(root, p.Version), ConfigDir: ConfigDirFor(root, p.Version), StateDir: StateDirFor(root, p.Version),
		LockedConfig: p.Config,
	}
}

// Resolve derives the packaged runtime identity, private paths and temporary
// engine port. The provider endpoint remains stable for this user's account;
// the engine's loopback port is private and may change on provider restart.
// The returned server configuration reads the selection at each launch, so
// the version Setup activates runs from the next launch on.
func Resolve(selected Selection, providerProgram, root string) (provider.Config, error) {
	return resolve(selected, providerProgram, root, resolveSteps{
		program: PackageProgramFor, verifyPackage: VerifyPackage, verifyBackend: verifyApprovedBackend,
	})
}

// resolveSteps are the installed-package lookups Resolve makes; tests
// replace them.
type resolveSteps struct {
	program       func(server.Package) (string, error)
	verifyPackage func(server.Package) (string, error)
	verifyBackend func(cacheDir string, p server.Package) error
}

func resolve(selected Selection, providerProgram, root string, steps resolveSteps) (provider.Config, error) {
	if err := selected.Validate(); err != nil {
		return provider.Config{}, err
	}
	if !filepath.IsAbs(providerProgram) || !filepath.IsAbs(root) {
		return provider.Config{}, ErrSelection
	}
	p := selectedPackage(selected)
	if _, err := steps.verifyPackage(p); err != nil {
		return provider.Config{}, err
	}
	if err := CheckModelPath(root, p.ModelRevision); err != nil {
		return provider.Config{}, err
	}
	err := withSetupLock(context.Background(), root, func() error {
		if legacy, err := legacyLayout(root); err != nil || legacy {
			if err == nil {
				err = fmt.Errorf("%w: the private engine folders predate per-version folders; %s", ErrSelection, engineFilesRemedy)
			}
			return err
		}
		verify := steps.verifyBackend
		if !selected.Image() {
			verify = verifySpeechBackend
		}
		if err := verify(CacheDirFor(root, p.Version), p); err != nil {
			return err
		}
		return upgradeLegacyConfig(root, ConfigDirFor(root, p.Version), p)
	})
	if err != nil {
		return provider.Config{}, err
	}
	runtimeProgram := filepath.Join(filepath.Dir(providerProgram), "openabstractions.exe")
	if info, err := os.Stat(runtimeProgram); err != nil || info.IsDir() {
		return provider.Config{}, fmt.Errorf("%w: packaged runtime", ErrSelection)
	}
	account, err := user.Current()
	if err != nil || account.Uid == "" {
		return provider.Config{}, fmt.Errorf("%w: user identity", ErrSelection)
	}
	key, err := endpointKey(account.Uid, root)
	if err != nil {
		return provider.Config{}, err
	}
	endpoint := listen.Endpoint("lemonade-" + key)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return provider.Config{}, fmt.Errorf("%w: private port: %v", ErrSelection, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	if port < 1 {
		return provider.Config{}, ErrSelection
	}
	return provider.Config{
		Name: ProviderID, Endpoint: endpoint, RuntimePrograms: []string{runtimeProgram},
		StoreRoot: filepath.Join(root, "store"), RecordPath: filepath.Join(root, "content-records.json"),
		Server: server.Config{
			// A speech-only selection serves no picture model and needs no
			// image backend.
			RequireVulkanBackend: selected.Image(),
			TempDir:              filepath.Join(root, "engine-temp"), ModelsDir: filepath.Join(root, "engine-models"), Port: port,
			Models: append([]string(nil), selected.Models...), Ready: time.Minute,
			// The installed declaration names both audio profiles. Admission
			// refuses an unstaged one before Lemonade starts, and each launch
			// proves the speech folders it finds before a route opens.
			Transcription: true, Speech: true,
			Bind: func(ctx context.Context) (server.Binding, error) { return bindSelected(ctx, root, steps.program) },
		},
	}, nil
}

// endpointKey names the provider endpoint of account's install at root: the
// account alone for the per-user root, so the installed endpoint stays as it
// was, and the account with the state folder's identity (volume serial
// number and file ID) for any other, so two isolated installs never share
// an endpoint and every path that reaches one folder names the same one.
func endpointKey(account, root string) (string, error) {
	key := account
	def, err := DefaultRoot()
	// An exact spelling of the default root keys by the account alone; any
	// other spelling is settled by file identity below.
	if err != nil || filepath.Clean(def) != filepath.Clean(root) {
		id, err := folderIdentity(root)
		if err != nil {
			return "", err
		}
		// A path that reaches the per-user root keeps its endpoint too.
		if defID, defErr := folderIdentity(def); defErr != nil || def == "" || defID != id {
			key += "\x00" + id
		}
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]), nil
}

// bindSelected reads the selection and names the package it binds. The
// launch then proves and holds that package's files.
func bindSelected(ctx context.Context, root string, program func(server.Package) (string, error)) (server.Binding, error) {
	selected, err := readSelection(ctx, SelectionPath(root))
	if err != nil {
		return server.Binding{}, err
	}
	p := selectedPackage(selected)
	path, err := program(p)
	if err != nil {
		return server.Binding{}, err
	}
	return bindingFor(root, p, path), nil
}
