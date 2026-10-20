package setup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

func runtimeFixture(t *testing.T, archivePath string) (backendRuntime, string) {
	t.Helper()
	r := backendRuntime{Name: "vc-runtime-fixture.zip", Prefix: "Microsoft.VC145.CRT/"}
	path := filepath.Join(filepath.Dir(archivePath), r.Name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	names := []string{"msvcp140.dll", "vcruntime140.dll", "vcruntime140_1.dll"}
	sort.Strings(names)
	for _, name := range names {
		data := []byte("fixture runtime " + name)
		entry, err := writer.Create(r.Prefix + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		r.Files = append(r.Files, server.ManifestFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	if err := errors.Join(writer.Close(), file.Close()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	r.Size, r.SHA256 = int64(len(data)), hex.EncodeToString(sum[:])
	return r, path
}

func TestSetupStagesPinnedRuntimeWithLlamaAndKeepsVerifiedTreeWithoutInputs(t *testing.T) {
	a, archive := fixtureArchive(t, map[string]string{"llama-server.exe": "server", "llama.dll": "library"})
	runtime, input := runtimeFixture(t, archive)
	a.runtime = &runtime
	a.manifest.Files = append(a.manifest.Files, runtime.Files...)
	cache := filepath.Join(t.TempDir(), "engine-cache")
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	for _, file := range runtime.Files {
		data, err := os.ReadFile(filepath.Join(a.manifest.Dir(cache), file.Name))
		if err != nil || string(data) != "fixture runtime "+file.Name {
			t.Fatalf("runtime %s not copied: %q, %v", file.Name, data, err)
		}
	}
	if err := server.VerifyBackendDir(a.manifest.Dir(cache), a.manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatalf("verified tree required retired input cache: %v", err)
	}
}

func TestRuntimeInputRefusalLeavesExistingBackendUnchanged(t *testing.T) {
	for _, mode := range []string{"missing", "changed ZIP", "wrong member pin"} {
		t.Run(mode, func(t *testing.T) {
			old, archive := fixtureArchive(t, map[string]string{"llama-server.exe": "server"})
			cache := filepath.Join(t.TempDir(), "engine-cache")
			if err := stageBackend(context.Background(), old, cache, archive, nil, false); err != nil {
				t.Fatal(err)
			}
			runtime, input := runtimeFixture(t, archive)
			next := old
			next.manifest.Version = "b-runtime"
			next.manifest.Files = append(append([]server.ManifestFile(nil), old.manifest.Files...), runtime.Files...)
			next.runtime = &runtime
			switch mode {
			case "missing":
				if err := os.Remove(input); err != nil {
					t.Fatal(err)
				}
			case "changed ZIP":
				if err := os.WriteFile(input, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong member pin":
				runtime.Files[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			}
			if err := stageBackend(context.Background(), next, cache, archive, nil, true); !errors.Is(err, ErrSelection) {
				t.Fatalf("runtime refusal = %v", err)
			}
			if err := server.VerifyBackendDir(old.manifest.Dir(cache), old.manifest); err != nil {
				t.Fatalf("runtime refusal changed prior backend: %v", err)
			}
		})
	}
}

func TestReplacePublisherOnlyB10825TreeWithBothArchives(t *testing.T) {
	files := map[string]string{}
	runtimeNames := map[string]bool{}
	for _, file := range llamaRuntimeInput.Files {
		runtimeNames[file.Name] = true
	}
	for _, file := range server.LlamaCppVulkanB10825.Required() {
		if !runtimeNames[file.Name] {
			files[file.Name] = "inert publisher " + file.Name
		}
	}
	if len(files) != 24 {
		t.Fatalf("publisher-only fixture has %d files", len(files))
	}
	a, archive := fixtureArchive(t, files)
	a.manifest.Version = "b10825"
	cache := filepath.Join(t.TempDir(), "engine-cache")
	if err := stageBackend(context.Background(), a, cache, archive, nil, false); err != nil {
		t.Fatal(err)
	}
	runtime, input := runtimeFixture(t, archive)
	next := a
	next.runtime = &runtime
	next.manifest.Files = append(append([]server.ManifestFile(nil), a.manifest.Files...), runtime.Files...)
	sort.Slice(next.manifest.Files, func(i, j int) bool { return next.manifest.Files[i].Name < next.manifest.Files[j].Name })
	if err := stageBackend(context.Background(), next, cache, archive, nil, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("unrequested replacement: %v", err)
	}
	if err := stageBackend(context.Background(), next, cache, archive, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := server.VerifyBackendDir(next.manifest.Dir(cache), next.manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(input); err != nil {
		t.Fatal(err)
	}
}
