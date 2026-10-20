package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePackageFixture writes files (slash paths) under a fresh package folder
// and returns the folder and the manifest that pins them.
func writePackageFixture(t *testing.T, files map[string][]byte) (string, PackageManifest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "package")
	m := PackageManifest{Name: "fixture-package"}
	for name, data := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		m.Files = append(m.Files, ManifestFile{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	}
	return root, m
}

// resources/defaults.json and backend_versions.json are read by lemond at run
// time; before they joined the manifest only lemond.exe was hashed.
func TestPackageManifestRefusals(t *testing.T) {
	files := map[string][]byte{
		"lemond.exe": []byte("server"), "LICENSE": []byte("apache"),
		"resources/defaults.json": []byte(`{"offline":false}`), "resources/backend_versions.json": []byte(`{"llamacpp":{}}`),
	}
	write := func(rel, data string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			path := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"accepted", func(*testing.T, string) {}},
		{"changed defaults, same size", write("resources/defaults.json", `{"offline":fals3}`)},
		{"changed backend versions", write("resources/backend_versions.json", `{"llamacpp":{"vulkan":"b1"}}`)},
		{"planted resource", write("resources/architecture_defaults.json", `{}`)},
		{"planted library", write("version.dll", "planted")},
		{"extra folder", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing license", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "LICENSE")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, m := writePackageFixture(t, files)
			c.mutate(t, root)
			err := VerifyPackageDir(root, m)
			if c.name == "accepted" {
				if err != nil {
					t.Fatalf("exact package refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrPackageManifest) {
				t.Fatalf("%s accepted: %v", c.name, err)
			}
		})
	}
}

func TestReviewedPackageManifest(t *testing.T) {
	if !Lemonade2026391.valid() || len(Lemonade2026391.Files) != 9 {
		t.Fatalf("reviewed package manifest has %d files", len(Lemonade2026391.Files))
	}
	for _, f := range Lemonade2026391.Files {
		switch f.Name {
		case "lemond.exe":
			if f.SHA256 != "4a62f8193799eb6d0cc28975bcc72338bd6cc795e6f9d977c3012cc2adcc8df7" {
				t.Fatal("lemond.exe differs from the review")
			}
		case "resources/backend_versions.json":
			if !strings.HasPrefix(f.SHA256, "47a8668b") || !strings.HasSuffix(f.SHA256, "d57c6b5") {
				t.Fatal("backend_versions.json differs from the review")
			}
		}
	}
	for _, name := range []string{"resources/defaults.json", "resources/backend_versions.json"} {
		if !Lemonade2026391.has(name) {
			t.Fatalf("%s not pinned", name)
		}
	}
}
