package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"

	identity "github.com/openabstractions/abstraction-identity"
)

// ErrPackageManifest reports an extracted Lemonade package that differs from
// its reviewed manifest: a missing, changed or extra file or folder.
var ErrPackageManifest = errors.New("lemonade: package differs from the reviewed manifest")

// PackageManifest pins every file of an extracted upstream package by its
// slash-separated path under the package folder, the folder that holds the
// launched executable.
type PackageManifest struct {
	Name  string
	Files []ManifestFile
}

func (m PackageManifest) valid() bool {
	if m.Name == "" || len(m.Files) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if f.Name == "" || !fs.ValidPath(f.Name) || f.Size < 0 || len(f.SHA256) != 64 || seen[f.Name] {
			return false
		}
		seen[f.Name] = true
	}
	return true
}

func (m PackageManifest) has(name string) bool {
	for _, f := range m.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// files is the manifest in the shared engine-file kit's form.
func (m PackageManifest) files() enginefiles.Manifest {
	out := enginefiles.Manifest{Files: make([]enginefiles.File, len(m.Files))}
	for i, f := range m.Files {
		out.Files[i] = enginefiles.File{Path: f.Name, Size: f.Size, SHA256: f.SHA256}
	}
	return out
}

// tree is the package folder as the shared engine-file kit proves it
// (INF-F1..INF-F4). lemond reads its resources folder at run time (backend
// versions, config defaults, the model registry), so those files are proven
// and held with the executable.
func (m PackageManifest) tree(root, stateDir string) (enginefiles.Tree, error) {
	if !m.valid() {
		return enginefiles.Tree{}, fmt.Errorf("%w: invalid manifest", ErrPackageManifest)
	}
	return enginefiles.Tree{Name: m.Name, Root: root, Manifest: m.files(), Record: filepath.Join(stateDir, "package.files")}, nil
}

// VerifyPackageDir hashes every file under root against m and refuses any
// file or folder the manifest does not name, and any link. It checks no
// folder ACL and records nothing; setup probes use it.
func VerifyPackageDir(root string, m PackageManifest) error {
	if !m.valid() {
		return fmt.Errorf("%w: invalid manifest", ErrPackageManifest)
	}
	if err := enginefiles.Verify(context.Background(), m.Name, root, m.files()); err != nil {
		return fmt.Errorf("%w: %w", ErrPackageManifest, err)
	}
	return nil
}

// Package is one reviewed Lemonade release the provider can run, with the
// image backend, llama.cpp build and model revision it was tested with
// (VISION 2026-10-01, research/lemonade-updates/DECISION.md). ID is the value
// of selection.json's package; Version is the release's catalog version and
// the name of its per-version folders under apps\lemonade, engine-cache,
// engine-config and engine-files.
type Package struct {
	ID, Version          string
	ArchiveSHA256        string // the upstream archive Setup installs
	Program              string // the executable under Manifest.Name
	ProgramSHA256        string
	Manifest             PackageManifest
	SDBackend            BackendManifest
	LlamaBackend         BackendManifest
	BackendArchiveSHA256 string // the image backend archive the pairing names
	ModelRevision        string
	// WhisperBackend and KokoroBackend are the speech backends this release
	// pins in its resources/backend_versions.json and the reviews approved:
	// whisper.cpp v1.8.4 (Vulkan, or CPU with LockedConfigWhisperCPU) and
	// Kokoros b17.
	WhisperBackend BackendManifest
	KokoroBackend  BackendManifest
	// Config returns the locked config.json bytes for this release.
	Config func(modelsDir string) ([]byte, error)
}

// Packages lists every Lemonade release this provider build can run: one
// entry per lemonade-engine pin of monitor/internal/setup/install_pins.json
// for windows/amd64, in the order the pins were added. It only grows while
// install_pins.json carries a pin; monitor/internal/setup's
// lemonade_pins_test.go refuses a pin the table does not know.
var Packages = []Package{{
	ID: "lemonade-embeddable-v2026.39.1-windows-amd64", Version: "v2026.39.1",
	ArchiveSHA256: "235c2361be3a9729a06b92c97e40a9530ebb8e5088bd3334524472d6da7fbbca",
	Program:       "lemond.exe", ProgramSHA256: "4a62f8193799eb6d0cc28975bcc72338bd6cc795e6f9d977c3012cc2adcc8df7",
	Manifest: Lemonade2026391, SDBackend: SDCppVulkan, LlamaBackend: LlamaCppVulkanB10825,
	BackendArchiveSHA256: "b9248856707de2e125538ede8a7dc71a9d800bba67e469e60881dae2a5a1c0a2",
	ModelRevision:        "5f526678002e43af5551dadb73ce2e8c91b43afe",
	WhisperBackend:       WhisperCppVulkan, KokoroBackend: KokorosB17,
	Config: LockedConfig,
}}

// ErrUnknownPackage reports a version or selection ID no entry of Packages
// names.
var ErrUnknownPackage = errors.New("lemonade: no reviewed package of this provider build has that version")

// FindPackage returns the entry whose Version is version.
func FindPackage(version string) (Package, bool) {
	for _, p := range Packages {
		if p.Version == version {
			return p, true
		}
	}
	return Package{}, false
}

// FindPackageID returns the entry whose ID is id, the value of a selection's
// package.
func FindPackageID(id string) (Package, bool) {
	for _, p := range Packages {
		if p.ID == id {
			return p, true
		}
	}
	return Package{}, false
}

// Valid refuses an entry with a missing pin, a program its manifest does not
// list, backends of one family, or a version that is not a safe folder name:
// a letter or digit followed by letters, digits, '.', '_', '+' or '-', no
// trailing dot, no Windows device name, and not "bin", the backend folder
// Lemonade reads directly under its cache.
func (p Package) Valid() error {
	if p.ID == "" || !validVersion(p.Version) || !hex64(p.ArchiveSHA256) || !hex64(p.ProgramSHA256) ||
		!hex64(p.BackendArchiveSHA256) || len(p.ModelRevision) != 40 || p.Config == nil {
		return fmt.Errorf("%w: package entry %q", ErrUnknownPackage, p.Version)
	}
	if !p.Manifest.valid() || p.Program == "" || filepath.Base(p.Program) != p.Program || !p.Manifest.has(p.Program) {
		return fmt.Errorf("%w: package entry %q manifest", ErrUnknownPackage, p.Version)
	}
	for _, f := range p.Manifest.Files {
		if f.Name == p.Program && f.SHA256 != p.ProgramSHA256 {
			return fmt.Errorf("%w: package entry %q program digest", ErrUnknownPackage, p.Version)
		}
	}
	families := map[string]bool{}
	for _, m := range []BackendManifest{p.SDBackend, p.LlamaBackend, p.WhisperBackend, p.KokoroBackend} {
		if !m.valid() || families[m.Family] {
			return fmt.Errorf("%w: package entry %q backends", ErrUnknownPackage, p.Version)
		}
		families[m.Family] = true
	}
	// The locked configuration names the whisper backend the entry pairs.
	if locked, err := p.Config(filepath.Join(os.TempDir(), "models")); err != nil || whisperBackendOf(locked) != p.WhisperBackend.Variant {
		return fmt.Errorf("%w: package entry %q: the locked configuration does not name whisper backend %s", ErrUnknownPackage, p.Version, p.WhisperBackend.Variant)
	}
	return nil
}

// ValidTable refuses a table with an invalid entry or two entries whose IDs
// or versions name one folder.
func ValidTable(packages []Package) error {
	ids, versions := map[string]bool{}, map[string]bool{}
	for _, p := range packages {
		if err := p.Valid(); err != nil {
			return err
		}
		id, version := strings.ToLower(p.ID), strings.ToLower(p.Version)
		if ids[id] || versions[version] {
			return fmt.Errorf("%w: package %q listed twice", ErrUnknownPackage, p.Version)
		}
		ids[id], versions[version] = true, true
	}
	return nil
}

func validVersion(v string) bool {
	// "bin" is a fixed folder name, compared as the file system compares names.
	if v == "" || len(v) > 64 || identity.SamePathName(v, "bin") || strings.HasSuffix(v, ".") {
		return false
	}
	for i, r := range v {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._+-", r)) {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(v, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	return !(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9')
}

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Lemonade2026391 is the extracted Embeddable Lemonade 2026.39.1 for Windows
// x64 (research/security-reviews/lemonade-2026.39.1.md). Recorded 2026-10-01
// from the reviewed archive lemonade-embeddable-2026.39.1-windows-x64.zip,
// 5,357,376 bytes, SHA-256
// 235c2361be3a9729a06b92c97e40a9530ebb8e5088bd3334524472d6da7fbbca, under its
// root folder. lemond.exe, lemonade.exe and resources/backend_versions.json
// match the digests the review records.
var Lemonade2026391 = PackageManifest{
	Name: "lemonade-embeddable-2026.39.1-windows-x64",
	Files: []ManifestFile{
		{"LICENSE", 19782, "81d41bd41eebf93d1d0e3dd8104a022cdbe520bbeceaee8ea30307b228ba46a6"},
		{"lemonade.exe", 3814400, "f255b0db975c02c1ee19e3da327ba4fa79d83d90a2fccd83331cb01d5a168d3c"},
		{"lemond.exe", 8385536, "4a62f8193799eb6d0cc28975bcc72338bd6cc795e6f9d977c3012cc2adcc8df7"},
		{"resources/backend_versions.json", 8469, "47a8668b4cc3d846596bdb9ec7a711cfb0ac954dcf3f66eba999dcb3dd57c6b5"},
		{"resources/bench_scenarios.json", 26579, "9ebb3f181c3e92f2a834a0544f85616db9100c754650b7f2c071aef146076774"},
		{"resources/defaults.json", 3670, "05628c8965a6a7485ac1236c85f39f4c204e34097c06a5f8f76fe235637f4afd"},
		{"resources/server_models.json", 73791, "df9f6a619d03ac01c88d0ff35279fa84b0c1d4c97306fbc0ca73b0d19751848a"},
		{"resources/toolDefinitions.json", 8472, "e9fdfb5232760bb91c58dbecbded972ff4cf402f4f073afabf68be9175b9e94b"},
		{"resources/vllm_model_config.json", 1718, "e404b2d2d2f8ceb3a6121d53fe438d7aa935af78ef6349ba025012ec02b2eb0d"},
	},
}
