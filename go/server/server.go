// Package server owns a Lemonade process started for the native image provider.
// Lemonade owns the image engine that it starts beneath that process.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openabstractions/abstraction-facade/go/programfiles"
	supervision "github.com/openabstractions/abstraction-facade/go/supervision"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-provider-kit/go/enginehttp"
	"github.com/openabstractions/abstraction-provider-kit/go/enginelaunch"
)

const pollInterval = 100 * time.Millisecond

var (
	ErrConfiguration = errors.New("lemonade: invalid configuration")
	ErrUnavailable   = errors.New("lemonade: unavailable")
)

// Config names an approved executable, private state, and an isolated loopback
// port. The caller supplies the exact model ids advertised by this provider.
type Config struct {
	// RuntimeEndpoint is the exact optional OA resolver selected by trusted bootstrap.
	RuntimeEndpoint string
	Program         string
	ProgramSHA256   string // Set by catalog-qualified setup; custom advanced mode is unqualified.
	// Package, when named, pins every file of the folder holding Program;
	// it is checked immediately before each launch.
	Package              PackageManifest
	RequireVulkanBackend bool
	// SDBackend and LlamaBackend pin the only backend folders admitted under
	// `<CacheDir>/bin`. Zero values select the reviewed SDCppVulkan and
	// LlamaCppVulkanB10825 manifests; b10825 is the build Lemonade 2026.39.1
	// pins.
	SDBackend    BackendManifest
	LlamaBackend BackendManifest
	CacheDir     string
	ConfigDir    string
	// TempDir is the child's TEMP and TMP. Empty selects engine-temp beside
	// CacheDir.
	TempDir string
	// ModelsDir is the locked config's models_dir. Empty selects
	// engine-models beside CacheDir.
	ModelsDir string
	// StateDir holds the metadata records of the package and backend
	// folders (INF-F2). Empty selects engine-files beside CacheDir.
	StateDir string
	Port     int
	Models   []string
	Ready    time.Duration
	Output   *os.File
	// LockedConfig returns the config.json bytes the launch compares; nil
	// selects LockedConfig.
	LockedConfig func(modelsDir string) ([]byte, error)
	// Bind, when set, names the package a launch runs. Start calls it at
	// each launch and takes Program, ProgramSHA256, Package, the backends,
	// CacheDir, ConfigDir, StateDir and LockedConfig from its Binding; the
	// fields above are then ignored. A run in progress keeps the binding it
	// proved and holds, and the next launch runs the binding Bind names then.
	Bind func(ctx context.Context) (Binding, error)

	// MaxImageBytes bounds each edit input image, as inference's
	// Config.MaxImageBytes does; zero selects inference.DefaultMaxImageBytes.
	MaxImageBytes int64

	// Transcription and Speech serve the audio profiles: the whisper.cpp
	// and Kokoros backends under <CacheDir>/bin, their models in ModelsDir
	// and the two audio routes (research/security-reviews/whisper.cpp-v1.8.4-win.md,
	// kokoros-b17-win.md). WhisperBackend, KokoroBackend, WhisperModel and
	// KokoroModel pin them; zero values select WhisperCppVulkan, KokorosB17,
	// WhisperLargeV3TurboModel and KokoroV1Model.
	Transcription, Speech         bool
	WhisperBackend, KokoroBackend BackendManifest
	WhisperModel, KokoroModel     SpeechModel
	// IdleStop stops the tree once no request has reached it for this long
	// after an audio request; zero selects DefaultIdleStop.
	IdleStop time.Duration

	// helperArgs lets tests run the test binary as a stand-in engine,
	// shutdownGrace gives that instance its finite stop grace, and folderACL
	// stands for the read-only folder ACL. These fields are package-private.
	helperArgs    []string
	shutdownGrace time.Duration
	folderACL     programfiles.ACL
	// chatBackend is the llama.cpp manifest the chat route requires; zero
	// selects the reviewed LlamaCppVulkanB10825. Tests set a fixture.
	chatBackend BackendManifest
}

// Server owns one contained Lemonade process tree.
type Server struct {
	cfg         Config
	mu          sync.Mutex
	proc        *supervision.Process
	exited      chan struct{}
	waitErr     error
	exitCode    int
	ready       bool
	starting    bool
	startDone   chan struct{}
	startCancel context.CancelFunc
	base        string
	client      *http.Client
	keys        launchKeys
	requests    chan struct{}
	systemDir   func() (string, error)
	sockets     func() ([]identity.Socket, error)
	// files proves and holds the package and backend folders for this
	// provider process (INF-F1..INF-F4).
	files *programfiles.Verifier
	// llamaHeld records that this launch proved and holds the llama.cpp
	// folder; launchEnv is the environment it gave Lemonade, which
	// llama-server inherits. The chat route checks both.
	llamaHeld bool
	launchEnv []string
	// launched is the configuration of the running launch, its binding
	// applied.
	launched Config
	// packageHeld records that this launch proved and holds the package
	// folder, Lemonade's working directory; speechHeld names the speech
	// backend and model folders it proved and holds. The audio routes check
	// both (audioReadyLocked).
	packageHeld bool
	speechHeld  map[string]bool
	// speechBackoff and speechRetry are the crash backoff of the speech
	// route (SpeechFailed).
	speechBackoff time.Duration
	speechRetry   time.Time
	// lastRequest is when a request last reached or left the tree; idleProc
	// is the launch whose idle stop is armed (armIdleStop). audioUsed records
	// that this launch served audio, after which every request's end arms
	// the idle stop. active is the provider's view of its own work
	// (BindActivity).
	lastRequest time.Time
	idleProc    *supervision.Process
	audioUsed   bool
	active      func(except string) bool
	// idleChecked, when set before the server starts, runs after each idle
	// check returns; tests wait on it.
	idleChecked func()
}

// Binding is the package one launch runs: its program, the manifests that
// pin the package and backend folders, and its per-version private folders.
type Binding struct {
	Program, ProgramSHA256 string
	Package                PackageManifest
	SDBackend              BackendManifest
	LlamaBackend           BackendManifest
	// WhisperBackend and KokoroBackend are the speech backends the package
	// pairs; zero values select the reviewed ones.
	WhisperBackend BackendManifest
	KokoroBackend  BackendManifest
	CacheDir       string
	ConfigDir      string
	StateDir       string
	LockedConfig   func(modelsDir string) ([]byte, error)
}

// apply returns cfg with b's fields, checked as New checks a Config without
// Bind.
func (b Binding) apply(cfg Config) (Config, error) {
	cfg.Program, cfg.ProgramSHA256, cfg.Package = b.Program, b.ProgramSHA256, b.Package
	cfg.SDBackend, cfg.LlamaBackend = b.SDBackend, b.LlamaBackend
	cfg.WhisperBackend, cfg.KokoroBackend = b.WhisperBackend, b.KokoroBackend
	cfg.CacheDir, cfg.ConfigDir, cfg.StateDir, cfg.LockedConfig = b.CacheDir, b.ConfigDir, b.StateDir, b.LockedConfig
	if !filepath.IsAbs(cfg.Program) || !filepath.IsAbs(cfg.CacheDir) || !filepath.IsAbs(cfg.ConfigDir) || !filepath.IsAbs(cfg.StateDir) {
		return Config{}, fmt.Errorf("%w: binding folders", ErrConfiguration)
	}
	if cfg.Package.Name != "" && (!cfg.Package.valid() || !cfg.Package.has(filepath.Base(cfg.Program))) {
		return Config{}, fmt.Errorf("%w: binding package manifest", ErrConfiguration)
	}
	return withBackends(cfg)
}

// withBackends selects the reviewed backends for zero manifests and checks
// them.
func withBackends(cfg Config) (Config, error) {
	if cfg.SDBackend.Family == "" {
		cfg.SDBackend = SDCppVulkan
	}
	if cfg.LlamaBackend.Family == "" {
		cfg.LlamaBackend = LlamaCppVulkanB10825
	}
	if cfg.WhisperBackend.Family == "" {
		cfg.WhisperBackend = WhisperCppVulkan
	}
	if cfg.KokoroBackend.Family == "" {
		cfg.KokoroBackend = KokorosB17
	}
	families := map[string]bool{}
	for _, m := range []BackendManifest{cfg.SDBackend, cfg.LlamaBackend, cfg.WhisperBackend, cfg.KokoroBackend} {
		if !m.valid() || families[m.Family] {
			return Config{}, fmt.Errorf("%w: backend manifest", ErrConfiguration)
		}
		families[m.Family] = true
	}
	if cfg.LockedConfig == nil {
		cfg.LockedConfig = LockedConfig
	}
	return cfg, nil
}

// launchConfig is the configuration of one launch: the fixed one, or the
// binding Bind names now.
func (s *Server) launchConfig(ctx context.Context) (Config, error) {
	if s.cfg.Bind == nil {
		return s.cfg, nil
	}
	b, err := s.cfg.Bind(ctx)
	if err != nil {
		return Config{}, fmt.Errorf("%w: binding: %w", ErrConfiguration, err)
	}
	return b.apply(s.cfg)
}

// RefusalReason names a typed launch refusal for the admission answer.
func RefusalReason(err error) string {
	var image *imageStagingRefusal
	if errors.As(err, &image) {
		return "engine:image:model-files"
	}
	var chat *ChatRefusal
	if errors.As(err, &chat) {
		return "engine:chat-route:" + chat.Condition
	}
	var audio *AudioRefusal
	if errors.As(err, &audio) {
		return "engine:audio:" + audio.Condition
	}
	switch {
	case errors.Is(err, ErrBackendManifest):
		return "engine:backend-unavailable"
	case errors.Is(err, ErrPackageManifest):
		return "engine:package-unavailable"
	case errors.Is(err, ErrLockedConfig):
		return "engine:config-mismatch"
	case errors.Is(err, ErrCollectionModel):
		return "engine:collection-model"
	case errors.Is(err, ErrWeightFormat):
		return "engine:weight-format"
	case errors.Is(err, ErrListenerNotLoopback):
		return "engine:listener-not-loopback"
	case errors.Is(err, ErrForbiddenArgument), errors.Is(err, ErrForbiddenEnvironment):
		return "engine:launch-plan"
	}
	return "engine:unavailable"
}

// ErrListenerNotLoopback reports a socket of the owned tree beyond loopback,
// or a tree whose listener on 127.0.0.1:<port> was not observed. It is
// abstraction-identity's ErrBeyondLoopback (ID-T7).
var ErrListenerNotLoopback = identity.ErrBeyondLoopback

// verifyLoopback is the provider kit's loopback check (inference CONTRACT.md
// INF-EL2) of the contained tree: every owned socket on 127.0.0.1 or ::1,
// and the tree's listener on 127.0.0.1:<port>. It stops nothing itself;
// callers stop the tree on an error.
func (s *Server) verifyLoopback(proc *supervision.Process) error {
	if proc == nil {
		return ErrUnavailable
	}
	return enginelaunch.CheckLoopback(s.sockets, s.cfg.Port, proc.Contains)
}

func New(cfg Config) (*Server, error) {
	if cfg.shutdownGrace <= 0 {
		cfg.shutdownGrace = defaultShutdownGrace
	}
	// A speech-only install declares no image model; AllowsModel then
	// answers false for every name.
	if cfg.Port < 1 || cfg.Port > 65535 || len(cfg.Models) == 0 && !cfg.Transcription && !cfg.Speech {
		return nil, ErrConfiguration
	}
	if cfg.chatBackend.Family == "" {
		cfg.chatBackend = LlamaCppVulkanB10825
	}
	if cfg.Bind != nil {
		// Each launch takes the program and its folders from the binding;
		// the shared folders are named here.
		if !filepath.IsAbs(cfg.TempDir) || !filepath.IsAbs(cfg.ModelsDir) {
			return nil, fmt.Errorf("%w: private folders", ErrConfiguration)
		}
	} else {
		if !filepath.IsAbs(cfg.Program) || !filepath.IsAbs(cfg.CacheDir) || !filepath.IsAbs(cfg.ConfigDir) {
			return nil, ErrConfiguration
		}
		if cfg.TempDir == "" {
			cfg.TempDir = filepath.Join(filepath.Dir(cfg.CacheDir), "engine-temp")
		}
		if cfg.ModelsDir == "" {
			cfg.ModelsDir = filepath.Join(filepath.Dir(cfg.CacheDir), "engine-models")
		}
		if cfg.StateDir == "" {
			cfg.StateDir = filepath.Join(filepath.Dir(cfg.CacheDir), "engine-files")
		}
		if !filepath.IsAbs(cfg.TempDir) || !filepath.IsAbs(cfg.ModelsDir) || !filepath.IsAbs(cfg.StateDir) {
			return nil, fmt.Errorf("%w: private folders", ErrConfiguration)
		}
		if cfg.Package.Name != "" && (!cfg.Package.valid() || !cfg.Package.has(filepath.Base(cfg.Program))) {
			return nil, fmt.Errorf("%w: package manifest", ErrConfiguration)
		}
		var err error
		if cfg, err = withBackends(cfg); err != nil {
			return nil, err
		}
		info, err := os.Stat(cfg.Program)
		if err != nil || info.IsDir() {
			return nil, fmt.Errorf("%w: executable", ErrConfiguration)
		}
	}
	seen := make(map[string]bool, len(cfg.Models))
	for _, model := range cfg.Models {
		if model == "" || len(model) > 256 || seen[model] {
			return nil, fmt.Errorf("%w: models", ErrConfiguration)
		}
		seen[model] = true
	}
	if err := checkDeclaredModels(cfg.ConfigDir, cfg.Models); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	if cfg.Ready <= 0 {
		cfg.Ready = 30 * time.Second
	}
	if cfg.Ready > 5*time.Minute {
		return nil, fmt.Errorf("%w: readiness budget", ErrConfiguration)
	}
	if cfg.WhisperModel.Name == "" {
		cfg.WhisperModel = WhisperLargeV3TurboModel
	}
	if cfg.KokoroModel.Name == "" {
		cfg.KokoroModel = KokoroV1Model
	}
	if !cfg.WhisperModel.valid() || !cfg.KokoroModel.valid() || cfg.WhisperModel.Folder == cfg.KokoroModel.Folder {
		return nil, fmt.Errorf("%w: speech model pins", ErrConfiguration)
	}
	for _, model := range cfg.Models {
		if model == cfg.WhisperModel.Name || model == cfg.KokoroModel.Name {
			return nil, fmt.Errorf("%w: an audio model is declared by its profile, not in Models", ErrConfiguration)
		}
	}
	if cfg.IdleStop <= 0 {
		cfg.IdleStop = DefaultIdleStop
	}
	cfg.Models = append([]string(nil), cfg.Models...)
	cfg.helperArgs = append([]string(nil), cfg.helperArgs...)
	return &Server{cfg: cfg, base: "http://127.0.0.1:" + strconv.Itoa(cfg.Port), requests: make(chan struct{}, 1), systemDir: systemWindowsDir, sockets: identity.Sockets,
		files: programfiles.NewVerifier(programfiles.Options{ACL: cfg.folderACL})}, nil
}

func (s *Server) Base() string { return s.base }

// PID is the process id of the running launch's Lemonade process, or 0 when
// no launch runs. The card lease is bound to it (provider card.go).
func (s *Server) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil {
		return 0
	}
	return s.proc.PID()
}

// Running reports whether a launch's tree runs: started and its exit not
// yet seen. A launch the idle stop or a failed check ended reads false.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || s.exited == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// ExitUnconfirmed reports a launch whose tree exit was seen but not
// confirmed: descendants remained after the forced stop, or the job's
// membership changed. Its files stay held and Start refuses until a stop
// confirms the exit; its card lease stays held too (holder.ExitConfirmer).
func (s *Server) ExitUnconfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc == nil || s.exited == nil {
		return false
	}
	select {
	case <-s.exited:
		return s.waitErr != nil
	default:
		return false
	}
}

// HTTP returns the stable client used by inference's existing OpenAI-compatible
// image adapter. It refuses requests until the owned child has been started.
func (s *Server) HTTP() *http.Client {
	return &http.Client{Transport: serverTransport{s: s}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type serverTransport struct{ s *Server }

// ErrForbiddenRoute reports a caller request outside the forwarded routes.
var ErrForbiddenRoute = errors.New("lemonade: route is not forwarded")

// forwardedRoutes are the only Lemonade routes an OA caller's work reaches:
// the OpenAI-compatible image generation and edit calls inference makes,
// which RoundTrip forwards once their text and size meet the
// stable-diffusion.cpp review (readImageBody), and
// the chat call, which RoundTrip forwards only once the running launch and
// the request meet the llama.cpp b10825 review (chatReadyLocked,
// checkChatBody), and the two audio calls, which RoundTrip forwards only once
// the running launch holds the reviewed speech files and the body passes the
// whisper.cpp or Kokoros policy package (audioReadyLocked, checkAudioBody).
// /internal/* (runtime settings, backend executables, shutdown), /mcp, pull,
// install, load, the Ollama routes, /audio/translations, the realtime
// WebSocket and every other route stay closed to callers.
var forwardedRoutes = map[string]bool{"/api/v1/images/generations": true, "/api/v1/images/edits": true, chatRoute: true,
	TranscriptionRoute: true, SpeechRoute: true}

func (s *Server) checkRoute(req *http.Request) error {
	u := req.URL
	if req.Method != http.MethodPost || u == nil || u.Scheme != "http" || u.Host != strings.TrimPrefix(s.base, "http://") ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !forwardedRoutes[u.Path] {
		return ErrForbiddenRoute
	}
	return nil
}

func (t serverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.s.checkRoute(req); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	select {
	case t.s.requests <- struct{}{}:
	case <-req.Context().Done():
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, req.Context().Err()
	}
	// Bodies are read once the request holds the provider's one request
	// slot, so at most one body is held in memory at a time.
	chat := req.URL.Path == chatRoute
	audio := req.URL.Path == TranscriptionRoute || req.URL.Path == SpeechRoute
	var chatBody, imageBody, audioBody []byte
	var imageType string
	if chat {
		body, err := readChatBody(req)
		if err != nil {
			<-t.s.requests
			return nil, err
		}
		chatBody = body
	} else if audio {
		body, err := readAudioBody(req)
		if err != nil {
			<-t.s.requests
			return nil, err
		}
		audioBody = body
	} else {
		body, contentType, err := t.s.readImageBody(req)
		if err != nil {
			<-t.s.requests
			return nil, err
		}
		imageBody, imageType = body, contentType
	}
	t.s.mu.Lock()
	client := t.s.client
	ready := t.s.ready
	apiKey := t.s.keys.api
	proc := t.s.proc
	var closed error
	if chat && ready {
		closed = t.s.chatReadyLocked()
	}
	if audio && ready {
		closed = t.s.audioReadyLocked(req.URL.Path)
		t.s.audioUsed = t.s.audioUsed || closed == nil
	}
	t.s.touchLocked()
	t.s.mu.Unlock()
	if client == nil || !ready || apiKey == "" {
		<-t.s.requests
		return nil, ErrUnavailable
	}
	if chat {
		if closed == nil {
			chatBody, closed = checkChatBody(chatBody, t.s.AllowsModel)
		}
		if closed != nil {
			<-t.s.requests
			return nil, closed
		}
	}
	var audioType string
	if audio {
		if closed == nil {
			audioBody, audioType, closed = checkAudioBody(req.URL.Path, audioBody, req.Header.Get("Content-Type"), t.s.AudioModel)
		}
		if closed != nil {
			<-t.s.requests
			return nil, closed
		}
	}
	// Lemonade starts sd-server on the first image request, so the tree's
	// sockets are checked before each request and again once it answers.
	if err := t.s.verifyLoopback(proc); err != nil {
		err = errors.Join(fmt.Errorf("%w: %w", ErrUnavailable, err), t.s.Close())
		<-t.s.requests
		return nil, err
	}
	// A RoundTripper must not change the caller's request; the clone carries
	// this launch's key in place of any caller-supplied Authorization.
	outbound := req.Clone(req.Context())
	outbound.Header.Set("Authorization", "Bearer "+apiKey)
	// The checked bytes replace the caller's body.
	checked, contentType := chatBody, "application/json"
	switch {
	case audio:
		checked, contentType = audioBody, audioType
	case !chat:
		checked, contentType = imageBody, imageType
	}
	outbound.Body = io.NopCloser(bytes.NewReader(checked))
	outbound.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(checked)), nil }
	outbound.ContentLength = int64(len(checked))
	outbound.TransferEncoding = nil
	outbound.Header.Set("Content-Type", contentType)
	resp, err := client.Transport.RoundTrip(outbound)
	if err != nil {
		if req.Context().Err() != nil {
			err = errors.Join(err, t.s.Close())
		}
		<-t.s.requests
		return nil, err
	}
	if err := t.s.verifyLoopback(proc); err != nil {
		resp.Body.Close()
		err = errors.Join(fmt.Errorf("%w: %w", ErrUnavailable, err), t.s.Close())
		<-t.s.requests
		return nil, err
	}
	resp.Body = &requestBody{ReadCloser: resp.Body, s: t.s, ctx: req.Context(), audio: audio}
	return resp, nil
}

// readChatBody reads and closes a chat request's body, bounded by
// chatBodyLimit.
func readChatBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, chatRefusal("request", "the request has no body")
	}
	defer req.Body.Close()
	body, err := io.ReadAll(io.LimitReader(req.Body, chatBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > chatBodyLimit {
		return nil, chatRefusal("request", "the body exceeds %d bytes", chatBodyLimit)
	}
	return body, nil
}

type requestBody struct {
	io.ReadCloser
	s    *Server
	ctx  context.Context
	once sync.Once
	// audio arms the idle stop once the answer is closed.
	audio bool
}

func (b *requestBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if b.ctx.Err() != nil {
			err = errors.Join(err, b.s.Close())
		}
		b.s.mu.Lock()
		b.s.touchLocked()
		used := b.s.audioUsed
		b.s.mu.Unlock()
		<-b.s.requests
		if b.audio || used {
			b.s.armIdleStop()
		}
	})
	return err
}

// AllowsModel prevents Lemonade's auto-load route from selecting an undeclared model.
func (s *Server) AllowsModel(model string) bool {
	for _, allowed := range s.cfg.Models {
		if model == allowed {
			return true
		}
	}
	return false
}

// CheckBackend gates admitted work before Lemonade could auto-fetch an absent
// backend or load an unreviewed one. Each backend folder is proven against
// its reviewed manifest through the shared engine-file kit: hashed at the
// first check of the provider process and after any mismatch, checked by
// metadata otherwise, with the read-only folder ACL (INF-F1..INF-F3). Any
// extra entry is refused.
func (s *Server) CheckBackend() error {
	cfg, err := s.launchConfig(context.Background())
	if err != nil {
		return err
	}
	return s.checkBackend(cfg)
}

// checkBackend proves the backend folders of one launch's configuration.
func (s *Server) checkBackend(cfg Config) error {
	trees, err := backendTrees(cfg.CacheDir, cfg.SDBackend, cfg.LlamaBackend, cfg.RequireVulkanBackend, cfg.StateDir, cfg.speechBackends()...)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	for _, b := range trees {
		if _, err := s.files.CheckRepairing(context.Background(), b.tree, log.Writer()); err != nil {
			return fmt.Errorf("%w: %w", ErrUnavailable, b.m.refusal(remedy(err)))
		}
	}
	return nil
}

// Start launches a contained child and accepts health only from that child's
// exact process generation. A failed or canceled start confirms tree exit.
func (s *Server) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: canceled before start: %w", ErrUnavailable, err)
	}
	s.mu.Lock()
	if s.starting {
		s.mu.Unlock()
		return fmt.Errorf("%w: start already in progress", ErrUnavailable)
	}
	if s.proc != nil {
		select {
		case <-s.exited:
			if s.waitErr != nil {
				s.mu.Unlock()
				return fmt.Errorf("%w: previous tree exit unconfirmed: %v", ErrUnavailable, s.waitErr)
			}
			s.clearLocked()
		default:
			if s.ready {
				s.mu.Unlock()
				return nil
			}
			s.mu.Unlock()
			return fmt.Errorf("%w: previous start still active", ErrUnavailable)
		}
	}
	// The package this launch runs is resolved now; the checks below, the
	// launch and the held trees all use it (DECISION 2026-10-01, point 4).
	cfg, err := s.launchConfig(ctx)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.checkBackend(cfg); err != nil {
		s.mu.Unlock()
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: port occupied: %v", ErrUnavailable, err)
	}
	listener.Close()
	if err := os.MkdirAll(cfg.CacheDir, 0700); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: cache: %v", ErrConfiguration, err)
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0700); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: config: %v", ErrConfiguration, err)
	}
	if err := os.MkdirAll(cfg.TempDir, 0700); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: temporary folder: %v", ErrConfiguration, err)
	}
	if err := errors.Join(checkCacheRoot(cfg.CacheDir), checkPrivateConfigWith(cfg.ConfigDir, cfg.ModelsDir, cfg.LockedConfig)); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	if err := errors.Join(checkRegistry(cfg.ConfigDir), checkModels(cfg.ModelsDir, cfg.speechModels()...), checkDeclaredModels(cfg.ConfigDir, cfg.Models)); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	if cfg.ProgramSHA256 != "" {
		file, err := os.Open(cfg.Program)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("%w: reviewed engine missing: %v", ErrConfiguration, err)
		}
		sum := sha256.New()
		_, copyErr := io.Copy(sum, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || hex.EncodeToString(sum.Sum(nil)) != cfg.ProgramSHA256 {
			s.mu.Unlock()
			return fmt.Errorf("%w: reviewed engine digest mismatch", ErrConfiguration)
		}
	}
	// The package and backend files stay held from here until the contained
	// tree's exit is confirmed (INF-F4).
	release, held, err := s.holdFiles(ctx, cfg)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	keys, err := newLaunchKeys(rand.Reader)
	if err != nil {
		release()
		s.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	plan, err := s.planFor(cfg, keys)
	if err != nil {
		release()
		s.mu.Unlock()
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	args := append(append([]string{}, cfg.helperArgs...), plan.Args...)
	startCtx, startCancel := context.WithCancel(ctx)
	s.starting, s.startDone, s.startCancel = true, make(chan struct{}), startCancel
	defer func() {
		startCancel()
		s.mu.Lock()
		s.starting, s.startCancel = false, nil
		close(s.startDone)
		s.startDone = nil
		s.mu.Unlock()
	}()
	proc, err := supervision.Start(startCtx, supervision.Options{
		Executable: plan.Program, Args: args, Env: plan.Env, Dir: plan.Dir, Stdout: cfg.Output, Stderr: cfg.Output,
		ForceTimeout: 20 * time.Second,
	})
	if err != nil {
		release()
		s.mu.Unlock()
		return fmt.Errorf("%w: contained start: %w", ErrUnavailable, err)
	}
	s.proc, s.keys = proc, keys
	logTransport(cfg)
	s.llamaHeld, s.launchEnv, s.launched = held.llama, plan.Env, cfg
	s.packageHeld, s.speechHeld = held.pkg, held.speech
	s.exited = make(chan struct{})
	exited := s.exited
	go func() {
		code, waitErr := proc.Wait()
		// An unconfirmed exit keeps the files held.
		if waitErr == nil {
			release()
		}
		s.exitCode, s.waitErr = code, waitErr
		close(exited)
	}()
	s.client, err = enginehttp.NewClient(s.base, identity.Process{PID: proc.PID(), StartTime: proc.StartTime()})
	if err != nil {
		cleanup := s.stopLocked()
		s.mu.Unlock()
		return errors.Join(fmt.Errorf("%w: bound client: %w", ErrUnavailable, err), cleanup)
	}
	s.mu.Unlock()
	ready, cancel := context.WithTimeout(startCtx, s.cfg.Ready)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	// A connect that can never succeed ends readiness after
	// enginehttp.UnreachableFor, the rule every engine readiness keeps.
	var run enginehttp.UnreachableRun
	for {
		select {
		case <-s.exited:
			s.mu.Lock()
			err := fmt.Errorf("%w: exited before ready (code %d): %v", ErrUnavailable, s.exitCode, s.waitErr)
			if s.waitErr == nil {
				s.clearLocked()
			}
			s.mu.Unlock()
			return err
		default:
		}
		healthy, probeErr := s.healthy(ready, keys.api)
		if end := run.Note(probeErr); end != nil {
			s.mu.Lock()
			cleanup := s.stopLocked()
			s.mu.Unlock()
			return errors.Join(fmt.Errorf("%w: readiness: %w", ErrUnavailable, end), cleanup)
		}
		if healthy {
			s.mu.Lock()
			if err := ready.Err(); err != nil {
				cleanup := s.stopLocked()
				s.mu.Unlock()
				return errors.Join(fmt.Errorf("%w: readiness: %w", ErrUnavailable, err), cleanup)
			}
			if err := s.verifyLoopback(s.proc); err != nil {
				cleanup := s.stopLocked()
				s.mu.Unlock()
				return errors.Join(fmt.Errorf("%w: %w", ErrUnavailable, err), cleanup)
			}
			s.ready = true
			s.mu.Unlock()
			return nil
		}
		select {
		case <-s.exited:
			s.mu.Lock()
			err := fmt.Errorf("%w: exited before ready (code %d): %v", ErrUnavailable, s.exitCode, s.waitErr)
			if s.waitErr == nil {
				s.clearLocked()
			}
			s.mu.Unlock()
			return err
		case <-ready.Done():
			s.mu.Lock()
			cleanup := s.stopLocked()
			s.mu.Unlock()
			return errors.Join(fmt.Errorf("%w: readiness: %w", ErrUnavailable, ready.Err()), cleanup)
		case <-ticker.C:
		}
	}
}

// healthy reads Lemonade's own health. The error is the request's failure,
// which the readiness loop reads only for enginehttp.ErrEngineUnreachable.
func (s *Server) healthy(ctx context.Context, apiKey string) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/v1/health", nil)
	if err != nil {
		return false, nil
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := s.client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, nil
	}
	var health struct {
		Status string `json:"status"`
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health) == nil && health.Status == "ok", nil
}

// Close asks Lemonade to unload its backend children, then confirms the full
// contained tree exited. A failed confirmation leaves the process record held.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.starting {
		s.startCancel()
		done := s.startDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
	}
	defer s.mu.Unlock()
	return s.stopLocked()
}

func (s *Server) stopLocked() error {
	if s.proc == nil {
		return nil
	}
	select {
	case <-s.exited:
		if s.waitErr != nil {
			return fmt.Errorf("%w: contained tree exit: %w", ErrUnavailable, s.waitErr)
		}
		s.clearLocked()
		return nil
	default:
	}
	var shutdownErr error
	if s.ready {
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		request, err := http.NewRequestWithContext(shutdown, http.MethodPost, s.base+"/internal/shutdown", nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+s.keys.admin)
			response, err := s.client.Do(request)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != http.StatusOK {
					shutdownErr = fmt.Errorf("%w: shutdown status %d", ErrUnavailable, response.StatusCode)
				}
			} else {
				shutdownErr = fmt.Errorf("%w: shutdown: %w", ErrUnavailable, err)
			}
		} else {
			shutdownErr = fmt.Errorf("%w: shutdown request: %w", ErrUnavailable, err)
		}
		cancel()
		if shutdownErr == nil {
			select {
			case <-s.exited:
				if s.waitErr != nil {
					return fmt.Errorf("%w: contained tree exit: %w", ErrUnavailable, s.waitErr)
				}
				s.clearLocked()
				return nil
			case <-time.After(s.cfg.shutdownGrace):
				shutdownErr = errShutdownGrace
			}
		}
	}
	forceErr := s.proc.Stop()
	select {
	case <-s.exited:
		if s.waitErr == nil {
			s.clearLocked()
		}
		if shutdownErr == errShutdownGrace && forceErr == nil && s.waitErr == nil {
			// Lemonade accepted the shutdown but its tree outlived the
			// grace, which a loaded machine can cause; the forced stop
			// confirmed the whole tree exited, so the stop succeeded.
			s.logf("lemonade: the tree did not exit within %v of its accepted shutdown; the forced stop confirmed its exit", s.cfg.shutdownGrace)
			return nil
		}
		return errors.Join(shutdownErr, forceErr, s.waitErr)
	case <-time.After(25 * time.Second):
		return errors.Join(shutdownErr, forceErr, fmt.Errorf("%w: process tree exit unconfirmed", ErrUnavailable))
	}
}

// defaultShutdownGrace is how long an accepted shutdown waits for the tree to exit
// before the forced stop.
const defaultShutdownGrace = 3 * time.Second

// errShutdownGrace is an accepted shutdown whose tree outlived the grace.
var errShutdownGrace = fmt.Errorf("%w: shutdown did not exit within grace", ErrUnavailable)

// logTransport distinguishes the running front-end from its backend plan;
// Lemonade starts those backends on demand, so their presence is not claimed.
func logTransport(cfg Config) {
	backendPin := func(m BackendManifest, program string) string {
		for _, f := range m.Files {
			if filepath.Base(f.Name) == program {
				return m.Family + "/" + m.Variant + "@" + m.Version + ":" + f.SHA256
			}
		}
		return "unqualified"
	}
	report := func(engine, pin, scope string) {
		const reason = "IPC not implemented in this adopter"
		slog.Info("engine transport", "engine", engine, "pin", pin, "transport", "tcp-loopback", "reason", reason, "scope", scope)
		if cfg.Output != nil {
			//unchecked: informational diagnostics do not alter the admitted launch
			fmt.Fprintf(cfg.Output, "INFO engine=%q pin=%q transport=tcp-loopback reason=%q scope=%s\n", engine, pin, reason, scope)
		}
	}
	pin := cfg.ProgramSHA256
	if pin == "" {
		pin = "unqualified"
	}
	report("lemond", pin, "launched")
	report("sd-server", backendPin(cfg.SDBackend, "sd-server.exe"), "adopter-plan")
	if cfg.Transcription {
		report("whisper-server", backendPin(cfg.WhisperBackend, "whisper-server.exe"), "adopter-plan")
	}
	if cfg.Speech {
		report("Kokoros", backendPin(cfg.KokoroBackend, "koko.exe"), "adopter-plan")
	}
}

// logf writes one provider line to the tree's output, where Lemonade's own
// log goes; nothing when the configuration names none.
func (s *Server) logf(format string, args ...any) {
	if s.cfg.Output != nil {
		//unchecked: a lost diagnostic line changes no outcome
		_, _ = fmt.Fprintln(s.cfg.Output, fmt.Sprintf(format, args...))
	}
}

func (s *Server) clearLocked() {
	s.proc, s.exited, s.client, s.ready, s.keys = nil, nil, nil, false, launchKeys{}
	s.waitErr, s.exitCode = nil, 0
	s.llamaHeld, s.launchEnv, s.launched = false, nil, Config{}
	s.packageHeld, s.speechHeld, s.audioUsed, s.idleProc = false, nil, false, nil
}
