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
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
	registration "github.com/openabstractions/abstraction-facade/go/abstraction/facade/registration"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-provider-lemonade/go/provider"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	"github.com/openabstractions/abstraction-provider-lemonade/go/setup"
	resource "github.com/openabstractions/abstraction-resource/go/client"
)

func main() {
	if declarationInvocation(os.Args[1:]) {
		if err := declarationCommand(os.Args[1:], os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
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
	words := setup.SetupCLIWords()
	if args[1] == words.Configure {
		configure(args[2:])
		return
	}
	if args[1] == words.Package {
		root, err := providerRoot()
		if err != nil {
			fail(err)
		}
		if err := packageCommand(context.Background(), root, args[2:], os.Stdout, os.Stderr); err != nil {
			fail(err)
		}
		return
	}
	if args[1] == words.Model {
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
	if len(args) == 3 && args[2] == "--"+words.JSON {
		switch args[1] {
		case words.Info:
			if err := writeSetupInfo(os.Stdout); err != nil {
				fail(err)
			}
			return
		case "setup-probe":
			if err := writeSetupProbe(os.Stdout); err != nil {
				fail(err)
			}
			return
		case words.Verify:
			if err := writeVerifiedSetup(os.Stdout); err != nil {
				fail(err)
			}
			return
		}
	}
	advanced(args[1:])
}

func declarationCommand(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 2 && args[0] == "declare" && (args[1] == "--help" || args[1] == "-h") {
		_, err := io.WriteString(output, "Usage: declare\nRead one typed RegistrationSpec from standard input (at most 262144 bytes), refresh the producer's exact reviewed engine/recipe facts, and write the generated declaration to standard output. Reads no setup, engine or model state and starts no provider.\n")
		return err
	}
	if len(args) != 1 || args[0] != "declare" {
		return errors.New("declare reads one typed RegistrationSpec from standard input and takes no options")
	}
	return writeDeclaration(input, output)
}

func declarationInvocation(args []string) bool {
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(args[i], "=")
		if name == "--state" || name == "--dir" {
			if !inline {
				i++
			}
			continue
		}
		return args[i] == "declare"
	}
	return false
}

// writeDeclaration refreshes exact producer facts in an explicitly supplied
// native declaration. It runs before root options, bootstrap or setup access.
func writeDeclaration(input io.Reader, output io.Writer) (err error) {
	defer func() {
		if value := recover(); value != nil {
			if refusal, ok := value.(*registration.Refusal); ok {
				err = refusal
				return
			}
			panic(value)
		}
	}()
	const limit = 262144
	raw, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("registration input exceeds 262144 bytes")
	}
	spec, err := registration.DecodeRegistrationSpec(raw)
	if err != nil {
		return err
	}
	if len(spec.Providers) == 0 || len(spec.Servers)+len(spec.Remotes)+len(spec.Applications) != 0 {
		return errors.New("declare requires native provider facets")
	}
	program, err := os.Executable()
	if err != nil {
		return err
	}
	spec.Program = program
	for i := range spec.Providers {
		facet := &spec.Providers[i]
		facet.Engines, facet.Recipes = nil, nil
		server.DeclareEngineFacts(facet)
	}
	encoded := registration.EncodeRegistrationSpec(spec)
	n, err := output.Write(encoded)
	if err == nil && n != len(encoded) {
		err = io.ErrShortWrite
	}
	return err
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
	info := setup.SetupInfoReply{Version: 2, ProviderRoot: root, ProviderID: setup.ProviderID, KnownPackages: []setup.KnownPackageInfo{}, StagedModels: []string{},
		DownloadDir: setup.DownloadDir(root)}
	for _, p := range server.Packages {
		info.KnownPackages = append(info.KnownPackages, setup.KnownPackageInfo{Version: p.Version, ArchiveSHA256: p.ArchiveSHA256, ProgramSHA256: p.ProgramSHA256, BackendArchiveSHA256: p.BackendArchiveSHA256, ModelRevision: p.ModelRevision})
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
		// A folder configured before installed.json existed gets one from its
		// selection and file sizes; the runtime still hashes every file.
		if err := setup.WriteMissingInstalled(context.Background(), root); err != nil {
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
		info.SelectionPresent, info.ActivePackage = true, &setup.ActivePackageInfo{Version: p.Version, Program: program}
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
	words := setup.SetupCLIWords()
	flags := flag.NewFlagSet("package "+args[0], flag.ContinueOnError)
	flags.SetOutput(usage)
	flags.Usage = func() {
		//unchecked: usage text; the parse error reports the outcome
		io.WriteString(usage, packageUsage)
	}
	var version, packageRoot, sd, llama string
	var asJSON, replace bool
	flags.StringVar(&version, words.Version, "", "the Lemonade version, as Setup's catalog names it")
	switch args[0] {
	case words.Check:
		flags.StringVar(&packageRoot, words.Root, "", "the version's install folder")
		flags.BoolVar(&asJSON, words.JSON, false, "print the result as JSON")
	case words.Activate:
		flags.StringVar(&sd, words.VulkanBackend, "", "reviewed local sd.cpp Vulkan archive of the version's pairing")
		flags.StringVar(&llama, "llamacpp-backend-archive", "", "reviewed local llama.cpp publisher archive of the version's pairing, beside "+llamaRuntimeArchive)
		flags.BoolVar(&replace, words.ReplaceBackend, false, "replace a backend folder of the version that does not prove")
	case words.Retire:
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
	if version == "" || flags.NArg() != 0 || args[0] == words.Check && (packageRoot == "" || !asJSON) {
		flags.Usage()
		return setup.ErrSelection
	}
	encoder := json.NewEncoder(out)
	switch args[0] {
	case words.Check:
		p, err := setup.CheckPackage(ctx, version, packageRoot)
		if err != nil {
			return err
		}
		return encoder.Encode(setup.PackageCheckReply{Version: 1, OK: true, BackendArchiveSHA256: p.BackendArchiveSHA256, ModelRevision: p.ModelRevision})
	case words.Activate:
		if err := setup.ActivatePackage(ctx, root, version, sd, llama, replace); err != nil {
			return err
		}
		return encoder.Encode(setup.PackageActivationReply{Version: 1, OK: true, ActiveVersion: version})
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
	words := setup.SetupCLIWords()
	if args[0] != words.Add {
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
	flags.StringVar(&id, words.ModelID, "", "the declared model to add")
	flags.Func(words.ModelFile, "ROLE=FILE: the verified download of one role", func(value string) error {
		role, path, ok := strings.Cut(value, "=")
		if !ok || !slices.Contains(words.ModelRoles[:], role) || !filepath.IsAbs(path) || files[role] != "" {
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
		return setup.SetupCLIWords().HeldExit
	case errors.Is(err, setup.ErrPackageRefused):
		return setup.SetupCLIWords().RefusedExit
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
	if verified {
		if err := setup.WriteInstalled(root); err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(setup.VerificationReply{Version: 1, Verified: verified})
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
	return installedEndpointFor(value, bootstrap.Endpoint)
}

func installedEndpointFor(value string, endpointFor func(string) (string, error)) (string, error) {
	selected, err := endpointFor("lemonade-image")
	if err != nil {
		return "", err
	}
	if value == "lemonade-image" || value == listen.Endpoint("lemonade-image") || value == selected {
		return selected, nil
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
	words := setup.SetupCLIWords()
	flags := flag.NewFlagSet(words.Configure, flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() {
		//unchecked: usage text; the parse error reports the outcome
		io.WriteString(out, configureUsage)
		flags.PrintDefaults()
	}
	var r configureRequest
	var activate bool
	flags.BoolVar(&activate, words.Reviewed, false, "activate the installed reviewed Lemonade package")
	flags.BoolVar(&r.engineFilesOnly, words.EngineFiles, false, "install only the engine files of an existing install; the private model stays as it is")
	flags.BoolVar(&r.replace.Backends, words.ReplaceBackend, false, "explicitly replace a backend folder that does not match its reviewed archive")
	flags.BoolVar(&r.replace.Model, words.ReplaceModel, false, "explicitly replace existing private model configuration")
	flags.StringVar(&r.main, words.Main, "", "approved local BF16 DiT file")
	flags.StringVar(&r.encoder, words.Encoder, "", "approved local text encoder file")
	flags.StringVar(&r.vae, words.VAE, "", "approved local VAE file")
	flags.StringVar(&r.sd, words.VulkanBackend, "", "reviewed local sd.cpp Vulkan archive")
	flags.StringVar(&r.llama, "llamacpp-backend-archive", "", "reviewed local llama.cpp b10825 Vulkan publisher archive beside "+llamaRuntimeArchive+" (optional)")
	flags.StringVar(&r.pkg, words.Package, "", "installed Lemonade version to select when nothing is selected yet (optional)")
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
func captureRuntimeEndpoint(cfg *provider.Config) {
	cfg.Server.RuntimeEndpoint, _ = bootstrap.DevelopmentEndpoint(cfg.Server.RuntimeEndpoint)
}

func serve(cfg provider.Config, state string) {
	if err := serveProvider(cfg, state); err != nil {
		fail(err)
	}
}

func serveProvider(cfg provider.Config, state string) error {
	captureRuntimeEndpoint(&cfg)
	report := func(err error) { fmt.Fprintln(os.Stderr, "lemonaded:", err) }
	cfg.OnError = report
	cfg.Leases = resource.Discover()
	handler, closeDiagnostics := resolvedDiagnostics("lemonaded")
	defer closeDiagnostics()
	gpu, err := newGPUGuard(state, os.Stderr, report, slog.New(handler))
	if err != nil {
		return err
	}
	cfg.GPU = gpu
	instance, err := provider.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gpuDone := make(chan struct{})
	go func() { defer close(gpuDone); gpu.Run(ctx) }()
	defer func() { stop(); <-gpuDone; gpu.Flush() }()
	err = instance.Serve(ctx)
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(exitCode(err))
}
