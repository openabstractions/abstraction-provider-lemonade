package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	cas "github.com/openabstractions/abstraction-cas/go"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

const ApprovedModel = "user.LocalApprovedKlein"

type BF16Files struct {
	Main    string
	Encoder string
	VAE     string
}

type weightPin struct {
	source string
	rel    string
	digest string
}

// ProbeApprovedBF16 is read-only reconciliation for an interrupted setup.
// Completely absent state is false,nil; partial or mismatched state is a
// typed error and must not be retried or overwritten automatically.
func ProbeApprovedBF16(ctx context.Context, root string) (bool, error) {
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
		configDir, locked, err := probeConfig(root)
		if err != nil {
			return err
		}
		complete, err = probeBF16(ctx, root, configDir, locked, approvedBF16Pins(BF16Files{}))
		return err
	})
	return complete, err
}

// probeConfig names the config folder a model probe reads: the root
// engine-config of a layout from before per-version folders, otherwise the
// folder of the selected package, or of the newest table entry when nothing
// is selected yet.
func probeConfig(root string) (string, func(string) ([]byte, error), error) {
	legacy, err := legacyLayout(root)
	if err != nil {
		return "", nil, err
	}
	if legacy {
		return filepath.Join(root, "engine-config"), server.LockedConfig, nil
	}
	p := server.Packages[len(server.Packages)-1]
	if selected, err := Read(SelectionPath(root)); err == nil {
		p = selectedPackage(selected)
	} else if !os.IsNotExist(err) {
		return "", nil, err
	}
	return ConfigDirFor(root, p.Version), p.Config, nil
}

// approvedModel is the starter model's entry of the provider's catalogue.
func approvedModel() server.ImageModel {
	m, _ := server.FindImageModel(ApprovedModel)
	return m
}

// approvedBF16Pins are the catalogue's Klein files (main, text encoder, VAE)
// under the snapshot's split_files folder, sourced from files.
func approvedBF16Pins(files BF16Files) []weightPin {
	sources := map[string]string{"main": files.Main, "text_encoder": files.Encoder, "vae": files.VAE}
	var pins []weightPin
	for _, f := range approvedModel().Files {
		rel := filepath.FromSlash(strings.TrimPrefix(f.Path, "split_files/"))
		pins = append(pins, weightPin{source: sources[f.Role], rel: rel, digest: f.SHA256})
	}
	return pins
}

// checkPinFormats admits safetensors and GGUF weights only; sd.cpp's pickle
// loaders for .ckpt, .pt and .pth stay unused.
func checkPinFormats(pins []weightPin) error {
	for _, pin := range pins {
		if !server.WeightFileAllowed(pin.rel) {
			return fmt.Errorf("%w: %w: %s", ErrSelection, server.ErrWeightFormat, filepath.Base(pin.rel))
		}
	}
	return nil
}

// probeBF16 checks the private model and the config folder of one package
// version: configDir holds that version's config.json, whose locked bytes
// locked returns, and the model registry.
func probeBF16(ctx context.Context, root, configDir string, locked func(string) ([]byte, error), pins []weightPin) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(configDir) {
		return false, ErrSelection
	}
	if err := checkPinFormats(pins); err != nil {
		return false, err
	}
	residue, err := ownedModelStaging(root, configDir)
	if err != nil {
		return false, err
	}
	if len(residue) != 0 {
		return false, fmt.Errorf("%w: incomplete private model staging", ErrSelection)
	}
	modelRoot := filepath.Join(root, "engine-models", modelRepositoryFolder)
	snapshot := filepath.Join(modelRoot, "snapshots", approvedRevision, "split_files")
	refs := filepath.Join(modelRoot, "refs", "main")
	configPath, registryPath := filepath.Join(configDir, "config.json"), filepath.Join(configDir, "user_models.json")
	paths := []string{snapshot, refs, configPath, registryPath}
	present := 0
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			present++
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	if present == 0 {
		return false, nil
	}
	if present != len(paths) {
		return false, fmt.Errorf("%w: partial private model setup", ErrSelection)
	}
	revision, err := os.ReadFile(refs)
	if err != nil || string(revision) != approvedRevision {
		return false, fmt.Errorf("%w: private model revision", ErrSelection)
	}
	wantConfig, wantRegistry, err := privateConfigBytes(root, locked)
	if err != nil {
		return false, err
	}
	legacyConfig, err := legacyConfigBytes(root)
	if err != nil {
		return false, err
	}
	// The pre-lock config.json is complete setup; Resolve upgrades it under
	// the setup lock before the provider serves.
	gotConfig, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(gotConfig, wantConfig) && !bytes.Equal(gotConfig, legacyConfig) {
		return false, fmt.Errorf("%w: private model configuration", ErrSelection)
	}
	gotRegistry, err := os.ReadFile(registryPath)
	if err != nil || !bytes.Equal(gotRegistry, wantRegistry) {
		return false, fmt.Errorf("%w: private model configuration", ErrSelection)
	}
	for _, pin := range pins {
		if err := checkDigestCtx(ctx, filepath.Join(snapshot, pin.rel), pin.digest, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// StageApprovedBF16 accepts three already-authorized local downloads on the
// volume of the provider's models folder (DownloadDir) and moves them into
// it by rename. Verification honors cancellation; existing private setup
// needs explicit replacement intent. Progress counts bytes verified.
// The model registry and config of p go into p's config folder.
func StageApprovedBF16(ctx context.Context, root string, p server.Package, files BF16Files, progress func(done, total int64) error, replace bool) error {
	return withSetupLock(ctx, root, func() error {
		return stageBF16(ctx, root, p, approvedBF16Pins(files), progress, replace)
	})
}

// modelRepositoryFolder is the Hugging Face cache folder of the private model.
const modelRepositoryFolder = "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b"

// installedModelRevision is the revision refs/main of the private model
// names, or "" when no model is staged.
func installedModelRevision(root string) (string, error) {
	data, err := cas.ReadLimit(filepath.Join(root, "engine-models", modelRepositoryFolder, "refs", "main"), 64)
	if err != nil || data == nil {
		return "", err
	}
	return string(data), nil
}

// stageBF16 moves the private model's downloads into place, verifies them
// and writes p's config folder.
func stageBF16(ctx context.Context, root string, p server.Package, pins []weightPin, progress func(done, total int64) error, replace bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(root) {
		return ErrSelection
	}
	if err := checkPinFormats(pins); err != nil {
		return err
	}
	if err := CheckModelPath(root, approvedRevision); err != nil {
		return err
	}
	if p.ModelRevision != approvedRevision {
		return fmt.Errorf("%w: Lemonade %s was tested with model revision %s, and setup stages %s", ErrSelection, p.Version, p.ModelRevision, approvedRevision)
	}
	configDir := ConfigDirFor(root, p.Version)
	residue, err := ownedModelStaging(root, configDir)
	if err != nil {
		return err
	}
	if len(residue) != 0 && !replace {
		return fmt.Errorf("%w: incomplete private model staging requires --replace-private-model", ErrSelection)
	}
	if replace {
		for _, path := range residue {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	modelRoot := filepath.Join(root, "engine-models", modelRepositoryFolder)
	snapshot := filepath.Join(modelRoot, "snapshots", approvedRevision, "split_files")
	if !replace {
		// A re-run over a staged model that verifies keeps it, as the
		// backend stage keeps a verified backend: configure then reaches
		// InstallFiles without copying the model again.
		if ready, err := probeBF16(ctx, root, configDir, p.Config, pins); err == nil && ready {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A verified model shared with another version needs only this
		// version's config folder.
		if verified, err := modelVerifies(ctx, root, pins); err == nil && verified && configAbsent(configDir) {
			return writeModelConfig(root, configDir, p.Config)
		}
		for _, path := range []string{snapshot, filepath.Join(configDir, "config.json"), filepath.Join(configDir, "user_models.json")} {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("%w: private model setup already exists and does not verify; replace it with --replace-private-model, or install only the engine files with --engine-files-only", ErrSelection)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	var sourceBytes int64
	for _, pin := range pins {
		if !filepath.IsAbs(pin.source) || pin.source == "" {
			return fmt.Errorf("%w: missing approved weight", ErrSelection)
		}
		info, err := os.Stat(pin.source)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%w: approved weight source", ErrSelection)
		}
		sourceBytes += info.Size()
	}
	var done int64
	account := func(n int64, total int64) error {
		done += n
		if err := ctx.Err(); err != nil {
			return err
		}
		if progress != nil {
			return progress(done, total)
		}
		return nil
	}
	_, existingErr := os.Stat(snapshot)
	newSnapshot := os.IsNotExist(existingErr) || replace
	if existingErr != nil && !os.IsNotExist(existingErr) {
		return existingErr
	}
	total := sourceBytes
	if err := os.MkdirAll(filepath.Dir(snapshot), 0700); err != nil {
		return err
	}
	var stage string
	if newSnapshot {
		var err error
		stage, err = os.MkdirTemp(filepath.Dir(snapshot), ".stage-")
		if err != nil {
			return err
		}
		// Each download moves into the stage by rename and is verified
		// there; a refusal moves every one back before the stage goes.
		var moved placements
		defer func() {
			moved.restore()
			_ = os.RemoveAll(stage)
		}()
		for _, pin := range pins {
			dest := filepath.Join(stage, pin.rel)
			if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
				return err
			}
			if err := moved.place(ctx, pin, dest, func(n int64) error { return account(n, total) }); err != nil {
				return err
			}
		}
	} else {
		for _, pin := range pins {
			if err := checkDigestCtx(ctx, filepath.Join(snapshot, pin.rel), pin.digest, func(n int64) error { return account(n, total) }); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if newSnapshot {
		backup := snapshot + ".previous"
		old := existingErr == nil
		if old {
			if _, err := os.Lstat(backup); err == nil {
				return fmt.Errorf("%w: prior model backup exists", ErrSelection)
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := retryPublicationRename(ctx, func() error {
				return os.Rename(snapshot, backup)
			}); err != nil {
				return err
			}
		}
		if err := retryPublicationRename(ctx, func() error {
			return os.Rename(stage, snapshot)
		}); err != nil {
			if old {
				_ = os.Rename(backup, snapshot)
			}
			return err
		}
		if old {
			_ = os.RemoveAll(backup)
		}
	}
	if err := os.MkdirAll(filepath.Join(modelRoot, "refs"), 0700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(modelRoot, "refs", "main"), []byte(approvedRevision)); err != nil {
		return err
	}
	return writeModelConfig(root, configDir, p.Config)
}

// writeModelConfig writes one version's locked config.json and the model
// registry into its config folder.
func writeModelConfig(root, configDir string, locked func(string) ([]byte, error)) error {
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return err
	}
	config, registry, err := privateConfigBytes(root, locked)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(configDir, "config.json"), config); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(configDir, "user_models.json"), registry)
}

// configAbsent reports a config folder that holds neither config.json nor
// the model registry.
func configAbsent(configDir string) bool {
	for _, name := range []string{"config.json", "user_models.json"} {
		if _, err := os.Lstat(filepath.Join(configDir, name)); !os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// modelVerifies reports a staged private model of the approved revision
// whose weights match their pins, whatever config folder names it.
func modelVerifies(ctx context.Context, root string, pins []weightPin) (bool, error) {
	if revision, err := installedModelRevision(root); err != nil || revision != approvedRevision {
		return false, err
	}
	snapshot := filepath.Join(root, "engine-models", modelRepositoryFolder, "snapshots", approvedRevision, "split_files")
	for _, pin := range pins {
		if err := checkDigestCtx(ctx, filepath.Join(snapshot, pin.rel), pin.digest, nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ownedModelStaging lists only this adapter's temporary names under its
// private directories and one version's config folder. A verified
// controller lock is required around retry.
func ownedModelStaging(root, configDir string) ([]string, error) {
	modelRoot := filepath.Join(root, "engine-models", modelRepositoryFolder)
	checks := []struct{ dir, prefix string }{
		{filepath.Join(modelRoot, "snapshots"), ".stage-"},
		{filepath.Join(modelRoot, "refs"), ".write-"},
		{configDir, ".write-"},
	}
	var paths []string
	for _, check := range checks {
		entries, err := os.ReadDir(check.dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), check.prefix) {
				paths = append(paths, filepath.Join(check.dir, entry.Name()))
			}
		}
	}
	return paths, nil
}

// privateConfigBytes returns the locked config.json the server compares
// before each launch of a version whose config function is locked, and the
// model registry.
func privateConfigBytes(root string, locked func(string) ([]byte, error)) ([]byte, []byte, error) {
	if locked == nil {
		return nil, nil, ErrSelection
	}
	config, err := locked(filepath.Join(root, "engine-models"))
	if err != nil {
		return nil, nil, err
	}
	registry, err := registryBytes(selectedModels(root))
	if err != nil {
		return nil, nil, err
	}
	return config, registry, nil
}

// registryBytes is Lemonade's user_models.json for the staged models: each
// model's catalogue entry under its registry name. The starter model alone
// encodes exactly as earlier releases staged it.
func registryBytes(models []string) ([]byte, error) {
	entries := make(map[string]any, len(models))
	for _, id := range models {
		m, ok := server.FindImageModel(id)
		if !ok {
			return nil, fmt.Errorf("%w: model %q is not declared", ErrSelection, id)
		}
		entries[m.RegistryName()] = m.RegistryEntry()
	}
	return json.Marshal(entries)
}

// pinnedModelInventory describes the selected picture models and declared
// speech models in their receipt order. The receipt writer checks presence.
func pinnedModelInventory(pictures []string) []InstalledModel {
	models := []InstalledModel{}
	for _, id := range pictures {
		m, ok := server.FindImageModel(id)
		if !ok || len(m.Files) == 0 {
			continue
		}
		entry := InstalledModel{Model: m.ID}
		for _, file := range m.Files {
			rel := path.Join("engine-models", filepath.ToSlash(file.CachePath()))
			entry.Files = append(entry.Files, InstalledFile{Path: rel, Size: file.Bytes, SHA256: file.SHA256})
		}
		models = append(models, entry)
	}
	for _, m := range []server.SpeechModel{server.WhisperLargeV3TurboModel, server.KokoroV1Model} {
		entry := InstalledModel{Model: m.Name}
		for i, rel := range m.Paths() {
			file := m.Files[i]
			entry.Files = append(entry.Files, InstalledFile{Path: path.Join("engine-models", rel), Size: file.Size, SHA256: file.SHA256})
		}
		models = append(models, entry)
	}
	return models
}

// legacyConfigBytes is the config.json setup wrote before the lock. It left
// offline, executable fetching, broadcast and backend arguments at Lemonade's
// defaults.
func legacyConfigBytes(root string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"models_dir": filepath.Join(root, "engine-models"), "auto_check_model_updates": false,
		"config_version": 2, "sdcpp": map[string]any{"backend": "vulkan"},
	})
}

// upgradeLegacyConfig replaces exactly the pre-lock config.json in p's config
// folder with p's locked bytes, so an existing private model need not be
// copied again. Any other content stays for the server's byte comparison to
// refuse.
func upgradeLegacyConfig(root, configDir string, p server.Package) error {
	path := filepath.Join(configDir, "config.json")
	got, err := cas.ReadLimit(path, 64<<10)
	if err != nil || got == nil {
		return err
	}
	legacy, err := legacyConfigBytes(root)
	if err != nil || !bytes.Equal(got, legacy) {
		return err
	}
	locked, _, err := privateConfigBytes(root, p.Config)
	if err != nil {
		return err
	}
	return writeAtomic(path, locked)
}

// engineFilesRemedy names the step that installs the engine files of an
// existing setup again; it moves a layout from before per-version folders.
const engineFilesRemedy = "run lemonaded configure --activate-reviewed-package --engine-files-only, or the Lemonade setup step in Abstraction Panel"

// legacyLayout reports a provider root from before per-version folders:
// Lemonade's backends directly under engine-cache\bin, or the config and
// model registry directly in engine-config.
func legacyLayout(root string) (bool, error) {
	for _, path := range []string{
		filepath.Join(root, "engine-cache", "bin"),
		filepath.Join(root, "engine-config", "config.json"),
		filepath.Join(root, "engine-config", "user_models.json"),
	} {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

// migrateLegacyLayout moves a layout from before per-version folders under
// p's version, the package the existing selection names:
// engine-cache\bin to engine-cache\<version>\bin and engine-config's
// config.json and user_models.json to engine-config\<version>. The engine-file
// records are bound to each tree's path, so the legacy ones are removed and
// the caller installs the engine files again. The private model is not read.
// It reports whether it moved anything; a second run moves nothing. The
// caller holds the setup lock.
func migrateLegacyLayout(root string, p server.Package) (bool, error) {
	legacy, err := legacyLayout(root)
	if err != nil || !legacy {
		return false, err
	}
	moves := [][2]string{
		{filepath.Join(root, "engine-cache", "bin"), filepath.Join(CacheDirFor(root, p.Version), "bin")},
		{filepath.Join(root, "engine-config", "config.json"), filepath.Join(ConfigDirFor(root, p.Version), "config.json")},
		{filepath.Join(root, "engine-config", "user_models.json"), filepath.Join(ConfigDirFor(root, p.Version), "user_models.json")},
	}
	for _, move := range moves {
		if _, err := os.Lstat(move[0]); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, err
		}
		if _, err := os.Lstat(move[1]); err == nil {
			return false, fmt.Errorf("%w: both %s and %s exist; remove the one you do not need, then %s", ErrSelection, move[0], move[1], engineFilesRemedy)
		} else if !os.IsNotExist(err) {
			return false, err
		}
		if err := os.MkdirAll(filepath.Dir(move[1]), 0700); err != nil {
			return false, err
		}
		if err := os.Rename(move[0], move[1]); err != nil {
			return false, fmt.Errorf("%w: moving %s to %s: %w", ErrSelection, move[0], move[1], err)
		}
	}
	entries, err := os.ReadDir(EngineFilesDir(root))
	if err != nil && !os.IsNotExist(err) {
		return true, err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".files") {
			if err := os.Remove(filepath.Join(EngineFilesDir(root), entry.Name())); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func checkDigest(path, expected string) error {
	return checkDigestCtx(context.Background(), path, expected, nil)
}

func checkDigestCtx(ctx context.Context, path, expected string, progress func(int64) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: approved weight: %v", ErrSelection, err)
	}
	defer f.Close()
	h := sha256.New()
	if err := transfer(ctx, h, f, progress); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return fmt.Errorf("%w: approved weight digest mismatch: %s", ErrSelection, filepath.Base(path))
	}
	return nil
}

func transfer(ctx context.Context, dest io.Writer, source io.Reader, progress func(int64) error) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			written, err := dest.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
			if progress != nil {
				if err := progress(int64(n)); err != nil {
					return err
				}
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func writeAtomic(path string, data []byte) error {
	return writeAtomicWithRetry(path, data, nil)
}

func writeAtomicWithRetry(path string, data []byte, retry func(func() error) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	publish := func() error { return os.Rename(tmp.Name(), path) }
	if retry != nil {
		return retry(publish)
	}
	return publish()
}
