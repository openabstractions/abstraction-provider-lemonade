//go:build windows

package server

import (
	"bytes"
	"context"
	"errors"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chatServer starts the stand-in engine with a llama.cpp fixture folder in
// the cache when llama is set. reviewed makes the fixture the manifest the
// chat route requires, standing in for b10825.
func chatServer(t *testing.T, llama, reviewed bool) (*Server, Config) {
	t.Helper()
	cfg := fakeConfig(t, "helper-ready")
	cfg.Models = []string{"user.Chat"}
	if llama {
		m, _ := writeBackendFixture(t, cfg.CacheDir, "llamacpp", "vulkan",
			map[string]string{"llama-server.exe": "server", "llama.dll": "llama"}, map[string]string{"ggml-rpc-server.exe": "rpc"})
		cfg.LlamaBackend = m
		if reviewed {
			cfg.chatBackend = m
		}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}

func postChat(t *testing.T, s *Server, body []byte) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.Base()+chatRoute, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.HTTP().Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status %d", resp.StatusCode)
	}
	return nil
}

func chatRequestsSeen(t *testing.T, cfg Config) int {
	t.Helper()
	seen, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(seen), "POST "+chatRoute)
}

func wantChatRefusal(t *testing.T, err error, condition string) {
	t.Helper()
	var refusal *ChatRefusal
	if !errors.As(err, &refusal) || refusal.Condition != condition || RefusalReason(err) != "engine:chat-route:"+condition {
		t.Fatalf("chat request: %v, want the %s condition", err, condition)
	}
}

// The chat route stays closed, with a reason naming the unmet condition,
// while no llama.cpp folder is proven and held for the launch, and while the
// held folder is a build other than the reviewed one.
func TestChatRouteClosedUntilTheReviewedLlamaFolderIsHeld(t *testing.T) {
	s, cfg := chatServer(t, false, false)
	wantChatRefusal(t, postChat(t, s, chatBody(t, nil)), "engine-files")
	if n := chatRequestsSeen(t, cfg); n != 0 {
		t.Fatalf("engine received %d chat requests", n)
	}
	s, cfg = chatServer(t, true, false)
	wantChatRefusal(t, postChat(t, s, chatBody(t, nil)), "engine-files")
	if n := chatRequestsSeen(t, cfg); n != 0 {
		t.Fatalf("engine received %d chat requests from an unreviewed build", n)
	}
}

// With the reviewed folder held, a chat request reaches the engine as the
// provider re-encoded it; load options, remote and local media URLs and video
// never do.
func TestChatRouteForwardsOnlyCheckedRequests(t *testing.T) {
	s, cfg := chatServer(t, true, true)
	body := chatBody(t, nil, map[string]any{"type": "text", "text": "what is this"}, imagePart(pngDataURI(t)))
	if err := postChat(t, s, body); err != nil {
		t.Fatalf("checked chat request refused: %v", err)
	}
	want, err := checkChatBody(body, allowChat)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "chat-body"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("engine received other bytes than the checked request: %v", err)
	}
	for condition, refused := range map[string][]byte{
		"load-options": chatBody(t, map[string]any{"ctx_size": 262144}),
		"media":        chatBody(t, nil, imagePart("http://169.254.169.254/latest/meta-data/")),
		"model":        bytes.Replace(chatBody(t, nil), []byte("user.Chat"), []byte("user.Other"), 1),
	} {
		wantChatRefusal(t, postChat(t, s, refused), condition)
	}
	wantChatRefusal(t, postChat(t, s, chatBody(t, nil, map[string]any{"type": "input_video", "input_video": map[string]any{"url": "file:///C:/x.mp4"}})), "media")
	if n := chatRequestsSeen(t, cfg); n != 1 {
		t.Fatalf("engine received %d chat requests, want the one checked request", n)
	}
}

// A caller's JPEG reaches the engine as the provider's PNG.
func TestChatRouteForwardsImagesAsProviderPNG(t *testing.T) {
	s, cfg := chatServer(t, true, true)
	if err := postChat(t, s, chatBody(t, nil, imagePart(jpegDataURI(t, 40, 30)))); err != nil {
		t.Fatalf("JPEG chat request refused: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "chat-body"))
	if err != nil {
		t.Fatal(err)
	}
	data := forwardedImage(t, got)
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" || config.Width != 40 || config.Height != 30 {
		t.Fatalf("engine received %s %dx%d: %v", format, config.Width, config.Height, err)
	}
}

// The launch's environment, which llama-server inherits, is checked again
// before each chat request.
func TestChatRouteClosedWhenTheLaunchEnvironmentFails(t *testing.T) {
	s, cfg := chatServer(t, true, true)
	s.mu.Lock()
	s.launchEnv = append(append([]string(nil), s.launchEnv...), "GGML_BACKEND_PATH=C:\\planted.dll")
	s.mu.Unlock()
	wantChatRefusal(t, postChat(t, s, chatBody(t, nil)), "environment")
	if n := chatRequestsSeen(t, cfg); n != 0 {
		t.Fatalf("engine received %d chat requests", n)
	}
}
