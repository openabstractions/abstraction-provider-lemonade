package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	cas "github.com/openabstractions/abstraction-cas/go"
	"github.com/openabstractions/abstraction-facade/go/programfiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// withSetupLock serializes provider-owned backend, model and selection state
// across processes. The CAS edit returns its unchanged value, so only its
// native lock file persists; a killed process releases that lock.
func withSetupLock(ctx context.Context, root string, work func() error) error {
	if !filepath.IsAbs(root) {
		return ErrSelection
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	return cas.ChangeLimitContext(ctx, filepath.Join(root, ".setup-transaction"), 1, func(cur []byte) ([]byte, error) {
		if err := work(); err != nil {
			return nil, err
		}
		return cur, nil
	})
}

// Replace names what configure may replace in an existing private setup.
type Replace struct {
	// Backends replaces a backend folder that does not prove against its
	// reviewed archive, and removes backend staging a killed configure left.
	Backends bool
	// Model copies the private model again over one that exists, and
	// removes model staging a killed configure left. A staged model that
	// verifies is kept without it.
	Model bool
}

// engineSteps are the reviewed-package checks and the engine-file install
// configure, activation and the probes run around their staging. Tests
// replace them; the reviewed package exists only in an installed setup.
type engineSteps struct {
	verifyPackage  func(p server.Package) (string, error)
	verifyBackend  func(cacheDir string, p server.Package) error
	installFiles   func(ctx context.Context, b server.Binding) error
	filesInstalled func(b server.Binding) (bool, error)
}

var reviewedSteps = engineSteps{
	verifyPackage:  VerifyPackage,
	verifyBackend:  verifyApprovedBackend,
	installFiles:   server.InstallFiles,
	filesInstalled: server.FilesInstalled,
}

// archivesFor pairs each named local archive with p's reviewed pin: the
// image backend first, then llama.cpp. An empty name stages nothing and
// leaves that backend folder as it is.
func archivesFor(p server.Package, sdBackendArchive, llamaBackendArchive string) ([]stagedArchive, error) {
	var archives []stagedArchive
	for _, named := range []struct {
		m    server.BackendManifest
		path string
	}{{p.SDBackend, sdBackendArchive}, {p.LlamaBackend, llamaBackendArchive}} {
		if named.path == "" {
			continue
		}
		a, ok := archiveFor(named.m)
		if !ok {
			return nil, fmt.Errorf("%w: no reviewed archive stages %s/%s %s", ErrSelection, named.m.Family, named.m.Variant, named.m.Version)
		}
		archives = append(archives, stagedArchive{a, named.path})
	}
	return archives, nil
}

// configurePackage is the package a configure run binds: the one named, the
// selection's, or the table's only entry. A named package other than the
// selected one is refused; `lemonaded package activate` moves the binding.
func configurePackage(root, version string) (server.Package, error) {
	selected, err := Read(SelectionPath(root))
	if err != nil && !os.IsNotExist(err) {
		return server.Package{}, err
	}
	if err == nil {
		p := selectedPackage(selected)
		if version != "" && version != p.Version {
			return server.Package{}, fmt.Errorf("%w: the selection binds Lemonade %s; Setup moves it to %s with lemonaded package activate", ErrSelection, p.Version, version)
		}
		return p, nil
	}
	if version == "" {
		if len(server.Packages) != 1 {
			return server.Package{}, fmt.Errorf("%w: name the Lemonade version with --package", ErrSelection)
		}
		return server.Packages[0], nil
	}
	p, ok := server.FindPackage(version)
	if !ok {
		return server.Package{}, fmt.Errorf("%w: %w: %q", ErrSelection, server.ErrUnknownPackage, version)
	}
	return p, nil
}

// ConfigureReviewed is the one guided transaction. Its final selection is
// published only after the reviewed app, backend and model have passed.
// version names the package to bind when nothing is selected yet; empty
// selects the bound one, or the table's only entry. llamaBackendArchive,
// when named, is the reviewed llama.cpp archive of that package, staged after
// the image backend; empty leaves the llama.cpp folder as it is.
func ConfigureReviewed(ctx context.Context, root, version, sdBackendArchive, llamaBackendArchive string, files BF16Files, replace Replace) error {
	if sdBackendArchive == "" {
		return fmt.Errorf("%w: the image backend archive", ErrSelection)
	}
	return withSetupLock(ctx, root, func() error {
		p, err := configurePackage(root, version)
		if err != nil {
			return err
		}
		archives, err := archivesFor(p, sdBackendArchive, llamaBackendArchive)
		if err != nil {
			return err
		}
		return configureReviewed(ctx, root, p, archives, approvedBF16Pins(files), replace, reviewedSteps)
	})
}

func configureReviewed(ctx context.Context, root string, p server.Package, archives []stagedArchive, pins []weightPin, replace Replace, steps engineSteps) error {
	if err := CheckModelPath(root, p.ModelRevision); err != nil {
		return err
	}
	program, err := steps.verifyPackage(p)
	if err != nil {
		return err
	}
	if _, err := migrateLegacyLayout(root, p); err != nil {
		return err
	}
	cache := CacheDirFor(root, p.Version)
	verify := func(cache string) error { return steps.verifyBackend(cache, p) }
	if err := stageBackends(ctx, cache, archives, replace.Backends, verify); err != nil {
		return err
	}
	if err := stageBF16(ctx, root, p, pins, nil, replace.Model); err != nil {
		return err
	}
	// The package and backend folders get the read-only folder ACL and
	// their metadata records; every launch checks both (INF-F3).
	if err := steps.installFiles(ctx, bindingFor(root, p, program)); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return Save(SelectionPath(root), selectionFor(p, selectedModels(root)))
}

// InstallEngineFiles is the configure step that installs only the engine
// files of the selected package (inference CONTRACT.md INF-F1..INF-F3,
// INF-F6): it moves a layout from before per-version folders under that
// package's version, stages each named backend archive through the provider
// kit, proves the image backend and every present backend folder, and gives
// the package and backend folders the read-only folder ACL and their
// records. It never reads, copies or replaces the private model and keeps
// the selection. An install whose launches refuse the folder ACL needs only
// this step.
func InstallEngineFiles(ctx context.Context, root, sdBackendArchive, llamaBackendArchive string, replaceBackends bool) error {
	return withSetupLock(ctx, root, func() error {
		selected, err := Read(SelectionPath(root))
		if err != nil {
			return fmt.Errorf("%w: the engine-files step needs a configured install; run lemonaded configure with the model files: %v", ErrSelection, err)
		}
		p := selectedPackage(selected)
		archives, err := archivesFor(p, sdBackendArchive, llamaBackendArchive)
		if err != nil {
			return err
		}
		return installEngineFiles(ctx, root, archives, replaceBackends, reviewedSteps)
	})
}

func installEngineFiles(ctx context.Context, root string, archives []stagedArchive, replaceBackends bool, steps engineSteps) error {
	selected, err := Read(SelectionPath(root))
	if err != nil {
		return fmt.Errorf("%w: the engine-files step needs a configured install; run lemonaded configure with the model files: %v", ErrSelection, err)
	}
	p := selectedPackage(selected)
	program, err := steps.verifyPackage(p)
	if err != nil {
		return err
	}
	if _, err := migrateLegacyLayout(root, p); err != nil {
		return err
	}
	cache := CacheDirFor(root, p.Version)
	verify := func(cache string) error { return steps.verifyBackend(cache, p) }
	if len(archives) == 0 {
		err = verify(cache)
	} else {
		err = stageBackends(ctx, cache, archives, replaceBackends, verify)
	}
	if err != nil {
		return err
	}
	if err := upgradeLegacyConfig(root, ConfigDirFor(root, p.Version), p); err != nil {
		return err
	}
	if err := steps.installFiles(ctx, bindingFor(root, p, program)); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return nil
}

// UnlockEngineFiles gives the package folder of every known version and
// every backend folder under root back to the account, so an uninstaller or
// a person can remove them (inference CONTRACT.md INF-F3). Launches refuse
// them until the engine-files step installs them again. The private model
// and the selection stay. It returns the folders it looked in; a missing one
// has nothing to unlock.
func UnlockEngineFiles(ctx context.Context, root string) ([]string, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrSelection
	}
	var folders []string
	for _, p := range server.Packages {
		program, err := PackageProgramFor(p)
		if err != nil {
			return nil, err
		}
		folders = append(folders, filepath.Dir(program))
	}
	folders = append(folders, filepath.Join(root, "engine-cache"))
	unlock := func() error {
		for _, folder := range folders {
			if err := programfiles.UnlockWithin(folder); err != nil {
				return err
			}
		}
		return nil
	}
	// No install has no setup lock to take, and taking it would create the
	// provider root.
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return folders, unlock()
	} else if err != nil {
		return nil, err
	}
	return folders, withSetupLock(ctx, root, unlock)
}

// ProbeReviewedSetup holds the same transaction lock across all four checks.
// An absent selection and model is false; a partial state is an error. A
// layout from before per-version folders is incomplete, which brings back
// the engine-files step that moves it.
func ProbeReviewedSetup(ctx context.Context, root string) (bool, error) {
	return probeReviewedSetup(ctx, root, approvedBF16Pins(BF16Files{}), server.ImageModels, reviewedSteps)
}

// probeReviewedSetup checks the starter model against pins and every other
// selected model's files against catalogue.
func probeReviewedSetup(ctx context.Context, root string, pins []weightPin, catalogue []server.ImageModel, steps engineSteps) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !filepath.IsAbs(root) {
		return false, ErrSelection
	}
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var complete bool
	err := withSetupLock(ctx, root, func() error {
		selected, selectErr := Read(SelectionPath(root))
		if selectErr != nil && !os.IsNotExist(selectErr) {
			return selectErr
		}
		if selectErr == nil {
			if legacy, err := legacyLayout(root); err != nil || legacy {
				return err
			}
		}
		configDir, locked, err := probeConfig(root)
		if err != nil {
			return err
		}
		model, err := probeBF16(ctx, root, configDir, locked, pins)
		if err != nil {
			return err
		}
		if selectErr == nil && !selected.Image() {
			// A speech-only selection: the image setup has not run, and a
			// picture model beside it is an interrupted configure.
			if model {
				return ErrSelection
			}
			return nil
		}
		if selectErr != nil {
			if model {
				return ErrSelection
			}
			// Nothing selected and no model: any backend folder or staging
			// residue is an interrupted configure.
			entries, err := os.ReadDir(filepath.Join(root, "engine-cache"))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			if len(entries) != 0 {
				return ErrSelection
			}
			return nil
		}
		if !model {
			return ErrSelection
		}
		if err := verifyAddedModels(ctx, root, selected.Models, catalogue); err != nil {
			return err
		}
		p := selectedPackage(selected)
		program, err := steps.verifyPackage(p)
		if err != nil {
			return err
		}
		if err := steps.verifyBackend(CacheDirFor(root, p.Version), p); err != nil {
			return err
		}
		// Without the read-only folder ACL every launch refuses (INF-F3); an
		// incomplete answer brings the guided configure step back.
		installed, err := steps.filesInstalled(bindingFor(root, p, program))
		if err != nil {
			return err
		}
		complete = installed
		return nil
	})
	return complete, err
}
