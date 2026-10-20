package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// Setup moves the provider's binding from one installed Lemonade version to
// the next through three verbs (research/lemonade-updates/DECISION.md,
// point 2): CheckPackage proves a staged tree, ActivatePackage binds it, and
// RetirePackage removes what the provider kept for a version it no longer
// runs. Only Setup calls them; Setup's journal stays the authority over
// which version is installed.

// ErrHeld reports a version whose files a launch holds; retire it again once
// the engine has exited.
var ErrHeld = errors.New("lemonade setup: held")

// ErrPackageRefused reports a definitive refusal of a package version: an
// unknown version, or a tree that differs from the version's manifest.
var ErrPackageRefused = errors.New("lemonade setup: package refused")

// CheckPackage proves the tree Setup staged at root, the version's install
// folder, against version's table entry: every file of the package folder
// is hashed and any extra file, changed file or link is refused. It changes
// no ACL, writes no record and leaves the selection as it is. It returns the
// entry, whose pairing the caller compares with its catalog.
func CheckPackage(ctx context.Context, version, root string) (server.Package, error) {
	if err := ctx.Err(); err != nil {
		return server.Package{}, err
	}
	p, ok := server.FindPackage(version)
	if !ok {
		return server.Package{}, fmt.Errorf("%w: %w: %q", ErrPackageRefused, server.ErrUnknownPackage, version)
	}
	if !filepath.IsAbs(root) {
		return server.Package{}, fmt.Errorf("%w: the package root must be an absolute path", ErrSelection)
	}
	// verifyPackageAt marks a definitive refusal itself; an unreadable file
	// is passed on for another attempt.
	if err := verifyPackageAt(p, filepath.Join(root, p.Manifest.Name, p.Program)); err != nil {
		return server.Package{}, err
	}
	return p, nil
}

// ActivatePackage binds version: it proves the version's installed package,
// installs its engine files (INF-F1, INF-F3, INF-F6), stages its paired
// backends into its own cache, writes its config folder, checks the private
// model's revision against its pairing, and writes the selection last, all
// under the setup lock. A backend archive left empty is staged from the
// currently bound version's folder when the pairing names the same build.
// A run with the selection already at version proves its files and returns
// without copying. A refusal leaves the selection as it was.
func ActivatePackage(ctx context.Context, root, version, sdBackendArchive, llamaBackendArchive string, replaceBackends bool) error {
	return withSetupLock(ctx, root, func() error {
		return activatePackage(ctx, root, version, sdBackendArchive, llamaBackendArchive, replaceBackends, reviewedSteps)
	})
}

func activatePackage(ctx context.Context, root, version, sdBackendArchive, llamaBackendArchive string, replaceBackends bool, steps engineSteps) error {
	p, ok := server.FindPackage(version)
	if !ok {
		return fmt.Errorf("%w: %w: %q", ErrPackageRefused, server.ErrUnknownPackage, version)
	}
	selected, err := Read(SelectionPath(root))
	if err != nil {
		return fmt.Errorf("%w: activation needs a configured install; Setup's Lemonade model step configures the first one: %v", ErrSelection, err)
	}
	bound := selectedPackage(selected)
	if _, err := migrateLegacyLayout(root, bound); err != nil {
		return err
	}
	revision, err := installedModelRevision(root)
	if err != nil {
		return err
	}
	if revision != p.ModelRevision {
		return fmt.Errorf("%w: Lemonade %s was tested with model revision %s and the private model is %q; replace the model through Setup's Lemonade model step (lemonaded configure --replace-private-model) first",
			ErrPackageRefused, p.Version, p.ModelRevision, revision)
	}
	program, err := steps.verifyPackage(p)
	if err != nil {
		return err
	}
	binding := bindingFor(root, p, program)
	if bound.ID == p.ID {
		if err := steps.verifyBackend(binding.CacheDir, p); err != nil {
			return err
		}
		installed, err := steps.filesInstalled(binding)
		if err != nil {
			return err
		}
		if !installed {
			return fmt.Errorf("%w: Lemonade %s is selected and its engine files are not installed; %s", ErrPackageRefused, p.Version, engineFilesRemedy)
		}
		return nil
	}
	if err := CheckModelPath(root, p.ModelRevision); err != nil {
		return err
	}
	archives, err := archivesFor(p, sdBackendArchive, llamaBackendArchive)
	if err != nil {
		return err
	}
	verify := func(cache string) error { return steps.verifyBackend(cache, p) }
	if err := stageBackends(ctx, binding.CacheDir, archives, replaceBackends, func(string) error { return nil }); err != nil {
		return err
	}
	staged := map[string]bool{}
	for _, a := range archives {
		staged[a.a.manifest.Family] = true
	}
	for _, m := range []server.BackendManifest{p.SDBackend, p.LlamaBackend} {
		if staged[m.Family] {
			continue
		}
		if err := seedBackend(ctx, CacheDirFor(root, bound.Version), binding.CacheDir, m, bound); err != nil {
			return err
		}
	}
	if err := verify(binding.CacheDir); err != nil {
		return err
	}
	configDir := ConfigDirFor(root, p.Version)
	if configAbsent(configDir) {
		if err := writeModelConfig(root, configDir, p.Config); err != nil {
			return err
		}
	} else {
		if err := upgradeLegacyConfig(root, configDir, p); err != nil {
			return err
		}
		// A folder an earlier activation wrote may predate a model added
		// since; its registry follows the selection's models.
		registry, err := registryBytes(selectedModels(root))
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(configDir, "user_models.json"), registry); err != nil {
			return err
		}
	}
	if err := steps.installFiles(ctx, binding); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return Save(SelectionPath(root), selectionFor(p, selectedModels(root)))
}

// seedBackend stages m into toCache from the bound version's folder when the
// bound version's pairing names the same build and its folder proves: every
// required file and version.txt are copied with O_EXCL into a staging folder
// beside the target, which is published by rename. A folder already at the
// target is left for the caller's proof. The caller installs the engine
// files, which hashes the copy in full.
func seedBackend(ctx context.Context, fromCache, toCache string, m server.BackendManifest, bound server.Package) error {
	target := m.Dir(toCache)
	a := backendArchive{manifest: m}
	// A copy a killed activation left behind is this function's own; the
	// version it was for is not bound, and the caller holds the setup lock.
	if entries, err := os.ReadDir(filepath.Dir(target)); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && backendTempName(entry.Name(), a.stagePrefix()) {
				if err := os.RemoveAll(filepath.Join(filepath.Dir(target), entry.Name())); err != nil {
					return err
				}
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if !m.Equal(bound.SDBackend) && !m.Equal(bound.LlamaBackend) {
		return nil
	}
	source := m.Dir(fromCache)
	if _, err := os.Lstat(source); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := server.VerifyBackendDir(source, m); err != nil {
		return fmt.Errorf("%w: the bound version's %s/%s folder: %w", ErrSelection, m.Family, m.Variant, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), a.stagePrefix())
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	names := []string{"version.txt"}
	for _, f := range m.Required() {
		names = append(names, f.Name)
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := copyExclusive(filepath.Join(source, name), filepath.Join(stage, name)); err != nil {
			return err
		}
	}
	return os.Rename(stage, target)
}

func copyExclusive(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Close())
}

// RetirePackage removes what the provider kept for version: its cache with
// the paired backends, its config folder and its engine-file records. It
// refuses the selected version, and returns ErrHeld while a launch holds the
// version's files: the cache folder first moves to a sibling name, which
// Windows refuses while a file in it is open. It never touches the private
// model, the content store or the selection. The package folder is Setup's
// to remove.
func RetirePackage(ctx context.Context, root, version string) error {
	p, ok := server.FindPackage(version)
	if !ok {
		return fmt.Errorf("%w: %w: %q", ErrPackageRefused, server.ErrUnknownPackage, version)
	}
	return withSetupLock(ctx, root, func() error {
		selected, err := Read(SelectionPath(root))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && selected.Package == p.ID {
			return fmt.Errorf("%w: Lemonade %s is the selected version", ErrPackageRefused, p.Version)
		}
		cache := CacheDirFor(root, p.Version)
		aside := filepath.Join(filepath.Dir(cache), "."+p.Version+".retiring")
		if _, err := os.Lstat(cache); err == nil {
			if _, err := os.Lstat(aside); err == nil {
				return fmt.Errorf("%w: %s and %s both exist; remove %s, then retry", ErrSelection, cache, aside, aside)
			}
			if err := os.Rename(cache, aside); err != nil {
				return fmt.Errorf("%w: Lemonade %s is in use: %w", ErrHeld, p.Version, err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := programfiles.UnlockWithin(aside); err != nil {
			return err
		}
		for _, folder := range []string{aside, ConfigDirFor(root, p.Version), StateDirFor(root, p.Version)} {
			if err := os.RemoveAll(folder); err != nil {
				return err
			}
		}
		return nil
	})
}
