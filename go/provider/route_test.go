package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// The composed provider routes each declared model of every profile to its
// own host: a request for it is refused by the provider's admission or its
// engine, never no_host. A provider whose router missed its host would answer
// no_host "router:not-here", as pytorchd's image service did until
// 2026-11-09. Nothing here starts Lemonade: the reviewed image model and the
// audio models are unstaged, and the transcription asks word timestamps.
func TestTheComposedProviderRoutesEveryDeclaredProfile(t *testing.T) {
	const image = "user.Z-Image-Turbo"
	dir := t.TempDir()
	leases := &fakeLeases{}
	p, err := New(Config{Endpoint: providerEndpoint(t, fmt.Sprintf("lemonade-route-%d-%d", os.Getpid(), servedProviders.Add(1))),
		RuntimePrograms: []string{os.Args[0]}, StoreRoot: filepath.Join(dir, "store"), RecordPath: filepath.Join(dir, "records.json"),
		Leases: leases, Amount: 1 << 30, Wait: time.Second,
		Server: server.Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: filepath.Join(dir, "config"), Port: 41234,
			Models: []string{image}, Transcription: true, Speech: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := inference.WithDesignatedRuntime(inference.WithRoute(context.Background(), inference.RouteNative, os.Args[0]))
	caller := inference.Subject{Account: "test", Program: os.Args[0]}
	local := []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}

	a := p.Host.Provider.StartImage(ctx, caller, wire.ImageRequest{Model: image, Prompt: "a lighthouse",
		Mode: wire.ImageModeGenerate, Count: 1, Size: "auto", Guarantees: local})
	if a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "engine:image:model-files" {
		t.Fatalf("image %s: %+v, wanted unavailable engine:image:model-files from admission", image, a)
	}
	a = p.Host.Provider.StartTranscription(ctx, caller, wire.TranscriptionRequest{Model: whispercpp.LargeV3Turbo,
		AudioDigest: "sha256:" + strings.Repeat("0", 64), MediaType: "audio/wav", Timestamps: wire.TimestampModeWord, Guarantees: local})
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || !strings.HasPrefix(a.Reason, "timestamps:") {
		t.Fatalf("transcription %s: %+v, wanted the transcriber's own timestamps refusal", whispercpp.LargeV3Turbo, a)
	}
	a = p.Host.Provider.StartSpeech(ctx, caller, wire.SpeechRequest{Model: kokoro.KokoroV1, Voice: "af_sky", Text: "Hello.",
		Format: wire.SpeechFormatWav, Guarantees: local})
	if a.Outcome != wire.StartOutcomeUnavailable || !strings.HasPrefix(a.Reason, "engine:audio:") {
		t.Fatalf("speech %s: %+v, wanted an unstaged engine:audio refusal from admission", kokoro.KokoroV1, a)
	}
	leases.mu.Lock()
	defer leases.mu.Unlock()
	if leases.next != 0 {
		t.Fatalf("a refused request took the card: %q", leases.calls)
	}
}
