package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
	router "github.com/openabstractions/abstraction-router/go"
)

// The admission hook refuses a model the host declares for another profile
// only, model:undeclared before Lemonade starts: the second line behind the
// router's own no_host (INF-A5, ROUTE-D9).
func TestAdmitRefusesAModelOfAnotherProfile(t *testing.T) {
	dir := t.TempDir()
	cfg := server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234,
		Models: []string{"user.A"}, Transcription: true, Speech: true}
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	host, declared := servedHost("lemonade", s)
	if len(declared) != 3 {
		t.Fatalf("declared %v", declared)
	}
	// The router host and the server read one table: every profile and model
	// pair answers the same on both.
	for _, profile := range []string{router.ProfileImage, router.ProfileTranscription, router.ProfileSpeech, router.ProfileChat} {
		for _, model := range []string{"user.A", whispercpp.LargeV3Turbo, kokoro.KokoroV1, "other"} {
			if host.ServesModel(profile, model) != s.ServesModel(profile, model) {
				t.Fatalf("%s %s: host %v, server %v", profile, model, host.ServesModel(profile, model), s.ServesModel(profile, model))
			}
		}
	}
	// A refused model takes no lease: the card is never asked.
	loads := 0
	ensure := func(ctx context.Context) error {
		loads++
		return ctx.Err()
	}
	admit := admitter(s, host, ensure, nil)
	for _, refused := range []struct{ profile, model string }{
		{router.ProfileImage, kokoro.KokoroV1},
		{router.ProfileImage, whispercpp.LargeV3Turbo},
		{router.ProfileSpeech, "user.A"},
		{router.ProfileSpeech, whispercpp.LargeV3Turbo},
		{router.ProfileTranscription, kokoro.KokoroV1},
		{router.ProfileChat, "user.A"},
		{"", "user.A"},
	} {
		ctx := inference.WithAdmissionProfile(context.Background(), refused.profile)
		if reason, ok := admit(ctx, host, refused.model); ok || reason != "model:undeclared" {
			t.Fatalf("%s %s admitted: %q %v", refused.profile, refused.model, reason, ok)
		}
	}
	if loads != 0 {
		t.Fatalf("an undeclared model asked the card %d time(s)", loads)
	}
	// A model asked for its own profile passes the declaration check; what
	// follows it (staged files, the backend) answers with its own words.
	for _, served := range []struct{ profile, model string }{
		{router.ProfileImage, "user.A"},
		{router.ProfileTranscription, whispercpp.LargeV3Turbo},
		{router.ProfileSpeech, kokoro.KokoroV1},
	} {
		ctx, cancel := context.WithCancel(inference.WithAdmissionProfile(context.Background(), served.profile))
		cancel()
		reason, _ := admit(ctx, host, served.model)
		if reason == "model:undeclared" {
			t.Fatalf("%s %s refused as undeclared", served.profile, served.model)
		}
		t.Logf("%s %s: %q", served.profile, served.model, reason)
	}
	// The declared image model starts Lemonade through the card alone; the
	// audio models stop earlier, at their unstaged files.
	if loads != 1 {
		t.Fatalf("the served models asked the card %d time(s), want 1", loads)
	}
	// Each model is offered through its own profile only.
	routes := router.New(host)
	routes.Survey()
	if d, _ := routes.Route(router.Request{Model: kokoro.KokoroV1, Profile: router.ProfileImage}); d.Verdict != router.NoHost {
		t.Fatalf("image kokoro-v1 routed %s (%s)", d.Verdict, d.Why)
	}
}

func TestAdmitRefusesUnstagedDeclaredImageBeforeEngineStart(t *testing.T) {
	dir := t.TempDir()
	s, err := server.New(server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"),
		ConfigDir: filepath.Join(dir, "config"), Port: 41234, Models: []string{"user.Z-Image-Turbo"}})
	if err != nil {
		t.Fatal(err)
	}
	host, _ := servedHost("lemonade-image", s)
	starts := 0
	admit := admitter(s, host, func(context.Context) error { starts++; return nil }, nil)
	ctx := inference.WithAdmissionProfile(context.Background(), router.ProfileImage)
	if reason, ok := admit(ctx, host, "user.Z-Image-Turbo"); ok || reason != "engine:image:model-files" {
		t.Fatalf("unstaged admission: %s %v", reason, ok)
	}
	if starts != 0 {
		t.Fatalf("unstaged model started engine %d times", starts)
	}
}
