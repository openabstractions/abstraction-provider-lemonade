//go:build windows

package setup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func TestPackageManifestHashes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "packaging", "windows-amd64.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		UpstreamApp struct {
			Archive string `json:"archive_sha256"`
			Program string `json:"program_sha256"`
		} `json:"upstream_app"`
		VulkanBackend struct {
			Archive string `json:"archive_sha256"`
			Program string `json:"program_sha256"`
		} `json:"vulkan_backend"`
		QualifiedModel struct {
			Main    string `json:"main_sha256"`
			Encoder string `json:"encoder_sha256"`
			VAE     string `json:"vae_sha256"`
		} `json:"qualified_model"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, hash := range []string{manifest.UpstreamApp.Archive, manifest.UpstreamApp.Program, manifest.VulkanBackend.Archive, manifest.VulkanBackend.Program, manifest.QualifiedModel.Main, manifest.QualifiedModel.Encoder, manifest.QualifiedModel.VAE} {
		if !hex64.MatchString(hash) {
			t.Fatalf("invalid 64-digit manifest hash: %q", hash)
		}
	}
	if manifest.VulkanBackend.Archive != VulkanBackendArchiveSHA256 || manifest.VulkanBackend.Program != VulkanServerSHA256 {
		t.Fatal("backend code/manifest pins differ")
	}
}

func TestApprovedVulkanBackendRequiresPinnedFiles(t *testing.T) {
	if err := VerifyApprovedVulkanBackend(t.TempDir()); !errors.Is(err, ErrSelection) {
		t.Fatalf("missing backend accepted: %v", err)
	}
}

func TestInterruptedBackendStageRequiresExplicitRecovery(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	parent := filepath.Dir(backendDir(cache))
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	archiveTemp := filepath.Join(parent, ".vulkan-archive-123456789")
	stageTemp := filepath.Join(parent, ".vulkan-stage-987654321")
	unrelated := filepath.Join(parent, "owner-file")
	for _, path := range []string{archiveTemp, unrelated} {
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(stageTemp, 0700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovedVulkanBackend(cache); !errors.Is(err, ErrSelection) {
		t.Fatalf("interrupted stage passed: %v", err)
	}
	if err := StageApprovedVulkanBackend(context.Background(), cache, filepath.Join(parent, "missing.zip"), nil, false); !errors.Is(err, ErrSelection) {
		t.Fatalf("unapproved cleanup accepted: %v", err)
	}
	for _, path := range []string{archiveTemp, stageTemp, unrelated} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("refused recovery changed %s: %v", path, err)
		}
	}
	if err := StageApprovedVulkanBackend(context.Background(), cache, filepath.Join(parent, "missing.zip"), nil, true); !errors.Is(err, ErrSelection) {
		t.Fatalf("invalid archive accepted after explicit recovery: %v", err)
	}
	for _, path := range []string{archiveTemp, stageTemp} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("owned residue remains: %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(unrelated); err != nil {
		t.Fatalf("foreign file removed: %v", err)
	}
}

func TestConcurrentReplacementCannotDeleteLiveBackendStage(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "engine-cache")
	parent := filepath.Dir(backendDir(cache))
	entered, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	stage := filepath.Join(parent, ".vulkan-stage-123456789")
	go func() {
		firstDone <- withSetupLock(context.Background(), root, func() error {
			if err := os.MkdirAll(stage, 0700); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- StageApprovedVulkanBackend(context.Background(), cache, filepath.Join(root, "missing.zip"), nil, true)
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("replacement escaped live stage lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("live stage removed: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; !errors.Is(err, ErrSelection) {
		t.Fatalf("invalid archive accepted: %v", err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("explicit recovery left stage: %v", err)
	}
}

func TestLiveStageApprovedVulkanBackend(t *testing.T) {
	archive := os.Getenv("OA_LEMONADE_VULKAN_ARCHIVE")
	if archive == "" {
		t.Skip("reviewed backend archive required")
	}
	z, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	var entryFound bool
	for _, entry := range z.File {
		if entry.Name == "sd-server.exe" {
			entryFound = true
			r, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			_, err = io.Copy(h, r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(h.Sum(nil)) != VulkanServerSHA256 {
				t.Fatal("reviewed archive executable pin differs")
			}
		}
	}
	z.Close()
	if !entryFound {
		t.Fatal("reviewed archive lacks sd-server.exe")
	}
	canceledCache := filepath.Join(t.TempDir(), "canceled-cache")
	ctx, stop := context.WithCancel(context.Background())
	err = StageApprovedVulkanBackend(ctx, canceledCache, archive, func(done, total int64) error {
		if done >= 1<<20 {
			stop()
			return ctx.Err()
		}
		return nil
	}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("backend cancellation not propagated: %v", err)
	}
	if _, err := os.Stat(backendDir(canceledCache)); !os.IsNotExist(err) {
		t.Fatal("canceled backend published")
	}
	cache := filepath.Join(t.TempDir(), "cache")
	var last, total int64
	if err := StageApprovedVulkanBackend(context.Background(), cache, archive, func(done, all int64) error { last, total = done, all; return nil }, false); err != nil {
		t.Fatal(err)
	}
	if last != total || total != VulkanBackendArchiveBytes+vulkanExpandedBytes {
		t.Fatalf("bad progress %d/%d", last, total)
	}
	if err := VerifyApprovedVulkanBackend(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backendDir(cache), "sd-server.exe"), []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApprovedVulkanBackend(cache); !errors.Is(err, ErrSelection) {
		t.Fatalf("replaced server accepted: %v", err)
	}
	if err := StageApprovedVulkanBackend(context.Background(), cache, archive, nil, true); err != nil {
		t.Fatalf("explicit backend replacement failed: %v", err)
	}
	if err := VerifyApprovedVulkanBackend(cache); err != nil {
		t.Fatalf("replaced backend not verified: %v", err)
	}
}
