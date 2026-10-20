package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
)

// InstallFiles hashes the bound package folder and each present backend
// folder under `<CacheDir>/bin`, gives every folder of them the read-only ACL
// and records their metadata under StateDir (inference CONTRACT.md
// INF-F1..INF-F3). Guided setup runs it once the package and backend are in
// place; every launch bound to the same package then proves and holds the
// same trees.
func InstallFiles(ctx context.Context, b Binding) error {
	if !filepath.IsAbs(b.Program) || !filepath.IsAbs(b.CacheDir) || !filepath.IsAbs(b.StateDir) {
		return ErrConfiguration
	}
	return installFiles(ctx, enginefiles.NewVerifier(enginefiles.Options{}), b)
}

func installFiles(ctx context.Context, v *enginefiles.Verifier, b Binding) error {
	t, err := b.Package.tree(filepath.Dir(b.Program), b.StateDir)
	if err != nil {
		return err
	}
	if err := v.Install(ctx, t); err != nil {
		return fmt.Errorf("%w: %w", ErrPackageManifest, err)
	}
	trees, err := backendTrees(b.CacheDir, b.SDBackend, b.LlamaBackend, false, b.StateDir, b.speechBackends()...)
	if err != nil {
		return err
	}
	for _, bt := range trees {
		if err := v.Install(ctx, bt.tree); err != nil {
			return bt.m.refusal(err)
		}
	}
	return nil
}

// speechBackends are the speech backends a binding pairs, the reviewed ones
// when it names none. A present speech folder is admitted and proven like
// the image and chat backends.
func (b Binding) speechBackends() []BackendManifest {
	whisper, kokoro := b.WhisperBackend, b.KokoroBackend
	if whisper.Family == "" {
		whisper = WhisperCppVulkan
	}
	if kokoro.Family == "" {
		kokoro = KokorosB17
	}
	return []BackendManifest{whisper, kokoro}
}

// FilesInstalled reports whether the bound package folder and every present
// backend folder have the read-only folder ACL InstallFiles sets. Setup's
// probe reports an incomplete setup without it, so the guided step that runs
// configure appears again.
func FilesInstalled(b Binding) (bool, error) {
	roots := []string{filepath.Dir(b.Program)}
	trees, err := backendTrees(b.CacheDir, b.SDBackend, b.LlamaBackend, false, "", b.speechBackends()...)
	if err != nil {
		return false, err
	}
	for _, bt := range trees {
		roots = append(roots, bt.tree.Root)
	}
	for _, root := range roots {
		ok, err := enginefiles.Installed(root)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// remedy names the step that installs the engine files again after a folder
// ACL refusal; it leaves the private model as it is.
func remedy(err error) error {
	if errors.Is(err, enginefiles.ErrFolderACL) {
		return fmt.Errorf("%w: run lemonaded configure --activate-reviewed-package --engine-files-only, or the Lemonade setup step in Abstraction Panel", err)
	}
	return err
}

// heldTrees names what one launch proved and holds: the package folder, the
// llama.cpp folder, and each speech backend and model folder
// (whisper-backend, kokoro-backend, whisper-model, kokoro-model).
type heldTrees struct {
	pkg, llama bool
	speech     map[string]bool
}

// holdFiles proves the package folder, every present backend folder and
// every present speech model folder and holds their files with read sharing
// only (INF-F1..INF-F4). The returned release closes them; Start calls it
// once the contained tree's exit is confirmed. A package refusal wraps
// ErrConfiguration and ErrPackageManifest; a backend refusal wraps
// ErrUnavailable and ErrBackendManifest; a model refusal wraps
// ErrConfiguration and ErrWeightFormat.
func (s *Server) holdFiles(ctx context.Context, cfg Config) (func() error, heldTrees, error) {
	var held []*enginefiles.Hold
	release := func() error {
		var errs []error
		for _, h := range held {
			errs = append(errs, h.Release())
		}
		return errors.Join(errs...)
	}
	got := heldTrees{speech: map[string]bool{}}
	if cfg.Package.Name != "" {
		t, err := cfg.Package.tree(filepath.Dir(cfg.Program), cfg.StateDir)
		if err != nil {
			return nil, heldTrees{}, fmt.Errorf("%w: %w", ErrConfiguration, err)
		}
		h, err := s.files.Launch(ctx, t)
		if err != nil {
			return nil, heldTrees{}, fmt.Errorf("%w: %w: %w", ErrConfiguration, ErrPackageManifest, remedy(err))
		}
		held = append(held, h)
		got.pkg = true
	}
	trees, err := backendTrees(cfg.CacheDir, cfg.SDBackend, cfg.LlamaBackend, cfg.RequireVulkanBackend, cfg.StateDir, cfg.speechBackends()...)
	if err != nil {
		release()
		return nil, heldTrees{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	for _, b := range trees {
		h, err := s.files.Launch(ctx, b.tree)
		if err != nil {
			release()
			return nil, heldTrees{}, fmt.Errorf("%w: %w", ErrUnavailable, b.m.refusal(remedy(err)))
		}
		held = append(held, h)
		switch b.m.Family {
		case cfg.LlamaBackend.Family:
			got.llama = true
		case cfg.WhisperBackend.Family:
			got.speech["whisper-backend"] = true
		case cfg.KokoroBackend.Family:
			got.speech["kokoro-backend"] = true
		}
	}
	models, err := speechModelTrees(cfg, cfg.StateDir)
	if err != nil {
		release()
		return nil, heldTrees{}, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	for _, m := range models {
		h, err := s.files.Launch(ctx, m.tree)
		if err != nil {
			release()
			return nil, heldTrees{}, fmt.Errorf("%w: %w: %s: %w", ErrConfiguration, ErrWeightFormat, m.tree.Name, remedy(err))
		}
		held = append(held, h)
		got.speech[m.what] = true
	}
	return release, got, nil
}
