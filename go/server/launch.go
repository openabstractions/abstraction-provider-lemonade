package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/openabstractions/abstraction-facade/go/supervision"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/llamacpp"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
)

// ErrForbiddenArgument reports a lemond argument outside the allow-list.
var ErrForbiddenArgument = errors.New("lemonade: launch argument outside the allow-list")

// LaunchPlan is the exact process the provider creates: the reviewed lemond
// executable, its complete argument list and its complete environment.
type LaunchPlan struct {
	Program string
	Args    []string
	Env     []string
	// Dir is the working directory: the package folder holding Program,
	// which the launch proves and holds. whisper-server and koko inherit it
	// from Lemonade; the whisper.cpp review's W1 reads request bytes as a
	// path relative to it, and espeak-rs prefers its espeak-ng-data folder
	// (whisper.cpp condition 5, Kokoros condition 5).
	Dir string
}

// lemondFlags is the complete set of lemond options OA passes, each with the
// only value it may carry. lemond reads its positional cache and config
// folders first (Lemonade 2026.39.1 src/cpp/server/main.cpp, CLIParser).
// Nothing else reaches the command line: llama.cpp options such as
// --sleep-idle-seconds, --host, --rpc, --tools, --agent, the MCP options, -hf
// and --model-url are refused here, and the locked config.json keeps every
// Lemonade backend `*_args` value empty.
var lemondFlags = map[string]func(value string, cfg Config) bool{
	"--port":         func(v string, cfg Config) bool { return v == strconv.Itoa(cfg.Port) },
	"--host":         func(v string, cfg Config) bool { return v == "127.0.0.1" },
	"--no-broadcast": nil,
}

func lemondArgs(cfg Config) []string {
	return []string{cfg.CacheDir, cfg.ConfigDir, "--port", strconv.Itoa(cfg.Port), "--host", "127.0.0.1", "--no-broadcast"}
}

// validateLemondArgs accepts exactly the private folders followed by each
// allow-listed flag once, with its only permitted value.
func validateLemondArgs(args []string, cfg Config) error {
	if len(args) < 2 || args[0] != cfg.CacheDir || args[1] != cfg.ConfigDir {
		return fmt.Errorf("%w: positional state folders", ErrForbiddenArgument)
	}
	seen := map[string]bool{}
	for i := 2; i < len(args); i++ {
		flag := args[i]
		check, ok := lemondFlags[flag]
		if !ok || seen[flag] {
			return fmt.Errorf("%w: %q", ErrForbiddenArgument, flag)
		}
		seen[flag] = true
		if check == nil {
			continue
		}
		if i+1 >= len(args) || !check(args[i+1], cfg) {
			return fmt.Errorf("%w: %s value", ErrForbiddenArgument, flag)
		}
		i++
	}
	if len(seen) != len(lemondFlags) {
		return fmt.Errorf("%w: required flag missing", ErrForbiddenArgument)
	}
	return nil
}

// launchKeys are the per-launch credentials. Lemonade 2026.39.1 reads
// LEMONADE_API_KEY and LEMONADE_ADMIN_API_KEY from its environment
// (src/cpp/server/server.cpp lines 660-668) and, when set, answers 401 to any
// /api, /v0, /v1, /mcp or /metrics request without one of them and to any
// /internal/* request without the admin key (lines 961-1017). The embeddable
// guide (docs/embeddable/runtime.md, "Launching" and "Authenticating
// Requests") passes the key in the child environment and sends it as
// `Authorization: Bearer KEY` on every request. The keys live only in this
// process's memory and the child's environment block.
type launchKeys struct {
	api, admin string
}

func newLaunchKeys(random io.Reader) (launchKeys, error) {
	var raw [64]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return launchKeys{}, fmt.Errorf("lemonade: launch key: %w", err)
	}
	keys := launchKeys{api: hex.EncodeToString(raw[:32]), admin: hex.EncodeToString(raw[32:])}
	clear(raw[:])
	if keys.api == keys.admin {
		return launchKeys{}, errors.New("lemonade: launch keys not distinct")
	}
	return keys, nil
}

// ErrForbiddenEnvironment reports a child environment entry outside the
// allow-list.
var ErrForbiddenEnvironment = errors.New("lemonade: environment variable outside the allow-list")

// childEnvironmentNames holds the fixed entries; exact selected OA resolver
// routing is validated separately. The Lemonade review's condition 3 drops HF_TOKEN, HF_ENDPOINT,
// HF_HOME, HF_HUB_CACHE, every other LEMONADE_* (for example
// LEMONADE_<RECIPE>_<BACKEND>_BIN, which replaces a backend executable, and
// LEMONADE_DEFAULTS_PATH), LLAMA_ARG_*, LLAMA_CACHE, proxy variables and
// cloud keys. SystemRoot, windir and SystemDrive let Windows components and
// Winsock load; TEMP and TMP point at the provider's private folder.
var childEnvironmentNames = map[string]bool{
	"SYSTEMROOT": true, "WINDIR": true, "SYSTEMDRIVE": true, "PATH": true, "TEMP": true, "TMP": true,
	"LEMONADE_API_KEY": true, "LEMONADE_ADMIN_API_KEY": true,
}

// childEnvironment builds the environment from nothing through the provider
// kit's clean-environment builder (inference CONTRACT.md INF-EL1), sorted as
// CreateProcess expects. PATH names only the Windows system folders: the
// reviews name no other executable location, and the locked config keeps
// llamacpp.prefer_system and flm.prefer_system off, so no backend is
// resolved through PATH.
func childEnvironment(windowsDir, tempDir string, keys launchKeys, runtimeEndpoint string) ([]string, error) {
	set := []string{"LEMONADE_API_KEY=" + keys.api, "LEMONADE_ADMIN_API_KEY=" + keys.admin}
	if runtimeEndpoint != "" {
		set = append(set, bootstrap.RuntimeEndpointVariable+"="+runtimeEndpoint)
	}
	env, err := supervision.CleanEnvironment{
		Temp: tempDir, WindowsDir: windowsDir, WindowsDefaultPath: true,
		Set: set,
	}.Build()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrForbiddenEnvironment, err)
	}
	return env, nil
}

// forbiddenEnvironment names are refused whatever childEnvironmentNames
// says. Lemonade 2026.39.1 on Windows starts llama-server and sd-server with
// its own environment block unchanged (CreateProcessA with a null environment
// when a backend adds no variable,
// src/cpp/server/utils/platform/process_windows.cpp; the Vulkan backends add
// none, and lemond never sets a variable of its own on Windows), so the child
// environment is also every engine's. The provider kit's
// llamacpp.ForbiddenEnvironment holds the llama.cpp b10825 review's
// condition 3 (inference CONTRACT.md INF-LC2). LLAMA_API_KEY, which that list
// leaves to a provider that sets its own key, is refused here: this provider
// gives llama-server no key. LEMONADE_* other than the two launch keys
// replace backends or defaults.
var launchKeyNames = map[string]bool{"LEMONADE_API_KEY": true, "LEMONADE_ADMIN_API_KEY": true}

// forbiddenEnvironment also refuses the speech engines' names: the
// whisper.cpp review's WHISPER_*, OVERRIDE_VITISAI_* and KMP_* and the
// Kokoros review's PIPER_*, ESPEAK*, ORT_* and SSL_CERT_* (condition 4 of
// each; inference CONTRACT.md INF-WC2, INF-KO2). Lemonade sets its own
// ESPEAK_DATA_PATH for koko inside its process; the provider gives it none.
func forbiddenEnvironment(upper string) bool {
	if llamacpp.ForbiddenEnvironment(upper) || whispercpp.ForbiddenEnvironment(upper) || kokoro.ForbiddenEnvironment(upper) || upper == "LLAMA_API_KEY" {
		return true
	}
	return strings.HasPrefix(upper, "LEMONADE_") && !launchKeyNames[upper]
}

func validateEnvironment(env []string, runtimeEndpoint string) error {
	seen := map[string]bool{}
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		routing := upper == bootstrap.RuntimeEndpointVariable
		allowed := childEnvironmentNames[upper]
		if routing {
			allowed = runtimeEndpoint != "" && name == bootstrap.RuntimeEndpointVariable && value == runtimeEndpoint
		}
		if !ok || !allowed || forbiddenEnvironment(upper) || seen[upper] || value == "" {
			return fmt.Errorf("%w: %q", ErrForbiddenEnvironment, name)
		}
		seen[upper] = true
	}
	expected := len(childEnvironmentNames)
	if runtimeEndpoint != "" {
		expected++
		if !seen[bootstrap.RuntimeEndpointVariable] {
			return fmt.Errorf("%w: selected runtime missing", ErrForbiddenEnvironment)
		}
	}
	if len(seen) != expected {
		return fmt.Errorf("%w: incomplete", ErrForbiddenEnvironment)
	}
	return nil
}

// plan builds and validates the launch plan for one set of keys. It starts
// nothing.
func (s *Server) plan(keys launchKeys) (LaunchPlan, error) { return s.planFor(s.cfg, keys) }

// planFor builds and validates the launch plan of one launch's
// configuration.
func (s *Server) planFor(cfg Config, keys launchKeys) (LaunchPlan, error) {
	args := lemondArgs(cfg)
	if err := validateLemondArgs(args, cfg); err != nil {
		return LaunchPlan{}, err
	}
	windowsDir, err := s.systemDir()
	if err != nil || !filepath.IsAbs(windowsDir) {
		return LaunchPlan{}, fmt.Errorf("%w: Windows folder: %v", ErrForbiddenEnvironment, err)
	}
	env, err := childEnvironment(windowsDir, cfg.TempDir, keys, cfg.RuntimeEndpoint)
	if err != nil {
		return LaunchPlan{}, err
	}
	if err := validateEnvironment(env, cfg.RuntimeEndpoint); err != nil {
		return LaunchPlan{}, err
	}
	return LaunchPlan{Program: cfg.Program, Args: args, Env: env, Dir: filepath.Dir(cfg.Program)}, nil
}
