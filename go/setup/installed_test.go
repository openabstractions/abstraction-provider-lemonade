package setup

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// installed.json lists each model the state folder serves with every file
// present at its pinned size, picture and speech models alike, with the
// path, size and SHA-256 the runtime re-hashes against.
func TestInstalledListsEveryServedModelWithItsFiles(t *testing.T) {
	klein, ok := server.FindImageModel(ApprovedModel)
	if !ok {
		t.Fatal("starter model is not declared")
	}
	have := map[string]int64{}
	for _, f := range klein.Files {
		have[path.Join("engine-models", filepath.ToSlash(f.CachePath()))] = f.Bytes
	}
	for i, rel := range server.KokoroV1Model.Paths() {
		have[path.Join("engine-models", rel)] = server.KokoroV1Model.Files[i].Size
	}
	present := func(rel string, size int64) bool { got, ok := have[rel]; return ok && got == size }
	models := installedModels([]string{ApprovedModel}, present)
	if len(models) != 2 || models[0].Model != ApprovedModel || models[1].Model != server.KokoroV1Model.Name {
		t.Fatalf("installed models = %+v", models)
	}
	for i, f := range klein.Files {
		got := models[0].Files[i]
		if got.Size != f.Bytes || got.SHA256 != f.SHA256 || got.Path != path.Join("engine-models", filepath.ToSlash(f.CachePath())) {
			t.Fatalf("picture file %d = %+v", i, got)
		}
	}
	if len(models[1].Files) != len(server.KokoroV1Model.Files) {
		t.Fatalf("speech files = %+v", models[1].Files)
	}
	// A file of the wrong size leaves its model out; Whisper was never staged.
	first := models[1].Files[1].Path
	have[first]++
	if models := installedModels([]string{ApprovedModel}, present); len(models) != 1 || models[0].Model != ApprovedModel {
		t.Fatalf("a resized speech file kept its model: %+v", models)
	}
}

// Every saved selection replaces installed.json in its state folder, so the
// runtime's file-backed store follows Setup.
func TestSaveWritesInstalled(t *testing.T) {
	root := t.TempDir()
	if err := Save(SelectionPath(root), selectionFor(testPackage, []string{ApprovedModel})); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(InstalledPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var record Installed
	if err := json.Unmarshal(data, &record); err != nil || record.Version != InstalledVersion || record.Models == nil || len(record.Models) != 0 {
		t.Fatalf("installed.json with no staged file = %s, %v", data, err)
	}
}
