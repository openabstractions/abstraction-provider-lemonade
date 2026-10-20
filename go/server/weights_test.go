package server

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWeightFormats(t *testing.T) {
	for name, want := range map[string]bool{
		"flux-2-klein-4b.safetensors": true, "model.GGUF": true, "mmproj-F16.gguf": true,
		"model.ckpt": false, "model.pt": false, "model.pth": false, "model.bin": false, "model.pkl": false,
		"imatrix.dat": false, "model.onnx": false, "model.safetensors.ckpt": false, "model": false,
	} {
		if WeightFileAllowed(name) != want {
			t.Fatalf("%s allowed = %v", name, !want)
		}
	}
}

// writeModelsFixture lays out the Hugging Face cache shape setup writes.
func writeModelsFixture(t *testing.T) (string, string) {
	t.Helper()
	models := filepath.Join(t.TempDir(), "engine-models")
	repo := filepath.Join(models, "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b")
	snapshot := filepath.Join(repo, "snapshots", "5f526678002e43af5551dadb73ce2e8c91b43afe", "split_files")
	for rel, content := range map[string]string{
		filepath.Join(snapshot, "diffusion_models", "flux-2-klein-4b.safetensors"): "main",
		filepath.Join(snapshot, "text_encoders", "qwen_3_4b.safetensors"):          "encoder",
		filepath.Join(snapshot, "vae", "flux2-vae.safetensors"):                    "vae",
		filepath.Join(repo, "refs", "main"):                                        "5f526678002e43af5551dadb73ce2e8c91b43afe",
		filepath.Join(repo, ".lemonade_registry.json"):                             "{}",
	} {
		if err := os.MkdirAll(filepath.Dir(rel), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rel, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return models, snapshot
}

func TestModelsFolderAdmitsOnlySafetensorsAndGGUF(t *testing.T) {
	if err := checkModels(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("absent models folder refused: %v", err)
	}
	models, _ := writeModelsFixture(t)
	if err := checkModels(models); err != nil {
		t.Fatalf("approved layout refused: %v", err)
	}
	for _, rel := range []string{
		filepath.Join("diffusion_models", "flux-2-klein-4b.ckpt"), filepath.Join("loras", "style.pt"),
		filepath.Join("upscalers", "esrgan.pth"), filepath.Join("vae", "vae.bin"), "imatrix.dat",
	} {
		t.Run(rel, func(t *testing.T) {
			models, snapshot := writeModelsFixture(t)
			path := filepath.Join(snapshot, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("pickle"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkModels(models); !errors.Is(err, ErrWeightFormat) {
				t.Fatalf("%s admitted: %v", rel, err)
			}
		})
	}
}

func approvedRegistry() map[string]any {
	return map[string]any{
		"LocalApprovedKlein": map[string]any{
			"source": "huggingface", "recipe": "sd-cpp", "size": 16.1,
			"checkpoints": map[string]string{
				"main":         "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/diffusion_models/flux-2-klein-4b.safetensors",
				"text_encoder": "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/text_encoders/qwen_3_4b.safetensors",
				"vae":          "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/vae/flux2-vae.safetensors",
			},
			"image_defaults": map[string]any{"steps": 4, "cfg_scale": 1, "width": 512, "height": 512},
		},
	}
}

func TestRegistryAdmitsOnlySafetensorsAndGGUFCheckpoints(t *testing.T) {
	write := func(t *testing.T, registry map[string]any) string {
		dir := t.TempDir()
		data, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "user_models.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if err := checkRegistry(t.TempDir()); err != nil {
		t.Fatalf("absent registry refused: %v", err)
	}
	if err := checkRegistry(write(t, approvedRegistry())); err != nil {
		t.Fatalf("approved registry refused: %v", err)
	}
	for name, mutate := range map[string]func(model map[string]any){
		"pickle checkpoint": func(m map[string]any) {
			m["checkpoints"].(map[string]string)["main"] = "Comfy-Org/x:split_files/diffusion_models/flux.ckpt"
		},
		"pt encoder": func(m map[string]any) { m["checkpoints"].(map[string]string)["text_encoder"] = "org/x:encoder.pt" },
		"repository only": func(m map[string]any) {
			delete(m, "checkpoints")
			m["checkpoint"] = "unsloth/some-model-GGUF"
		},
		"no checkpoint":       func(m map[string]any) { delete(m, "checkpoints") },
		"per-model arguments": func(m map[string]any) { m["recipe_options"] = map[string]any{"sd-cpp_args": "--listen-ip 0.0.0.0"} },
		"other recipe":        func(m map[string]any) { m["recipe"] = "vllm" },
	} {
		t.Run(name, func(t *testing.T) {
			registry := approvedRegistry()
			mutate(registry["LocalApprovedKlein"].(map[string]any))
			if err := checkRegistry(write(t, registry)); !errors.Is(err, ErrWeightFormat) {
				t.Fatalf("%s admitted: %v", name, err)
			}
		})
	}
}
