package setup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// fixtureArchive writes a ZIP holding entries and returns the archive pin
// and manifest a staging of it is checked against. Names in excluded are
// archive entries the manifest requires absent.
func fixtureArchive(t *testing.T, entries map[string]string, excluded ...string) (backendArchive, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "llama-fixture.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	m := server.BackendManifest{Family: "llamacpp", Variant: "vulkan", Version: "b-fixture", Excluded: excluded}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(entries[name])); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(entries[name]))
		m.Files = append(m.Files, server.ManifestFile{Name: name, Size: int64(len(entries[name])), SHA256: hex.EncodeToString(sum[:])})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return backendArchive{manifest: m, size: int64(len(data)), sha256: hex.EncodeToString(sum[:])}, path
}

// The staged llama.cpp folder holds exactly the files review condition 5
// keeps and Lemonade's version.txt: excluded archive entries are never
// written, and the folder proves against the manifest.
func TestStagedLlamaBackendKeepsOnlyTheReviewedFiles(t *testing.T) {
	a, archive := fixtureArchive(t, map[string]string{
		"llama-server.exe": "server", "llama.dll": "llama", "ggml-rpc-server.exe": "rpc", "llama-cli.exe": "cli",
	}, "ggml-rpc-server.exe", "llama-cli.exe")
	cache := filepath.Join(t.TempDir(), "engine-cache")
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	dir := a.manifest.Dir(cache)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if len(got) != 3 || got[0] != "llama-server.exe" || got[1] != "llama.dll" || got[2] != "version.txt" {
		t.Fatalf("staged folder holds %v", got)
	}
	if err := server.VerifyBackendDir(dir, a.manifest); err != nil {
		t.Fatalf("staged folder does not prove: %v", err)
	}
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatalf("a staged folder that proves was not kept: %v", err)
	}
}

func TestLlamaBackendArchiveRefusals(t *testing.T) {
	good, archive := fixtureArchive(t, map[string]string{"llama-server.exe": "server", "llama.dll": "llama"})
	extra, extraArchive := fixtureArchive(t, map[string]string{"llama-server.exe": "server", "llama.dll": "llama", "version.dll": "planted"})
	extra.manifest = good.manifest
	wrongDigest := good
	wrongDigest.sha256 = "0" + good.sha256[1:]
	for name, c := range map[string]struct {
		a       backendArchive
		archive string
	}{
		"unpinned archive digest":    {wrongDigest, archive},
		"entry outside the manifest": {extra, extraArchive},
	} {
		t.Run(name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "engine-cache")
			if err := stageBackend(context.Background(), c.a, cache, c.archive, nil, false); !errors.Is(err, ErrSelection) {
				t.Fatalf("%s staged: %v", name, err)
			}
			if _, err := os.Lstat(c.a.manifest.Dir(cache)); !os.IsNotExist(err) {
				t.Fatalf("%s published a backend folder: %v", name, err)
			}
		})
	}
}

// The llama.cpp staging pin is the b10825 review's archive and manifest.
func TestLlamaArchivePinIsTheReviewedB10825(t *testing.T) {
	if llamaArchive.size != 35227898 || llamaArchive.sha256 != "8907e5f3b74cb57d93fd94e187a87d85be24dc64e3b0ff2bc1b287da17b31446" ||
		llamaArchive.manifest.Version != "b10825" || len(llamaArchive.manifest.Files) != len(server.LlamaCppVulkanB10825.Files) {
		t.Fatalf("llama.cpp staging pin: %d bytes, %s, %s", llamaArchive.size, llamaArchive.sha256, llamaArchive.manifest.Version)
	}
	var kept int64
	for _, f := range server.LlamaCppVulkanB10825.Required() {
		kept += f.Size
	}
	if llamaArchive.expanded() != kept || vulkanArchive.expanded() != vulkanExpandedBytes {
		t.Fatalf("expanded sizes %d and %d", llamaArchive.expanded(), vulkanArchive.expanded())
	}
}

// configure with --llamacpp-backend-archive and --replace-backend
// replaces a llama.cpp folder of another build: the image backend that
// already proves is kept, and the whole cache is proven only after both
// stages, so the stale folder cannot block its own replacement.
func TestConfigureStagingReplacesAnotherLlamaBuild(t *testing.T) {
	sd, sdZip := fixtureArchive(t, map[string]string{"sd-server.exe": "sd"})
	sd.manifest.Family = "sd-cpp"
	llama, llamaZip := fixtureArchive(t, map[string]string{"llama-server.exe": "server", "ggml-rpc-server.exe": "rpc"}, "ggml-rpc-server.exe")
	cache := filepath.Join(t.TempDir(), "engine-cache")
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
	if err := stageBackends(context.Background(), cache, []stagedArchive{{sd, sdZip}, {llama, llamaZip}}, true, verify); err != nil {
		t.Fatalf("the other build blocked its replacement: %v", err)
	}
	if err := verify(cache); err != nil {
		t.Fatal(err)
	}
}

// installer/third-party.tsv records the llama.cpp archive and its 52 files
// with the hashes this module enforces: the archive by setup/backend.go, each
// file by server/manifest.go, excluded exactly when the manifest excludes it.
// No other llama.cpp row of the lemonade build remains.
func TestPinRecordsMatchTheLlamaManifest(t *testing.T) {
	data, err := os.ReadFile(privateInput(t, "..", "..", "..", "..", "installer", "third-party.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	m := server.LlamaCppVulkanB10825
	excluded := map[string]bool{}
	for _, name := range m.Excluded {
		excluded[name] = true
	}
	const archiveName = "llama-b10825-bin-win-vulkan-x64.zip"
	files := map[string][]string{}
	archiveRows := 0
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		row := strings.Split(line, "\t")
		if len(row) != 9 || row[0] != "lemonade" || !strings.HasPrefix(row[1], "llama") {
			continue
		}
		if row[1] == archiveName {
			archiveRows++
			if row[2] != "archive" || row[3] != m.Version || row[4] != "sha256:"+llamaArchive.sha256 || row[5] != "publisher" ||
				row[6] != LlamaBackendURL || row[8] != "openabstractions-flat/abstraction-provider-lemonade/go/setup/backend.go" {
				t.Fatalf("archive row %q", row)
			}
			continue
		}
		name, ok := strings.CutPrefix(row[1], "llamacpp/vulkan/")
		if !ok || files[name] != nil {
			t.Fatalf("unexpected or repeated llama.cpp row %q", row[1])
		}
		files[name] = row
	}
	if archiveRows != 1 || len(files) != len(m.Files) {
		t.Fatalf("%d archive rows and %d file rows, want 1 and %d", archiveRows, len(files), len(m.Files))
	}
	for _, f := range m.Files {
		row := files[f.Name]
		isRuntime := false
		for _, runtime := range llamaRuntimeInput.Files {
			isRuntime = isRuntime || f.Name == runtime.Name
		}
		if isRuntime {
			if row == nil || row[2] != "runtime" || row[3] != llamaRuntimeVersion || row[4] != "sha256:"+f.SHA256 ||
				row[5] != "derived:"+llamaRuntimeInput.Name || row[6] != llamaRuntimeURL || row[8] != "monitor/internal/setup/vc_runtime_asset.json" {
				t.Fatalf("runtime %s: pin row %q", f.Name, row)
			}
			continue
		}
		kind := "engine"
		if excluded[f.Name] {
			kind = "excluded"
		}
		if row == nil || row[2] != kind || row[3] != m.Version || row[4] != "sha256:"+f.SHA256 || row[5] != "derived:"+archiveName ||
			row[6] != LlamaBackendURL || row[8] != "openabstractions-flat/abstraction-provider-lemonade/go/server/manifest.go" {
			t.Fatalf("%s: pin row %q, manifest %s %s", f.Name, row, kind, f.SHA256)
		}
	}
}

// OA_LLAMA_B10825_ZIP names a copy of llama-b10825-bin-win-vulkan-x64.zip; the
// test stages it into a temporary cache and proves the folder. The pinned
// runtime release input must be beside that source ZIP. Nothing in it runs.
func TestLiveStageReviewedLlamaBackend(t *testing.T) {
	archive := os.Getenv("OA_LLAMA_B10825_ZIP")
	if archive == "" {
		t.Skip("OA_LLAMA_B10825_ZIP names no copy of llama-b10825-bin-win-vulkan-x64.zip")
	}
	cache := filepath.Join(t.TempDir(), "engine-cache")
	check := func(want int) {
		t.Helper()
		python, tool := os.Getenv("OA_RUNTIME_CHECK_PYTHON"), os.Getenv("OA_RUNTIME_IMPORT_CHECK")
		if python == "" || tool == "" {
			return
		}
		command := exec.Command(python, tool, llamaArchive.manifest.Dir(cache))
		output, err := command.CombinedOutput()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != want {
			t.Fatalf("native import check got %d want %d: %s", code, want, output)
		}
		findings := 0
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "FAIL  ") {
				findings++
			}
		}
		t.Logf("native PE import check exit %d, findings %d", code, findings)
	}
	// Reproduce the publisher-only folder through the same staging owner.
	before := llamaArchive
	before.runtime = nil
	before.manifest.Files = nil
	for _, file := range llamaArchive.manifest.Files {
		isRuntime := false
		for _, runtime := range llamaRuntimeInput.Files {
			isRuntime = isRuntime || file.Name == runtime.Name
		}
		if !isRuntime {
			before.manifest.Files = append(before.manifest.Files, file)
		}
	}
	if err := stageBackend(context.Background(), before, cache, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	check(1)
	var last, total int64
	if err := stageBackend(context.Background(), llamaArchive, cache, archive, func(done, all int64) error { last, total = done, all; return nil }, true); err != nil {
		t.Fatal(err)
	}
	check(0)
	// The exported step also proves the whole cache, whose image backend
	// this test does not stage.
	if err := StageApprovedLlamaBackend(context.Background(), cache, archive, nil, false); !errors.Is(err, server.ErrBackendManifest) {
		t.Fatalf("llama.cpp staged without the image backend: %v", err)
	}
	var runtimeBytes int64
	for _, file := range llamaRuntimeInput.Files {
		runtimeBytes += file.Size
	}
	if last != total || total != llamaArchive.size+llamaArchive.expanded()-runtimeBytes {
		t.Fatalf("progress %d/%d", last, total)
	}
	if err := server.VerifyBackendDir(server.LlamaCppVulkanB10825.Dir(cache), server.LlamaCppVulkanB10825); err != nil {
		t.Fatal(err)
	}
}
