package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	supervision "github.com/openabstractions/abstraction-facade/go/supervision"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
)

// Kokoros condition 1 and whisper.cpp condition 1: the manifests are the
// reviews' own. The Kokoros lines hash to the review's manifest digest; the
// whisper.cpp folders keep the server and its libraries and exclude SDL2.dll
// and every command-line tool.
func TestSpeechManifestsAreTheReviews(t *testing.T) {
	var lines []string
	for _, f := range KokorosB17.Files {
		lines = append(lines, fmt.Sprintf("%s\t%d\t%s\n", f.Name, f.Size, f.SHA256))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	if len(KokorosB17.Files) != 365 || hex.EncodeToString(sum[:]) != "c363d20cb9c91e30e241de62c225f04c7e1b6b622fb36ab0685a4f26617093d2" || !KokorosB17.valid() {
		t.Fatalf("Kokoros manifest: %d files, digest %x", len(KokorosB17.Files), sum)
	}
	for _, m := range []BackendManifest{WhisperCppVulkan, WhisperCppCPU} {
		var kept []string
		for _, f := range m.Required() {
			kept = append(kept, f.Name)
		}
		want := []string{"ggml-base.dll", "ggml-cpu.dll", "ggml.dll", "whisper-server.exe", "whisper.dll"}
		if m.Variant == "vulkan" {
			want = []string{"ggml-base.dll", "ggml-cpu.dll", "ggml-vulkan.dll", "ggml.dll", "whisper-server.exe", "whisper.dll"}
		}
		if !m.valid() || strings.Join(kept, " ") != strings.Join(want, " ") {
			t.Fatalf("%s keeps %q", m.Variant, kept)
		}
	}
	if err := Packages[0].Valid(); err != nil || !sameManifest(Packages[0].WhisperBackend, WhisperCppVulkan) || !sameManifest(Packages[0].KokoroBackend, KokorosB17) {
		t.Fatalf("package entry speech pairing: %v", err)
	}
}

// whisper.cpp condition 2: the locked configuration names the whisper
// backend explicitly, never auto, which can select the refused NPU build.
func TestLockedConfigNamesTheWhisperBackend(t *testing.T) {
	models := filepath.Join(t.TempDir(), "models")
	for _, c := range []struct {
		locked func(string) ([]byte, error)
		want   string
	}{{LockedConfig, "vulkan"}, {LockedConfigWhisperCPU, "cpu"}} {
		data, err := c.locked(models)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Whisper map[string]any `json:"whispercpp"`
			Kokoro  map[string]any `json:"kokoro"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			t.Fatal(err)
		}
		w := cfg.Whisper
		if w["backend"] != c.want || w["npu_bin"] != "builtin" || w["cpu_bin"] != "builtin" || w["vulkan_bin"] != "builtin" ||
			w["args"] != "" || w["npu_args"] != "" || w["cpu_args"] != "" || w["vulkan_args"] != "" {
			t.Fatalf("whispercpp section %v", w)
		}
		if cfg.Kokoro["cpu_bin"] != "builtin" {
			t.Fatalf("kokoro section %v", cfg.Kokoro)
		}
	}
	bad := Packages[0]
	bad.WhisperBackend = WhisperCppCPU
	if err := bad.Valid(); err == nil {
		t.Fatal("a CPU pairing with the Vulkan lock was accepted")
	}
	bad.Config = LockedConfigWhisperCPU
	if err := bad.Valid(); err != nil {
		t.Fatalf("CPU pairing with its lock: %v", err)
	}
}

// The lock before the whisper backend was named is replaced in place; any
// other difference still refuses the launch.
func TestPreviousLockedConfigIsReplaced(t *testing.T) {
	dir, models := t.TempDir(), filepath.Join(t.TempDir(), "models")
	previous, err := previousLockedConfig(models)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), previous, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateConfig(dir, models); err != nil {
		t.Fatal(err)
	}
	want, _ := LockedConfig(models)
	if got, _ := os.ReadFile(filepath.Join(dir, "config.json")); !bytes.Equal(got, want) {
		t.Fatalf("config.json %s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), bytes.Replace(previous, []byte(`"offline":true`), []byte(`"offline":false`), 1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateConfig(dir, models); !errors.Is(err, ErrLockedConfig) {
		t.Fatalf("a changed previous lock was accepted: %v", err)
	}
}

// Condition 4 of both reviews: the speech engines' variable names are refused
// whatever the allow-list says.
func TestForbiddenEnvironmentCoversTheSpeechEngines(t *testing.T) {
	for _, name := range []string{"WHISPER_ARG_DEVICE", "OVERRIDE_VITISAI_MODEL_PATH", "KMP_BLOCKTIME", "PIPER_ESPEAKNG_DATA_DIRECTORY", "ESPEAK_DATA_PATH", "ORT_DISABLE", "SSL_CERT_FILE", "LEMONADE_WHISPERCPP_VULKAN_BIN", "LEMONADE_KOKORO_CPU_BIN"} {
		if !forbiddenEnvironment(name) {
			t.Errorf("%s admitted", name)
		}
		if err := validateEnvironment([]string{name + "=x"}, ""); !errors.Is(err, ErrForbiddenEnvironment) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// whisper.cpp condition 7, Kokoros condition 6: only the two audio routes
// are added; translations, the realtime route and the /v1 aliases stay
// closed.
func TestOnlyTheTwoAudioRoutesAreForwarded(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:41234"}
	for _, path := range []string{TranscriptionRoute, SpeechRoute} {
		req, _ := http.NewRequest(http.MethodPost, s.base+path, strings.NewReader("{}"))
		if err := s.checkRoute(req); err != nil {
			t.Fatalf("%s refused: %v", path, err)
		}
	}
	for _, path := range []string{"/api/v1/audio/translations", "/v1/audio/transcriptions", "/v1/audio/speech", "/api/v1/realtime",
		"/api/v1/audio/transcriptions?model=x", "/api/v1/audio/voices", "/api/v1/audio/speech/stream"} {
		req, _ := http.NewRequest(http.MethodPost, s.base+path, strings.NewReader("{}"))
		if err := s.checkRoute(req); !errors.Is(err, ErrForbiddenRoute) {
			t.Fatalf("%s forwarded: %v", path, err)
		}
	}
}

// Condition 5 of both reviews: Lemonade starts in the package folder it
// proves and holds, which whisper-server and koko inherit.
func TestLaunchRunsInThePackageFolder(t *testing.T) {
	windowsLaunch(t)
	program := filepath.Join(t.TempDir(), "package", "lemond.exe")
	s := &Server{systemDir: func() (string, error) { return `C:\Windows`, nil }}
	cfg := Config{Program: program, CacheDir: filepath.Join(t.TempDir(), "cache"), ConfigDir: filepath.Join(t.TempDir(), "config"), TempDir: filepath.Join(t.TempDir(), "temp"), Port: 4000}
	plan, err := s.planFor(cfg, launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dir != filepath.Dir(program) {
		t.Fatalf("working directory %q", plan.Dir)
	}
}

// whisper.cpp condition 6, Kokoros condition 2: the speech model files are
// admitted by their exact pinned paths, never by extension; a .bin, .onnx or
// index.json anywhere else and the NPU's .rai cache are refused.
func TestCheckModelsAdmitsPinnedSpeechFilesByPath(t *testing.T) {
	write := func(t *testing.T, root, rel string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pinned := []SpeechModel{WhisperLargeV3TurboModel, KokoroV1Model}
	root := t.TempDir()
	for _, m := range pinned {
		for _, p := range m.Paths() {
			write(t, root, p)
		}
		write(t, root, m.Folder+"/refs/main")
	}
	if err := checkModels(root, pinned...); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"models--ggerganov--whisper.cpp/snapshots/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-tiny.bin",
		"models--other--repo/snapshots/1/ggml-large-v3-turbo.bin",
		"models--ggerganov--whisper.cpp/snapshots/5359861c739e955e79d9a303bcbc70fb988958b1/ggml-large-v3-turbo-encoder-vitisai.rai",
		"models--other--repo/snapshots/1/kokoro-v1.0.onnx",
		"models--other--repo/snapshots/1/index.json",
		"models--mikkoph--kokoro-onnx/snapshots/other/index.json",
	} {
		root := t.TempDir()
		write(t, root, rel)
		if err := checkModels(root, pinned...); !errors.Is(err, ErrWeightFormat) {
			t.Errorf("%s admitted: %v", rel, err)
		}
	}
	// Without the pins, a speech file is refused as any other format is.
	if err := checkModels(root); !errors.Is(err, ErrWeightFormat) {
		t.Fatalf("speech files admitted without pins: %v", err)
	}
}

// The model tree pins refs/main and the snapshot files and nothing else.
func TestSpeechModelTree(t *testing.T) {
	tree, err := KokoroV1Model.Tree(`C:\m`, `C:\s`)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range tree.Manifest.Files {
		paths = append(paths, f.Path)
	}
	want := "refs/main snapshots/abf6e2a176872a599e992bd9cf38b370d0eea9e3/index.json snapshots/abf6e2a176872a599e992bd9cf38b370d0eea9e3/kokoro-v1.0.onnx snapshots/abf6e2a176872a599e992bd9cf38b370d0eea9e3/voices-v1.0.bin"
	if strings.Join(paths, " ") != want || tree.Root != filepath.Join(`C:\m`, "models--mikkoph--kokoro-onnx") {
		t.Fatalf("tree %q at %s", paths, tree.Root)
	}
}

// The audio bodies the provider forwards are the policy packages' own: the
// transcription form rebuilt with the provider's WAV and audio.wav, the
// speech body rebuilt with pcm; engine fields are refused.
func TestCheckAudioBody(t *testing.T) {
	pcm := []byte{1, 0, 2, 0}
	wav := append([]byte("RIFF\x28\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00"), pcm...)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, _ := mw.CreateFormFile("file", `C:\Users\x\evil.wav`)
	part.Write(wav)
	mw.WriteField("model", whispercpp.LargeV3Turbo)
	mw.WriteField("response_format", "verbose_json")
	mw.Close()
	audio := func(m string) bool { return m == whispercpp.LargeV3Turbo || m == kokoro.KokoroV1 }
	checked, contentType, err := checkAudioBody(TranscriptionRoute, b.Bytes(), mw.FormDataContentType(), audio)
	if err != nil {
		t.Fatal(err)
	}
	form, err := whispercpp.ParseForm(checked, contentType)
	if err != nil || !bytes.Equal(form.Audio, wav) || !bytes.Contains(checked, []byte(`filename="audio.wav"`)) || bytes.Contains(checked, []byte("evil")) {
		t.Fatalf("forwarded form %q %v", checked, err)
	}
	if _, _, err := checkAudioBody(TranscriptionRoute, b.Bytes(), mw.FormDataContentType(), func(string) bool { return false }); !errors.Is(err, ErrAudioRoute) {
		t.Fatalf("undeclared model: %v", err)
	}
	speech, contentType, err := checkAudioBody(SpeechRoute, []byte(`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav"}`), "application/json", audio)
	if err != nil || contentType != "application/json" || !bytes.Contains(speech, []byte(`"response_format":"pcm"`)) || !bytes.Contains(speech, []byte(`"stream":false`)) {
		t.Fatalf("forwarded speech %s %v", speech, err)
	}
	for _, body := range []string{
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","initial_silence":9999999}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","lang_code":"vi"}`,
		`{"model":"kokoro-v1","voice":"vi_x","input":"Hi","response_format":"wav"}`,
	} {
		if _, _, err := checkAudioBody(SpeechRoute, []byte(body), "application/json", audio); !errors.Is(err, ErrAudioRoute) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// Without a held package folder, held speech files, the explicit backend and
// a clean environment, the audio routes stay closed, each naming its
// condition; a speech crash closes speech for the backoff only.
func TestAudioReadiness(t *testing.T) {
	windowsLaunch(t)
	models := filepath.Join(t.TempDir(), "models")
	env, err := childEnvironment(`C:\Windows`, filepath.Join(t.TempDir(), "temp"), launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)}, "")
	if err != nil {
		t.Fatal(err)
	}
	ready := func() *Server {
		launched := Config{ModelsDir: models, WhisperBackend: WhisperCppVulkan, KokoroBackend: KokorosB17, LockedConfig: LockedConfig}
		return &Server{launched: launched, launchEnv: env, packageHeld: true,
			speechHeld: map[string]bool{"whisper-backend": true, "whisper-model": true, "kokoro-backend": true, "kokoro-model": true}}
	}
	for _, route := range []string{TranscriptionRoute, SpeechRoute} {
		if err := ready().audioReadyLocked(route); err != nil {
			t.Fatalf("%s: %v", route, err)
		}
	}
	selected := ready()
	selected.launched.RuntimeEndpoint = "private-selected"
	selected.cfg.RuntimeEndpoint = "later-config"
	selected.launchEnv = append(slices.Clone(env), "ABSTRACTION_RUNTIME_ENDPOINT=private-selected")
	if err := selected.audioReadyLocked(SpeechRoute); err != nil {
		t.Fatal(err)
	}
	selected.launchEnv = append(slices.Clone(env), "ABSTRACTION_RUNTIME_ENDPOINT=later-config")
	if err := selected.audioReadyLocked(SpeechRoute); err == nil {
		t.Fatal("audio accepted current config over launched selection")
	}
	condition := func(err error) string {
		var refusal *AudioRefusal
		if errors.As(err, &refusal) {
			return refusal.Condition
		}
		return fmt.Sprint(err)
	}
	for want, change := range map[string]func(*Server){
		"package":      func(s *Server) { s.packageHeld = false },
		"engine-files": func(s *Server) { s.speechHeld["whisper-backend"] = false },
		"model-files":  func(s *Server) { s.speechHeld["whisper-model"] = false },
		"backend":      func(s *Server) { s.launched.WhisperBackend = WhisperCppCPU },
		"environment":  func(s *Server) { s.launchEnv = append(s.launchEnv, "WHISPER_ARG_DEVICE=1") },
	} {
		s := ready()
		change(s)
		if got := condition(s.audioReadyLocked(TranscriptionRoute)); got != want {
			t.Errorf("%s: refusal %q", want, got)
		}
	}
	s := ready()
	s.speechHeld["kokoro-model"] = false
	if got := condition(s.audioReadyLocked(SpeechRoute)); got != "model-files" {
		t.Errorf("kokoro model: %q", got)
	}
	s = ready()
	if err := s.SpeechFailed(kokoro.InputDigest("secret text"), errors.New("connection reset")); err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "sha256:") {
		t.Fatalf("crash report %v", err)
	}
	if got := condition(s.audioReadyLocked(SpeechRoute)); got != "backoff" {
		t.Fatalf("speech after a crash: %q", got)
	}
	if err := s.audioReadyLocked(TranscriptionRoute); err != nil {
		t.Fatalf("transcription after a speech crash: %v", err)
	}
	first := s.speechBackoff
	_ = s.SpeechFailed("sha256:x", errors.New("again"))
	if s.speechBackoff != 2*first {
		t.Fatalf("backoff %v after %v", s.speechBackoff, first)
	}
	s.SpeechCompleted()
	if err := s.audioReadyLocked(SpeechRoute); err != nil {
		t.Fatalf("speech after a completed request: %v", err)
	}
	if RefusalReason(fmt.Errorf("wrapped: %w", &AudioRefusal{Condition: "backoff"})) != "engine:audio:backoff" {
		t.Fatal("refusal reason")
	}
}

// stopIfIdle stops only the launch it names, and only while no request holds
// the slot and no model other than the excepted one has work.
func TestStopIfIdle(t *testing.T) {
	launch := func() (*Server, *supervision.Process) {
		exited := make(chan struct{})
		close(exited)
		proc := &supervision.Process{}
		return &Server{requests: make(chan struct{}, 1), proc: proc, exited: exited}, proc
	}
	s, proc := launch()
	if stopped, _, _ := s.stopIfIdle(&supervision.Process{}, "", 0); stopped || s.proc != proc {
		t.Fatal("a stale launch stopped the running one")
	}
	s.requests <- struct{}{}
	if stopped, _, _ := s.stopIfIdle(proc, "", 0); stopped {
		t.Fatal("stopped with a request in flight")
	}
	<-s.requests
	s.BindActivity(func(except string) bool { return except != "kokoro-v1" })
	if stopped, wait, _ := s.stopIfIdle(proc, "", time.Minute); stopped || wait != time.Minute {
		t.Fatalf("stopped under another model's work, or waits %v", wait)
	}
	s.lastRequest = time.Now()
	if stopped, wait, _ := s.stopIfIdle(proc, "kokoro-v1", time.Minute); stopped || wait <= 0 || wait > time.Minute {
		t.Fatalf("stopped within the idle window, or waits %v", wait)
	}
	if stopped, _, err := s.stopIfIdle(proc, "kokoro-v1", 0); !stopped || err != nil || s.proc != nil {
		t.Fatalf("an idle launch was not stopped: %v", err)
	}
	if stopped, wait, _ := s.stopIfIdle(proc, "", time.Minute); stopped || wait != 0 {
		t.Fatal("a stopped launch was stopped again")
	}
}

// whisper.cpp condition 2: Lemonade's NPU build (whispercpp/npu) is never
// admitted, beside the reviewed Vulkan folder or alone.
func TestBackendTreesRefuseTheWhisperNPUFolder(t *testing.T) {
	for _, variants := range [][]string{{"npu"}, {"vulkan", "npu"}} {
		cache := t.TempDir()
		for _, variant := range variants {
			if err := os.MkdirAll(filepath.Join(cache, "bin", "whispercpp", variant), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		_, err := backendTrees(cache, SDCppVulkan, LlamaCppVulkanB10825, false, "", WhisperCppVulkan, KokorosB17)
		if !errors.Is(err, ErrBackendManifest) || !strings.Contains(err.Error(), "whispercpp/npu") {
			t.Fatalf("%v: %v", variants, err)
		}
	}
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "bin", "whispercpp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backendTrees(empty, SDCppVulkan, LlamaCppVulkanB10825, false, "", WhisperCppVulkan, KokorosB17); err != nil {
		t.Fatalf("an empty whispercpp folder: %v", err)
	}
}
