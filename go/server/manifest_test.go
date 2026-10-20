package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
)

// openACL stands for the shared kit's read-only folder ACL, whose own tests
// set and check the real one.
type openACL struct{}

func (openACL) Restrict([]string) error                 { return nil }
func (openACL) Check(string, *enginefiles.Handle) error { return nil }

// writeBackendFixture writes a backend folder and returns the manifest that
// pins it. excluded names are added to the manifest but not written.
func writeBackendFixture(t *testing.T, cacheDir, family, variant string, files map[string]string, excluded map[string]string) (BackendManifest, string) {
	t.Helper()
	m := BackendManifest{Family: family, Variant: variant, Version: "fixture-1"}
	dir := m.Dir(cacheDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	add := func(name, content string) {
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, ManifestFile{Name: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
	}
	for name, content := range files {
		add(name, content)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range excluded {
		add(name, content)
		m.Excluded = append(m.Excluded, name)
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("fixture-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestBackendManifestRefusals(t *testing.T) {
	type mutation struct {
		name   string
		mutate func(t *testing.T, dir string)
	}
	write := func(name, content string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []mutation{
		{"accepted", func(*testing.T, string) {}},
		{"changed library, same size", write("ggml.dll", "GGML")},
		{"changed library, other size", write("ggml.dll", "ggml-longer")},
		{"planted library", write("version.dll", "planted")},
		{"excluded tool present", write("rpc-server.exe", "rpc")},
		{"wrong version", write("version.txt", "fixture-2")},
		{"missing library", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "ggml.dll")); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing version", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "version.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"subfolder", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "plugins"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, dir := writeBackendFixture(t, t.TempDir(), "sd-cpp", "vulkan",
				map[string]string{"server.exe": "server", "ggml.dll": "ggml"}, map[string]string{"rpc-server.exe": "rpc"})
			c.mutate(t, dir)
			err := VerifyBackendDir(dir, m)
			if c.name == "accepted" {
				if err != nil {
					t.Fatalf("exact reviewed folder refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrBackendManifest) {
				t.Fatalf("%s accepted: %v", c.name, err)
			}
		})
	}
}

func TestBackendTreeAdmitsOnlyReviewedFolders(t *testing.T) {
	cache := t.TempDir()
	sd, _ := writeBackendFixture(t, cache, "sd-cpp", "vulkan", map[string]string{"sd-server.exe": "sd"}, nil)
	llama := BackendManifest{Family: "llamacpp", Variant: "vulkan", Version: "b1", Files: []ManifestFile{{"llama-server.exe", 1, strings.Repeat("0", 64)}}}
	if err := CheckBackendTree(cache, sd, llama, true); err != nil {
		t.Fatalf("reviewed image backend refused: %v", err)
	}
	if err := CheckBackendTree(t.TempDir(), sd, llama, true); !errors.Is(err, ErrBackendManifest) {
		t.Fatalf("required image backend absent but admitted: %v", err)
	}
	if err := CheckBackendTree(t.TempDir(), sd, llama, false); err != nil {
		t.Fatalf("empty cache refused when the image backend is optional: %v", err)
	}
	for _, extra := range []string{
		filepath.Join("bin", "sd-cpp", "cpu"),
		filepath.Join("bin", "sd-cpp", "vulkan.staging"),
		filepath.Join("bin", "whispercpp"),
		filepath.Join("bin", "llamacpp", "vulkan"),
		filepath.Join("bin", "llamacpp", "cpu"),
	} {
		path := filepath.Join(cache, extra)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := CheckBackendTree(cache, sd, llama, true); !errors.Is(err, ErrBackendManifest) {
			t.Fatalf("unreviewed %s admitted: %v", extra, err)
		}
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "bin", "loose.dll"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckBackendTree(cache, sd, llama, true); !errors.Is(err, ErrBackendManifest) {
		t.Fatalf("loose file in bin admitted: %v", err)
	}
}

func TestLlamaBackendFolderMustMatchWhenPresent(t *testing.T) {
	cache := t.TempDir()
	sd, _ := writeBackendFixture(t, cache, "sd-cpp", "vulkan", map[string]string{"sd-server.exe": "sd"}, nil)
	llama, dir := writeBackendFixture(t, cache, "llamacpp", "vulkan",
		map[string]string{"llama-server.exe": "server", "llama.dll": "llama"}, map[string]string{"ggml-rpc-server.exe": "rpc"})
	if err := CheckBackendTree(cache, sd, llama, true); err != nil {
		t.Fatalf("reviewed llama.cpp folder refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ggml-rpc-server.exe"), []byte("rpc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckBackendTree(cache, sd, llama, true); !errors.Is(err, ErrBackendManifest) || !strings.Contains(err.Error(), "excluded file present: ggml-rpc-server.exe") {
		t.Fatalf("reviewed RPC server beside llama-server admitted or not named as excluded: %v", err)
	}
}

// The recorded manifests carry the reviews' file counts and pinned digests.
func TestReviewedManifestsMatchReviews(t *testing.T) {
	if !SDCppVulkan.valid() || !LlamaCppVulkanB10825.valid() {
		t.Fatal("reviewed manifest invalid")
	}
	if len(SDCppVulkan.Files) != 21 || len(SDCppVulkan.Excluded) != 0 {
		t.Fatalf("sd.cpp manifest has %d files, review records 21", len(SDCppVulkan.Files))
	}
	if len(LlamaCppVulkanB10825.Files) != 52 {
		t.Fatalf("llama.cpp manifest has %d files, review records 52", len(LlamaCppVulkanB10825.Files))
	}
	digest := func(m BackendManifest, name string) string {
		for _, f := range m.Files {
			if f.Name == name {
				return f.SHA256
			}
		}
		return ""
	}
	if digest(SDCppVulkan, "sd-server.exe") != "e45bbbe59f30746a7933c63c400a00996b8a3059335bae878b11bbdc555dceae" {
		t.Fatal("sd-server.exe digest differs from the review")
	}
	if digest(LlamaCppVulkanB10825, "llama-server.exe") != "de24bae95807c16ae93afca2ffdd57a85a252d7bfa59c768d201422c6e8dd8f0" {
		t.Fatal("llama-server.exe digest differs from the review")
	}
	required := map[string]bool{}
	for _, f := range LlamaCppVulkanB10825.Required() {
		required[f.Name] = true
	}
	for _, name := range []string{"llama-server.exe", "llama-server-impl.dll", "llama-common.dll", "llama.dll", "mtmd.dll", "ggml.dll", "ggml-base.dll", "ggml-vulkan.dll", "libomp.dll"} {
		if !required[name] {
			t.Fatalf("llama-server dependency %s not required", name)
		}
	}
	for name := range required {
		if strings.HasSuffix(name, ".exe") && name != "llama-server.exe" {
			t.Fatalf("command-line tool %s admitted; review condition 5 removes it", name)
		}
	}
	if required["ggml-rpc-server.exe"] || required["ggml-rpc.dll"] {
		t.Fatal("RPC server or client admitted")
	}
}

// A llama.cpp folder of another build, such as a b10723 folder staged before
// Lemonade 2026.39.1, refuses every launch; the refusal names the configure
// step that replaces it.
func TestOtherLlamaBuildRefusalNamesTheFix(t *testing.T) {
	cache := t.TempDir()
	sd, _ := writeBackendFixture(t, cache, "sd-cpp", "vulkan", map[string]string{"sd-server.exe": "sd"}, nil)
	dir := LlamaCppVulkanB10825.Dir(cache)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("b10723\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := CheckBackendTree(cache, sd, LlamaCppVulkanB10825, true)
	if !errors.Is(err, ErrBackendManifest) || !strings.Contains(err.Error(), "lemonaded configure") ||
		!strings.Contains(err.Error(), "--llamacpp-backend-archive") || !strings.Contains(err.Error(), "--engine-files-only") ||
		!strings.Contains(err.Error(), "--replace-backend") || strings.Contains(err.Error(), "--replace-private-model") {
		t.Fatalf("refusal names no fix: %v", err)
	}
}

// Lemonade 2026.39.1 pins llama.cpp b10825 (its resources/backend_versions.json);
// the provider admits only that build's reviewed folder by default.
func TestNewSelectsTheReviewedB10825Backend(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234, Models: []string{"user.A"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.cfg.LlamaBackend.Version != "b10825" || !sameManifest(s.cfg.LlamaBackend, LlamaCppVulkanB10825) {
		t.Fatalf("default llama.cpp backend is %s", s.cfg.LlamaBackend.Version)
	}
}

// The b10825 manifest is the review's: 52 archive entries, each digest as the
// review's file table records it, and review condition 5's reduced set.
func TestB10825ManifestMatchesTheReview(t *testing.T) {
	m := LlamaCppVulkanB10825
	if !m.valid() || m.Family != "llamacpp" || m.Variant != "vulkan" || m.Version != "b10825" || len(m.Files) != 52 {
		t.Fatalf("b10825 manifest: valid %v, %d files", m.valid(), len(m.Files))
	}
	kept := map[string]bool{}
	for _, f := range m.Required() {
		kept[f.Name] = true
	}
	want := []string{"LICENSE-LLVM-OpenMP", "ggml-base.dll", "ggml-cpu-alderlake.dll", "ggml-cpu-cannonlake.dll", "ggml-cpu-cascadelake.dll",
		"ggml-cpu-cooperlake.dll", "ggml-cpu-haswell.dll", "ggml-cpu-icelake.dll", "ggml-cpu-ivybridge.dll", "ggml-cpu-piledriver.dll",
		"ggml-cpu-sandybridge.dll", "ggml-cpu-sapphirerapids.dll", "ggml-cpu-skylakex.dll", "ggml-cpu-sse42.dll", "ggml-cpu-x64.dll",
		"ggml-cpu-zen4.dll", "ggml-vulkan.dll", "ggml.dll", "libomp.dll", "llama-common.dll", "llama-server-impl.dll", "llama-server.exe",
		"llama.dll", "mtmd.dll"}
	if len(kept) != len(want) {
		t.Fatalf("b10825 keeps %d files, condition 5 keeps %d", len(kept), len(want))
	}
	for _, name := range want {
		if !kept[name] {
			t.Fatalf("b10825 does not keep %s", name)
		}
	}
	review, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "research", "security-reviews", "llama.cpp-b10825-win-vulkan-x64.md"))
	if os.IsNotExist(err) {
		t.Skip("the review is not in this tree")
	}
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	_, table, _ := strings.Cut(string(review), "## File manifest")
	table, _, _ = strings.Cut(table, "\n## ")
	for _, line := range strings.Split(table, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) != 4 {
			continue
		}
		fields := strings.Fields(cells[2])
		if len(fields) == 0 {
			continue
		}
		digest := strings.Trim(fields[0], "`")
		if len(digest) != 64 {
			continue
		}
		for _, name := range strings.Split(cells[1], ",") {
			digests[strings.Trim(strings.TrimSpace(name), "`")] = digest
		}
	}
	if len(digests) != 52 {
		t.Fatalf("read %d digests from the review", len(digests))
	}
	for _, f := range m.Files {
		if digests[f.Name] != f.SHA256 {
			t.Fatalf("%s: manifest %s, review %s", f.Name, f.SHA256, digests[f.Name])
		}
	}
}
