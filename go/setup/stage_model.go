package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// stageModelPrefix names this step's temporary files beside their target.
// The server refuses any file in the models folder that is not a weight, so
// each run removes the ones a killed run left before it stages again.
const stageModelPrefix = ".stage-model-"

// linkFile makes a hard link; tests replace it.
var linkFile = os.Link

// StageModel adds one declared picture model other than the starter model
// (server.ImageModels) to a configured provider: the starter setup has
// selected a package and staged the engine. Each weight file goes to its
// repository's Hugging Face cache path under the private models folder. A
// file already staged there that verifies is kept. A file whose SHA-256 a
// staged file of another declared model already has is a hard link to that
// staged file, so a shared file is stored once and needs no source; a link
// that fails is refused, never replaced by a copy. Any other file is moved
// by rename from sources (role to absolute path, a download in DownloadDir
// on the models folder's volume) and verified before it is published; a
// refusal moves each download back. The model's registry entry and the
// selection follow last, so a refusal publishes no model the engine could
// serve.
func StageModel(ctx context.Context, root, id string, sources map[string]string, progress func(done, total int64) error) error {
	m, ok := server.FindImageModel(id)
	if !ok {
		return fmt.Errorf("%w: model %q is not declared", ErrSelection, id)
	}
	return withSetupLock(ctx, root, func() error {
		return stageModel(ctx, root, m, server.ImageModels, sources, progress)
	})
}

// stageModel is StageModel under the setup lock, with the catalogue whose
// staged files may be shared.
func stageModel(ctx context.Context, root string, m server.ImageModel, catalogue []server.ImageModel, sources map[string]string, progress func(done, total int64) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(root) || m.ID == ApprovedModel {
		return fmt.Errorf("%w: only a declared model other than the starter model is added", ErrSelection)
	}
	if err := m.Valid(); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	selected, err := Read(SelectionPath(root))
	if err != nil {
		return fmt.Errorf("%w: the picture engine needs its starter setup before another model: %v", ErrSelection, err)
	}
	modelsDir := filepath.Join(root, "engine-models")
	for _, f := range m.Files {
		if path := filepath.Join(modelsDir, f.CachePath()); len(path) >= 260 {
			return fmt.Errorf("%w: model path exceeds Lemonade's Windows path limit (%d characters)", ErrSelection, len(path))
		}
	}
	if err := removeModelStaging(modelsDir, m); err != nil {
		return err
	}
	var total, done int64
	for _, f := range m.Files {
		total += f.Bytes
	}
	account := func(n int64) error {
		done += n
		if progress != nil {
			return progress(done, total)
		}
		return nil
	}
	// Every file is placed beside its target first; only after all of them
	// verify does any take its final name. A refusal returns each moved
	// download to its source and removes each link.
	type placed struct{ temp, target string }
	var ready []placed
	var moved placements
	defer func() {
		moved.restore()
		for _, p := range ready {
			_ = os.Remove(p.temp)
		}
	}()
	for _, f := range m.Files {
		target := filepath.Join(modelsDir, f.CachePath())
		if _, err := os.Lstat(target); err == nil {
			if err := checkDigestCtx(ctx, target, f.SHA256, account); err != nil {
				return fmt.Errorf("%w: a staged %s file does not verify; remove the model in Setup and add it again: %v", ErrSelection, f.Role, err)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		temp, err := os.CreateTemp(filepath.Dir(target), stageModelPrefix)
		if err != nil {
			return err
		}
		tempName := temp.Name()
		temp.Close()
		if err := os.Remove(tempName); err != nil {
			return err
		}
		ready = append(ready, placed{tempName, target})
		if shared := stagedCopy(ctx, modelsDir, f, catalogue); shared != "" {
			// OA never makes a copy of a model for an engine (VISION
			// 2026-10-01): a shared file is the same file, reached by a
			// hard link, or the step refuses.
			if err := linkFile(shared, tempName); err != nil {
				return fmt.Errorf("%w: the %s file of %s is already staged for another model and needs a hard link to it (%v); keep the provider's models folder on one volume that supports hard links, such as NTFS, and add the model again", ErrSelection, f.Role, m.ID, err)
			}
			if err := account(f.Bytes); err != nil {
				return err
			}
			continue
		}
		source := sources[f.Role]
		if !filepath.IsAbs(source) {
			return fmt.Errorf("%w: the %s file of %s needs its verified download", ErrSelection, f.Role, m.ID)
		}
		if info, err := os.Lstat(source); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: the %s download of %s is missing", ErrSelection, f.Role, m.ID)
		}
		if err := moved.place(ctx, weightPin{source: source, rel: f.Path, digest: f.SHA256}, tempName, account); err != nil {
			return err
		}
	}
	for _, p := range ready {
		if err := os.Rename(p.temp, p.target); err != nil {
			return err
		}
	}
	ready = nil
	for _, f := range m.Files {
		refs := filepath.Join(modelsDir, f.CacheFolder(), "refs", "main")
		current, err := os.ReadFile(refs)
		switch {
		case err == nil && string(current) != f.Revision:
			return fmt.Errorf("%w: %s is staged at another revision", ErrSelection, f.Repository)
		case err == nil:
			continue
		case !os.IsNotExist(err):
			return err
		}
		if err := os.MkdirAll(filepath.Dir(refs), 0700); err != nil {
			return err
		}
		if err := writeAtomic(refs, []byte(f.Revision)); err != nil {
			return err
		}
	}
	if slices.Contains(selected.Models, m.ID) {
		return nil
	}
	next := selectionFor(selectedPackage(selected), append(selected.Models, m.ID))
	if err := next.Validate(); err != nil {
		return err
	}
	registry, err := registryBytes(next.Models)
	if err != nil {
		return err
	}
	configDir := ConfigDirFor(root, selectedPackage(selected).Version)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(configDir, "user_models.json"), registry); err != nil {
		return err
	}
	return Save(SelectionPath(root), next)
}

// verifyAddedModels hashes every file of each selected model after the
// starter model against catalogue; a missing or changed file is an error,
// since the selection already serves it.
func verifyAddedModels(ctx context.Context, root string, models []string, catalogue []server.ImageModel) error {
	for _, id := range models {
		if id == ApprovedModel {
			continue
		}
		i := slices.IndexFunc(catalogue, func(m server.ImageModel) bool { return m.ID == id })
		if i < 0 {
			return fmt.Errorf("%w: selected model %q is not declared", ErrSelection, id)
		}
		for _, f := range catalogue[i].Files {
			if err := checkDigestCtx(ctx, filepath.Join(root, "engine-models", f.CachePath()), f.SHA256, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// stagedCopy is a staged file of another declared model with f's SHA-256
// that verifies, or "".
func stagedCopy(ctx context.Context, modelsDir string, f server.ModelFile, catalogue []server.ImageModel) string {
	for _, other := range catalogue {
		for _, candidate := range other.Files {
			if candidate.SHA256 != f.SHA256 || candidate.CachePath() == f.CachePath() {
				continue
			}
			path := filepath.Join(modelsDir, candidate.CachePath())
			if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
				continue
			}
			if checkDigestCtx(ctx, path, f.SHA256, nil) == nil {
				return path
			}
		}
	}
	return ""
}

// removeModelStaging removes the temporary files a killed run of this step
// left beside m's targets.
func removeModelStaging(modelsDir string, m server.ImageModel) error {
	seen := map[string]bool{}
	for _, f := range m.Files {
		dir := filepath.Dir(filepath.Join(modelsDir, f.CachePath()))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), stageModelPrefix) {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
