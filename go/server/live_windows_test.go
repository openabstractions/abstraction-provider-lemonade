//go:build windows

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// engineRequest sends a diagnostic request with this launch's API key over
// the bound client. Only these opt-in live tests use it; OA callers reach
// Lemonade through HTTP(), which forwards the image routes alone.
func (s *Server) engineRequest(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	client, key := s.client, s.keys.api
	s.mu.Unlock()
	if client == nil || key == "" {
		return nil, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return client.Do(req)
}

// This opt-in check uses an explicitly selected checkout binary. It gives the
// process private state and a fresh port, then confirms model inventory through
// the same bound client used by inference.
func TestLiveApprovedBinary(t *testing.T) {
	program := os.Getenv("OA_LEMONADE_TEST_BINARY")
	if program == "" {
		t.Skip("set OA_LEMONADE_TEST_BINARY to a reviewed checkout binary")
	}
	state := t.TempDir()
	s, err := New(Config{
		Program: program, CacheDir: filepath.Join(state, "cache"), ConfigDir: filepath.Join(state, "config"),
		Port: freePort(t), Models: []string{"Flux-2-Klein-4B"}, Ready: 45 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("contained Lemonade shutdown: %v", err)
		}
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base()+"/api/v1/models?show_all=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.engineRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("model inventory status %d", resp.StatusCode)
	}
	var inventory struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&inventory); err != nil {
		t.Fatal(err)
	}
	for _, model := range inventory.Data {
		if model.ID == "Flux-2-Klein-4B" {
			return
		}
	}
	ids := make([]string, 0, len(inventory.Data))
	for _, model := range inventory.Data {
		ids = append(ids, model.ID)
	}
	t.Fatalf("pinned build did not advertise built-in Flux-2-Klein-4B; ids=%v", ids)
}
