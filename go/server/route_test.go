package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// forbiddenRoutes are requests OA callers must never reach Lemonade with.
var forbiddenRoutes = []struct{ method, path string }{
	{http.MethodPost, "/internal/set"}, {http.MethodPost, "/internal/shutdown"}, {http.MethodGet, "/internal/config"},
	{http.MethodPost, "/internal/pin"}, {http.MethodPost, "/internal/cleanup-cache"}, {http.MethodPost, "/mcp"},
	{http.MethodPost, "/api/v1/pull"}, {http.MethodPost, "/api/v1/install"}, {http.MethodPost, "/v1/pull"},
	{http.MethodPost, "/api/v1/load"}, {http.MethodGet, "/api/v1/models"}, {http.MethodPost, "/v1/cloud/auth"},
	{http.MethodGet, "/api/v1/images/generations"}, {http.MethodPost, "/api/v1/images/generations?model=x"},
	{http.MethodPost, "/api/v1/images/generations/../../../internal/set"}, {http.MethodPost, "/api/v1/images%2Fgenerations"},
	{http.MethodPost, "/API/V1/IMAGES/GENERATIONS"}, {http.MethodPost, "/v1/images/generations"},
	// Lemonade's Ollama layer and its other completion routes take load
	// options from the body; only the chat call reaches the chat gate.
	{http.MethodPost, "/api/chat"}, {http.MethodPost, "/api/generate"}, {http.MethodPost, "/api/pull"}, {http.MethodPost, "/api/create"},
	{http.MethodPost, "/v1/chat/completions"}, {http.MethodPost, "/api/v1/completions"}, {http.MethodPost, "/api/v1/responses"},
	{http.MethodPost, "/api/v1/chat/completions?ctx_size=1"}, {http.MethodGet, "/api/v1/chat/completions"},
}

// opaqueRequest names the image route in Path while URL.Opaque, which
// net/http writes on the request line in place of the path, names
// /internal/set.
func opaqueRequest(t *testing.T, base string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/images/generations", strings.NewReader(`{"host":"0.0.0.0"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.URL.Opaque = "//" + req.URL.Host + "/internal/set"
	return req
}

func TestRefusalReasons(t *testing.T) {
	for err, want := range map[error]string{
		ErrBackendManifest: "engine:backend-unavailable", ErrLockedConfig: "engine:config-mismatch",
		ErrWeightFormat: "engine:weight-format", ErrListenerNotLoopback: "engine:listener-not-loopback",
		ErrForbiddenArgument: "engine:launch-plan", ErrForbiddenEnvironment: "engine:launch-plan", ErrUnavailable: "engine:unavailable",
	} {
		wrapped := fmt.Errorf("%w: %w: detail", ErrConfiguration, err)
		if got := RefusalReason(wrapped); got != want {
			t.Fatalf("%v: reason %q, want %q", err, got, want)
		}
	}
}

func TestOnlyImageRoutesAndTheChatGateAreForwarded(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:41234"}
	for _, path := range []string{"/api/v1/images/generations", "/api/v1/images/edits", chatRoute} {
		req, err := http.NewRequest(http.MethodPost, s.base+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.checkRoute(req); err != nil {
			t.Fatalf("%s refused: %v", path, err)
		}
	}
	for _, route := range forbiddenRoutes {
		req, err := http.NewRequest(route.method, s.base+route.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.checkRoute(req); !errors.Is(err, ErrForbiddenRoute) {
			t.Fatalf("%s %s forwarded: %v", route.method, route.path, err)
		}
	}
	if err := s.checkRoute(opaqueRequest(t, s.base)); !errors.Is(err, ErrForbiddenRoute) {
		t.Fatalf("opaque URL forwarded: %v", err)
	}
	other, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:13305/api/v1/images/generations", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.checkRoute(other); !errors.Is(err, ErrForbiddenRoute) {
		t.Fatalf("other engine address forwarded: %v", err)
	}
}
