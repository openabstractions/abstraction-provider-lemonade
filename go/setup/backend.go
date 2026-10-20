package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

const VulkanBackendURL = "https://github.com/leejet/stable-diffusion.cpp/releases/download/master-843-462d675/sd-master-462d675-bin-win-vulkan-x64.zip"
const VulkanBackendArchiveSHA256 = "b9248856707de2e125538ede8a7dc71a9d800bba67e469e60881dae2a5a1c0a2"
const VulkanBackendArchiveBytes int64 = 38865230
const VulkanBackendVersion = "master-843-462d675"
const VulkanServerSHA256 = "e45bbbe59f30746a7933c63c400a00996b8a3059335bae878b11bbdc555dceae"
const vulkanExpandedBytes int64 = 114122885

// LlamaBackendURL is llama.cpp b10825, Windows x64 Vulkan, the build Lemonade
// 2026.39.1 pins. Its size and SHA-256 are GitHub's asset digest, matched by
// research/security-reviews/llama.cpp-b10825-win-vulkan-x64.md.
const LlamaBackendURL = "https://github.com/ggml-org/llama.cpp/releases/download/b10825/llama-b10825-bin-win-vulkan-x64.zip"
const LlamaBackendArchiveSHA256 = "8907e5f3b74cb57d93fd94e187a87d85be24dc64e3b0ff2bc1b287da17b31446"
const LlamaBackendArchiveBytes int64 = 35227898

// vulkanEntries are the archive entries of the reviewed manifest; the server
// package owns the per-file sizes and digests.
var vulkanEntries = func() map[string]server.ManifestFile {
	entries := make(map[string]server.ManifestFile, len(server.SDCppVulkan.Files))
	for _, f := range server.SDCppVulkan.Files {
		entries[f.Name] = f
	}
	return entries
}()

// The speech backends Lemonade 2026.39.1 pins, at the digests their reviews
// computed and GitHub records: whisper.cpp v1.8.4 Vulkan and CPU from
// lemonade-sdk/whisper.cpp-rocm (research/security-reviews/whisper.cpp-v1.8.4-win.md;
// the NPU build is refused) and Kokoros b17 from lemonade-sdk/Kokoros
// (kokoros-b17-win.md). koko.exe is GPL-3.0-or-later: OA never ships it, and
// Setup downloads it from the publisher with the person's consent.
const (
	WhisperVulkanBackendURL                 = "https://github.com/lemonade-sdk/whisper.cpp-rocm/releases/download/v1.8.4/whisper-v1.8.4-windows-vulkan-x64.zip"
	WhisperVulkanBackendArchiveSHA256       = "e0d20a0f92e31b98adc0faf71172efc810b701e6391a9d858ca045bff26f77cd"
	WhisperVulkanBackendArchiveBytes  int64 = 21203995
	WhisperCPUBackendURL                    = "https://github.com/lemonade-sdk/whisper.cpp-rocm/releases/download/v1.8.4/whisper-v1.8.4-windows-cpu-x64.zip"
	WhisperCPUBackendArchiveSHA256          = "2f494218bce1ea61dfcd88c318c4747b44e497789c993f2e7f81feabe94dfd8d"
	WhisperCPUBackendArchiveBytes     int64 = 4327767
	KokoroBackendURL                        = "https://github.com/lemonade-sdk/Kokoros/releases/download/b17/kokoros-windows-x86_64.tar.gz"
	KokoroBackendArchiveSHA256              = "df403e86f13b0f964a5a2d4b8ab4a1a6749c0613bb67e45825a4a8913b29c4ef"
	KokoroBackendArchiveBytes         int64 = 19202614
)

// A backendArchive is one reviewed backend archive, pinned by size and
// SHA-256, and the manifest of the folder it is staged into. Every archive
// entry must be a manifest file; the excluded ones are never written.
// prefix is the folder inside the archive that holds the tree, as Lemonade
// strips it.
type backendArchive struct {
	manifest server.BackendManifest
	size     int64
	sha256   string
	prefix   string
}

var (
	vulkanArchive        = backendArchive{manifest: server.SDCppVulkan, size: VulkanBackendArchiveBytes, sha256: VulkanBackendArchiveSHA256}
	llamaArchive         = backendArchive{manifest: server.LlamaCppVulkanB10825, size: LlamaBackendArchiveBytes, sha256: LlamaBackendArchiveSHA256}
	whisperVulkanArchive = backendArchive{manifest: server.WhisperCppVulkan, size: WhisperVulkanBackendArchiveBytes, sha256: WhisperVulkanBackendArchiveSHA256}
	whisperCPUArchive    = backendArchive{manifest: server.WhisperCppCPU, size: WhisperCPUBackendArchiveBytes, sha256: WhisperCPUBackendArchiveSHA256}
	kokoroArchive        = backendArchive{manifest: server.KokorosB17, size: KokoroBackendArchiveBytes, sha256: KokoroBackendArchiveSHA256, prefix: "windows-x86_64/"}
	// knownArchives are the reviewed backend archives a package entry's
	// backends are staged from; each server.Packages backend has one.
	knownArchives = []backendArchive{vulkanArchive, llamaArchive, whisperVulkanArchive, whisperCPUArchive, kokoroArchive}
	// reviewedPair is the backend pairing of the exported staging steps.
	reviewedPair = server.Package{SDBackend: server.SDCppVulkan, LlamaBackend: server.LlamaCppVulkanB10825,
		WhisperBackend: server.WhisperCppVulkan, KokoroBackend: server.KokorosB17}
)

// archiveFor is the reviewed archive whose manifest is m.
func archiveFor(m server.BackendManifest) (backendArchive, bool) {
	for _, a := range knownArchives {
		if a.manifest.Equal(m) {
			return a, true
		}
	}
	return backendArchive{}, false
}

// cacheLayout names the provider root that holds cacheDir and the folder of
// the records of the backend trees under it: `<root>\engine-cache\<version>`
// keeps them in `<root>\engine-files\<version>`, and a cache folder directly
// under a root, the layout before per-version folders, in
// `<root>\engine-files`.
func cacheLayout(cacheDir string) (root, stateDir string) {
	parent := filepath.Dir(cacheDir)
	if filepath.Base(parent) == "engine-cache" {
		root = filepath.Dir(parent)
		return root, StateDirFor(root, filepath.Base(cacheDir))
	}
	return parent, EngineFilesDir(parent)
}

// expanded is the byte count of the files staging writes.
func (a backendArchive) expanded() int64 {
	var n int64
	for _, f := range a.manifest.Required() {
		n += f.Size
	}
	return n
}

// Temporary names beside the backend folder, under Lemonade's
// `<cache>/bin/<family>`: os.CreateTemp and os.MkdirTemp append the decimal
// form of a random uint32.
func (a backendArchive) archivePrefix() string { return "." + a.manifest.Variant + "-archive-" }
func (a backendArchive) stagePrefix() string   { return "." + a.manifest.Variant + "-stage-" }

func backendDir(cacheDir string) string { return vulkanArchive.manifest.Dir(cacheDir) }

// A killed configure can leave only these adapter-created siblings. Do not
// silently treat an otherwise valid backend as committed while they remain.
func backendStaging(cacheDir string) ([]string, error) { return vulkanArchive.staging(cacheDir) }

func (a backendArchive) staging(cacheDir string) ([]string, error) {
	parent := filepath.Dir(a.manifest.Dir(cacheDir))
	entries, err := os.ReadDir(parent)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		if name == a.manifest.Variant+".previous" {
			return nil, fmt.Errorf("%w: interrupted backend replacement backup", ErrSelection)
		}
		if backendTempName(name, a.archivePrefix()) && entry.Type().IsRegular() || backendTempName(name, a.stagePrefix()) && entry.IsDir() {
			paths = append(paths, filepath.Join(parent, name))
		} else if strings.HasPrefix(name, a.archivePrefix()) || strings.HasPrefix(name, a.stagePrefix()) {
			return nil, fmt.Errorf("%w: unexpected backend staging entry", ErrSelection)
		}
	}
	return paths, nil
}

// os.CreateTemp/MkdirTemp append the decimal form of a random uint32.
func backendTempName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if len(suffix) < 1 || len(suffix) > 10 {
		return false
	}
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// VerifyApprovedVulkanBackend is the admission gate preventing Lemonade from
// silently fetching a missing or replaced backend on first model load.
func VerifyApprovedVulkanBackend(cacheDir string) error {
	root, _ := cacheLayout(cacheDir)
	return withSetupLock(context.Background(), root, func() error {
		return verifyApprovedVulkanBackend(cacheDir)
	})
}

// verifyApprovedVulkanBackend proves the whole backend cache against the
// reviewed sd.cpp and llama.cpp b10825 manifests.
func verifyApprovedVulkanBackend(cacheDir string) error {
	return verifyApprovedBackend(cacheDir, reviewedPair)
}

// verifyApprovedBackend proves one version's backend cache against p's
// pairing: the sd.cpp folder is required, a llama.cpp folder is optional and
// must be p's build, and no staging residue of either may remain.
func verifyApprovedBackend(cacheDir string, p server.Package) error {
	return verifyBackendFor(cacheDir, p, true)
}

// verifySpeechBackend is verifyApprovedBackend for a speech-only selection:
// every present folder must be p's, and the sd.cpp folder may be absent.
func verifySpeechBackend(cacheDir string, p server.Package) error {
	return verifyBackendFor(cacheDir, p, false)
}

func verifyBackendFor(cacheDir string, p server.Package, requireSD bool) error {
	if !filepath.IsAbs(cacheDir) {
		return ErrSelection
	}
	speech := speechBackendsOf(p)
	for _, m := range append([]server.BackendManifest{p.SDBackend, p.LlamaBackend}, speech...) {
		residue, err := backendArchive{manifest: m}.staging(cacheDir)
		if err != nil {
			return err
		}
		if len(residue) != 0 {
			return fmt.Errorf("%w: incomplete private backend staging", ErrSelection)
		}
	}
	if err := server.CheckBackendTree(cacheDir, p.SDBackend, p.LlamaBackend, requireSD, speech...); err != nil {
		return fmt.Errorf("%w: approved Vulkan backend: %w", ErrSelection, err)
	}
	return nil
}

// StageApprovedVulkanBackend installs a public-job-downloaded archive into
// private Lemonade cache only after exact archive verification. The archive
// carries upstream MIT notices, preserved with every DLL and executable.
func StageApprovedVulkanBackend(ctx context.Context, cacheDir, archivePath string, progress func(done, total int64) error, replace bool) error {
	root, _ := cacheLayout(cacheDir)
	return withSetupLock(ctx, root, func() error {
		return stageApprovedVulkanBackend(ctx, cacheDir, archivePath, progress, replace)
	})
}

func stageApprovedVulkanBackend(ctx context.Context, cacheDir, archivePath string, progress func(done, total int64) error, replace bool) error {
	if err := stageBackend(ctx, vulkanArchive, cacheDir, archivePath, progress, replace); err != nil {
		return err
	}
	return verifyApprovedVulkanBackend(cacheDir)
}

// StageApprovedLlamaBackend stages the reviewed llama.cpp b10825 archive into
// Lemonade's `<cache>/bin/llamacpp/vulkan`, the folder Lemonade starts
// llama-server from and sets as its working directory on Windows
// (llamacpp_server.cpp). Only the files review condition 5 keeps are
// written; ggml-rpc-server.exe, ggml-rpc.dll and the command-line tools stay
// in the archive. The image backend must already be staged.
func StageApprovedLlamaBackend(ctx context.Context, cacheDir, archivePath string, progress func(done, total int64) error, replace bool) error {
	root, _ := cacheLayout(cacheDir)
	return withSetupLock(ctx, root, func() error {
		return stageApprovedLlamaBackend(ctx, cacheDir, archivePath, progress, replace)
	})
}

func stageApprovedLlamaBackend(ctx context.Context, cacheDir, archivePath string, progress func(done, total int64) error, replace bool) error {
	if err := stageBackend(ctx, llamaArchive, cacheDir, archivePath, progress, replace); err != nil {
		return err
	}
	return verifyApprovedVulkanBackend(cacheDir)
}

// A stagedArchive is one reviewed archive and the local file that holds it.
type stagedArchive struct {
	a    backendArchive
	path string
}

// stageBackends stages each archive in turn and proves the whole cache with
// verify only after the last one. A folder of another build, such as a
// llama.cpp b10723 folder, is replaced under replace instead of failing the
// cache proof before its own replacement runs.
func stageBackends(ctx context.Context, cacheDir string, archives []stagedArchive, replace bool, verify func(string) error) error {
	for _, s := range archives {
		if err := stageBackend(ctx, s.a, cacheDir, s.path, nil, replace); err != nil {
			return err
		}
	}
	return verify(cacheDir)
}

// backendFiles installs staged backend trees for this process. Tests give it
// a folder ACL that leaves their temporary folders removable.
var backendFiles = enginefiles.NewVerifier(enginefiles.Options{})

// stageBackend stages a into Lemonade's `<cache>/bin/<family>/<variant>`
// through the provider kit's archive install (inference CONTRACT.md INF-F6):
// the archive's size and SHA-256 are checked against a before any entry is
// read, each kept entry is chosen by its pinned name and written with O_EXCL
// through a reader bounded by its pinned size into a temporary folder,
// excluded entries are never written, version.txt joins them, and the
// proven folder replaces the old one (kept as `.previous` until the new one
// is installed) only under replace. A folder that already proves against a's
// manifest is kept. The kit then gives the folder the read-only ACL and
// writes its record beside the other engine-file records, as ConfigureReviewed
// does again with the package (server.InstallFiles).
func stageBackend(ctx context.Context, a backendArchive, cacheDir, archivePath string, progress func(done, total int64) error, replace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(cacheDir) || !filepath.IsAbs(archivePath) {
		return ErrSelection
	}
	residue, err := a.staging(cacheDir)
	if err != nil {
		return err
	}
	if len(residue) != 0 {
		if !replace {
			return fmt.Errorf("%w: incomplete private backend staging requires --replace-backend", ErrSelection)
		}
		for _, path := range residue {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	_, stateDir := cacheLayout(cacheDir)
	tree, err := a.manifest.StagedTree(cacheDir, stateDir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	parent := filepath.Dir(tree.Root)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, a.stagePrefix())
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	_, err = backendFiles.InstallFromArchive(ctx, enginefiles.ArchiveInstall{
		Archive:  enginefiles.PinnedArchive{Path: archivePath, Size: a.size, SHA256: a.sha256},
		Tree:     tree,
		Prefix:   a.prefix,
		Excluded: a.manifest.Excluded,
		Extra:    map[string][]byte{"version.txt": a.manifest.VersionFile()},
		Stage:    stage,
		Replace:  replace,
		Progress: progress,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return nil
}
