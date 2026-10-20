package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-provider-lemonade/go/provider"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	"github.com/openabstractions/abstraction-provider-lemonade/go/setup"
)

func TestCaptureRuntimeEndpoint(t *testing.T) {
	t.Setenv("ABSTRACTION_RUNTIME_ENDPOINT", "selected-private")
	cfg := provider.Config{}
	captureRuntimeEndpoint(&cfg)
	if cfg.Server.RuntimeEndpoint != "selected-private" {
		t.Fatal(cfg.Server.RuntimeEndpoint)
	}
	t.Setenv("ABSTRACTION_RUNTIME_ENDPOINT", "later-ambient")
	if cfg.Server.RuntimeEndpoint != "selected-private" {
		t.Fatal("capture changed")
	}
	cfg.Server.RuntimeEndpoint = "explicit-private"
	captureRuntimeEndpoint(&cfg)
	if cfg.Server.RuntimeEndpoint != "explicit-private" {
		t.Fatal("explicit selection lost")
	}
	t.Setenv("ABSTRACTION_RUNTIME_ENDPOINT", "")
	cfg.Server.RuntimeEndpoint = ""
	captureRuntimeEndpoint(&cfg)
	if cfg.Server.RuntimeEndpoint != "" {
		t.Fatal("installed default changed")
	}
}

// standInCache isolates provider data, caches and package paths.
func standInCache(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
}

func TestInstalledSetupBridgeIsReadOnlyAndUsesDeclarationIdentity(t *testing.T) {
	standInCache(t)
	root, err := setup.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeSetupInfo(&out); err != nil {
		t.Fatal(err)
	}
	var info setup.SetupInfoReply
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Version != 2 || info.ProviderRoot != root || info.ProviderID != setup.ProviderID || info.SelectionPresent || info.ActivePackage != nil ||
		len(info.KnownPackages) != len(server.Packages) {
		t.Fatalf("invalid setup info: %+v", info)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only info created state: %v", err)
	}
	out.Reset()
	if err := writeSetupProbe(&out); err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Version  int  `json:"version"`
		Complete bool `json:"complete"`
	}
	if err := json.Unmarshal(out.Bytes(), &probe); err != nil {
		t.Fatal(err)
	}
	if probe.Version != 1 || probe.Complete {
		t.Fatalf("invalid empty probe: %+v", probe)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only probe created state: %v", err)
	}
	out.Reset()
	if err := writeVerifiedSetup(&out); err != nil {
		t.Fatal(err)
	}
	var verified struct {
		Version  int  `json:"version"`
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(out.Bytes(), &verified); err != nil {
		t.Fatal(err)
	}
	if verified.Version != 1 || verified.Verified {
		t.Fatalf("invalid empty verification: %+v", verified)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read-only verification created state: %v", err)
	}
}

// setup-info reply version 2 names the selected package's version and
// program, and every known package in table order with its pairing;
// package_program is gone.
func TestSetupInfoNamesTheActivePackageAndEveryKnownPackage(t *testing.T) {
	standInCache(t)
	local := os.Getenv("LOCALAPPDATA")
	saved := server.Packages
	second := saved[0]
	second.ID, second.Version = "lemonade-embeddable-v2026.40.0-windows-amd64", "v2026.40.0"
	second.ArchiveSHA256 = strings.Repeat("c", 64)
	server.Packages = []server.Package{saved[0], second}
	t.Cleanup(func() { server.Packages = saved })
	root, err := setup.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := setup.Save(setup.SelectionPath(root), setup.Selection{Version: setup.SelectionVersion, Package: second.ID, Activated: true, Models: []string{setup.ApprovedModel}}); err != nil {
		t.Fatal(err)
	}
	// A folder configured before installed.json existed gets one here.
	if err := os.Remove(setup.InstalledPath(root)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeSetupInfo(&out); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(setup.InstalledPath(root)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("setup-info wrote no installed.json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "provider_root", "provider_id", "selection_present", "active_package", "known_packages", "staged_models", "download_dir"} {
		if raw[field] == nil {
			t.Fatalf("setup-info lacks %s: %s", field, out.String())
		}
	}
	if raw["package_program"] != nil || len(raw) != 8 {
		t.Fatalf("setup-info carries other fields: %s", out.String())
	}
	var info setup.SetupInfoReply
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.DownloadDir != setup.DownloadDir(info.ProviderRoot) || filepath.Dir(info.DownloadDir) != info.ProviderRoot {
		t.Fatalf("setup-info names download folder %q beside no models folder", info.DownloadDir)
	}
	program := filepath.Join(local, "OpenAbstractions", "apps", "lemonade", "v2026.40.0", saved[0].Manifest.Name, "lemond.exe")
	if info.Version != 2 || !info.SelectionPresent || info.ActivePackage == nil || info.ActivePackage.Version != "v2026.40.0" || info.ActivePackage.Program != program {
		t.Fatalf("active package: %+v", info.ActivePackage)
	}
	want := []setup.KnownPackageInfo{
		{Version: saved[0].Version, ArchiveSHA256: saved[0].ArchiveSHA256, ProgramSHA256: saved[0].ProgramSHA256, BackendArchiveSHA256: saved[0].BackendArchiveSHA256, ModelRevision: saved[0].ModelRevision},
		{Version: "v2026.40.0", ArchiveSHA256: strings.Repeat("c", 64), ProgramSHA256: second.ProgramSHA256, BackendArchiveSHA256: second.BackendArchiveSHA256, ModelRevision: second.ModelRevision},
	}
	if len(info.KnownPackages) != 2 || info.KnownPackages[0] != want[0] || info.KnownPackages[1] != want[1] {
		t.Fatalf("known packages %+v, want %+v", info.KnownPackages, want)
	}
	if err := os.WriteFile(setup.SelectionPath(root), []byte(`{"version":1,"package":"custom-exe","activated":true,"models":["user.LocalApprovedKlein"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSetupInfo(io.Discard); !errors.Is(err, setup.ErrSelection) {
		t.Fatalf("a selection of an unknown package was reported: %v", err)
	}
}

// The package verbs take exactly their flags; held exits 3 and a refusal 2.
func TestPackageVerbs(t *testing.T) {
	var out, usage bytes.Buffer
	if err := packageCommand(context.Background(), t.TempDir(), nil, &out, &usage); err != nil || !strings.Contains(out.String(), "package activate") {
		t.Fatalf("no-argument help: %v %q", err, out.String())
	}
	for _, args := range [][]string{
		{"check", "--version", "v2026.39.1", "--root", `C:\x`},
		{"check", "--version", "v2026.39.1", "--json"},
		{"activate"},
		{"activate", "--version", "v2026.39.1", "--root", `C:\x`},
		{"retire", "--version", "v2026.39.1", "extra"},
		{"remove", "--version", "v2026.39.1"},
	} {
		if err := packageCommand(context.Background(), t.TempDir(), args, io.Discard, io.Discard); !errors.Is(err, setup.ErrSelection) {
			t.Fatalf("%q accepted: %v", args, err)
		}
	}
	root := t.TempDir()
	out.Reset()
	err := packageCommand(context.Background(), root, []string{"check", "--version", "v1999.1.1", "--root", root, "--json"}, &out, io.Discard)
	if exitCode(err) != 2 || out.Len() != 0 {
		t.Fatalf("an unknown version: exit %d, %v, %q", exitCode(err), err, out.String())
	}
	if exitCode(fmt.Errorf("x: %w", setup.ErrHeld)) != 3 || !strings.Contains(setup.ErrHeld.Error(), "held") || exitCode(errors.New("other")) != 1 {
		t.Fatal("exit codes")
	}
	out.Reset()
	if err := packageCommand(context.Background(), root, []string{"retire", "--version", "v2026.39.1"}, &out, io.Discard); err != nil {
		t.Fatalf("retiring a version with no folders: %v", err)
	}
	if got, want := out.String(), "{\"version\":1,\"ok\":true}\n"; got != want {
		t.Fatalf("retire reply %q, want %q", got, want)
	}
}

func TestInstalledDeclarationEndpoint(t *testing.T) {
	standInCache(t)
	if _, err := installedEndpoint("lemonade-image"); !errors.Is(err, bootstrap.ErrNoTrustedInstallation) {
		t.Fatalf("default wrapper refusal: %v", err)
	}
	want := `\\.\pipe\oa-test-lemonade-image`
	endpointFor := func(name string) (string, error) {
		if name != "lemonade-image" {
			t.Fatal(name)
		}
		return want, nil
	}
	for _, arg := range []string{"lemonade-image", want} {
		got, err := installedEndpointFor(arg, endpointFor)
		if err != nil || got != want {
			t.Fatalf("declared endpoint %q: %q, %v", arg, got, err)
		}
	}
	namespaced := listen.Endpoint("provider-" + strings.Repeat("a", 32))
	// The runtime's namespaced provider endpoint is a Windows pipe name; the
	// reviewed Lemonade package runs on Windows x64 only.
	if got, err := installedEndpointFor(namespaced, endpointFor); runtime.GOOS == "windows" && (err != nil || got != namespaced) {
		t.Fatalf("runtime namespaced endpoint refused: %q, %v", got, err)
	}
	for _, bad := range []string{"provider-" + strings.Repeat("a", 32), listen.Endpoint("provider-" + strings.Repeat("A", 32)), "http://127.0.0.1:8000", namespaced + "x"} {
		if _, err := installedEndpointFor(bad, endpointFor); !errors.Is(err, setup.ErrSelection) {
			t.Fatalf("foreign endpoint %q accepted: %v", bad, err)
		}
	}
	if _, err := installedEndpointFor("other-provider", endpointFor); !errors.Is(err, setup.ErrSelection) {
		t.Fatalf("foreign endpoint accepted: %v", err)
	}
}

// configure stages backends and the model; --engine-files-only installs the
// engine files of an existing install and takes no model flag.
// --replace-backend and --replace-private-model each replace one thing.
func TestConfigureFlags(t *testing.T) {
	model := []string{"--bf16-main", `C:\m`, "--bf16-encoder", `C:\e`, "--bf16-vae", `C:\v`}
	full := append([]string{"--activate-reviewed-package", "--vulkan-backend-archive", `C:\sd.zip`}, model...)
	with := func(extra string) []string { return append(append([]string(nil), full...), extra) }
	for name, tc := range map[string]struct {
		args []string
		want configureRequest
	}{
		"full": {full, configureRequest{main: `C:\m`, encoder: `C:\e`, vae: `C:\v`, sd: `C:\sd.zip`}},
		"replace the model only": {with("--replace-private-model"),
			configureRequest{main: `C:\m`, encoder: `C:\e`, vae: `C:\v`, sd: `C:\sd.zip`, replace: setup.Replace{Model: true}}},
		"replace the backends only": {with("--replace-backend"),
			configureRequest{main: `C:\m`, encoder: `C:\e`, vae: `C:\v`, sd: `C:\sd.zip`, replace: setup.Replace{Backends: true}}},
		"engine files": {[]string{"--activate-reviewed-package", "--engine-files-only"}, configureRequest{engineFilesOnly: true}},
		"engine files with a backend": {[]string{"--activate-reviewed-package", "--engine-files-only", "--llamacpp-backend-archive", `C:\l.zip`, "--replace-backend"},
			configureRequest{engineFilesOnly: true, llama: `C:\l.zip`, replace: setup.Replace{Backends: true}}},
		"a named package": {with("--package=v2026.39.1"),
			configureRequest{pkg: "v2026.39.1", main: `C:\m`, encoder: `C:\e`, vae: `C:\v`, sd: `C:\sd.zip`}},
		"speech": {[]string{"--activate-reviewed-package", "--speech-only", "--whisper-backend-archive", `C:\w.zip`, "--kokoro-backend-archive", `C:\k.tar.gz`,
			"--whisper-model", `C:\g.bin`, "--kokoro-index", `C:\i.json`, "--kokoro-model", `C:\k.onnx`, "--kokoro-voices", `C:\v.bin`, "--replace-backend"},
			configureRequest{speechOnly: true, replace: setup.Replace{Backends: true}, speech: setup.SpeechFiles{WhisperArchive: `C:\w.zip`, KokoroArchive: `C:\k.tar.gz`,
				WhisperModel: `C:\g.bin`, KokoroIndex: `C:\i.json`, KokoroModel: `C:\k.onnx`, KokoroVoices: `C:\v.bin`}}},
	} {
		got, err := parseConfigure(tc.args, io.Discard)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v", name, got, err)
		}
	}
	for name, args := range map[string][]string{
		"no activation":                 {"--vulkan-backend-archive", `C:\sd.zip`, "--bf16-main", `C:\m`, "--bf16-encoder", `C:\e`, "--bf16-vae", `C:\v`},
		"no model":                      {"--activate-reviewed-package", "--vulkan-backend-archive", `C:\sd.zip`},
		"no image backend":              append([]string{"--activate-reviewed-package"}, model...),
		"engine files with a model":     append([]string{"--activate-reviewed-package", "--engine-files-only"}, model...),
		"engine files replacing model":  {"--activate-reviewed-package", "--engine-files-only", "--replace-private-model"},
		"engine files with a package":   {"--activate-reviewed-package", "--engine-files-only", "--package", "v2026.39.1"},
		"positional":                    {"--activate-reviewed-package", "--engine-files-only", "extra"},
		"speech without --speech-only":  append(append([]string(nil), full...), "--whisper-model", `C:\g.bin`),
		"speech-only with nothing":      {"--activate-reviewed-package", "--speech-only"},
		"speech-only with a model":      append([]string{"--activate-reviewed-package", "--speech-only", "--whisper-model", `C:\g.bin`}, model...),
		"speech-only with engine files": {"--activate-reviewed-package", "--speech-only", "--engine-files-only", "--whisper-model", `C:\g.bin`},
	} {
		if _, err := parseConfigure(args, io.Discard); !errors.Is(err, setup.ErrSelection) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	var help bytes.Buffer
	if _, err := parseConfigure([]string{"-h"}, &help); !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "--engine-files-only") || !strings.Contains(help.String(), "--speech-only") || !strings.Contains(help.String(), "GPL-3.0") {
		t.Fatalf("configure help names no engine-files step: %v %q", err, help.String())
	}
}

func TestConfigureHelpNamesPinnedRuntimeBesidePublisherArchive(t *testing.T) {
	var out strings.Builder
	_, err := parseConfigure([]string{"--help"}, &out)
	if err == nil || !strings.Contains(out.String(), "publisher archive beside "+llamaRuntimeArchive) {
		t.Fatalf("configure help omits paired input: %v, %s", err, out.String())
	}
}

// setup-info names the staged models the selection serves, in its order,
// and an empty list before any setup.
func TestSetupInfoNamesTheStagedModels(t *testing.T) {
	standInCache(t)
	root, err := setup.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	read := func() setup.SetupInfoReply {
		var out bytes.Buffer
		if err := writeSetupInfo(&out); err != nil {
			t.Fatal(err)
		}
		var info setup.SetupInfoReply
		if err := json.Unmarshal(out.Bytes(), &info); err != nil || info.StagedModels == nil {
			t.Fatalf("setup-info %q: %v", out.String(), err)
		}
		return info
	}
	if info := read(); len(info.StagedModels) != 0 {
		t.Fatalf("staged models before setup: %v", info.StagedModels)
	}
	models := []string{setup.ApprovedModel, "user.Z-Image-Turbo"}
	if err := setup.Save(setup.SelectionPath(root), setup.Selection{Version: setup.SelectionVersion, Package: server.Packages[0].ID, Activated: true, Models: models}); err != nil {
		t.Fatal(err)
	}
	if info := read(); fmt.Sprint(info.StagedModels) != fmt.Sprint(models) {
		t.Fatalf("staged models %v", info.StagedModels)
	}
}

// model add takes one declared model and one absolute download per role,
// and passes them to the provider's staging step unchanged.
func TestModelAddVerb(t *testing.T) {
	var got struct {
		id    string
		files map[string]string
	}
	saved := stageModel
	stageModel = func(_ context.Context, _ string, id string, files map[string]string, _ func(int64, int64) error) error {
		got.id, got.files = id, files
		return nil
	}
	t.Cleanup(func() { stageModel = saved })
	var out bytes.Buffer
	if err := modelCommand(context.Background(), t.TempDir(), nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "model add") {
		t.Fatalf("no-argument help: %v %q", err, out.String())
	}
	main, vae := filepath.Join(t.TempDir(), "main"), filepath.Join(t.TempDir(), "vae")
	out.Reset()
	if err := modelCommand(context.Background(), t.TempDir(), []string{"add", "--id", "user.Z-Image-Turbo", "--file", "main=" + main, "--file", "vae=" + vae}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.id != "user.Z-Image-Turbo" || len(got.files) != 2 || got.files["main"] != main || got.files["vae"] != vae {
		t.Fatalf("staged %+v", got)
	}
	var reply struct {
		Version int    `json:"version"`
		OK      bool   `json:"ok"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil || reply.Version != 1 || !reply.OK || reply.Model != "user.Z-Image-Turbo" {
		t.Fatalf("reply %q: %v", out.String(), err)
	}
	for _, args := range [][]string{
		{"add"},
		{"add", "--id", "user.Z-Image-Turbo", "--file", "main"},
		{"add", "--id", "user.Z-Image-Turbo", "--file", "main=relative"},
		{"add", "--id", "user.Z-Image-Turbo", "--file", "main=" + main, "--file", "main=" + vae},
		{"add", "--id", "user.Z-Image-Turbo", "--file", "lora=" + main},
		{"add", "--id", "user.Z-Image-Turbo", "extra"},
		{"remove", "--id", "user.Z-Image-Turbo"},
	} {
		got.id = ""
		if err := modelCommand(context.Background(), t.TempDir(), args, io.Discard, io.Discard); !errors.Is(err, setup.ErrSelection) || got.id != "" {
			t.Fatalf("%q accepted: %v", args, err)
		}
	}
}

// --state and --dir stand anywhere on the command line, root the provider
// state and the package lookups, and are refused when relative, repeated or
// empty.
func TestRootOptions(t *testing.T) {
	t.Cleanup(func() { stateRoot = ""; setup.UsePackagesDir("") })
	state, dir := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "apps")
	rest, err := applyRootOptions([]string{"configure", "--state", state, "--activate-reviewed-package", "--dir=" + dir, "--speech-only"})
	if err != nil || strings.Join(rest, " ") != "configure --activate-reviewed-package --speech-only" {
		t.Fatalf("rest %q: %v", rest, err)
	}
	if root, err := providerRoot(); err != nil || root != state {
		t.Fatalf("provider root %s: %v", root, err)
	}
	if got, err := setup.PackagesDir(); err != nil || got != dir {
		t.Fatalf("packages folder %s: %v", got, err)
	}
	if err := checkServeEndpoint("lemonade-image"); !errors.Is(err, setup.ErrSelection) {
		t.Fatalf("serve --endpoint with --state: %v", err)
	}
	if err := checkServeEndpoint(""); err != nil {
		t.Fatalf("serve with --state: %v", err)
	}
	// A folder open to other accounts is refused (setup.EnsurePrivateFolder).
	for _, args := range [][]string{{"--state", "relative"}, {"--dir"}, {"--state", state, "--state", state}, {"--dir=relative"}, {"--state", t.TempDir()}} {
		if _, err := applyRootOptions(args); !errors.Is(err, setup.ErrSelection) {
			t.Fatalf("%q: %v", args, err)
		}
	}
	stateRoot = ""
	if root, err := providerRoot(); err != nil || root == state {
		t.Fatalf("default root %s: %v", root, err)
	}
	if err := checkServeEndpoint("lemonade-image"); err != nil {
		t.Fatalf("serve --endpoint without --state: %v", err)
	}
}
