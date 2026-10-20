//go:build windows

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-inference/go/providerhost/enginefiles"
	"golang.org/x/sys/windows"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func fakeConfig(t *testing.T, mode string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"),
		Port: freePort(t), Models: []string{"user.ApprovedKlein"}, Ready: 3 * time.Second,
		helperArgs: []string{"-test.run=^TestLemonadeHelperProcess$", "--", mode},
		folderACL:  openACL{},
	}
}

// INF-F4: while the engine runs, its package files are held with read
// sharing only; a write, rename or delete fails until the tree's exit is
// confirmed.
func TestPackageFilesAreHeldWhileTheEngineRuns(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	program, err := os.ReadFile(cfg.Program)
	if err != nil {
		t.Fatal(err)
	}
	root, m := writePackageFixture(t, map[string][]byte{
		"lemond.exe": program, "resources/defaults.json": []byte(`{"host":"localhost"}`),
	})
	cfg.Program, cfg.Package = filepath.Join(root, "lemond.exe"), m
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defaults := filepath.Join(root, "resources", "defaults.json")
	for name, try := range map[string]func() error{
		"write":  func() error { return os.WriteFile(defaults, []byte(`{"host":"0.0.0.0"}`), 0600) },
		"rename": func() error { return os.Rename(defaults, defaults+".old") },
		"delete": func() error { return os.Remove(defaults) },
	} {
		if err := try(); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			s.Close()
			t.Fatalf("%s of a package file while the engine runs: %v", name, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(defaults, []byte(`{"host":"localhost"}`), 0600); err != nil {
		t.Fatalf("package file still held after the engine exited: %v", err)
	}
}

// INF-F3: with the real folder ACL, a package folder that setup never
// installed refuses the launch; InstallFiles gives it the read-only ACL and
// the launch proceeds.
func TestLaunchRefusesAPackageWithoutTheReadOnlyACL(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	cfg.folderACL = nil
	program, err := os.ReadFile(cfg.Program)
	if err != nil {
		t.Fatal(err)
	}
	root, m := writePackageFixture(t, map[string][]byte{"lemond.exe": program, "resources/defaults.json": []byte(`{}`)})
	t.Cleanup(func() { enginefiles.Unlock(root) })
	cfg.Program, cfg.Package = filepath.Join(root, "lemond.exe"), m
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Start(context.Background())
	if !errors.Is(err, ErrPackageManifest) || !errors.Is(err, enginefiles.ErrFolderACL) || s.proc != nil || !strings.Contains(err.Error(), "lemonaded configure") ||
		!strings.Contains(err.Error(), "--engine-files-only") {
		t.Fatalf("a package without the read-only ACL launched, or the refusal names no remedy: %v", err)
	}
	if RefusalReason(err) != "engine:package-unavailable" {
		t.Fatalf("reason %q", RefusalReason(err))
	}
	binding := Binding{Program: cfg.Program, Package: m, SDBackend: SDCppVulkan, LlamaBackend: LlamaCppVulkanB10825, CacheDir: cfg.CacheDir, StateDir: s.cfg.StateDir}
	if ok, err := FilesInstalled(binding); err != nil || ok {
		t.Fatalf("a package never installed counts as installed: %v %v", ok, err)
	}
	if err := InstallFiles(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	if ok, err := FilesInstalled(binding); err != nil || !ok {
		t.Fatalf("the installed package does not count as installed: %v %v", ok, err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("the installed package was refused: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedProgramReplacementBeforeStartRefused(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	program := filepath.Join(t.TempDir(), "reviewed.exe")
	source, err := os.Open(cfg.Program)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := os.Create(program)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(dest, h), source)
	source.Close()
	if err != nil {
		dest.Close()
		t.Fatal(err)
	}
	if err := dest.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Program, cfg.ProgramSHA256 = program, hex.EncodeToString(h.Sum(nil))
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(program, []byte("replaced after provider construction"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrConfiguration) || s.proc != nil {
		t.Fatalf("replaced package launched: %v", err)
	}
}

// evidenceDir is the stand-in engine's TEMP, the provider's default
// engine-temp folder beside the cache.
func evidenceDir(cfg Config) string { return filepath.Join(filepath.Dir(cfg.CacheDir), "engine-temp") }

// Lemonade 2026.39.1 moves config.json, jobs.json, recipe_options.json and
// user_models.json from the cache root into the config folder at every start
// when the config folder lacks them (main.cpp line 153, path_utils.cpp
// migrate_legacy_json_files_to_config_dir). A file planted there would be
// installed after the provider's config check.
func TestCacheRootLegacyFileRefusesLaunch(t *testing.T) {
	for _, name := range []string{"recipe_options.json", "config.json", "jobs.json", "user_models.json", "notes.txt"} {
		t.Run(name, func(t *testing.T) {
			cfg := fakeConfig(t, "helper-ready")
			if err := os.MkdirAll(cfg.CacheDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.CacheDir, name), []byte(`{"user.ApprovedKlein":{"sd-cpp_args":"--listen-ip 0.0.0.0"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = s.Start(context.Background())
			if !errors.Is(err, ErrLockedConfig) || s.proc != nil {
				if s.proc != nil {
					s.Close()
				}
				t.Fatalf("%s in the cache root launched: %v", name, err)
			}
			if RefusalReason(err) != "engine:config-mismatch" {
				t.Fatalf("reason %q", RefusalReason(err))
			}
		})
	}
}

// A changed resource file of the pinned package refuses the next launch.
func TestChangedPackageResourceRefusesLaunch(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	program, err := os.ReadFile(cfg.Program)
	if err != nil {
		t.Fatal(err)
	}
	root, m := writePackageFixture(t, map[string][]byte{
		"lemond.exe": program, "resources/defaults.json": []byte(`{"host":"localhost"}`),
	})
	cfg.Program, cfg.Package = filepath.Join(root, "lemond.exe"), m
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "resources", "defaults.json"), []byte(`{"host":"0.0.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	err = s.Start(context.Background())
	if !errors.Is(err, ErrPackageManifest) || s.proc != nil {
		t.Fatalf("changed package resource launched: %v", err)
	}
	if RefusalReason(err) != "engine:package-unavailable" {
		t.Fatalf("reason %q", RefusalReason(err))
	}
}

func TestMissingReviewedBackendRefusesBeforeChildStart(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	cfg.RequireVulkanBackend = true
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrBackendManifest) || s.proc != nil {
		t.Fatalf("missing backend allowed engine launch: %v", err)
	}
}

// Each refusal below passed the gate while only sd-server.exe was hashed and
// the other files were checked for presence.
func TestReviewedBackendGateBeforeStart(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	manifest, base := writeBackendFixture(t, cfg.CacheDir, "sd-cpp", "vulkan", map[string]string{
		"sd-server.exe": "reviewed server", "ggml-vulkan.dll": "reviewed dll",
	}, nil)
	cfg.RequireVulkanBackend = true
	cfg.SDBackend = manifest
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dll := filepath.Join(base, "ggml-vulkan.dll")
	refused := func(what string) {
		t.Helper()
		if err := s.Start(context.Background()); !errors.Is(err, ErrUnavailable) || !errors.Is(err, ErrBackendManifest) || s.proc != nil {
			t.Fatalf("%s allowed restart: %v", what, err)
		}
	}
	if err := os.Remove(dll); err != nil {
		t.Fatal(err)
	}
	refused("missing backend DLL")
	if err := os.WriteFile(dll, []byte("replaced dll"), 0600); err != nil {
		t.Fatal(err)
	}
	refused("same-size replaced backend DLL")
	if err := os.WriteFile(dll, []byte("reviewed dll"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "version.dll"), []byte("planted"), 0600); err != nil {
		t.Fatal(err)
	}
	refused("planted DLL beside the backend")
	if err := os.Remove(filepath.Join(base, "version.dll")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sd-server.exe"), []byte("changed server"), 0600); err != nil {
		t.Fatal(err)
	}
	refused("changed backend server")
}

func TestLemonadeHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-- helper-") {
		return
	}
	mode := ""
	for _, arg := range os.Args {
		if strings.HasPrefix(arg, "helper-") {
			mode = arg
		}
	}
	// Evidence goes to the child's private TEMP, outside the cache root the
	// provider checks before each launch.
	evidence := os.Getenv("TEMP")
	port := ""
	for i, arg := range os.Args {
		if arg == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if mode == "helper-exit" {
		os.Exit(3)
	}
	if mode == "helper-stall" {
		time.Sleep(time.Minute)
	}
	if mode == "helper-env-probe" {
		// The stand-in llama-server records the names it inherited, never
		// the values.
		var names []string
		for _, item := range os.Environ() {
			if name, _, _ := strings.Cut(item, "="); name != "" {
				names = append(names, name)
			}
		}
		if err := os.WriteFile(filepath.Join(evidence, "grandchild-env"), []byte(strings.Join(names, "\n")), 0600); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
	if _, err := strconv.Atoi(port); err != nil {
		os.Exit(4)
	}
	// Authenticate the way Lemonade 2026.39.1 does with both keys set:
	// /internal/* needs the admin key, /api/* either key.
	apiKey, adminKey := os.Getenv("LEMONADE_API_KEY"), os.Getenv("LEMONADE_ADMIN_API_KEY")
	if len(apiKey) != 64 || len(adminKey) != 64 || apiKey == adminKey {
		os.Exit(6)
	}
	if err := os.WriteFile(filepath.Join(evidence, "env-seen"), []byte(strings.Join(os.Environ(), "\n")), 0600); err != nil {
		os.Exit(7)
	}
	if mode == "helper-grandchild" {
		// A nil Env inherits this process's block, as Lemonade's
		// CreateProcessA with a null environment does for llama-server.
		probe := exec.Command(os.Args[0], "-test.run=^TestLemonadeHelperProcess$", "--", "helper-env-probe")
		if err := probe.Run(); err != nil {
			os.Exit(8)
		}
	}
	mux := http.NewServeMux()
	http.DefaultServeMux = mux
	var logMu sync.Mutex
	authenticated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logMu.Lock()
		if f, err := os.OpenFile(filepath.Join(evidence, "requests.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			_, _ = fmt.Fprintln(f, r.Method, r.URL.RequestURI())
			f.Close()
		}
		logMu.Unlock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		allowed := token == adminKey || !strings.HasPrefix(r.URL.Path, "/internal/") && token == apiKey
		if !allowed {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	http.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
	})
	http.HandleFunc("/internal/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if mode == "helper-bad-shutdown" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"shutting down"}`)
		_ = os.WriteFile(filepath.Join(evidence, "shutdown-seen"), []byte("yes"), 0600)
		if mode == "helper-slow-shutdown" {
			// Accepted, but the tree outlives the provider's grace, as a
			// loaded machine can make it.
			return
		}
		go func() { time.Sleep(50 * time.Millisecond); os.Exit(0) }()
	})
	http.HandleFunc("/api/v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":[]}`)
	})
	http.HandleFunc("/api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = os.WriteFile(filepath.Join(evidence, "chat-body"), body, 0600)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[]}`)
	})
	http.HandleFunc("/api/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile(filepath.Join(evidence, "generation-started"), []byte("yes"), 0600)
		<-r.Context().Done()
	})
	// The stand-in whisper-server and koko behind Lemonade's audio routes
	// record the bytes they receive; helper-speech-crash drops the
	// connection the way a koko that exits mid-request does.
	http.HandleFunc("/api/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = os.WriteFile(filepath.Join(evidence, "transcription-body"), body, 0600)
		_ = os.WriteFile(filepath.Join(evidence, "transcription-type"), []byte(r.Header.Get("Content-Type")), 0600)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"text":"harbour lighthouse","language":"en","duration":0.5,"segments":[{"id":0,"start":0,"end":0.5,"text":"harbour lighthouse"}]}`)
	})
	http.HandleFunc("/api/v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		logMu.Lock()
		if f, err := os.OpenFile(filepath.Join(evidence, "speech-bodies"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			_, _ = fmt.Fprintln(f, string(body))
			f.Close()
		}
		logMu.Unlock()
		if mode == "helper-speech-crash" {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "audio/l16;rate=24000;endianness=little-endian")
		_, _ = w.Write([]byte{1, 0, 2, 0, 3, 0})
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, authenticated); err != nil {
		os.Exit(5)
	}
}

func TestStartReadyAndOwnedClose(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s.AllowsModel("user.ApprovedKlein") || s.AllowsModel("user.Other") {
		t.Fatal("model allowlist did not match exact registered id")
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Base() != "http://127.0.0.1:"+strconv.Itoa(cfg.Port) {
		t.Fatalf("unexpected private address: %s", s.Base())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir(cfg), "shutdown-seen")); err != nil {
		t.Fatalf("owned child did not receive graceful shutdown: %v", err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		t.Fatalf("owned child still holds port: %v", err)
	}
	l.Close()
}

// The stand-in engine answers 401 without this launch's keys, as Lemonade
// does once LEMONADE_API_KEY and LEMONADE_ADMIN_API_KEY are set: readiness,
// the image route and shutdown all pass only with them.
func TestEveryRequestCarriesTheLaunchKey(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := s.keys
	request, err := http.NewRequest(http.MethodPost, s.Base()+"/api/v1/images/edits", strings.NewReader(`{"model":"user.ApprovedKlein"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer caller-supplied")
	response, err := s.HTTP().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || request.Header.Get("Authorization") != "Bearer caller-supplied" {
		t.Fatalf("image request status %d; caller header %q", response.StatusCode, request.Header.Get("Authorization"))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("admin-key shutdown: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.keys == first || s.keys.api == "" {
		t.Fatal("launch keys reused across launches")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.keys != (launchKeys{}) {
		t.Fatal("keys retained after the tree exited")
	}
}

// Control, MCP, pull and install routes never reach the running engine
// through the client OA callers use.
func TestCallerClientForwardsOnlyImageRoutes(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, route := range forbiddenRoutes {
		req, err := http.NewRequest(route.method, s.Base()+route.path, strings.NewReader(`{"host":"0.0.0.0"}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp, err := s.HTTP().Do(req); !errors.Is(err, ErrForbiddenRoute) {
			if resp != nil {
				resp.Body.Close()
			}
			t.Fatalf("%s %s forwarded: %v", route.method, route.path, err)
		}
	}
	if resp, err := s.HTTP().Do(opaqueRequest(t, s.Base())); !errors.Is(err, ErrForbiddenRoute) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("opaque URL forwarded: %v", err)
	}
	seen, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(seen)), "\n") {
		if line != "GET /api/v1/health" {
			t.Fatalf("engine received %q", line)
		}
	}
}

// The owner tables read through iphlpapi show the stand-in engine's listener
// under its own process id.
func TestLoopbackCheckReadsTheSystemTables(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sockets, err := identity.Sockets()
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pid := s.proc.PID()
	s.mu.Unlock()
	for _, l := range sockets {
		if l.Proto == "tcp" && l.Port == cfg.Port && l.PID == pid && l.Addr.String() == "127.0.0.1" {
			return
		}
	}
	t.Fatalf("listener 127.0.0.1:%d of process %d not in the owner table", cfg.Port, pid)
}

// widen adds a wildcard socket owned by the running tree to the real table,
// standing in for Lemonade after /internal/set moved its host to 0.0.0.0.
// The test binary itself never binds beyond loopback.
func widen(s *Server) func() ([]identity.Socket, error) {
	return func() ([]identity.Socket, error) {
		sockets, err := identity.Sockets()
		return append(sockets, identity.Socket{Proto: "tcp", Addr: netip.MustParseAddr("0.0.0.0"), Port: 13305, PID: s.proc.PID()}), err
	}
}

func TestWideListenerAtReadinessStopsTheTree(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.sockets = widen(s)
	if err := s.Start(context.Background()); !errors.Is(err, ErrListenerNotLoopback) || s.proc != nil {
		t.Fatalf("tree listening beyond loopback admitted: %v", err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		t.Fatalf("refused tree still holds its port: %v", err)
	}
	l.Close()
}

func TestWideListenerDuringServiceStopsTheTree(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.sockets = widen(s)
	s.mu.Unlock()
	request, err := http.NewRequest(http.MethodPost, s.Base()+"/api/v1/images/edits", strings.NewReader(`{"model":"user.ApprovedKlein"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := s.HTTP().Do(request); !errors.Is(err, ErrListenerNotLoopback) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("request served by a tree beyond loopback: %v", err)
	}
	if s.proc != nil {
		t.Fatal("tree beyond loopback left running")
	}
}

// A pickle weight in the private models folder refuses the launch.
func TestPickleWeightRefusesLaunch(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	path := filepath.Join(filepath.Dir(cfg.CacheDir), "engine-models", "models--org--repo", "snapshots", "rev", "model.ckpt")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pickle"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrWeightFormat) || s.proc != nil {
		t.Fatalf("pickle weight launched: %v", err)
	}
}

// A changed config.json refuses the launch before any process is created.
func TestLockedConfigMismatchRefusesLaunch(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ConfigDir, "config.json")
	locked, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(locked), `"no_fetch_executables":true`, `"no_fetch_executables":false`, 1)
	if changed == string(locked) {
		t.Fatal("fixture did not change the config")
	}
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrLockedConfig) || !errors.Is(err, ErrConfiguration) || s.proc != nil {
		t.Fatalf("changed config launched: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != changed {
		t.Fatal("refusal rewrote the evidence")
	}
}

// The child process itself sees only the allow-listed names, whatever the
// provider inherited.
func TestChildProcessSeesOnlyTheAllowList(t *testing.T) {
	for _, name := range []string{"HF_TOKEN", "LEMONADE_SDCPP_VULKAN_BIN", "LLAMA_ARG_HOST", "HTTPS_PROXY", "OPENAI_API_KEY"} {
		t.Setenv(name, "inherited")
	}
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seen, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "env-seen"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range strings.Split(string(seen), "\n") {
		name, _, _ := strings.Cut(item, "=")
		if !childEnvironmentNames[strings.ToUpper(name)] {
			t.Fatalf("child inherited %s", name)
		}
		if strings.EqualFold(name, "TEMP") && !strings.HasSuffix(item, filepath.Join(filepath.Dir(cfg.CacheDir), "engine-temp")) {
			t.Fatalf("child TEMP outside the private folder: %s", item)
		}
	}
}

// The stand-in lemond starts a stand-in llama-server the way Lemonade does on
// Windows, inheriting its environment block: the grandchild sees only the
// allow-listed names, none of the llama.cpp review's GGML_*, MTMD_* or
// LLAMA_* names the provider inherited.
func TestEngineGrandchildInheritsOnlyTheAllowList(t *testing.T) {
	for _, name := range []string{"GGML_BACKEND_PATH", "GGML_VK_VISIBLE_DEVICES", "MTMD_BACKEND_DEVICE", "LLAMA_ARG_MEDIA_PATH", "LLAMA_API_KEY", "HF_TOKEN"} {
		t.Setenv(name, "inherited")
	}
	cfg := fakeConfig(t, "helper-grandchild")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seen, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "grandchild-env"))
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(string(seen), "\n")
	if len(names) != len(childEnvironmentNames) {
		t.Fatalf("grandchild inherited %d names: %q", len(names), names)
	}
	for _, name := range names {
		upper := strings.ToUpper(name)
		if !childEnvironmentNames[upper] || forbiddenEnvironment(upper) {
			t.Fatalf("grandchild inherited %s", name)
		}
	}
}

func TestFailedGracefulShutdownReportsUnverifiedCleanup(t *testing.T) {
	cfg := fakeConfig(t, "helper-bad-shutdown")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed shutdown must report unverified descendants: %v", err)
	}
}

// An accepted shutdown whose tree outlives the grace is stopped by force;
// once the forced stop confirms the whole tree exited, the stop succeeds,
// with one line in the tree's output saying so. A refused shutdown still
// reports unverified cleanup (TestFailedGracefulShutdownReportsUnverifiedCleanup).
func TestAForcedStopThatConfirmsTheExitSucceeds(t *testing.T) {
	grace := shutdownGrace
	shutdownGrace = 200 * time.Millisecond
	defer func() { shutdownGrace = grace }()
	cfg := fakeConfig(t, "helper-slow-shutdown")
	output, err := os.Create(filepath.Join(t.TempDir(), "lemonade.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cfg.Output = output
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a forced stop that confirmed the exit: %v", err)
	}
	log, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "the forced stop confirmed its exit") {
		t.Fatalf("no line for the forced stop in the output:\n%s", log)
	}
}

func TestOccupiedPortAndFailedStartup(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("occupied port: %v", err)
	}
	if s.proc != nil {
		t.Fatal("occupied port launched child")
	}

	cfg = fakeConfig(t, "helper-exit")
	s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrUnavailable) || s.proc != nil {
		t.Fatalf("failed startup did not reap child: %v", err)
	}
}

func TestCanceledReadinessReapsChild(t *testing.T) {
	cfg := fakeConfig(t, "helper-stall")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := s.Start(ctx); !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) || s.proc != nil {
		t.Fatalf("canceled start did not reap child: %v", err)
	}
}

func TestCanceledBeforeStartLaunchesNothing(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.Canceled) || s.proc != nil {
		t.Fatalf("pre-canceled start launched child: %v", err)
	}
	if _, err := os.Stat(cfg.CacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-canceled start created private state: %v", err)
	}
}

func TestCloseCancelsBlockedStart(t *testing.T) {
	cfg := fakeConfig(t, "helper-stall")
	cfg.Ready = 10 * time.Second
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- s.Start(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		active := s.starting && s.proc != nil
		s.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("start never reached readiness wait")
		}
		time.Sleep(10 * time.Millisecond)
	}
	begin := time.Now()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 3*time.Second {
		t.Fatal("Close waited for readiness budget")
	}
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatalf("start was not canceled: %v", err)
	}
	if s.proc != nil {
		t.Fatal("canceled child tree was not confirmed stopped")
	}
}

func TestCanceledImageStopsOwnedTree(t *testing.T) {
	cfg := fakeConfig(t, "helper-ready")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base()+"/api/v1/images/generations", strings.NewReader(`{"model":"user.ApprovedKlein","prompt":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	requestDone := make(chan error, 1)
	go func() { _, err := s.HTTP().Do(request); requestDone <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(evidenceDir(cfg), "generation-started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never reached owned child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("canceled generation returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled generation did not stop promptly")
	}
	if s.proc != nil {
		t.Fatal("canceled generation left owned tree running")
	}
}
