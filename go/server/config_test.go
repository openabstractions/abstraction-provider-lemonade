package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockedConfigCarriesTheReviewConditions(t *testing.T) {
	models := filepath.Join(t.TempDir(), "engine-models")
	data, err := LockedConfig(models)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"host": "127.0.0.1", "broadcast": false, "offline": true, "no_fetch_executables": true,
		"auto_check_model_updates": false, "auto_update_models": false, "allowed_origins": "",
		"inhibit_suspend": false, "models_dir": models, "extra_models_dir": "", "config_version": float64(2),
	} {
		if cfg[key] != want {
			t.Fatalf("%s = %v, want %v", key, cfg[key], want)
		}
	}
	if providers, ok := cfg["cloud_providers"].([]any); !ok || len(providers) != 0 {
		t.Fatalf("cloud_providers = %v", cfg["cloud_providers"])
	}
	if telemetry, _ := cfg["telemetry"].(map[string]any); telemetry["enabled"] != false {
		t.Fatalf("telemetry = %v", cfg["telemetry"])
	}
	// Every backend section of Lemonade 2026.39.1's resources/defaults.json.
	for _, section := range []string{"acestep", "ds4", "flm", "hrx", "kokoro", "llamacpp", "moonshine", "onnxruntime",
		"openmoss", "ryzenai", "sdcpp", "thenoise", "thinksound", "trellis", "vllm", "whispercpp"} {
		values, ok := cfg[section].(map[string]any)
		if !ok || len(values) == 0 {
			t.Fatalf("backend section %s not locked", section)
		}
		for key, value := range values {
			switch {
			case strings.HasSuffix(key, "_bin") && value != "builtin":
				t.Fatalf("%s.%s = %v", section, key, value)
			case (key == "args" || strings.HasSuffix(key, "_args")) && value != "":
				t.Fatalf("%s.%s = %v", section, key, value)
			case key == "prefer_system" && value != false:
				t.Fatalf("%s.prefer_system = %v", section, value)
			}
		}
	}
	for _, section := range []string{"llamacpp", "flm"} {
		if cfg[section].(map[string]any)["prefer_system"] != false {
			t.Fatalf("%s resolves executables through PATH", section)
		}
	}
	if sd := cfg["sdcpp"].(map[string]any); sd["backend"] != "vulkan" || sd["vulkan_args"] != "" || sd["args"] != "" {
		t.Fatalf("sdcpp = %v", sd)
	}
	for _, forbidden := range []string{"--sleep-idle-seconds", "--rpc", "--tools", "--agent", "mcp", "-hf", "--model-url", "0.0.0.0"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("locked config carries %s", forbidden)
		}
	}
	if _, err := LockedConfig("engine-models"); !errors.Is(err, ErrLockedConfig) {
		t.Fatalf("relative models folder accepted: %v", err)
	}
}

// Every llama.cpp option the b10825 review forbids, in each spelling
// llama-server accepts, is refused in an argument string Lemonade would pass
// on; reviewed options pass.
func TestForbiddenLlamaFlags(t *testing.T) {
	for _, args := range []string{
		"--sleep-idle-seconds 60", "--host 0.0.0.0", "--rpc 192.0.2.1:50052", "--tools all", "--tools-runtime docker", "-ag", "--agent",
		"--mcp-servers-config mcp.json", "--mcp-servers-json {}", "--webui-mcp-proxy", "--ui-mcp-proxy",
		"-hf org/repo", "-hfr org/repo", "--hf-repo org/repo", "-hff m.gguf", "--hf-file m.gguf", "-hft tok", "--hf-token tok",
		"-mu https://x.invalid/m.gguf", "--model-url https://x.invalid/m.gguf", "-mmu https://x.invalid/p.gguf", "--mmproj-url https://x.invalid/p.gguf",
		"-dr org/model", "--docker-repo org/model", "--spec-draft-hf org/repo", "-hfd org/repo", "-hfrd org/repo", "--hf-repo-draft org/repo",
		"--models-dir C:\\m", "--models-preset C:\\p.ini", "--media-path C:\\media\\", "--path C:\\ui", "--ui-config-file C:\\ui.json",
		"--webui-config-file C:\\ui.json", "--slot-save-path C:\\slots", "--log-file C:\\l.log", "--log-prompts-dir C:\\p",
		"--video-ffmpeg-dir C:\\ffmpeg", "--cors-origins *",
		"--temp 0.6 --tools=all", "--temp 0.6 '--rpc' x", `--temp 0.6 "--host=0.0.0.0"`,
		// Lemonade's tokenizer splices quoted pieces into one word.
		`--ho"st" 0.0.0.0`, `--t'ools' all`, `-"-rpc" x`, `--temp "0.6"`,
	} {
		if err := checkLlamaArgs(args); !errors.Is(err, ErrForbiddenLlamaFlag) {
			t.Fatalf("%q accepted: %v", args, err)
		}
	}
	for _, args := range []string{"", "--temp 0.6 --top-p 0.95", "-b 2048 -ub 1024"} {
		if err := checkLlamaArgs(args); err != nil {
			t.Fatalf("%q refused: %v", args, err)
		}
	}
}

// Review condition 2 keeps llamacpp_args empty in config.json: a backend
// argument in the locked configuration is refused before it is written, and
// a forbidden llama.cpp option is named as one.
func TestLockedConfigHoldsNoBackendArguments(t *testing.T) {
	models := filepath.Join(t.TempDir(), "engine-models")
	cfg := lockedConfig(models)
	if err := checkBackendArgs(cfg); err != nil {
		t.Fatalf("locked config refused: %v", err)
	}
	for section, key := range map[string]string{"llamacpp": "vulkan_args", "sdcpp": "args", "whispercpp": "npu_args"} {
		cfg := lockedConfig(models)
		cfg[section].(map[string]any)[key] = "--temp 0.6"
		if err := checkBackendArgs(cfg); !errors.Is(err, ErrLockedConfig) {
			t.Fatalf("%s.%s accepted: %v", section, key, err)
		}
	}
	cfg = lockedConfig(models)
	cfg["llamacpp"].(map[string]any)["args"] = "--tools all"
	if err := checkBackendArgs(cfg); !errors.Is(err, ErrLockedConfig) || !errors.Is(err, ErrForbiddenLlamaFlag) {
		t.Fatalf("forbidden llama.cpp option in the locked config: %v", err)
	}
}

// A provider killed between creating its temporary file and renaming it
// leaves that file behind; the next launch removes it and keeps every other
// refusal.
func TestInterruptedConfigWriteDoesNotBlockLaunches(t *testing.T) {
	models := filepath.Join(t.TempDir(), "engine-models")
	dir := t.TempDir()
	if err := checkPrivateConfig(dir, models); err != nil {
		t.Fatal(err)
	}
	stale, err := os.CreateTemp(dir, configTempPrefix)
	if err != nil {
		t.Fatal(err)
	}
	stale.Close()
	if err := checkPrivateConfig(dir, models); err != nil {
		t.Fatalf("interrupted provider write blocks the launch: %v", err)
	}
	if _, err := os.Lstat(stale.Name()); !os.IsNotExist(err) {
		t.Fatalf("stale temporary file kept: %v", err)
	}
	for _, name := range []string{".write-123", configTempPrefix + "x1", configTempPrefix} {
		dir := t.TempDir()
		if err := checkPrivateConfig(dir, models); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkPrivateConfig(dir, models); !errors.Is(err, ErrLockedConfig) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, configTempPrefix+"42"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateConfig(dir, models); !errors.Is(err, ErrLockedConfig) {
		t.Fatalf("folder with the temporary name accepted: %v", err)
	}
}

func TestPrivateConfigIsWrittenOnceThenCompared(t *testing.T) {
	models := filepath.Join(t.TempDir(), "engine-models")
	locked, err := LockedConfig(models)
	if err != nil {
		t.Fatal(err)
	}
	fresh := func(t *testing.T) string {
		dir := t.TempDir()
		if err := checkPrivateConfig(dir, models); err != nil {
			t.Fatalf("empty private folder: %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "config.json")); err != nil || !bytes.Equal(got, locked) {
			t.Fatalf("written config differs: %v", err)
		}
		if err := checkPrivateConfig(dir, models); err != nil {
			t.Fatalf("locked config refused: %v", err)
		}
		return dir
	}
	fresh(t)
	var indented bytes.Buffer
	if err := json.Indent(&indented, locked, "", "  "); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(dir string) error{
		"offline off": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "config.json"), bytes.Replace(locked, []byte(`"offline":true`), []byte(`"offline":false`), 1), 0600)
		},
		"backend arguments": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "config.json"), bytes.Replace(locked, []byte(`"vulkan_args":""`), []byte(`"vulkan_args":"--listen-ip 0.0.0.0"`), 1), 0600)
		},
		"same values, other bytes": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "config.json"), indented.Bytes(), 0600)
		},
		"other models folder": func(dir string) error {
			other, err := LockedConfig(filepath.Join(filepath.Dir(models), "elsewhere"))
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "config.json"), other, 0600)
		},
		"per-model recipe options": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "recipe_options.json"), []byte(`{}`), 0600)
		},
		"interrupted Lemonade save": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "config.json.tmp"), locked, 0600)
		},
		"subfolder": func(dir string) error { return os.Mkdir(filepath.Join(dir, "profiles"), 0700) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := fresh(t)
			if err := mutate(dir); err != nil {
				t.Fatal(err)
			}
			if err := checkPrivateConfig(dir, models); !errors.Is(err, ErrLockedConfig) {
				t.Fatalf("%s accepted: %v", name, err)
			}
		})
	}
}
