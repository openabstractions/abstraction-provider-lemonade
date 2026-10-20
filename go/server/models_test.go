package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pins below were read from the Hugging Face API on 2026-10-01 (each
// file's LFS oid and size at the named repository revision).
var wantImageFiles = map[string][]ModelFile{
	"user.Z-Image-Turbo": {
		{Role: "main", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/diffusion_models/z_image_turbo_bf16.safetensors", Bytes: 12309866400, SHA256: "2407613050b809ffdff18a4ac99af83ea6b95443ecebdf80e064a79c825574a6"},
		{Role: "text_encoder", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/text_encoders/qwen_3_4b.safetensors", Bytes: 8044982048, SHA256: "6c671498573ac2f7a5501502ccce8d2b08ea6ca2f661c458e708f36b36edfc5a"},
		{Role: "vae", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/vae/ae.safetensors", Bytes: 335304388, SHA256: "afc8e28272cd15db3919bacdb6918ce9c1ed22e96cb12c4d5ed0fba823529e38"},
	},
	"user.Qwen-Image-2512": {
		{Role: "main", Repository: "unsloth/Qwen-Image-2512-GGUF", Revision: "1626d7531f84b4d2ea1cd6d2e69f41ec027dd354", Path: "qwen-image-2512-Q5_0.gguf", Bytes: 14400813600, SHA256: "c077d5a3f867b6441235d071d76912ce6575db9832651e8b1769e8329d33d798"},
		{Role: "text_encoder", Repository: "unsloth/Qwen2.5-VL-7B-Instruct-GGUF", Revision: "68bb8bc4b7df5289c143aaec0ab477a7d4051aab", Path: "Qwen2.5-VL-7B-Instruct-UD-Q4_K_XL.gguf", Bytes: 4785159040, SHA256: "8edd10e3ae170ab8b8d114e70763ff63a220f16deec9a56acf9330034efc0bb5"},
		{Role: "vae", Repository: "Comfy-Org/Qwen-Image_ComfyUI", Revision: "1f12b17be14c89b026c51a91d67c32f84bb047bc", Path: "split_files/vae/qwen_image_vae.safetensors", Bytes: 253806246, SHA256: "a70580f0213e67967ee9c95f05bb400e8fb08307e017a924bf3441223e023d1f"},
	},
}

func TestImageModelsDeclareThePinnedModels(t *testing.T) {
	if err := ValidImageModels(ImageModels); err != nil {
		t.Fatal(err)
	}
	for id, files := range wantImageFiles {
		m, ok := FindImageModel(id)
		if !ok {
			t.Fatalf("%s is not declared", id)
		}
		if len(m.Files) != len(files) {
			t.Fatalf("%s files = %+v", id, m.Files)
		}
		for i := range files {
			if m.Files[i] != files[i] {
				t.Fatalf("%s file %d = %+v, want %+v", id, i, m.Files[i], files[i])
			}
		}
		if m.License.Name != "Apache-2.0" || m.License.URL != "https://www.apache.org/licenses/LICENSE-2.0" || m.License.Text == "" || !strings.HasPrefix(m.Source, "https://huggingface.co/") {
			t.Fatalf("%s licence = %+v, source %q", id, m.License, m.Source)
		}
	}
	// Lemonade 2026.39.1's server_models.json defaults for each model.
	z, _ := FindImageModel("user.Z-Image-Turbo")
	if z.Preset != (ImagePreset{Steps: 9, CFGScale: 1, Width: 1024, Height: 1024}) || z.Edit {
		t.Fatalf("Z-Image-Turbo preset = %+v edit %v", z.Preset, z.Edit)
	}
	q, _ := FindImageModel("user.Qwen-Image-2512")
	if q.Preset != (ImagePreset{Steps: 20, CFGScale: 2.5, Width: 512, Height: 512, Sampler: "euler", FlowShift: 3, EngineFlags: "--diffusion-fa --offload-to-cpu"}) || q.Edit {
		t.Fatalf("Qwen-Image-2512 preset = %+v edit %v", q.Preset, q.Edit)
	}
	k, _ := FindImageModel("user.LocalApprovedKlein")
	if k.Preset != (ImagePreset{Steps: 4, CFGScale: 1, Width: 512, Height: 512}) || !k.Edit || k.Bytes() != 16132299052 {
		t.Fatalf("Klein = %+v", k)
	}
	if z.Bytes() != 20690152836 || q.Bytes() != 19439778886 {
		t.Fatalf("sizes %d %d", z.Bytes(), q.Bytes())
	}
}

func TestImageModelRefusesUnpinnedOrPickleDeclarations(t *testing.T) {
	base, _ := FindImageModel("user.Qwen-Image-2512")
	edit := func(change func(*ImageModel)) ImageModel {
		m := base
		m.Files = append([]ModelFile(nil), base.Files...)
		change(&m)
		return m
	}
	for name, m := range map[string]ImageModel{
		"ckpt main":       edit(func(m *ImageModel) { m.Files[0].Path = "qwen-image-2512.ckpt" }),
		"pt encoder":      edit(func(m *ImageModel) { m.Files[1].Path = "encoder.pt" }),
		"bin vae":         edit(func(m *ImageModel) { m.Files[2].Path = "split_files/vae/vae.bin" }),
		"short digest":    edit(func(m *ImageModel) { m.Files[0].SHA256 = "c077" }),
		"upper digest":    edit(func(m *ImageModel) { m.Files[0].SHA256 = strings.ToUpper(m.Files[0].SHA256) }),
		"branch revision": edit(func(m *ImageModel) { m.Files[0].Revision = "main" }),
		"no size":         edit(func(m *ImageModel) { m.Files[0].Bytes = 0 }),
		"escaping path":   edit(func(m *ImageModel) { m.Files[0].Path = "../qwen.gguf" }),
		"bad repository":  edit(func(m *ImageModel) { m.Files[0].Repository = "unsloth" }),
		"unknown role":    edit(func(m *ImageModel) { m.Files[0].Role = "lora" }),
		"repeated role":   edit(func(m *ImageModel) { m.Files[1].Role = "main" }),
		"no main":         edit(func(m *ImageModel) { m.Files = m.Files[1:] }),
		"zero steps":      edit(func(m *ImageModel) { m.Preset.Steps = 0 }),
		"wide preset":     edit(func(m *ImageModel) { m.Preset.Width = 100000 }),
		"unknown sampler": edit(func(m *ImageModel) { m.Preset.Sampler = "--lora-model-dir" }),
		"negative cfg":    edit(func(m *ImageModel) { m.Preset.CFGScale = -1 }),
		"unreviewed flag": edit(func(m *ImageModel) { m.Preset.EngineFlags = "--diffusion-fa --lora-model-dir C:/loras" }),
		"listen flag":     edit(func(m *ImageModel) { m.Preset.EngineFlags = "--listen-ip 0.0.0.0" }),
		"repeated flag":   edit(func(m *ImageModel) { m.Preset.EngineFlags = "--offload-to-cpu --offload-to-cpu" }),
		"quoted flag":     edit(func(m *ImageModel) { m.Preset.EngineFlags = `"--offload-to-cpu"` }),
		"spaced flags":    edit(func(m *ImageModel) { m.Preset.EngineFlags = "--diffusion-fa  --offload-to-cpu" }),
		"flag with value": edit(func(m *ImageModel) { m.Preset.EngineFlags = "--offload-to-cpu=1" }),
		"no licence text": edit(func(m *ImageModel) { m.License.Text = "" }),
		"plain http":      edit(func(m *ImageModel) { m.License.URL = "http://www.apache.org/licenses/LICENSE-2.0" }),
		"bare id":         edit(func(m *ImageModel) { m.ID = "Qwen-Image-2512" }),
		"directive in id": edit(func(m *ImageModel) { m.ID = "user.<sd_cpp_extra_args>" }),
	} {
		t.Run(name, func(t *testing.T) {
			if err := m.Valid(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	pickle := edit(func(m *ImageModel) { m.Files[0].Path = "model.ckpt" })
	if err := pickle.Valid(); !errors.Is(err, ErrWeightFormat) {
		t.Fatalf("pickle refusal = %v, want ErrWeightFormat", err)
	}
	twice := append(append([]ImageModel(nil), ImageModels...), base)
	if err := ValidImageModels(twice); err == nil {
		t.Fatal("a model declared twice was accepted")
	}
}

// The reviewed packaging manifest records the starter model as
// qualified_model and every other declared model under declared_models; the
// Panel's Setup plans are checked against the same record.
func TestImageModelsMatchThePackagingManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows-amd64.json"))
	if err != nil {
		t.Fatal(err)
	}
	type file struct {
		Role, Repository, Revision, Path, SHA256 string
		Bytes                                    int64
	}
	var manifest struct {
		Qualified struct {
			ID, Source, Revision, License string
			Main                          string `json:"main_sha256"`
			Encoder                       string `json:"encoder_sha256"`
			VAE                           string `json:"vae_sha256"`
			Total                         int64  `json:"total_bytes"`
		} `json:"qualified_model"`
		Declared []struct {
			ID, Title, Source, License string
			LicenseURL                 string `json:"license_url"`
			LicenseText                string `json:"license_text"`
			EngineFlags                string `json:"engine_flags"`
			Total                      int64  `json:"total_bytes"`
			Files                      []file
		} `json:"declared_models"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	k := ImageModels[0]
	q := manifest.Qualified
	if k.ID != q.ID || k.Files[0].Repository != q.Source || k.Files[0].Revision != q.Revision || k.License.Name != q.License ||
		k.Files[0].SHA256 != q.Main || k.Files[1].SHA256 != q.Encoder || k.Files[2].SHA256 != q.VAE || k.Bytes() != q.Total {
		t.Fatalf("starter model %+v differs from qualified_model %+v", k, q)
	}
	if len(manifest.Declared) != len(ImageModels)-1 {
		t.Fatalf("manifest declares %d models, the provider %d", len(manifest.Declared), len(ImageModels)-1)
	}
	for i, d := range manifest.Declared {
		m := ImageModels[i+1]
		if m.ID != d.ID || m.Title != d.Title || m.Source != d.Source || m.License != (ModelLicense{Name: d.License, URL: d.LicenseURL, Text: d.LicenseText}) ||
			m.Bytes() != d.Total || len(m.Files) != len(d.Files) || m.Preset.EngineFlags != d.EngineFlags {
			t.Fatalf("%s differs from the manifest entry %+v", m.ID, d)
		}
		for j, f := range d.Files {
			if m.Files[j] != (ModelFile{Role: f.Role, Repository: f.Repository, Revision: f.Revision, Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256}) {
				t.Fatalf("%s file %d differs from the manifest: %+v", m.ID, j, f)
			}
		}
	}
}

// Each declared model's Lemonade registry entry passes the provider's own
// registry check, carries recipe_options only as its reviewed engine flags
// (Qwen-Image-2512's sdcpp_args), and its files lie in the Hugging Face cache
// layout the models-folder check admits.
func TestImageModelRegistryAndCacheLayoutPassTheProviderChecks(t *testing.T) {
	root := t.TempDir()
	config, models := filepath.Join(root, "engine-config"), filepath.Join(root, "engine-models")
	registry := map[string]any{}
	for _, m := range ImageModels {
		entry := m.RegistryEntry()
		options, ok := entry["recipe_options"].(map[string]any)
		if ok != (m.Preset.EngineFlags != "") || ok && (len(options) != 1 || options["sdcpp_args"] != m.Preset.EngineFlags) {
			t.Fatalf("%s recipe_options = %v", m.ID, entry["recipe_options"])
		}
		defaults, _ := entry["image_defaults"].(map[string]any)
		if defaults["steps"] != m.Preset.Steps || defaults["cfg_scale"] != m.Preset.CFGScale {
			t.Fatalf("%s image_defaults = %v", m.ID, defaults)
		}
		registry[m.RegistryName()] = entry
		for _, f := range m.Files {
			path := filepath.Join(models, f.CachePath())
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(f.Role), 0600); err != nil {
				t.Fatal(err)
			}
			refs := filepath.Join(models, f.CacheFolder(), "refs", "main")
			if err := os.MkdirAll(filepath.Dir(refs), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(refs, []byte(f.Revision), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "user_models.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkRegistry(config); err != nil {
		t.Fatalf("registry refused: %v", err)
	}
	if err := checkModels(models); err != nil {
		t.Fatalf("cache layout refused: %v", err)
	}
	q, _ := FindImageModel("user.Qwen-Image-2512")
	if got := filepath.ToSlash(q.Files[0].CachePath()); got != "models--unsloth--Qwen-Image-2512-GGUF/snapshots/1626d7531f84b4d2ea1cd6d2e69f41ec027dd354/qwen-image-2512-Q5_0.gguf" {
		t.Fatalf("cache path = %s", got)
	}
	entry := q.RegistryEntry()
	if entry["checkpoints"].(map[string]string)["text_encoder"] != "unsloth/Qwen2.5-VL-7B-Instruct-GGUF:Qwen2.5-VL-7B-Instruct-UD-Q4_K_XL.gguf" || entry["size"] != 19.4 {
		t.Fatalf("registry entry = %v", entry)
	}
}

// recipe_options in the registry is admitted only as exactly the engine flags
// the catalogue declares for that registry name; any other model, flag, key
// or spelling is refused.
func TestRegistryAdmitsOnlyAModelsReviewedEngineFlags(t *testing.T) {
	write := func(t *testing.T, name string, options any) string {
		t.Helper()
		m, _ := FindImageModel("user.Qwen-Image-2512")
		entry := m.RegistryEntry()
		delete(entry, "recipe_options")
		if options != nil {
			entry["recipe_options"] = options
		}
		data, err := json.Marshal(map[string]any{name: entry})
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "user_models.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	reviewed := map[string]any{"sdcpp_args": "--diffusion-fa --offload-to-cpu"}
	if err := checkRegistry(write(t, "Qwen-Image-2512", reviewed)); err != nil {
		t.Fatalf("reviewed flags refused: %v", err)
	}
	for name, tc := range map[string]struct {
		model   string
		options any
	}{
		"another model":       {"Z-Image-Turbo", reviewed},
		"undeclared model":    {"Qwen-Image-GGUF", reviewed},
		"added flag":          {"Qwen-Image-2512", map[string]any{"sdcpp_args": "--diffusion-fa --offload-to-cpu --lora-model-dir C:/x"}},
		"listener flag":       {"Qwen-Image-2512", map[string]any{"sdcpp_args": "--listen-ip 0.0.0.0"}},
		"one of two":          {"Qwen-Image-2512", map[string]any{"sdcpp_args": "--offload-to-cpu"}},
		"other option":        {"Qwen-Image-2512", map[string]any{"sdcpp_args": "--diffusion-fa --offload-to-cpu", "steps": 50}},
		"backend choice":      {"Qwen-Image-2512", map[string]any{"sd-cpp_backend": "cpu"}},
		"not an object":       {"Qwen-Image-2512", "--diffusion-fa --offload-to-cpu"},
		"arguments as a list": {"Qwen-Image-2512", map[string]any{"sdcpp_args": []string{"--diffusion-fa", "--offload-to-cpu"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkRegistry(write(t, tc.model, tc.options)); !errors.Is(err, ErrWeightFormat) {
				t.Fatalf("admitted: %v", err)
			}
		})
	}
}
