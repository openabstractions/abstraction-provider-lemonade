// lemonaded serves the reviewed portable Lemonade app as a native OA image
// provider after guided activation, and as a transcription and speech
// provider once the reviewed speech files are staged. It never uses an
// installed personal server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-provider-lemonade/go/provider"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	"github.com/openabstractions/abstraction-provider-lemonade/go/setup"
	resource "github.com/openabstractions/abstraction-resource/go/client"
)

func main() {
	rest, err := applyRootOptions(os.Args[1:])
	if err != nil {
		fail(err)
	}
	args := append([]string{os.Args[0]}, rest...)
	if len(args) == 1 {
		serveInstalled("")
		return
	}
	if len(args) == 4 && args[1] == "serve" && args[2] == "--endpoint" {
		serveInstalled(args[3])
		return
	}
	if args[1] == "configure" {
		configure(args[2:])
		return
	}
	if args[1] == "package" {
		root, err := providerRoot()
		if err != nil {
			fail(err)
		}
		if err := packageCommand(context.Background(), root, args[2:], os.Stdout, os.Stderr); err != nil {
			fail(err)
		}
		return
	}
	if args[1] == "model" {
		root, err := providerRoot()
		if err != nil {
			fail(err)
		}
		if err := modelCommand(context.Background(), root, args[2:], os.Stdout, os.Stderr); err != nil {
			fail(err)
		}
		return
	}
	if len(args) == 3 && args[1] == "engine" && args[2] == "unlock" {
		if err := unlockEngine(os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	if len(args) == 3 && args[2] == "--json" {
		switch args[1] {
		case "setup-info":
			if err := writeSetupInfo(os.Stdout); err != nil {
				fail(err)
			}
			return
		case "setup-probe":
			if err := writeSetupProbe(os.Stdout); err != nil {
				fail(err)
			}
			return
		case "verify-setup":
			if err := writeVerifiedSetup(os.Stdout); err != nil {
				fail(err)
			}
			return
		}
	}
	advanced(args[1:])
}

// stateRoot is the provider state folder --state names; empty selects
// setup.DefaultRoot.
var stateRoot string

// rootOptionsUsage documents the options every command takes.
const rootOptionsUsage = `Every command also takes:
  --state DIR  the provider state folder (selection, engine cache, config,
               models and engine-file records) in place of the per-user
               %LOCALAPPDATA%\OpenAbstractions\providers\lemonade
  --dir DIR    the folder that holds each installed Lemonade version as
               <version>\<archive root>\lemond.exe, in place of
               %LOCALAPPDATA%\OpenAbstractions\apps\lemonade
A test stages an isolated install under both and removes them afterwards.
`

// applyRootOptions removes --state DIR and --dir DIR (or --state=DIR and
// --dir=DIR) from args, wherever they stand, and applies them: --state roots
// providerRoot and --dir every package lookup. Each must be absolute and
// given once. It returns the remaining arguments.
func applyRootOptions(args []string) ([]string, error) {
	values := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--state" && name != "--dir" {
			rest = append(rest, args[i])
			continue
		}
		if !inline {
			if i+1 == len(args) {
				return nil, fmt.Errorf("%w: %s needs a folder", setup.ErrSelection, name)
			}
			i++
			value = args[i]
		}
		if _, twice := values[name]; twice {
			return nil, fmt.Errorf("%w: %s is given twice", setup.ErrSelection, name)
		}
		if !filepath.IsAbs(value) {
			return nil, fmt.Errorf("%w: %s must name an absolute folder", setup.ErrSelection, name)
		}
		values[name] = filepath.Clean(value)
	}
	// Each folder is local and private to this account, created so when
	// absent.
	for _, name := range []string{"--state", "--dir"} {
		if value, ok := values[name]; ok {
			if err := setup.EnsurePrivateFolder(value); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if dir, ok := values["--dir"]; ok {
		if err := setup.UsePackagesDir(dir); err != nil {
			return nil, err
		}
	}
	stateRoot = values["--state"]
	return rest, nil
}

// providerRoot is the provider state folder: --state's, or the per-user one.
func providerRoot() (string, error) {
	if stateRoot != "" {
		return stateRoot, nil
	}
	return setup.DefaultRoot()
}

// setupInfo is the reply of setup-info --json, version 2: the provider's
// private root and identity, the package the selection binds, and every
// package this build can run, in table order.
type setupInfo struct {
	Version          int            `json:"version"`
	ProviderRoot     string         `json:"provider_root"`
	ProviderID       string         `json:"provider_id"`
	SelectionPresent bool           `json:"selection_present"`
	ActivePackage    *activePackage `json:"active_package,omitempty"`
	KnownPackages    []knownPackage `json:"known_packages"`
	// StagedModels are the picture models the selection serves, the starter
	// model first; empty before setup. The provider is the authority for
	// this list; Setup and the runtime read it here.
	StagedModels []string `json:"staged_models"`
	// DownloadDir is where Setup writes model downloads: on the models
	// folder's volume, so configure and model add publish each by rename.
	DownloadDir string `json:"download_dir"`
}

type activePackage struct {
	Version string `json:"version"`
	Program string `json:"program"`
}

type knownPackage struct {
	Version              string `json:"version"`
	ArchiveSHA256        string `json:"archive_sha256"`
	ProgramSHA256        string `json:"program_sha256"`
	BackendArchiveSHA256 string `json:"backend_archive_sha256"`
	ModelRevision        string `json:"model_revision"`
}

// The installed setup bridge is a fixed, bounded JSON contract. It never
// launches the foreign engine or downloads model bytes. A selection file
// that does not name a known package makes it fail.
func writeSetupInfo(out io.Writer) error {
	root, err := providerRoot()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(root) || len(root) > 4096 {
		return setup.ErrSelection
	}
	info := setupInfo{Version: 2, ProviderRoot: root, ProviderID: setup.ProviderID, KnownPackages: []knownPackage{}, StagedModels: []string{},
		DownloadDir: setup.DownloadDir(root)}
	for _, p := range server.Packages {
		info.KnownPackages = append(info.KnownPackages, knownPackage{p.Version, p.ArchiveSHA256, p.ProgramSHA256, p.BackendArchiveSHA256, p.ModelRevision})
	}
	selection, err := os.Lstat(setup.SelectionPath(root))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if !selection.Mode().IsRegular() {
			return setup.ErrSelection
		}
		selected, err := setup.Read(setup.SelectionPath(root))
		if err != nil {
			return err
		}
		// A speech-only selection leaves Setup's picture step to run.
		if !selected.Image() {
			return json.NewEncoder(out).Encode(info)
		}
		p, _ := server.FindPackageID(selected.Package)
		program, err := setup.PackageProgramFor(p)
		if err != nil {
			return err
		}
		if len(program) > 4096 {
			return setup.ErrSelection
		}
		info.SelectionPresent, info.ActivePackage = true, &activePackage{Version: p.Version, Program: program}
		info.StagedModels = append(info.StagedModels, selected.Models...)
	}
	return json.NewEncoder(out).Encode(info)
}

const packageUsage = `Usage:
  lemonaded package check --version V --root DIR --json
  lemonaded package activate --version V [--vulkan-backend-archive ZIP]
                             [--llamacpp-backend-archive ZIP] [--replace-backend]
  lemonaded package retire --version V

Setup moves the provider from one installed Lemonade version to the next with
these verbs; nothing else calls them. check proves the tree Setup staged at
DIR against version V's reviewed manifest and prints V's tested pairing; it
changes no ACL, record or selection. activate installs V's engine files,
stages V's paired backends into V's own cache (from the named archives, or
from the bound version's folder when the pairing names the same build),
writes V's config folder, checks the private model's revision and selects V
last; it is idempotent. retire removes V's cache, config folder and engine-file
records; it refuses the selected version and exits 3 while a launch holds V.
A refusal exits 2; any other failure exits 1.

` + rootOptionsUsage

// packageCommand runs one package verb and writes its success object to
// out.
func packageCommand(ctx context.Context, root string, args []string, out, usage io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		//unchecked: usage text; the exit code reports the outcome
		io.WriteString(out, packageUsage)
		return nil
	}
	flags := flag.NewFlagSet("package "+args[0], flag.ContinueOnError)
	flags.SetOutput(usage)
	flags.Usage = func() {
		//unchecked: usage text; the parse error reports the outcome
		io.WriteString(usage, packageUsage)
	}
	var version, packageRoot, sd, llama string
	var asJSON, replace bool
	flags.StringVar(&version, "version", "", "the Lemonade version, as Setup's catalog names it")
	switch args[0] {
	case "check":
		flags.StringVar(&packageRoot, "root", "", "the version's install folder")
		flags.BoolVar(&asJSON, "json", false, "print the result as JSON")
	case "activate":
		flags.StringVar(&sd, "vulkan-backend-archive", "", "reviewed local sd.cpp Vulkan archive of the version's pairing")
		flags.StringVar(&llama, "llamacpp-backend-archive", "", "reviewed local llama.cpp archive of the version's pairing")
		flags.BoolVar(&replace, "replace-backend", false, "replace a backend folder of the version that does not prove")
	case "retire":
	default:
		//unchecked: usage text; the returned error reports the outcome
		io.WriteString(usage, packageUsage)
		return fmt.Errorf("%w: unknown package verb %q", setup.ErrSelection, args[0])
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", setup.ErrSelection, err)
	}
	if version == "" || flags.NArg() != 0 || args[0] == "check" && (packageRoot == "" || !asJSON) {
		flags.Usage()
		return setup.ErrSelection
	}
	encoder := json.NewEncoder(out)
	switch args[0] {
	case "check":
		p, err := setup.CheckPackage(ctx, version, packageRoot)
		if err != nil {
			return err
		}
		return encoder.Encode(struct {
			Version              int    `json:"version"`
			OK                   bool   `json:"ok"`
			BackendArchiveSHA256 string `json:"backend_archive_sha256"`
			ModelRevision        string `json:"model_revision"`
		}{1, true, p.BackendArchiveSHA256, p.ModelRevision})
	case "activate":
		if err := setup.ActivatePackage(ctx, root, version, sd, llama, replace); err != nil {
			return err
		}
		return encoder.Encode(struct {
			Version       int    `json:"version"`
			OK            bool   `json:"ok"`
			ActiveVersion string `json:"active_version"`
		}{1, true, version})
	default:
		if err := setup.RetirePackage(ctx, root, version); err != nil {
			return err
		}
		return encoder.Encode(struct {
			Version int  `json:"version"`
			OK      bool `json:"ok"`
		}{1, true})
	}
}

const modelUsage = `Usage:
  lemonaded model add --id MODEL [--file ROLE=FILE]...

Setup adds one declared picture model other than the starter model to a
configured provider with this verb. Each --file names the verified download
of one role (main, text_encoder or vae) as an absolute path on the volume of
the provider's models folder, such as setup-info's download_dir; the
download moves into the models folder by rename and is never copied. A role
whose file is already staged, by this model or by another with the same
SHA-256, needs no --file. The model's files are verified before any is
published, a refusal moves each download back, and the registry entry and
the selection follow last. A failure exits 1.
`

// stageModel is the provider's staging step; tests replace it.
var stageModel = setup.StageModel

// modelCommand runs one model verb and writes its success object to out.
func modelCommand(ctx context.Context, root string, args []string, out, usage io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		//unchecked: usage text; the exit code reports the outcome
		io.WriteString(out, modelUsage)
		return nil
	}
	if args[0] != "add" {
		//unchecked: usage text; the returned error reports the outcome
		io.WriteString(usage, modelUsage)
		return fmt.Errorf("%w: unknown model verb %q", setup.ErrSelection, args[0])
	}
	flags := flag.NewFlagSet("model add", flag.ContinueOnError)
	flags.SetOutput(usage)
	flags.Usage = func() {
		//unchecked: usage text; the parse error reports the outcome
		io.WriteString(usage, modelUsage)
	}
	var id string
	files := map[string]string{}
	var fileErr error
	flags.StringVar(&id, "id", "", "the declared model to add")
	flags.Func("file", "ROLE=FILE: the verified download of one role", func(value string) error {
		role, path, ok := strings.Cut(value, "=")
		if !ok || role != "main" && role != "text_encoder" && role != "vae" || !filepath.IsAbs(path) || files[role] != "" {
			fileErr = fmt.Errorf("%w: --file %q", setup.ErrSelection, value)
			return fileErr
		}
		files[role] = path
		return nil
	})
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if fileErr != nil {
			return fileErr
		}
		return fmt.Errorf("%w: %v", setup.ErrSelection, err)
	}
	if id == "" || flags.NArg() != 0 {
		flags.Usage()
		return setup.ErrSelection
	}
	if err := stageModel(ctx, root, id, files, nil); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version int    `json:"version"`
		OK      bool   `json:"ok"`
		Model   string `json:"model"`
	}{1, true, id})
}

// exitCode maps a failure to the package verbs' exit codes: 3 while a launch
// holds the version, 2 for a definitive refusal, 1 otherwise.
func exitCode(err error) int {
	switch {
	case errors.Is(err, setup.ErrHeld):
		return 3
	case errors.Is(err, setup.ErrPackageRefused):
		return 2
	}
	return 1
}

// ProbeApprovedBF16 distinguishes absent state from exact completion. A
// partial or altered snapshot produces a nonzero exit and no success JSON.
func writeSetupProbe(out io.Writer) error {
	root, err := providerRoot()
	if err != nil {
		return err
	}
	complete, err := setup.ProbeApprovedBF16(context.Background(), root)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version  int  `json:"version"`
		Complete bool `json:"complete"`
	}{1, complete})
}

// The deeper final check is deliberately separate from cheap setup-info:
// it hashes the approved 16 GB snapshot and verifies the installed app.
func writeVerifiedSetup(out io.Writer) error {
	root, err := providerRoot()
	if err != nil {
		return err
	}
	verified, err := setup.ProbeReviewedSetup(context.Background(), root)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Version  int  `json:"version"`
		Verified bool `json:"verified"`
	}{1, verified})
}

func serveInstalled(endpoint string) {
	if err := checkServeEndpoint(endpoint); err != nil {
		fail(err)
	}
	root, err := providerRoot()
	if err != nil {
		fail(err)
	}
	selected, err := setup.Read(setup.SelectionPath(root))
	if err != nil {
		fail(fmt.Errorf("choose Lemonade and a model in Abstraction Panel: %w", err))
	}
	self, err := os.Executable()
	if err != nil {
		fail(err)
	}
	cfg, err := setup.Resolve(selected, self, root)
	if err != nil {
		fail(err)
	}
	if endpoint != "" {
		bound, err := installedEndpoint(endpoint)
		if err != nil {
			fail(err)
		}
		cfg.Endpoint = bound
	}
	serve(cfg, root)
}

// checkServeEndpoint refuses serve --endpoint with --state: an isolated
// install serves at the endpoint its state folder derives (setup.Resolve),
// and the installed declaration's endpoint stays the per-user install's.
func checkServeEndpoint(endpoint string) error {
	if stateRoot != "" && endpoint != "" {
		return fmt.Errorf("%w: serve --endpoint names the installed provider's endpoint; with --state the endpoint derives from the state folder", setup.ErrSelection)
	}
	return nil
}

func installedEndpoint(value string) (string, error) {
	if value == "lemonade-image" || value == listen.Endpoint("lemonade-image") {
		return listen.Endpoint("lemonade-image"), nil
	}
	const namespaced = `\\.\pipe\openabstractions-provider-`
	if len(value) == len(namespaced)+32 && strings.HasPrefix(value, namespaced) {
		for _, ch := range value[len(namespaced):] {
			if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
				return "", setup.ErrSelection
			}
		}
		return value, nil
	}
	return "", setup.ErrSelection
}

const configureUsage = `Usage:
  lemonaded configure --activate-reviewed-package --vulkan-backend-archive ZIP
                      --bf16-main FILE --bf16-encoder FILE --bf16-vae FILE
                      [--package VERSION] [--llamacpp-backend-archive ZIP]
                      [--replace-backend] [--replace-private-model]
  lemonaded configure --activate-reviewed-package --engine-files-only
                      [--vulkan-backend-archive ZIP] [--llamacpp-backend-archive ZIP]
                      [--replace-backend]
  lemonaded configure --activate-reviewed-package --speech-only
                      [--whisper-backend-archive ZIP] [--kokoro-backend-archive TGZ]
                      [--whisper-model FILE]
                      [--kokoro-index FILE --kokoro-model FILE --kokoro-voices FILE]
                      [--replace-backend]

The first form stages the reviewed backends and the private model and saves
the selection. Each model file moves by rename from its download, which
must be on the models folder's volume (setup-info's download_dir); a staged backend or model that verifies is kept. --package
names the installed Lemonade version to select when nothing is selected yet;
lemonaded package activate moves an existing selection. The second form
installs only the engine files of the selected version: it moves private
folders from before per-version folders, stages each named backend archive,
proves the package and backend folders and makes them read-only. It never
reads or copies the private model. The third form stages the reviewed
speech files into the selected version: whisper.cpp v1.8.4 and Kokoros b17
from their archives, Whisper-Large-v3-Turbo and kokoro-v1 from their files,
each checked against its pinned SHA-256 and made read-only. On a state
folder with no selection it needs only the installed reviewed Lemonade
package: it stages the speech files, installs the package's engine files
and saves a speech-only selection; the picture model and the image backend
are not needed, and the first form adds them later. koko.exe is
GPL-3.0-or-later; Setup downloads it from its publisher with consent.

Options:
` + rootOptionsUsage + `
`

// configureRequest is one parsed configure command line.
type configureRequest struct {
	engineFilesOnly    bool
	pkg                string
	main, encoder, vae string
	sd, llama          string
	replace            setup.Replace
	speechOnly         bool
	speech             setup.SpeechFiles
}

// parseConfigure reads configure's flags; -h writes the usage to out and
// returns flag.ErrHelp.
func parseConfigure(args []string, out io.Writer) (configureRequest, error) {
	flags := flag.NewFlagSet("configure", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		//unchecked: usage text; the parse error reports the outcome
		io.WriteString(out, configureUsage)
		flags.PrintDefaults()
	}
	var r configureRequest
	var activate bool
	flags.BoolVar(&activate, "activate-reviewed-package", false, "activate the installed reviewed Lemonade package")
	flags.BoolVar(&r.engineFilesOnly, "engine-files-only", false, "install only the engine files of an existing install; the private model stays as it is")
	flags.BoolVar(&r.replace.Backends, "replace-backend", false, "explicitly replace a backend folder that does not match its reviewed archive")
	flags.BoolVar(&r.replace.Model, "replace-private-model", false, "explicitly replace existing private model configuration")
	flags.StringVar(&r.main, "bf16-main", "", "approved local BF16 DiT file")
	flags.StringVar(&r.encoder, "bf16-encoder", "", "approved local text encoder file")
	flags.StringVar(&r.vae, "bf16-vae", "", "approved local VAE file")
	flags.StringVar(&r.sd, "vulkan-backend-archive", "", "reviewed local sd.cpp Vulkan archive")
	flags.StringVar(&r.llama, "llamacpp-backend-archive", "", "reviewed local llama.cpp b10825 Vulkan archive (optional)")
	flags.StringVar(&r.pkg, "package", "", "installed Lemonade version to select when nothing is selected yet (optional)")
	flags.BoolVar(&r.speechOnly, "speech-only", false, "stage only the reviewed speech files into the selected version")
	flags.StringVar(&r.speech.WhisperArchive, "whisper-backend-archive", "", "reviewed local whisper.cpp v1.8.4 archive of the version's pairing")
	flags.StringVar(&r.speech.KokoroArchive, "kokoro-backend-archive", "", "reviewed local Kokoros b17 archive (GPL-3.0-or-later)")
	flags.StringVar(&r.speech.WhisperModel, "whisper-model", "", "local ggml-large-v3-turbo.bin")
	flags.StringVar(&r.speech.KokoroIndex, "kokoro-index", "", "local kokoro-v1 index.json")
	flags.StringVar(&r.speech.KokoroModel, "kokoro-model", "", "local kokoro-v1.0.onnx")
	flags.StringVar(&r.speech.KokoroVoices, "kokoro-voices", "", "local voices-v1.0.bin")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return configureRequest{}, err
		}
		return configureRequest{}, fmt.Errorf("%w: %v", setup.ErrSelection, err)
	}
	speechGiven := r.speech != setup.SpeechFiles{}
	switch {
	case !activate || flags.NArg() != 0:
		return configureRequest{}, setup.ErrSelection
	case r.speechOnly && (r.engineFilesOnly || r.main != "" || r.encoder != "" || r.vae != "" || r.sd != "" || r.llama != "" || r.replace.Model || r.pkg != ""):
		return configureRequest{}, fmt.Errorf("%w: --speech-only takes only speech flags and --replace-backend", setup.ErrSelection)
	case r.speechOnly && !speechGiven, !r.speechOnly && speechGiven:
		return configureRequest{}, fmt.Errorf("%w: speech flags go with --speech-only", setup.ErrSelection)
	case r.speechOnly:
		return r, nil
	case r.engineFilesOnly && (r.main != "" || r.encoder != "" || r.vae != "" || r.replace.Model || r.pkg != ""):
		return configureRequest{}, fmt.Errorf("%w: --engine-files-only takes no model or package flag", setup.ErrSelection)
	case !r.engineFilesOnly && (r.main == "" || r.encoder == "" || r.vae == "" || r.sd == ""):
		return configureRequest{}, setup.ErrSelection
	}
	return r, nil
}

func configure(args []string) {
	root, err := providerRoot()
	if err != nil {
		fail(err)
	}
	r, err := parseConfigure(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fail(err)
	}
	if !filepath.IsAbs(root) {
		fail(setup.ErrSelection)
	}
	if r.speechOnly {
		err = setup.StageSpeech(context.Background(), root, r.speech, r.replace.Backends)
	} else if r.engineFilesOnly {
		err = setup.InstallEngineFiles(context.Background(), root, r.sd, r.llama, r.replace.Backends)
	} else {
		err = setup.ConfigureReviewed(context.Background(), root, r.pkg, r.sd, r.llama, setup.BF16Files{Main: r.main, Encoder: r.encoder, VAE: r.vae}, r.replace)
	}
	if err != nil {
		fail(err)
	}
}

// unlockEngine gives the package folder of every known version and every
// backend folder back to this account so they can be removed; uninstallers
// run it before the provider goes.
func unlockEngine(out io.Writer) error {
	root, err := providerRoot()
	if err != nil {
		return err
	}
	folders, err := setup.UnlockEngineFiles(context.Background(), root)
	if err != nil {
		return err
	}
	//unchecked: CLI output; the exit code reports the outcome
	fmt.Fprintf(out, "%s belong to this account again; Lemonade refuses them until lemonaded configure --activate-reviewed-package --engine-files-only installs them again\n",
		strings.Join(folders, " and "))
	return nil
}

func advanced(args []string) {
	var cfg provider.Config
	var runtimes, models string
	flags := flag.NewFlagSet("advanced", flag.ExitOnError)
	flags.StringVar(&cfg.Name, "name", "lemonade", "native provider name")
	flags.StringVar(&cfg.Endpoint, "endpoint", "", "designated native endpoint")
	flags.StringVar(&runtimes, "runtime-programs", "", "comma-separated absolute OA runtime executables")
	flags.StringVar(&cfg.StoreRoot, "store-root", "", "private provider content store")
	flags.StringVar(&cfg.RecordPath, "record-path", "", "provider content record path")
	flags.StringVar(&cfg.Server.Program, "lemonade-program", "", "explicit Lemonade executable")
	flags.StringVar(&cfg.Server.CacheDir, "cache-dir", "", "private Lemonade cache directory")
	flags.StringVar(&cfg.Server.ConfigDir, "config-dir", "", "private Lemonade configuration directory")
	flags.IntVar(&cfg.Server.Port, "port", 0, "private loopback port")
	flags.StringVar(&models, "models", "", "comma-separated approved Lemonade model IDs")
	flags.DurationVar(&cfg.Server.Ready, "ready-timeout", 30*time.Second, "Lemonade readiness deadline")
	flags.Int64Var(&cfg.Amount, "card-bytes", 0, "card lease admission estimate in bytes (default: the declared models' pinned sizes)")
	if err := flags.Parse(args); err != nil {
		fail(err)
	}
	if runtimes != "" {
		cfg.RuntimePrograms = strings.Split(runtimes, ",")
	}
	if models != "" {
		cfg.Server.Models = strings.Split(models, ",")
	}
	serve(cfg, "")
}

// serve runs the provider with its card lease taken through the resource
// service at its default endpoint (ABSTRACTION_RESOURCE_ENDPOINT, or the
// installed runtime's) and its GPU fault handling, whose quarantine is kept
// in state.
func serve(cfg provider.Config, state string) {
	report := func(err error) { fmt.Fprintln(os.Stderr, "lemonaded:", err) }
	cfg.OnError = report
	cfg.Leases = resource.Discover()
	gpu, err := newGPUGuard(state, os.Stderr, report)
	if err != nil {
		fail(err)
	}
	cfg.GPU = gpu
	instance, err := provider.New(cfg)
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go gpu.Run(ctx)
	err = instance.Serve(ctx)
	gpu.Flush()
	if err != nil && ctx.Err() == nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(exitCode(err))
}
