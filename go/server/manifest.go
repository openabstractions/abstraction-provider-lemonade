package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/openabstractions/abstraction-facade/go/programfiles"

	identity "github.com/openabstractions/abstraction-identity"
)

// ErrBackendManifest reports a backend folder that differs from its reviewed
// manifest: a missing, changed, excluded or extra file, or a wrong version.
var ErrBackendManifest = errors.New("lemonade: backend differs from the reviewed manifest")

// ManifestFile is one file of a reviewed backend archive.
type ManifestFile struct {
	Name   string
	Size   int64
	SHA256 string
}

// BackendManifest pins one backend folder under Lemonade's `<cache>/bin`. Files
// lists every entry of the reviewed archive. Excluded names are archive entries
// OA's review requires to be absent; every other entry must be present with its
// recorded size and digest. The folder may hold nothing else except
// Lemonade's version.txt, whose trimmed content must equal Version.
type BackendManifest struct {
	Family   string // Lemonade recipe folder, for example "sd-cpp".
	Variant  string // Lemonade backend folder, for example "vulkan".
	Version  string
	Files    []ManifestFile
	Excluded []string
}

func (m BackendManifest) valid() bool {
	if m.Family == "" || m.Variant == "" || m.Version == "" || len(m.Files) == 0 ||
		filepath.Base(m.Family) != m.Family || filepath.Base(m.Variant) != m.Variant {
		return false
	}
	names := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		// A name is a slash-separated path inside the folder: Kokoros keeps
		// espeak-ng-data/ beside koko.exe.
		if f.Name == "" || !fs.ValidPath(f.Name) || f.Name == "version.txt" || f.Size < 0 || len(f.SHA256) != 64 || names[f.Name] {
			return false
		}
		names[f.Name] = true
	}
	for _, name := range m.Excluded {
		if !names[name] {
			return false
		}
	}
	return true
}

// Required lists the manifest files that must be present.
func (m BackendManifest) Required() []ManifestFile {
	excluded := make(map[string]bool, len(m.Excluded))
	for _, name := range m.Excluded {
		excluded[name] = true
	}
	var required []ManifestFile
	for _, f := range m.Files {
		if !excluded[f.Name] {
			required = append(required, f)
		}
	}
	return required
}

// Dir is the backend folder Lemonade resolves for this manifest.
func (m BackendManifest) Dir(cacheDir string) string {
	return filepath.Join(cacheDir, "bin", m.Family, m.Variant)
}

// tree is the backend folder as the shared engine-file kit proves it
// (INF-F1..INF-F4): every required file and version.txt, whose trimmed
// content must equal Version and whose exact bytes the tree then pins. The
// folder may hold nothing else. Windows loads a DLL planted beside an
// executable, so an extra file is as unsafe as a changed one.
func (m BackendManifest) tree(dir, stateDir string) (programfiles.Tree, error) {
	if !m.valid() {
		return programfiles.Tree{}, fmt.Errorf("%w: invalid manifest", ErrBackendManifest)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return programfiles.Tree{}, fmt.Errorf("%w: %s/%s missing", ErrBackendManifest, m.Family, m.Variant)
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return programfiles.Tree{}, fmt.Errorf("%w: %s/%s is not a plain folder", ErrBackendManifest, m.Family, m.Variant)
	}
	version, err := readSmall(filepath.Join(dir, "version.txt"), 256)
	if errors.Is(err, fs.ErrNotExist) {
		return programfiles.Tree{}, fmt.Errorf("%w: %s/%s: version.txt missing", ErrBackendManifest, m.Family, m.Variant)
	}
	if err != nil || strings.TrimSpace(string(version)) != m.Version {
		return programfiles.Tree{}, fmt.Errorf("%w: %s/%s: version is not %s; %s", ErrBackendManifest, m.Family, m.Variant, m.Version, m.replaceCommand())
	}
	return m.treeWith(dir, stateDir, version), nil
}

// treeWith is the backend folder's tree whose version.txt holds version.
func (m BackendManifest) treeWith(dir, stateDir string, version []byte) programfiles.Tree {
	sum := sha256.Sum256(version)
	files := []programfiles.File{{Path: "version.txt", Size: int64(len(version)), SHA256: hex.EncodeToString(sum[:])}}
	for _, f := range m.Required() {
		files = append(files, programfiles.File{Path: f.Name, Size: f.Size, SHA256: f.SHA256})
	}
	return programfiles.Tree{
		Name: m.Family + "/" + m.Variant, Root: dir, Manifest: programfiles.Manifest{Files: files},
		Record: filepath.Join(stateDir, "backend-"+m.Family+"-"+m.Variant+".files"),
	}
}

// VersionFile is the version.txt setup writes beside a staged backend's
// files: the manifest's Version and a line end.
func (m BackendManifest) VersionFile() []byte { return []byte(m.Version + "\n") }

// StagedTree is the tree setup installs into `<cacheDir>/bin/<family>/<variant>`
// from the reviewed archive: the required files and VersionFile, recorded
// under stateDir as every launch reads it.
func (m BackendManifest) StagedTree(cacheDir, stateDir string) (programfiles.Tree, error) {
	if !m.valid() {
		return programfiles.Tree{}, fmt.Errorf("%w: invalid manifest", ErrBackendManifest)
	}
	return m.treeWith(m.Dir(cacheDir), stateDir, m.VersionFile()), nil
}

// replaceCommand names the configure step that stages the reviewed build in
// place of another one, such as a llama.cpp b10723 folder staged before
// Lemonade 2026.39.1 pinned b10825. The engine-files step leaves the
// private model as it is.
func (m BackendManifest) replaceCommand() string {
	const step = "run lemonaded configure --activate-reviewed-package --engine-files-only"
	if m.Family == "llamacpp" {
		return step + " --llamacpp-backend-archive <llama-" + m.Version + "-bin-win-vulkan-x64.zip> --replace-backend; place " + llamaRuntimeArchive + " beside the publisher ZIP"
	}
	return step + " --vulkan-backend-archive <reviewed archive> --replace-backend"
}

// refusal wraps a shared-kit refusal of the backend folder in
// ErrBackendManifest and names an excluded file as excluded.
func (m BackendManifest) refusal(err error) error {
	var mismatch *programfiles.MismatchError
	if errors.As(err, &mismatch) && mismatch.Kind == programfiles.Extra {
		for _, name := range m.Excluded {
			if identity.SamePathName(mismatch.Path, name) {
				return fmt.Errorf("%w: %s/%s: excluded file present: %s", ErrBackendManifest, m.Family, m.Variant, mismatch.Path)
			}
		}
	}
	return fmt.Errorf("%w: %w; %s", ErrBackendManifest, err, m.replaceCommand())
}

// VerifyBackendDir hashes every file in dir against m and refuses any entry
// the manifest does not name. It checks no folder ACL and records nothing;
// setup probes use it.
func VerifyBackendDir(dir string, m BackendManifest) error {
	t, err := m.tree(dir, "")
	if err != nil {
		return err
	}
	if err := programfiles.Verify(context.Background(), t.Name, t.Root, t.Manifest); err != nil {
		return m.refusal(err)
	}
	return nil
}

func readSmall(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file too large")
	}
	return data, nil
}

// backendTree is one present backend folder with the manifest that pins it.
type backendTree struct {
	m    BackendManifest
	tree programfiles.Tree
}

// backendTrees admits only reviewed backends in Lemonade's `<cache>/bin` and
// returns the tree of each backend folder present. The image backend folder
// must be present when requireSD is set. A llama.cpp folder and each speech
// backend folder (whisper.cpp, Kokoros) are optional. Any other recipe
// folder, variant (whispercpp/npu among them), staging residue or file is
// refused.
func backendTrees(cacheDir string, sd, llama BackendManifest, requireSD bool, stateDir string, speech ...BackendManifest) ([]backendTree, error) {
	bin := filepath.Join(cacheDir, "bin")
	entries, err := os.ReadDir(bin)
	if os.IsNotExist(err) {
		if requireSD {
			return nil, fmt.Errorf("%w: %s/%s missing", ErrBackendManifest, sd.Family, sd.Variant)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: backend folder unreadable", ErrBackendManifest)
	}
	manifests := map[string]BackendManifest{sd.Family: sd, llama.Family: llama}
	for _, m := range speech {
		if _, taken := manifests[m.Family]; taken || !m.valid() {
			return nil, fmt.Errorf("%w: speech backend %s", ErrBackendManifest, m.Family)
		}
		manifests[m.Family] = m
	}
	sdSeen := false
	var trees []backendTree
	for _, entry := range entries {
		m, ok := manifests[entry.Name()]
		if !ok || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: unreviewed backend entry: %s", ErrBackendManifest, entry.Name())
		}
		variants, err := os.ReadDir(filepath.Join(bin, m.Family))
		if err != nil {
			return nil, fmt.Errorf("%w: %s unreadable", ErrBackendManifest, m.Family)
		}
		for _, variant := range variants {
			if variant.Name() != m.Variant {
				return nil, fmt.Errorf("%w: unreviewed backend entry: %s/%s", ErrBackendManifest, m.Family, variant.Name())
			}
		}
		if len(variants) == 0 {
			continue
		}
		t, err := m.tree(m.Dir(cacheDir), stateDir)
		if err != nil {
			return nil, err
		}
		trees = append(trees, backendTree{m: m, tree: t})
		if m.Family == sd.Family {
			sdSeen = true
		}
	}
	if requireSD && !sdSeen {
		return nil, fmt.Errorf("%w: %s/%s missing", ErrBackendManifest, sd.Family, sd.Variant)
	}
	return trees, nil
}

// CheckBackendTree admits only reviewed backends in Lemonade's `<cache>/bin`
// and hashes each present backend folder against its manifest. The image
// backend folder must match sd when requireSD is set. A llama.cpp folder is
// optional and, when present, must match llama; so is each speech backend,
// which must match its manifest. It checks no folder ACL and records
// nothing; setup probes use it.
func CheckBackendTree(cacheDir string, sd, llama BackendManifest, requireSD bool, speech ...BackendManifest) error {
	trees, err := backendTrees(cacheDir, sd, llama, requireSD, "", speech...)
	if err != nil {
		return err
	}
	for _, b := range trees {
		if err := programfiles.Verify(context.Background(), b.tree.Name, b.tree.Root, b.tree.Manifest); err != nil {
			return b.m.refusal(err)
		}
	}
	return nil
}

// SDCppVulkan is stable-diffusion.cpp master-843-462d675, Windows x64 Vulkan
// (research/security-reviews/stable-diffusion.cpp-master-843-462d675.md).
// Recorded 2026-10-01 from the reviewed archive
// sd-master-462d675-bin-win-vulkan-x64.zip, 38,865,230 bytes, SHA-256
// b9248856707de2e125538ede8a7dc71a9d800bba67e469e60881dae2a5a1c0a2.
var SDCppVulkan = BackendManifest{
	Family: "sd-cpp", Variant: "vulkan", Version: "master-843-462d675",
	Files: []ManifestFile{
		{"ggml-base.dll", 657920, "fc4b7d28e9057d915e68a06debe65b4d89eea2a2a0b1d716d6d7a442ad4519c0"},
		{"ggml-cpu-alderlake.dll", 832000, "3a5ab1f52648ff40f73be578f24fba2bfd4eb3680daf66e24b009a70db25488c"},
		{"ggml-cpu-cannonlake.dll", 876544, "8720650111b2856cfdfb8f7a51a5ed7a129fbf78400564f511201a4778218cde"},
		{"ggml-cpu-cascadelake.dll", 874496, "10cf847bdd439ed2598e49b701c0565a516aae359242f7b205914b9d08c529d2"},
		{"ggml-cpu-haswell.dll", 833024, "85afeea59a3f613b60b0415f1d766cd11ea9a600e5d8be0f9bc26e73a692dbdf"},
		{"ggml-cpu-icelake.dll", 874496, "99aec7f7b42db3b0aca854a0903180fd934f1378c6d6dda04333b23a54d06be0"},
		{"ggml-cpu-sandybridge.dll", 806400, "70fc87cc9b6bd54d71c67750f5d5c96de85bf811da89081062f65690b314749b"},
		{"ggml-cpu-skylakex.dll", 876544, "87a86bf4262e5c3bc03d261088eeaa2711e9dca2bf7f35c35ff8014a6080e739"},
		{"ggml-cpu-sse42.dll", 793600, "72c99ca5c9bd4572fb2a4db3e07a752d924d73ed2192246e98fa146a7a3c4747"},
		{"ggml-cpu-x64.dll", 794112, "8e9ca8084955e08b5dc82373f18003c1e8e970bd6d0147e9ffb8773377448a67"},
		{"ggml-vulkan.dll", 50544640, "76eb7a34115da8a5ef8a8e2d065ade87ef624fc966a9be38501f533e3d59b9ba"},
		{"ggml.dll", 63488, "5c1bd056e0019853b5cd3273685a355db3a7a0d89ec0c705a63dbef94f30e565"},
		{"ggml.txt", 1099, "bcd8ec749126d45cb06737d0690295d73df4b6e7e194205bcf91190368f27285"},
		{"libsharpyuv.dll", 24576, "6ea16345927abecd4aab1ae403b2f8d1fdce7108c564d27fae31ceb697c465bd"},
		{"libwebp.dll", 373248, "5106ef770c86a86ce608c3552392cbc1b6f7239765eced7715d862cce308de01"},
		{"libwebpmux.dll", 41984, "a1d37240c9b0c88f3da51475e831b0e57d45f14cf70e5215624c0b193f38f6f2"},
		{"sd-cli.exe", 642560, "c8ca2f7f415c0e0a5df7ded0837a5810e22e326870af72b3bedf207f4508f65d"},
		{"sd-server.exe", 1112064, "e45bbbe59f30746a7933c63c400a00996b8a3059335bae878b11bbdc555dceae"},
		{"stable-diffusion.cpp.txt", 1082, "4e76049398df734a97db46f3bd4b5cbc460cb5f6bfaf737cdc3d6fbb8ab3de27"},
		{"stable-diffusion.dll", 52770816, "e48ad012df24d1098feb46e2127a3c37b4035f16e44b5a485da019597074b742"},
		{"webm.dll", 328192, "898533cd95c668f9dc500e28e4bccbd4f84e8c7bd8074ddea753ee591d3c1791"},
	},
}

// llamaCppExcluded are the archive entries the llama.cpp review requires
// absent (condition 5): the RPC server and client and every command-line tool.
var llamaCppExcluded = []string{
	"ggml-rpc-server.exe", "ggml-rpc.dll", "llama.exe",
	"llama-batched-bench.exe", "llama-batched-bench-impl.dll", "llama-bench.exe", "llama-bench-impl.dll",
	"llama-cli.exe", "llama-cli-impl.dll", "llama-completion.exe", "llama-completion-impl.dll",
	"llama-fit-params.exe", "llama-fit-params-impl.dll", "llama-gemma3-cli.exe", "llama-gguf-split.exe",
	"llama-imatrix.exe", "llama-llava-cli.exe", "llama-minicpmv-cli.exe", "llama-mtmd-cli.exe",
	"llama-mtmd-debug.exe", "llama-perplexity.exe", "llama-perplexity-impl.dll", "llama-quantize.exe",
	"llama-quantize-impl.dll", "llama-qwen2vl-cli.exe", "llama-results.exe", "llama-tokenize.exe", "llama-tts.exe",
}

// LlamaCppVulkanB10825 is llama.cpp b10825, Windows x64 Vulkan, the build
// Lemonade 2026.39.1 pins in its resources/backend_versions.json
// (research/security-reviews/llama.cpp-b10825-win-vulkan-x64.md). Recorded
// from the review's manifest.tsv of the archive
// llama-b10825-bin-win-vulkan-x64.zip, 35,227,898 bytes, SHA-256
// 8907e5f3b74cb57d93fd94e187a87d85be24dc64e3b0ff2bc1b287da17b31446 (GitHub's
// asset digest; Sigstore attestation verified by the review). Condition 5
// keeps 27 files: llama-server.exe, the libraries its import tables reach,
// the ggml CPU and Vulkan backends, the OpenMP licence and three pinned runtime
// files. installer/third-party.tsv records the 52 publisher files with this
// file as their authority; the three runtime rows cite vc_runtime_asset.json.
var LlamaCppVulkanB10825 = BackendManifest{
	Family: "llamacpp", Variant: "vulkan", Version: "b10825",
	Files: append([]ManifestFile{
		{"LICENSE-LLVM-OpenMP", 19741, "fdad1758a9e1f9d5a81e18879b3406772115edc92c24bfa36b70c654f325e8e4"},
		{"ggml-base.dll", 796160, "40090a7e59b1e26f1e8c6483dd7b0d935a5dad7036c29347616d7c88bc7754e1"},
		{"ggml-cpu-alderlake.dll", 1230336, "45ede07bf41f84a15fcdf62e572259ce6051ca6512e2ff0701cd19ecc9c3897e"},
		{"ggml-cpu-cannonlake.dll", 1448448, "2afa5395a56476fd2defb1320715f5374da4f88222c98cdfc82b7860e881077d"},
		{"ggml-cpu-cascadelake.dll", 1434112, "8670dbb1ec128a8a14c0a505624c103ce00958bc8de3a141aea440d7f590bf01"},
		{"ggml-cpu-cooperlake.dll", 1434624, "300897211946ab72c2d55e1bf41ea86cc1b4313dc2aa4effc9efedc41b8e7ece"},
		{"ggml-cpu-haswell.dll", 1235968, "998bdb373f5c328122a83f559e806f3d87d9ac9175031e8fd98ec84efc4c6363"},
		{"ggml-cpu-icelake.dll", 1440768, "472972a1c0b4cb4574a4137e21fc1714cf37a18236bae06fa0e204fc9fbd33c4"},
		{"ggml-cpu-ivybridge.dll", 1127424, "01e1629b38f0521352f1867b3b78972b55f2914cfdb34eb1d694c5753f573950"},
		{"ggml-cpu-piledriver.dll", 1131520, "41aac7b7f6cdeff98b23fd6e3a8024c9de51139d19b2a91e26153d6b3c2b76ac"},
		{"ggml-cpu-sandybridge.dll", 1107968, "3772d31c15c4b1b4e948e649e7babcc03d7890b4deb945c4f4cb28cc3de8d644"},
		{"ggml-cpu-sapphirerapids.dll", 1711616, "deea88459bdbc6f1bc180306ea1e8551c7720bd0a4cdadc3847b690a89f01fc0"},
		{"ggml-cpu-skylakex.dll", 1441792, "d8d44d32c72b497e95bc3e02d35df799742821f42b02adc0a74ecade9aaf34f0"},
		{"ggml-cpu-sse42.dll", 932352, "f9e9a57219508df92f7b276e984f58e2dbedd83ee337d356618da3da6b3da7c9"},
		{"ggml-cpu-x64.dll", 924160, "3e05d0c094941d5678fcfa55152663abe375b3555f563e5779eeaac74b40ad48"},
		{"ggml-cpu-zen4.dll", 1441280, "f39567b48214d230a65f3534befd53d3c897359734c55bc151e619fbc4e4e329"},
		{"ggml-rpc-server.exe", 101376, "ae70c6528909066fcd4975cb1ea574614ce1ea01d486ad03c893ae139e741b3f"},
		{"ggml-rpc.dll", 168448, "9cfffebe45dd8b56727634823b699fa488d44f26eed527847b7ae22364e7fe0a"},
		{"ggml-vulkan.dll", 55572992, "cb03e41776af38d0e28dbc3815a8eefbe4538be5661157aeb38a6b48cc65c540"},
		{"ggml.dll", 79872, "8f8173ea400583a20a74bd66ed4a940971c891a33b8f9b6384b33e95248c73f7"},
		{"libomp.dll", 768000, "a12116ba72d1d6820407cf30be23da04ce79d6bb8a71a5ee71759c5a1faa6f1c"},
		{"llama-batched-bench-impl.dll", 61440, "f0d105036eb15ef465958044af3c8d3ca1395896fd91f5876292a9ddb102d834"},
		{"llama-batched-bench.exe", 9216, "45bcc331e8c1305d774569f5d694f2b418de9a7698718d02b3e774e0b004e6d5"},
		{"llama-bench-impl.dll", 399872, "64b7e70a64f0d4bfe0f0090593040004bfcdb19f4d2bdf2f4c3c9865ff5cd7ae"},
		{"llama-bench.exe", 9216, "47ae94a5575e7bde5d2c58e7cd2af709c6367ca6cfc6a7bacfa5b2e6ba0edd59"},
		{"llama-cli-impl.dll", 3296256, "2718af1013f8ccd8475a57aafbd5cb85e3f471fa4c8e3c00ce05626f668aa28b"},
		{"llama-cli.exe", 9216, "5722d011e118640f4dad0510f99f86b3a0122f8ce19276a5b253df3a91abb6ba"},
		{"llama-common.dll", 7802880, "30232986786d21d8795c2c6afed58e022f9730a0ba33d952d97510686f199180"},
		{"llama-completion-impl.dll", 133632, "5d64a9ffbf3a2a1fd67c5e9ac626cf33e2842d0df06cee3e93df440bcaf21324"},
		{"llama-completion.exe", 9216, "5f1220f25c93384eeb67e1efd0a4d577b5312387a6ad50f557edad892b57e723"},
		{"llama-fit-params-impl.dll", 41472, "53469d4b7c4e0105a1036eb818f9b33990d3797a4958119a59829ea00da8a548"},
		{"llama-fit-params.exe", 9216, "8274052aef3073a886cdfeb36e3fb788fd5f3aeaceedb4f1acab7ffb32b39345"},
		{"llama-gemma3-cli.exe", 27136, "293f408e5ba98337a94cf11c46f0fee972d6bf12abcfa50d09fe2882e7ad907c"},
		{"llama-gguf-split.exe", 60416, "e0c272fc78d202d1b867a93d238b23824315acda636bc3da2f17755d576d5284"},
		{"llama-imatrix.exe", 255488, "a125fac41685a9d9de6e5089c4be9fb7849ab94c520f3d99a2765cc9f65cc907"},
		{"llama-llava-cli.exe", 27136, "293f408e5ba98337a94cf11c46f0fee972d6bf12abcfa50d09fe2882e7ad907c"},
		{"llama-minicpmv-cli.exe", 27136, "293f408e5ba98337a94cf11c46f0fee972d6bf12abcfa50d09fe2882e7ad907c"},
		{"llama-mtmd-cli.exe", 90112, "d071ffadcfe35275e7a496334e55af4b274ee912f5834188a966caa90c9e5a7b"},
		{"llama-mtmd-debug.exe", 55296, "7d4b330278f2c15879da25c59d33ac2c182e144a5232e560d06023f02a4cde2d"},
		{"llama-perplexity-impl.dll", 194048, "712029f23bcc3e312b22fb70835788618719c27b0acade3959b4a55e3ce6042f"},
		{"llama-perplexity.exe", 9216, "e3a8e069f9ee22278a2ca2a283fdf128e3e64cf86b9c5e09166f5df36e3bd337"},
		{"llama-quantize-impl.dll", 137216, "80787050a6b8c3bb2a5cacffa263b0120a8dd6863a90660b849cdb4e3e50f6f7"},
		{"llama-quantize.exe", 9216, "74fefabcccfbaeb6a1e6084b1c9181efa09de19900473c2cc7296518d0e2ced5"},
		{"llama-qwen2vl-cli.exe", 27136, "293f408e5ba98337a94cf11c46f0fee972d6bf12abcfa50d09fe2882e7ad907c"},
		{"llama-results.exe", 46080, "2930d2c42881af7af564dd528e9afc6478e3ef2e8dc10235223bc12e0fbc90ed"},
		{"llama-server-impl.dll", 8893952, "685165c39d202aa76aac5852d79ff970d5f96f8130943e3c7b3cb98f78e7faec"},
		{"llama-server.exe", 9216, "de24bae95807c16ae93afca2ffdd57a85a252d7bfa59c768d201422c6e8dd8f0"},
		{"llama-tokenize.exe", 58368, "519743e463b1ee55c484169ad47d473cb62e49cf992b76620f83079e1f18f64b"},
		{"llama-tts.exe", 41472, "4ecf056b04b5bbbd8b9fe0aefa875215aaa287f5d31d7d825ad6baea80af2968"},
		{"llama.dll", 3143168, "46d129df4f270ee91ef49d95173548321d6ab3ab08d03452f8985de9a783e5b4"},
		{"llama.exe", 103936, "d632af6b5ee183f7f1b4f52b4f1c04f9fe04f22365cde506beeb7937379a73a5"},
		{"mtmd.dll", 1771008, "e774481b5f4c874477e4958cff4d0ca2741dcd3078561c1b9e43ac6b067bceee"},
	}, llamaRuntimeFiles()...),
	Excluded: llamaCppExcluded,
}

// Equal reports whether m and o pin the same folder, version and files.
func (m BackendManifest) Equal(o BackendManifest) bool { return sameManifest(m, o) }

// sameManifest reports two manifests that pin the same folder, version and
// files.
func sameManifest(a, b BackendManifest) bool {
	if a.Family != b.Family || a.Variant != b.Variant || a.Version != b.Version || len(a.Files) != len(b.Files) || len(a.Excluded) != len(b.Excluded) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	for i := range a.Excluded {
		if a.Excluded[i] != b.Excluded[i] {
			return false
		}
	}
	return true
}
