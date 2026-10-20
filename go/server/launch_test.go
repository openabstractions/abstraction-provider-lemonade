package server

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// windowsLaunch skips a test of the launch plan or its environment, which
// is built from the Windows system folder: the reviewed Lemonade package is
// Windows x64 only (packaging/windows-amd64.json, system_other.go).
func windowsLaunch(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the reviewed Lemonade package and its launch plan are Windows only")
	}
}

func planConfig(t *testing.T) Config {
	t.Helper()
	windowsLaunch(t)
	dir := t.TempDir()
	return Config{
		Program: os.Args[0], CacheDir: filepath.Join(dir, "engine-cache"), ConfigDir: filepath.Join(dir, "engine-config"),
		Port: 41234, Models: []string{"user.ApprovedKlein"},
	}
}

func TestChildEnvironmentIsBuiltFromNothing(t *testing.T) {
	for name, value := range map[string]string{
		"HF_TOKEN": "hf_secret", "HF_ENDPOINT": "https://mirror.invalid", "HF_HOME": `C:\hf`, "HF_HUB_CACHE": `C:\hf\hub`,
		"LEMONADE_SDCPP_VULKAN_BIN": `C:\planted\sd-server.exe`, "LEMONADE_DEFAULTS_PATH": `C:\planted\defaults.json`,
		"LEMONADE_ALLOWED_ORIGINS": "https://example.invalid", "LEMONADE_OPENAI_API_KEY": "sk-cloud",
		"LLAMA_ARG_HOST": "0.0.0.0", "LLAMA_ARG_RPC": "192.0.2.1:50052", "LLAMA_CACHE": `C:\llama`,
		"HTTP_PROXY": "http://proxy.invalid:8080", "HTTPS_PROXY": "http://proxy.invalid:8080", "ALL_PROXY": "socks5://proxy.invalid",
		"NO_PROXY": "*", "OPENAI_API_KEY": "sk-user", "ROCM_PATH": `C:\planted\rocm`, "VK_ICD_FILENAMES": `C:\planted\icd.json`,
		"SystemRoot": `C:\planted`, "PATH": `C:\planted;C:\Windows\System32`,
	} {
		t.Setenv(name, value)
	}
	cfg := planConfig(t)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.systemDir = func() (string, error) { return `C:\Windows`, nil }
	keys := launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)}
	plan, err := s.plan(keys)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(filepath.Dir(cfg.CacheDir), "engine-temp")
	want := []string{
		"LEMONADE_ADMIN_API_KEY=" + keys.admin, "LEMONADE_API_KEY=" + keys.api,
		`PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem`,
		"SystemDrive=C:", `SystemRoot=C:\Windows`, "TEMP=" + temp, "TMP=" + temp, `windir=C:\Windows`,
	}
	if !slices.Equal(plan.Env, want) {
		// Names only: a failing run must not print the inherited values.
		names := make([]string, 0, len(plan.Env))
		for _, item := range plan.Env {
			name, _, _ := strings.Cut(item, "=")
			names = append(names, name)
		}
		t.Fatalf("child environment names %q differ from the allow-list", names)
	}
	for _, bad := range []string{"HF_TOKEN=x", "LEMONADE_SDCPP_VULKAN_BIN=x", "LLAMA_ARG_HOST=0.0.0.0", "HTTPS_PROXY=x", "OPENAI_API_KEY=x"} {
		if err := validateEnvironment(append(slices.Clone(want), bad)); !errors.Is(err, ErrForbiddenEnvironment) {
			t.Fatalf("%s accepted: %v", bad, err)
		}
	}
}

// Lemonade on Windows starts llama-server and sd-server with its own
// environment block (CreateProcessA with a null environment when the Vulkan
// backend adds no variable, process_windows.cpp), so the names the llama.cpp
// b10825 review forbids are refused by name as well: GGML_BACKEND_PATH loads
// the DLL it names, MTMD_* reach the multimodal helper, LLAMA_* carry
// LLAMA_ARG_* options, LLAMA_API_KEY and LLAMA_CACHE. A name added to the
// allow-list by mistake is still refused.
func TestForbiddenEngineEnvironmentRefusedWhateverTheAllowList(t *testing.T) {
	windowsLaunch(t)
	temp := filepath.Join(t.TempDir(), "engine-temp")
	keys := launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)}
	base, err := childEnvironment(`C:\Windows`, temp, keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEnvironment(base); err != nil {
		t.Fatalf("reviewed environment refused: %v", err)
	}
	for _, name := range []string{
		"GGML_BACKEND_PATH", "GGML_VK_VISIBLE_DEVICES", "ggml_backend_path", "MTMD_BACKEND_DEVICE", "LLAMA_ARG_MEDIA_PATH",
		"LLAMA_API_KEY", "LLAMA_CACHE", "HF_TOKEN", "HTTPS_PROXY", "http_proxy", "ALL_PROXY", "NO_PROXY", "LEMONADE_SDCPP_VULKAN_BIN",
	} {
		upper := strings.ToUpper(name)
		childEnvironmentNames[upper] = true
		err := validateEnvironment(append(slices.Clone(base), name+"=x"))
		delete(childEnvironmentNames, upper)
		if !errors.Is(err, ErrForbiddenEnvironment) {
			t.Fatalf("%s accepted once allow-listed: %v", name, err)
		}
	}
}

func TestLaunchPlanIsTheAllowList(t *testing.T) {
	cfg := planConfig(t)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.plan(launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{cfg.CacheDir, cfg.ConfigDir, "--port", "41234", "--host", "127.0.0.1", "--no-broadcast"}
	if plan.Program != cfg.Program || !slices.Equal(plan.Args, want) {
		t.Fatalf("launch plan %q %q", plan.Program, plan.Args)
	}
}

func TestLaunchKeysComeFromTheRandomSource(t *testing.T) {
	a, err := newLaunchKeys(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newLaunchKeys(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if a == b || a.api == a.admin || !hex64.MatchString(a.api) || !hex64.MatchString(a.admin) {
		t.Fatalf("keys not fresh, distinct 256-bit values: %+v %+v", a, b)
	}
	if _, err := newLaunchKeys(bytes.NewReader(make([]byte, 64))); err == nil {
		t.Fatal("identical admin and API keys accepted")
	}
	if _, err := newLaunchKeys(bytes.NewReader(make([]byte, 40))); err == nil {
		t.Fatal("short random read accepted")
	}
}

func TestLaunchKeysTravelOnlyInTheChildEnvironment(t *testing.T) {
	t.Setenv("LEMONADE_API_KEY", "inherited-api")
	t.Setenv("LEMONADE_ADMIN_API_KEY", "inherited-admin")
	s, err := New(planConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	keys := launchKeys{api: strings.Repeat("a", 64), admin: strings.Repeat("b", 64)}
	plan, err := s.plan(keys)
	if err != nil {
		t.Fatal(err)
	}
	var api, admin []string
	for _, item := range plan.Env {
		name, value, _ := strings.Cut(item, "=")
		switch strings.ToUpper(name) {
		case "LEMONADE_API_KEY":
			api = append(api, value)
		case "LEMONADE_ADMIN_API_KEY":
			admin = append(admin, value)
		}
	}
	if !slices.Equal(api, []string{keys.api}) || !slices.Equal(admin, []string{keys.admin}) {
		t.Fatalf("child keys %q %q", api, admin)
	}
	for _, arg := range plan.Args {
		if strings.Contains(arg, keys.api) || strings.Contains(arg, keys.admin) {
			t.Fatal("key on the command line")
		}
	}
}

// The provider never passes these llama.cpp or lemond options. Each is refused
// by the allow-list wherever it appears in the argument list.
func TestForbiddenLaunchArgumentsRefused(t *testing.T) {
	cfg := planConfig(t)
	base := lemondArgs(cfg)
	if err := validateLemondArgs(base, cfg); err != nil {
		t.Fatalf("reviewed plan refused: %v", err)
	}
	for _, extra := range [][]string{
		{"--sleep-idle-seconds", "60"}, {"--rpc", "192.0.2.1:50052"}, {"--tools", "all"}, {"--agent"},
		{"--mcp-servers-config", "mcp.json"}, {"--mcp-servers-json", "{}"}, {"--webui-mcp-proxy"},
		{"-hf", "org/repo"}, {"--hf-repo", "org/repo"}, {"--model-url", "https://example.invalid/m.gguf"},
		{"--slot-save-path", "C:\\slots"}, {"--llamacpp-args", "--rpc x"}, {"--port", "41234"}, {"--host", "127.0.0.1"},
		{"--log-file", "C:\\out.log"}, {"C:\\another-cache"},
	} {
		for _, at := range []int{2, len(base)} {
			args := slices.Concat(base[:at], extra, base[at:])
			if err := validateLemondArgs(args, cfg); !errors.Is(err, ErrForbiddenArgument) {
				t.Fatalf("%q at %d accepted: %v", extra, at, err)
			}
		}
	}
	wide := slices.Clone(base)
	wide[slices.Index(wide, "--host")+1] = "0.0.0.0"
	if err := validateLemondArgs(wide, cfg); !errors.Is(err, ErrForbiddenArgument) {
		t.Fatalf("wildcard host accepted: %v", err)
	}
	other := slices.Clone(base)
	other[slices.Index(other, "--port")+1] = "13305"
	if err := validateLemondArgs(other, cfg); !errors.Is(err, ErrForbiddenArgument) {
		t.Fatalf("other port accepted: %v", err)
	}
	if err := validateLemondArgs(slices.DeleteFunc(slices.Clone(base), func(a string) bool { return a == "--no-broadcast" }), cfg); !errors.Is(err, ErrForbiddenArgument) {
		t.Fatalf("broadcast left on: %v", err)
	}
	for _, token := range base {
		if strings.HasPrefix(strings.ToUpper(token), "LLAMA_ARG_") {
			t.Fatalf("launch plan carries %s", token)
		}
	}
}
