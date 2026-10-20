package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

// InstalledVersion is the version of installed.json this provider writes.
const InstalledVersion = 1

// Installed is the record of every model this state folder serves. The
// runtime reads the provider's declared file-backed store and re-hashes
// each file the store lists. Reading it does not start the provider.
type Installed struct {
	Version int              `json:"version"`
	Models  []InstalledModel `json:"models"`
}

// InstalledModel is one model and each of its files.
type InstalledModel struct {
	Model string          `json:"model"`
	Files []InstalledFile `json:"files"`
}

// InstalledFile is one file: its slash-separated path relative to the state
// folder, its size and its SHA-256 in lowercase hex.
type InstalledFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// InstalledPath is the state folder's installed-models record.
func InstalledPath(root string) string { return filepath.Join(root, "installed.json") }

// installedModels lists each model Setup verified and the selection serves,
// with every pinned file present at its pinned size: the selection's picture
// models, then each staged speech model. A model with a missing or resized
// file is left out.
func installedModels(pictures []string, present func(rel string, size int64) bool) []InstalledModel {
	return slices.DeleteFunc(pinnedModelInventory(pictures), func(model InstalledModel) bool {
		for _, file := range model.Files {
			if !present(file.Path, file.Size) {
				return true
			}
		}
		return false
	})
}

// WriteInstalled replaces installed.json in one rename with what the state
// folder serves now. Every selection change and every speech staging calls
// it after Setup verified the files it adds, and verify-setup after a deep
// verification, which records an install made before installed.json existed.
func WriteInstalled(root string) error {
	var pictures []string
	if selected, err := Read(SelectionPath(root)); err == nil {
		pictures = selected.Models
	}
	present := func(rel string, size int64) bool {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil && info.Mode().IsRegular() && info.Size() == size
	}
	data, err := json.MarshalIndent(Installed{Version: InstalledVersion, Models: installedModels(pictures, present)}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	return writeAtomic(InstalledPath(root), append(data, '\n'))
}

// WriteMissingInstalled records a state folder configured before
// installed.json existed: under the setup lock, so it never races a Setup
// step, it writes the record from the selection and file sizes when none is
// there. The runtime still hashes every file it names.
func WriteMissingInstalled(ctx context.Context, root string) error {
	return withSetupLock(ctx, root, func() error {
		if _, err := os.Lstat(InstalledPath(root)); !os.IsNotExist(err) {
			return err
		}
		return WriteInstalled(root)
	})
}
