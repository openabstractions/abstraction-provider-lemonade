//go:build windows

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in acceptance uses existing approved weights and a separate cache.
// The locked config is offline with executable fetching off, so the reviewed
// sd.cpp Vulkan backend must already be staged in <state>/cache (setup's
// StageApprovedVulkanBackend does that). The test installs it as setup's
// InstallFiles does, which leaves the backend folder read-only; enginefiles'
// Unlock opens it again.
func TestLiveApprovedBF16(t *testing.T) {
	program := os.Getenv("OA_LEMONADE_TEST_BINARY")
	state := os.Getenv("OA_LEMONADE_TEST_STATE")
	main := os.Getenv("OA_LEMONADE_BF16_MAIN")
	encoder := os.Getenv("OA_LEMONADE_BF16_ENCODER")
	vae := os.Getenv("OA_LEMONADE_BF16_VAE")
	const backend = "vulkan"
	if program == "" || state == "" || main == "" || encoder == "" || vae == "" {
		t.Skip("explicit binary, private state with the staged Vulkan backend and three approved weights required")
	}
	const revision = "5f526678002e43af5551dadb73ce2e8c91b43afe"
	const model = "user.LocalApprovedKlein"
	modelRoot := filepath.Join(state, "models", "models--Comfy-Org--vae-text-encorder-for-flux-klein-4b")
	snapshot := filepath.Join(modelRoot, "snapshots", revision, "split_files")
	for _, file := range [][2]string{
		{main, filepath.Join(snapshot, "diffusion_models", "flux-2-klein-4b.safetensors")},
		{encoder, filepath.Join(snapshot, "text_encoders", "qwen_3_4b.safetensors")},
		{vae, filepath.Join(snapshot, "vae", "flux2-vae.safetensors")},
	} {
		if err := os.MkdirAll(filepath.Dir(file[1]), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(file[1]); os.IsNotExist(err) {
			if err := os.Link(file[0], file[1]); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(modelRoot, "refs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelRoot, "refs", "main"), []byte(revision), 0600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(state, "config")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := LockedConfig(filepath.Join(state, "models"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := json.Marshal(map[string]any{
		"LocalApprovedKlein": map[string]any{
			"source": "huggingface", "recipe": "sd-cpp", "size": 16.1,
			"checkpoints": map[string]string{
				"main":         "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/diffusion_models/flux-2-klein-4b.safetensors",
				"text_encoder": "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/text_encoders/qwen_3_4b.safetensors",
				"vae":          "Comfy-Org/vae-text-encorder-for-flux-klein-4b:split_files/vae/flux2-vae.safetensors",
			},
			"image_defaults": map[string]any{"steps": 4, "cfg_scale": 1, "width": 512, "height": 512},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "user_models.json"), registry, 0600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(filepath.Join(state, "lemonade.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	s, err := New(Config{Program: program, CacheDir: filepath.Join(state, "cache"), ConfigDir: configDir,
		ModelsDir: filepath.Join(state, "models"), RequireVulkanBackend: true,
		Port: freePort(t), Models: []string{model}, Ready: time.Minute, Output: logFile})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Setup's install step: the staged backend gets the read-only folder ACL
	// and its metadata record (INF-F3); every launch checks both.
	trees, err := backendTrees(s.cfg.CacheDir, SDCppVulkan, LlamaCppVulkanB10825, true, s.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range trees {
		if err := s.files.Install(ctx, b.tree); err != nil {
			t.Fatal(err)
		}
	}
	startedAt := time.Now()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("engine startup/readiness: %s", time.Since(startedAt))
	t.Cleanup(func() {
		stoppingAt := time.Now()
		if err := s.Close(); err != nil {
			t.Errorf("contained shutdown: %v", err)
		}
		t.Logf("final shutdown: %s; total elapsed: %s", time.Since(stoppingAt), time.Since(startedAt))
	})
	inspect, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base()+"/api/v1/models/"+model, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.engineRequest(inspect)
	if err != nil {
		t.Fatal(err)
	}
	modelBody, err := io.ReadAll(io.LimitReader(result.Body, 1<<20))
	result.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("private model inventory status %d: %s", result.StatusCode, modelBody)
	}
	var modelState struct {
		Downloaded bool `json:"downloaded"`
	}
	if err := json.Unmarshal(modelBody, &modelState); err != nil {
		t.Fatal(err)
	}
	if !modelState.Downloaded {
		t.Fatalf("private cache not recognized; refusing model download: %s", modelBody)
	}
	post := func(path, payload string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base()+path, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		send := s.HTTP().Do
		if !forwardedRoutes[path] {
			send = s.engineRequest
		}
		resp, err := send(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status %d: %s", path, resp.StatusCode, body)
		}
		return string(body)
	}
	loadAt := time.Now()
	load := post("/api/v1/load", fmt.Sprintf(`{"model_name":%q,"sdcpp_backend":%q,"steps":4,"width":512,"height":512}`, model, backend))
	t.Logf("model load elapsed: %s; response: %s", time.Since(loadAt), load)
	renderAt := time.Now()
	generated := post("/api/v1/images/generations", fmt.Sprintf(`{"model":%q,"prompt":"a single red apple on a white table","size":"512x512","n":1,"response_format":"b64_json"}`, model))
	t.Logf("image request elapsed: %s", time.Since(renderAt))
	var imageResponse struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(generated), &imageResponse); err != nil {
		t.Fatal(err)
	}
	if len(imageResponse.Data) != 1 || imageResponse.Data[0].B64 == "" {
		t.Fatalf("image response did not contain one b64_json image: %s", generated)
	}
	pixels, err := base64.StdEncoding.DecodeString(imageResponse.Data[0].B64)
	if err != nil {
		t.Fatal(err)
	}
	imageInfo, format, err := image.DecodeConfig(bytes.NewReader(pixels))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" || imageInfo.Width != 512 || imageInfo.Height != 512 {
		t.Fatalf("unexpected output image: %s %dx%d", format, imageInfo.Width, imageInfo.Height)
	}
	if err := os.WriteFile(filepath.Join(state, "sample-"+backend+".png"), pixels, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("BF16 PNG generated (%d decoded bytes, %d response bytes, SHA-256 %x); startup-to-image: %s", len(pixels), len(generated), sha256.Sum256(pixels), time.Since(startedAt))
	if os.Getenv("OA_LEMONADE_TEST_CANCEL") == "1" {
		requestCtx, stop := context.WithCancel(ctx)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.Base()+"/api/v1/images/generations",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"prompt":"a blue ceramic vase","size":"512x512","n":1,"steps":40,"response_format":"b64_json"}`, model)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		ended := make(chan error, 1)
		go func() {
			response, err := s.HTTP().Do(request)
			if response != nil {
				response.Body.Close()
			}
			ended <- err
		}()
		time.Sleep(time.Second)
		cancelAt := time.Now()
		stop()
		select {
		case err := <-ended:
			if err == nil {
				t.Fatal("canceled live generation returned success")
			}
		case <-time.After(30 * time.Second):
			t.Fatal("canceled live generation did not stop contained tree")
		}
		s.mu.Lock()
		stillOwned := s.proc != nil
		s.mu.Unlock()
		if stillOwned {
			t.Fatal("canceled live generation left owned tree")
		}
		t.Logf("live generation cancellation stopped the owned process tree in %s", time.Since(cancelAt))
	}
}
