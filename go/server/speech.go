package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
	supervision "github.com/openabstractions/abstraction-facade/go/supervision"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
	router "github.com/openabstractions/abstraction-router/go"
)

// The audio routes Lemonade 2026.39.1 serves through whisper-server and koko
// (research/security-reviews/whisper.cpp-v1.8.4-win.md condition 7,
// kokoros-b17-win.md condition 6). No other audio route is forwarded: not
// /audio/translations, not the realtime WebSocket, not the /v1 aliases.
const (
	TranscriptionRoute = "/api/v1/audio/transcriptions"
	SpeechRoute        = "/api/v1/audio/speech"
)

// Body bounds of the audio routes: a transcription form carries at most a
// 16 MiB WAV and a few short fields; a speech body at most 4096 characters.
const (
	transcriptionBodyLimit = whispercpp.MaxAudioBytes + 64<<10
	speechBodyLimit        = 64 << 10
)

// DefaultIdleStop is how long the tree keeps running after its last request
// once it has served audio (whisper.cpp condition 9, Kokoros condition 9).
const DefaultIdleStop = 5 * time.Minute

// Speech crash backoff (Kokoros condition 8): the first refusal window after a
// crash, doubled at each further crash up to the cap, cleared by a completed
// request.
const (
	speechBackoffFirst = 5 * time.Second
	speechBackoffCap   = 5 * time.Minute
)

// ErrAudioRoute reports an audio request or launch the provider refuses.
// Every refusal is an *AudioRefusal naming the unmet condition.
var ErrAudioRoute = errors.New("lemonade: audio route refused")

// An AudioRefusal names the review condition an audio request or the running
// launch does not meet: package (the provider-held working directory,
// condition 5), engine-files and model-files (conditions 1, 2 and 6),
// backend (whisper.cpp condition 2), environment (condition 4), backoff
// (Kokoros condition 8), disabled for a profile this provider does not
// serve, and request for a body the policy packages refuse.
type AudioRefusal struct {
	Condition string
	Detail    string
	err       error
}

func (e *AudioRefusal) Error() string {
	return "lemonade: audio route refused: " + e.Condition + ": " + e.Detail
}

func (e *AudioRefusal) Is(target error) bool { return target == ErrAudioRoute }

func (e *AudioRefusal) Unwrap() error { return e.err }

func audioRefusal(condition string, err error, format string, args ...any) error {
	return &AudioRefusal{Condition: condition, Detail: fmt.Sprintf(format, args...), err: err}
}

// SpeechModel pins one Lemonade model in the Hugging Face cache layout of
// the private models folder: models--<owner>--<repo>, holding refs/main with
// the revision and snapshots/<revision>/<file> for each pinned file, and
// nothing else (whisper.cpp condition 6, Kokoros condition 2).
type SpeechModel struct {
	Name     string // Lemonade's model id
	Folder   string // models--<owner>--<repo>
	Revision string
	Files    []ManifestFile
}

// WhisperLargeV3TurboModel is ggerganov/whisper.cpp's ggml-large-v3-turbo.bin
// at revision 5359861c, the only model the whisper.cpp review approves. Its
// legacy ggml format is admitted by this exact path and digest, never by the
// .bin extension, which PyTorch pickles also use.
var WhisperLargeV3TurboModel = SpeechModel{
	Name: whispercpp.LargeV3Turbo, Folder: "models--ggerganov--whisper.cpp", Revision: "5359861c739e955e79d9a303bcbc70fb988958b1",
	Files: []ManifestFile{{"ggml-large-v3-turbo.bin", 1624555275, "1fc70f774d38eb169993ac391eea357ef47c88757ef72ee5943879b7e8e2bc69"}},
}

// KokoroV1Model is mikkoph/kokoro-onnx at revision abf6e2a1, the only model
// the Kokoros review approves. index.json steers the paths Lemonade passes to
// koko, so its bytes are pinned with the weights and voices.
var KokoroV1Model = SpeechModel{
	Name: kokoro.KokoroV1, Folder: "models--mikkoph--kokoro-onnx", Revision: "abf6e2a176872a599e992bd9cf38b370d0eea9e3",
	Files: []ManifestFile{
		{"index.json", 96, "c2fb8a8173680fed91dba2b15d835e5b90630ded75953f40d23f9cefe76ceb75"},
		{"kokoro-v1.0.onnx", 325532387, "7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5"},
		{"voices-v1.0.bin", 28214398, "bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d"},
	},
}

func (m SpeechModel) valid() bool {
	if m.Name == "" || m.Folder == "" || filepath.Base(m.Folder) != m.Folder || !strings.HasPrefix(m.Folder, "models--") || len(m.Revision) != 40 || len(m.Files) == 0 {
		return false
	}
	for _, f := range m.Files {
		if f.Name == "" || filepath.Base(f.Name) != f.Name || f.Size < 0 || !hex64(f.SHA256) {
			return false
		}
	}
	return true
}

// Dir is the model's cache folder under modelsDir.
func (m SpeechModel) Dir(modelsDir string) string { return filepath.Join(modelsDir, m.Folder) }

// Paths are the model's pinned files, slash-separated relative to the
// models folder.
func (m SpeechModel) Paths() []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, path.Join(m.Folder, "snapshots", m.Revision, f.Name))
	}
	return out
}

// RefsMain is the refs/main file the cache layout carries: the revision.
func (m SpeechModel) RefsMain() []byte { return []byte(m.Revision) }

// Tree is the model folder as the shared engine-file kit proves it: refs/main
// and the snapshot's pinned files, and nothing else.
func (m SpeechModel) Tree(modelsDir, stateDir string) (programfiles.Tree, error) {
	if !m.valid() {
		return programfiles.Tree{}, fmt.Errorf("%w: invalid model pin %s", ErrWeightFormat, m.Name)
	}
	ref := m.RefsMain()
	sum := sha256.Sum256(ref)
	files := []programfiles.File{{Path: "refs/main", Size: int64(len(ref)), SHA256: hex.EncodeToString(sum[:])}}
	for _, f := range m.Files {
		files = append(files, programfiles.File{Path: path.Join("snapshots", m.Revision, f.Name), Size: f.Size, SHA256: f.SHA256})
	}
	return programfiles.Tree{Name: m.Folder, Root: m.Dir(modelsDir), Manifest: programfiles.Manifest{Files: files},
		Record: filepath.Join(stateDir, "model-"+m.Folder+".files")}, nil
}

// speechTrees are one launch's speech folders that are present: each pinned
// backend under the cache and each pinned model under the models folder.
// held names which of them the launch proves and holds.
type speechTree struct {
	what string // whisper-backend, kokoro-backend, whisper-model, kokoro-model
	tree programfiles.Tree
}

// speechModelTrees returns the trees of the speech models whose folders
// exist under modelsDir. A present folder is proven whether or not its
// profile is served, as a present backend folder is.
func speechModelTrees(cfg Config, stateDir string) ([]speechTree, error) {
	var trees []speechTree
	for _, m := range []struct {
		what  string
		model SpeechModel
	}{{"whisper-model", cfg.WhisperModel}, {"kokoro-model", cfg.KokoroModel}} {
		if _, err := os.Lstat(m.model.Dir(cfg.ModelsDir)); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrWeightFormat, m.model.Folder, err)
		}
		t, err := m.model.Tree(cfg.ModelsDir, stateDir)
		if err != nil {
			return nil, err
		}
		trees = append(trees, speechTree{what: m.what, tree: t})
	}
	return trees, nil
}

// speechBackends are the speech backend manifests a configuration admits
// under <cache>/bin. Each folder present is proven and held whether or not
// its profile is served; Transcription and Speech open the routes.
func (cfg Config) speechBackends() []BackendManifest {
	return []BackendManifest{cfg.WhisperBackend, cfg.KokoroBackend}
}

// speechModels are the model pins a configuration admits in the models
// folder, by exact path.
func (cfg Config) speechModels() []SpeechModel {
	return []SpeechModel{cfg.WhisperModel, cfg.KokoroModel}
}

// ProfileModels is which model serves which profile on this provider, in
// profile order: the declared Models for image, Whisper for transcription and
// Kokoro for speech when the server enables each. It is the provider's one
// table (router CONTRACT.md ROUTE-D9): the router host, the admission hook
// and the image body check all read it.
func (s *Server) ProfileModels() ([]string, map[string][]string) {
	profiles := []string{router.ProfileImage}
	byProfile := map[string][]string{router.ProfileImage: append([]string(nil), s.cfg.Models...)}
	if s.cfg.Transcription {
		profiles = append(profiles, router.ProfileTranscription)
		byProfile[router.ProfileTranscription] = []string{s.cfg.WhisperModel.Name}
	}
	if s.cfg.Speech {
		profiles = append(profiles, router.ProfileSpeech)
		byProfile[router.ProfileSpeech] = []string{s.cfg.KokoroModel.Name}
	}
	return profiles, byProfile
}

// ServesModel reports whether model serves profile on this provider
// (ProfileModels).
func (s *Server) ServesModel(profile, model string) bool {
	_, byProfile := s.ProfileModels()
	return model != "" && slices.Contains(byProfile[profile], model)
}

// AudioModel reports whether model is the audio model of a profile this
// provider serves: Whisper-Large-v3-Turbo for transcription, kokoro-v1 for
// speech. AllowsModel answers for the image and chat models only.
func (s *Server) AudioModel(model string) bool {
	return s.ServesModel(router.ProfileTranscription, model) || s.ServesModel(router.ProfileSpeech, model)
}

// CheckAudio reports the first condition that keeps model's audio route
// closed for the running launch, or nil. Admission calls it after Start.
func (s *Server) CheckAudio(model string) error {
	route := ""
	switch {
	case s.cfg.Transcription && model == s.cfg.WhisperModel.Name:
		route = TranscriptionRoute
	case s.cfg.Speech && model == s.cfg.KokoroModel.Name:
		route = SpeechRoute
	default:
		return audioRefusal("disabled", nil, "%s is not the audio model of a profile this provider serves", model)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return fmt.Errorf("%w: not started", ErrUnavailable)
	}
	return s.audioReadyLocked(route)
}

// AudioStaged reports, before anything starts, whether model's backend and
// model folders exist for the launch the binding names now: an unstaged
// profile is refused engine:audio:engine-files or engine:audio:model-files
// without starting Lemonade. The launch proves the folders it finds.
func (s *Server) AudioStaged(ctx context.Context, model string) error {
	cfg, err := s.launchConfig(ctx)
	if err != nil {
		return err
	}
	backend, pin := cfg.WhisperBackend, s.cfg.WhisperModel
	switch {
	case s.cfg.Transcription && model == s.cfg.WhisperModel.Name:
	case s.cfg.Speech && model == s.cfg.KokoroModel.Name:
		backend, pin = cfg.KokoroBackend, s.cfg.KokoroModel
	default:
		return audioRefusal("disabled", nil, "%s is not the audio model of a profile this provider serves", model)
	}
	if _, err := os.Lstat(backend.Dir(cfg.CacheDir)); err != nil {
		return audioRefusal("engine-files", err, "the %s/%s backend is not staged; run lemonaded configure --activate-reviewed-package --speech-only", backend.Family, backend.Variant)
	}
	if _, err := os.Lstat(pin.Dir(s.cfg.ModelsDir)); err != nil {
		return audioRefusal("model-files", err, "%s is not staged; run lemonaded configure --activate-reviewed-package --speech-only", pin.Name)
	}
	return nil
}

// audioReadyLocked reports the first condition of the running launch that
// keeps route closed. The package folder, Lemonade's working directory, must
// be proven and held (condition 5). The route's backend and model folders
// must be the pinned ones, proven and held (conditions 1, 2 and 6). The
// locked configuration must name the whisper backend explicitly and carry no
// backend argument (whisper.cpp conditions 2 and 3), and the environment this
// launch gave Lemonade, which its engines inherit, must pass the allow-list
// and the speech engines' forbidden names (condition 4). Speech also waits
// out a crash backoff (Kokoros condition 8). The caller holds s.mu.
func (s *Server) audioReadyLocked(route string) error {
	if !s.packageHeld {
		return audioRefusal("package", nil, "no proven package folder is held as Lemonade's working directory; run lemonaded configure --activate-reviewed-package --engine-files-only")
	}
	backend, model := "whisper-backend", "whisper-model"
	if route == SpeechRoute {
		backend, model = "kokoro-backend", "kokoro-model"
	}
	if !s.speechHeld[backend] {
		return audioRefusal("engine-files", nil, "no proven %s folder is held for this launch", backend)
	}
	if !s.speechHeld[model] {
		return audioRefusal("model-files", nil, "no proven %s folder is held for this launch", model)
	}
	if route == TranscriptionRoute {
		if got := lockedWhisperBackend(s.launched); got != s.launched.WhisperBackend.Variant {
			return audioRefusal("backend", nil, "the locked configuration names whisper backend %q, not the held %q", got, s.launched.WhisperBackend.Variant)
		}
	}
	if err := checkBackendArgs(lockedConfigFor(s.launched.ModelsDir, s.launched.WhisperBackend.Variant)); err != nil {
		return audioRefusal("arguments", err, "%v", err)
	}
	if err := validateEnvironment(s.launchEnv, s.launched.RuntimeEndpoint); err != nil {
		return audioRefusal("environment", err, "%v", err)
	}
	if route == SpeechRoute && time.Now().Before(s.speechRetry) {
		return audioRefusal("backoff", nil, "the speech engine stopped during a request; it is admitted again after %s", s.speechRetry.Format(time.RFC3339))
	}
	return nil
}

// lockedWhisperBackend is the whispercpp.backend value of the launch's
// locked configuration bytes, or "" when it cannot be read.
func lockedWhisperBackend(cfg Config) string {
	locked := cfg.LockedConfig
	if locked == nil {
		locked = LockedConfig
	}
	data, err := locked(cfg.ModelsDir)
	if err != nil {
		return ""
	}
	return whisperBackendOf(data)
}

// whisperBackendOf is the whispercpp.backend value of config.json bytes, or
// "" when it cannot be read.
func whisperBackendOf(data []byte) string {
	var parsed struct {
		Whisper struct {
			Backend string `json:"backend"`
		} `json:"whispercpp"`
	}
	if json.Unmarshal(data, &parsed) != nil {
		return ""
	}
	return parsed.Whisper.Backend
}

// readAudioBody reads and closes an audio request's body, bounded per route.
func readAudioBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, audioRefusal("request", nil, "the request has no body")
	}
	defer req.Body.Close()
	limit := int64(speechBodyLimit)
	if req.URL.Path == TranscriptionRoute {
		limit = transcriptionBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, audioRefusal("request", nil, "the body exceeds %d bytes", limit)
	}
	return body, nil
}

// checkAudioBody reads an audio request through its policy package and
// returns the bytes the provider sends and their content type: the
// transcription form rebuilt with the provider's WAV and the constant file
// name (whisper.cpp condition 7), the speech body rebuilt with pcm, the
// review's fields and nothing else (Kokoros condition 6). Lemonade then
// parses exactly what was checked.
func checkAudioBody(route string, body []byte, contentType string, audioModel func(string) bool) ([]byte, string, error) {
	if route == TranscriptionRoute {
		form, err := whispercpp.ParseForm(body, contentType)
		if err != nil {
			return nil, "", audioRefusal("request", err, "%v", err)
		}
		if !audioModel(form.Model) {
			return nil, "", audioRefusal("request", nil, "the form names no declared model")
		}
		checked, checkedType := form.Body()
		return checked, checkedType, nil
	}
	synthesis, _, err := kokoro.ParseSynthesis(body)
	if err != nil {
		return nil, "", audioRefusal("request", err, "%v", err)
	}
	if !audioModel(kokoro.KokoroV1) {
		return nil, "", audioRefusal("request", nil, "speech is not served")
	}
	return synthesis.Body(), "application/json", nil
}

// BindActivity gives the server the provider's view of its own work: active
// reports whether any declared model other than except has an admitted or
// running operation (inference Provider.ActiveForModel). The idle stop and
// the speech crash handling never stop the tree while it reports work.
// active runs while the server's lock is held and must not call the server.
func (s *Server) BindActivity(active func(except string) bool) {
	s.mu.Lock()
	s.active = active
	s.mu.Unlock()
}

// stopIfIdle stops launch proc in one hold of the server's lock, so no
// admission or request can reach the tree between the check and the stop.
// It stops nothing when proc is no longer the running launch, a start is in
// progress, idle is positive and a request reached or left the tree within
// it, a request holds the slot, or a declared model other than except has
// admitted or running work. wait is how long the caller waits before asking
// again; zero means the launch is gone or was stopped.
func (s *Server) stopIfIdle(proc *supervision.Process, except string, idle time.Duration) (stopped bool, wait time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proc == nil || s.proc != proc || s.starting {
		return false, 0, nil
	}
	if idle > 0 {
		if left := time.Until(s.lastRequest.Add(idle)); left > 0 {
			return false, left, nil
		}
	}
	if len(s.requests) != 0 || s.active != nil && s.active(except) {
		return false, idle, nil
	}
	return true, 0, s.stopLocked()
}

// SpeechFailed records that the speech engine stopped during a request
// (Kokoros condition 8). The provider never resubmits the text and refuses
// speech, and speech only, for a backoff that doubles with each further
// crash. When no request is in flight and no other model has work, it stops
// the launch the crash happened in, so the next admitted request starts koko
// afresh; otherwise the tree keeps serving that work and the idle stop ends
// it later. inputDigest names the input in the returned error, which the
// caller logs; the text never appears.
func (s *Server) SpeechFailed(inputDigest string, cause error) error {
	s.mu.Lock()
	switch {
	case s.speechBackoff == 0:
		s.speechBackoff = speechBackoffFirst
	case s.speechBackoff < speechBackoffCap:
		s.speechBackoff = min(2*s.speechBackoff, speechBackoffCap)
	}
	s.speechRetry = time.Now().Add(s.speechBackoff)
	retry, proc := s.speechRetry, s.proc
	s.mu.Unlock()
	report := fmt.Errorf("lemonade: the speech engine stopped during input %s; speech is admitted again after %s: %w", inputDigest, retry.Format(time.RFC3339), cause)
	_, _, err := s.stopIfIdle(proc, s.cfg.KokoroModel.Name, 0)
	return errors.Join(report, err)
}

// SpeechCompleted clears the crash backoff after a completed speech request.
func (s *Server) SpeechCompleted() {
	s.mu.Lock()
	s.speechBackoff, s.speechRetry = 0, time.Time{}
	s.mu.Unlock()
}

// touchLocked records a request reaching or leaving the tree; the idle window
// runs from the latest. The caller holds s.mu.
func (s *Server) touchLocked() { s.lastRequest = time.Now() }

// armIdleStop arms the idle stop of the running launch after a request ends
// on a launch that has served audio. The tree stops once IdleStop has passed
// since the last request reached or left it, no request is in flight and no
// declared model of any profile has an admitted or running operation. Work
// present at the window's end moves the check one window on. One check runs
// per launch; a later Start runs the tree again on demand.
func (s *Server) armIdleStop() {
	s.mu.Lock()
	proc, idle := s.proc, s.cfg.IdleStop
	if proc == nil || s.idleProc == proc {
		s.mu.Unlock()
		return
	}
	s.idleProc = proc
	s.mu.Unlock()
	var check func()
	check = func() {
		//unchecked: an idle stop that fails leaves the record held; the next Start reports it
		_, wait, _ := s.stopIfIdle(proc, "", idle)
		if s.idleChecked != nil {
			s.idleChecked()
		}
		if wait > 0 {
			time.AfterFunc(wait, check)
		}
	}
	time.AfterFunc(idle, check)
}
