package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrLockedConfig reports private Lemonade configuration that differs from
// the locked bytes, or an unexpected file beside it.
var ErrLockedConfig = errors.New("lemonade: private config.json differs from the locked configuration")

const configLimit = 64 << 10

// configTempPrefix names the provider's own temporary config files;
// os.CreateTemp appends decimal digits.
const configTempPrefix = ".server-write-"

// privateConfigFiles are the only files the provider admits in ConfigDir:
// the locked config.json and the model registry setup writes. Lemonade reads
// per-model backend arguments from recipe_options.json in the same folder
// (Lemonade 2026.39.1 src/cpp/server/model_manager.cpp), so any other file
// refuses the launch.
var privateConfigFiles = map[string]bool{"config.json": true, "user_models.json": true}

// LockedConfig is the complete config.json the provider writes for Lemonade
// 2026.39.1, per condition 4 of research/security-reviews/lemonade-2026.39.1.md:
// loopback host, no broadcast, offline, no executable fetching, no model
// update checks, no extra origins or cloud providers, telemetry off, no
// suspend inhibition, models inside provider state, the sd.cpp Vulkan backend,
// and for every backend section of the release's resources/defaults.json each
// `*_bin` set to "builtin", each `*_args` empty and `prefer_system` off.
// Lemonade deep-merges this over its defaults (config_file.cpp,
// ConfigFile::load), so every key named here wins over a changed default.
// json.Marshal sorts keys, which makes the bytes deterministic.
//
// whispercpp.backend is set explicitly to vulkan
// (research/security-reviews/whisper.cpp-v1.8.4-win.md condition 2):
// Lemonade's default auto takes the first supported entry, which starts with
// npu and can select the refused NPU build. LockedConfigWhisperCPU names the
// CPU build, the documented alternative.
func LockedConfig(modelsDir string) ([]byte, error) { return lockedConfigBytes(modelsDir, "vulkan") }

// LockedConfigWhisperCPU is LockedConfig with whisper.cpp's CPU backend, for
// a package entry that pairs WhisperCppCPU.
func LockedConfigWhisperCPU(modelsDir string) ([]byte, error) {
	return lockedConfigBytes(modelsDir, "cpu")
}

func lockedConfigBytes(modelsDir, whisperBackend string) ([]byte, error) {
	if !filepath.IsAbs(modelsDir) {
		return nil, fmt.Errorf("%w: models folder must be absolute", ErrLockedConfig)
	}
	if whisperBackend != "vulkan" && whisperBackend != "cpu" {
		return nil, fmt.Errorf("%w: whisper backend %q", ErrLockedConfig, whisperBackend)
	}
	cfg := lockedConfigFor(modelsDir, whisperBackend)
	if err := checkBackendArgs(cfg); err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

// lockedConfig is LockedConfig's configuration, the one the chat gate
// checks for backend arguments.
func lockedConfig(modelsDir string) map[string]any { return lockedConfigFor(modelsDir, "vulkan") }

// previousLockedConfig is the locked configuration before whispercpp.backend
// and the Vulkan build's vulkan_bin and vulkan_args were named (2026-10-01).
// A private config.json with exactly these bytes is replaced by the current
// locked bytes; any other difference refuses the launch.
func previousLockedConfig(modelsDir string) ([]byte, error) {
	cfg := lockedConfigFor(modelsDir, "vulkan")
	whisper := cfg["whispercpp"].(map[string]any)
	for _, key := range []string{"backend", "vulkan_args", "vulkan_bin"} {
		delete(whisper, key)
	}
	return json.Marshal(cfg)
}

// lockedConfigFor is the configuration LockedConfig checks and marshals,
// with whispercpp.backend set to whisperBackend.
func lockedConfigFor(modelsDir, whisperBackend string) map[string]any {
	cfg := lockedBase(modelsDir)
	cfg["whispercpp"].(map[string]any)["backend"] = whisperBackend
	return cfg
}

// lockedBase is the configuration without the speech backend choice.
func lockedBase(modelsDir string) map[string]any {
	bins := func(keys ...string) map[string]any {
		section := map[string]any{}
		for _, key := range keys {
			switch {
			case len(key) > 4 && key[len(key)-4:] == "_bin":
				section[key] = "builtin"
			default:
				section[key] = ""
			}
		}
		return section
	}
	llamacpp := bins("args", "cpu_args", "cpu_bin", "cuda_bin", "rocm_args", "rocm_bin", "vulkan_args", "vulkan_bin")
	llamacpp["prefer_system"] = false
	flm := bins("args")
	flm["prefer_system"] = false
	sdcpp := bins("args", "cpu_args", "cpu_bin", "cuda_args", "cuda_bin", "rocm_args", "rocm_bin", "vulkan_args", "vulkan_bin")
	sdcpp["backend"] = "vulkan"
	thenoise := bins("rocm_bin", "lora_dir", "upscaler_dir")
	return map[string]any{
		"config_version":           2,
		"host":                     "127.0.0.1",
		"broadcast":                false,
		"offline":                  true,
		"no_fetch_executables":     true,
		"auto_check_model_updates": false,
		"auto_update_models":       false,
		"allowed_origins":          "",
		"cloud_providers":          []string{},
		"extra_models_dir":         "",
		"models_dir":               modelsDir,
		"inhibit_suspend":          false,
		"telemetry":                map[string]any{"enabled": false},
		"acestep":                  bins("cuda_bin", "rocm_bin", "vulkan_bin"),
		"ds4":                      bins("args"),
		"flm":                      flm,
		"hrx":                      bins("args", "hrx_bin"),
		"kokoro":                   bins("cpu_bin"),
		"llamacpp":                 llamacpp,
		"moonshine":                bins("args", "cpu_args", "cpu_bin"),
		"onnxruntime":              bins("args", "cpu_args", "cpu_bin"),
		"openmoss":                 bins("cuda_bin", "rocm_bin", "vulkan_bin"),
		"ryzenai":                  bins("server_bin"),
		"sdcpp":                    sdcpp,
		"thenoise":                 thenoise,
		"thinksound":               bins("cuda_bin", "rocm_bin", "vulkan_bin"),
		"trellis":                  bins("args", "cuda_bin", "rocm_bin", "vulkan_bin"),
		"vllm":                     bins("args"),
		"whispercpp":               bins("args", "cpu_args", "cpu_bin", "npu_args", "npu_bin", "vulkan_args", "vulkan_bin"),
	}
}

// checkPrivateConfig writes the locked config.json into an empty private
// folder and otherwise compares it byte for byte. A difference or an
// unexpected file refuses the launch.
func checkPrivateConfig(configDir, modelsDir string) error {
	return checkPrivateConfigWith(configDir, modelsDir, LockedConfig)
}

// checkPrivateConfigWith is checkPrivateConfig against the locked bytes of
// one package entry's config function.
func checkPrivateConfigWith(configDir, modelsDir string, locked func(string) ([]byte, error)) error {
	if locked == nil {
		locked = LockedConfig
	}
	want, err := locked(modelsDir)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(configDir)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLockedConfig, err)
	}
	present := false
	for _, entry := range entries {
		if providerTemp(entry) {
			// Left by a provider that died between CreateTemp and Rename.
			if err := os.Remove(filepath.Join(configDir, entry.Name())); err != nil {
				return fmt.Errorf("%w: stale %s: %v", ErrLockedConfig, entry.Name(), err)
			}
			continue
		}
		if !privateConfigFiles[entry.Name()] || !entry.Type().IsRegular() {
			return fmt.Errorf("%w: unexpected entry %s", ErrLockedConfig, entry.Name())
		}
		present = present || entry.Name() == "config.json"
	}
	path := filepath.Join(configDir, "config.json")
	if !present {
		return writePrivate(path, want)
	}
	got, err := readSmall(path, configLimit)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLockedConfig, err)
	}
	if bytes.Equal(got, want) {
		return nil
	}
	// The lock before the whisper backend was named is replaced; nothing
	// else is.
	if previous, err := previousLockedConfig(modelsDir); err == nil && bytes.Equal(got, previous) {
		return writePrivate(path, want)
	}
	return ErrLockedConfig
}

// providerTemp reports a regular file named by writePrivate's CreateTemp:
// the prefix followed by one to ten decimal digits.
func providerTemp(entry os.DirEntry) bool {
	suffix, ok := strings.CutPrefix(entry.Name(), configTempPrefix)
	if !ok || len(suffix) < 1 || len(suffix) > 10 || !entry.Type().IsRegular() {
		return false
	}
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// checkCacheRoot admits only the `bin` folder in Lemonade's cache folder.
// At every start Lemonade 2026.39.1 moves config.json, jobs.json,
// recipe_options.json and user_models.json from the cache root into the
// config folder when the config folder lacks them (src/cpp/server/main.cpp
// line 153, migrate_legacy_paths; src/cpp/server/utils/path_utils.cpp
// migrate_legacy_json_files_to_config_dir). A file there would be installed
// after checkPrivateConfig ran, so any entry other than `bin` refuses the
// launch.
func checkCacheRoot(cacheDir string) error {
	entries, err := os.ReadDir(cacheDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cache folder: %v", ErrLockedConfig, err)
	}
	for _, entry := range entries {
		if entry.Name() != "bin" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: unexpected entry %s in the cache folder", ErrLockedConfig, entry.Name())
		}
	}
	return nil
}

func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), configTempPrefix)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
