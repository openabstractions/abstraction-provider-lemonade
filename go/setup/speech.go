package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// SpeechFiles are the local copies of the reviewed speech artifacts Setup
// downloaded: the whisper.cpp and Kokoros backend archives and the model
// files (research/security-reviews/whisper.cpp-v1.8.4-win.md,
// kokoros-b17-win.md). Every path is absolute; an empty archive or model
// leaves that profile as it is.
type SpeechFiles struct {
	WhisperArchive, KokoroArchive          string
	WhisperModel                           string // ggml-large-v3-turbo.bin
	KokoroIndex, KokoroModel, KokoroVoices string // index.json, kokoro-v1.0.onnx, voices-v1.0.bin
}

// speechBackendsOf is p's speech pairing, the reviewed builds when the entry
// names none.
func speechBackendsOf(p server.Package) []server.BackendManifest {
	whisper, kokoro := p.WhisperBackend, p.KokoroBackend
	if whisper.Family == "" {
		whisper = server.WhisperCppVulkan
	}
	if kokoro.Family == "" {
		kokoro = server.KokorosB17
	}
	return []server.BackendManifest{whisper, kokoro}
}

// modelsDirFor is the private models folder every version shares.
func modelsDirFor(root string) string { return filepath.Join(root, "engine-models") }

// speechModelFiles pairs each pinned file of m with its local source.
func speechModelFiles(m server.SpeechModel, sources ...string) ([]weightPin, error) {
	if len(sources) != len(m.Files) {
		return nil, fmt.Errorf("%w: %s needs %d files", ErrSelection, m.Name, len(m.Files))
	}
	pins := make([]weightPin, len(m.Files))
	for i, f := range m.Files {
		if sources[i] == "" || !filepath.IsAbs(sources[i]) {
			return nil, fmt.Errorf("%w: %s: %s must be an absolute path", ErrSelection, m.Name, f.Name)
		}
		pins[i] = weightPin{source: sources[i], rel: f.Name, digest: f.SHA256}
	}
	return pins, nil
}

// StageSpeech stages the speech artifacts into the selected version's
// private folders: each backend archive into Lemonade's
// `<cache>/bin/<family>/<variant>` through the provider kit's archive install
// (INF-F6), each model into the Hugging Face cache layout of the models
// folder, moved by rename from its local file on that folder's volume
// (DownloadDir) and checked against its pinned SHA-256.
// Every tree is proven in full, given the read-only folder ACL and recorded
// as each launch reads it. A folder that proves is kept; one that does not
// is refused unless replace. Nothing is downloaded; Setup brings the files
// with the person's consent.
func StageSpeech(ctx context.Context, root string, files SpeechFiles, replace bool) error {
	if !filepath.IsAbs(root) {
		return ErrSelection
	}
	return withSetupLock(ctx, root, func() error { return stageSpeech(ctx, root, files, replace, backendFiles, reviewedSteps) })
}

// stageSpeech stages files into the selected version. On a state folder
// with no selection it binds the table's package, which must be installed
// and prove (speech runs inside Lemonade; the picture model and the image
// backend are not needed), stages the files, installs the package and
// backend folders' engine files and saves a speech-only selection last.
func stageSpeech(ctx context.Context, root string, files SpeechFiles, replace bool, v *enginefiles.Verifier, steps engineSteps) error {
	selected, err := readSelection(ctx, SelectionPath(root))
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return fmt.Errorf("%w: the Lemonade selection does not read: %v", ErrSelection, err)
	}
	var p server.Package
	var program string
	if fresh {
		if p, err = configurePackage(root, ""); err != nil {
			return err
		}
		if program, err = steps.verifyPackage(p); err != nil {
			return packageRequired(p, err)
		}
		if legacy, err := legacyLayout(root); err != nil || legacy {
			return errors.Join(fmt.Errorf("%w: the state folder holds an older Lemonade layout; %s", ErrSelection, engineFilesRemedy), err)
		}
	} else {
		p = selectedPackage(selected)
	}
	cacheDir, stateDir := CacheDirFor(root, p.Version), StateDirFor(root, p.Version)
	backends := speechBackendsOf(p)
	for i, archive := range []string{files.WhisperArchive, files.KokoroArchive} {
		if archive == "" {
			continue
		}
		a, ok := archiveFor(backends[i])
		if !ok {
			return fmt.Errorf("%w: no reviewed archive stages %s/%s", ErrSelection, backends[i].Family, backends[i].Variant)
		}
		if err := stageBackend(ctx, a, cacheDir, archive, nil, replace); err != nil {
			return err
		}
	}
	if files.WhisperModel != "" {
		pins, err := speechModelFiles(server.WhisperLargeV3TurboModel, files.WhisperModel)
		if err != nil {
			return err
		}
		if err := stageSpeechModel(ctx, v, root, stateDir, server.WhisperLargeV3TurboModel, pins, replace); err != nil {
			return err
		}
	}
	if files.KokoroIndex != "" || files.KokoroModel != "" || files.KokoroVoices != "" {
		pins, err := speechModelFiles(server.KokoroV1Model, files.KokoroIndex, files.KokoroModel, files.KokoroVoices)
		if err != nil {
			return err
		}
		if err := stageSpeechModel(ctx, v, root, stateDir, server.KokoroV1Model, pins, replace); err != nil {
			return err
		}
	}
	if !fresh {
		if selected.Image() {
			return verifyApprovedBackend(cacheDir, p)
		}
		return verifySpeechBackend(cacheDir, p)
	}
	if err := verifySpeechBackend(cacheDir, p); err != nil {
		return err
	}
	// The package and speech backend folders get the read-only folder ACL and
	// their records, as configure gives the image install (INF-F3).
	if err := steps.installFiles(ctx, bindingFor(root, p, program)); err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	return Save(SelectionPath(root), speechSelectionFor(p))
}

// packageRequired names exactly what speech needs when p's package does not
// prove: the reviewed Lemonade package in its folder.
func packageRequired(p server.Package, cause error) error {
	program, err := PackageProgramFor(p)
	if err != nil {
		program = "<the package folder>"
	}
	return fmt.Errorf("%w: speech runs inside the reviewed Lemonade package %s (%s, program SHA-256 %s); install it so that %s exists, or name the folder that holds %s with --dir: %w",
		ErrSelection, p.Version, p.Manifest.Name, p.ProgramSHA256, program, p.Version, cause)
}

// stageSpeechModel moves m's files by rename into a stage folder beside the
// model's cache folder, each checked against its pin there; a refusal moves
// each back to its source. It adds
// refs/main and replaces the folder with the stage, then installs the tree:
// hashed in full, read-only folders, a record under stateDir.
func stageSpeechModel(ctx context.Context, v *enginefiles.Verifier, root, stateDir string, m server.SpeechModel, pins []weightPin, replace bool) error {
	modelsDir := modelsDirFor(root)
	tree, err := m.Tree(modelsDir, stateDir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSelection, err)
	}
	if _, err := os.Lstat(tree.Root); err == nil {
		if err := v.Install(ctx, tree); err == nil {
			return nil
		} else if !replace {
			return fmt.Errorf("%w: the %s model folder does not prove; replace it with --replace-backend: %w", ErrSelection, m.Name, err)
		}
		if err := enginefiles.Unlock(tree.Root); err != nil {
			return err
		}
		if err := os.RemoveAll(tree.Root); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(modelsDir, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(modelsDir, "."+m.Folder+"-stage-")
	if err != nil {
		return err
	}
	var moved placements
	defer func() {
		moved.restore()
		_ = os.RemoveAll(stage)
	}()
	snapshot := filepath.Join(stage, "snapshots", m.Revision)
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		return err
	}
	for _, pin := range pins {
		if err := moved.place(ctx, pin, filepath.Join(snapshot, pin.rel), nil); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(stage, "refs"), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, "refs", "main"), m.RefsMain(), 0600); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(stage, tree.Root); err != nil {
		return err
	}
	if err := v.Install(ctx, tree); err != nil {
		//unchecked: the refused tree is this stage's own; removal is best effort before the refusal is reported
		_ = enginefiles.Unlock(tree.Root)
		for _, pin := range pins {
			//unchecked: as placements.restore, a download that cannot move back is downloaded again
			_ = os.Rename(filepath.Join(tree.Root, "snapshots", m.Revision, pin.rel), pin.source)
		}
		return errors.Join(fmt.Errorf("%w: %w", ErrSelection, err), os.RemoveAll(tree.Root))
	}
	return nil
}
