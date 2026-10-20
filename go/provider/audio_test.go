package provider

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
)

// fakeServer stands for the owned Lemonade: its address and the speech crash
// bookkeeping.
type fakeServer struct {
	base      string
	mu        sync.Mutex
	failed    []string
	completed int
}

func (f *fakeServer) Base() string { return f.base }
func (f *fakeServer) SpeechFailed(digest string, cause error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, digest)
	return errors.New("lemonade: the speech engine stopped during input " + digest)
}
func (f *fakeServer) SpeechCompleted() {
	f.mu.Lock()
	f.completed++
	f.mu.Unlock()
}

func speech(text, voice string, format wire.SpeechFormat) wire.SpeechRequest {
	return wire.SpeechRequest{Model: kokoro.KokoroV1, Voice: voice, Text: text, Format: format}
}

// Kokoros condition 6: formats other than wav and pcm, non-English voices,
// mixes and text outside NFC read unsupported_feature before any engine work.
func TestSpeakerUnsupported(t *testing.T) {
	var s speaker
	for want, req := range map[string]wire.SpeechRequest{
		"":               speech("Hello.", "af_sky", wire.SpeechFormatWav),
		"format:mp3":     speech("Hello.", "af_sky", wire.SpeechFormatMp3),
		"voice:alloy":    speech("Hello.", "alloy", wire.SpeechFormatWav),
		"voice:jf_alpha": speech("Hello.", "jf_alpha", wire.SpeechFormatPcm),
		"text":           speech("café", "af_sky", wire.SpeechFormatWav),
		"model:tts-1":    {Model: "tts-1", Voice: "af_sky", Text: "Hi", Format: wire.SpeechFormatWav},
	} {
		if got := s.Unsupported(req); got != want {
			t.Errorf("%+v: %q, want %q", req, got, want)
		}
	}
}

// Kokoros condition 7: koko's PCM comes back as the provider's own WAV, or
// as PCM when the caller asked for it; the request carries pcm and the
// review's fields only.
func TestSpeakerWritesTheWAVHeader(t *testing.T) {
	var body []byte
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "audio/l16;rate=24000;endianness=little-endian")
		w.Write([]byte{1, 0, 2, 0})
	}))
	defer engine.Close()
	srv := &fakeServer{base: engine.URL}
	s := speaker{server: srv, client: engine.Client()}
	reply := s.Run(context.Background(), kokoro.KokoroV1, speech("Done.", "af_sky", wire.SpeechFormatWav))
	if reply.Outcome != wire.ReplyOutcomeCompleted || len(reply.Audio) != 48 || string(reply.Audio[:4]) != "RIFF" || binary.LittleEndian.Uint32(reply.Audio[24:28]) != 24000 || srv.completed != 1 {
		t.Fatalf("reply %+v", reply)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil || sent["response_format"] != "pcm" || len(sent) != 5 {
		t.Fatalf("sent %s", body)
	}
	reply = s.Run(context.Background(), kokoro.KokoroV1, speech("Done.", "af_sky", wire.SpeechFormatPcm))
	if reply.Outcome != wire.ReplyOutcomeCompleted || string(reply.Audio) != "\x01\x00\x02\x00" {
		t.Fatalf("pcm reply %+v", reply)
	}
}

// Kokoros condition 8: an engine that drops the connection fails the
// request once; the provider never resubmits the text, and its log names the
// input by digest only.
func TestSpeakerNeverResubmitsAfterACrash(t *testing.T) {
	var requests atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.ReadAll(r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer engine.Close()
	srv := &fakeServer{base: engine.URL}
	var logged []string
	s := speaker{server: srv, client: engine.Client(), onError: func(err error) { logged = append(logged, err.Error()) }}
	text := "the secret release plan"
	reply := s.Run(context.Background(), kokoro.KokoroV1, speech(text, "af_sky", wire.SpeechFormatWav))
	if reply.Outcome != wire.ReplyOutcomeUnavailable || reply.Reason != "engine:crashed" || len(reply.Audio) != 0 {
		t.Fatalf("reply %+v", reply)
	}
	if requests.Load() != 1 || len(srv.failed) != 1 || srv.failed[0] != kokoro.InputDigest(text) {
		t.Fatalf("%d requests, failures %q", requests.Load(), srv.failed)
	}
	for _, line := range logged {
		if strings.Contains(line, "secret") {
			t.Fatalf("the log carries the text: %s", line)
		}
	}
	// A 5xx from Lemonade, whose koko exited, counts the same.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()
	srv.base = failing.URL
	s.client = failing.Client()
	if reply := s.Run(context.Background(), kokoro.KokoroV1, speech(text, "af_sky", wire.SpeechFormatWav)); reply.Reason != "engine:crashed" || requests.Load() != 2 || len(srv.failed) != 2 {
		t.Fatalf("5xx reply %+v, %d requests", reply, requests.Load())
	}
}

// An answer that is not koko's PCM is refused without counting as a crash.
func TestSpeakerRefusesOtherAnswers(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write([]byte("RIFF\xff\xff\xff\xffWAVE"))
	}))
	defer engine.Close()
	srv := &fakeServer{base: engine.URL}
	reply := speaker{server: srv, client: engine.Client()}.Run(context.Background(), kokoro.KokoroV1, speech("Done.", "af_sky", wire.SpeechFormatWav))
	if reply.Outcome != wire.ReplyOutcomeUnavailable || reply.Reason != "upstream:answer" || len(srv.failed) != 0 {
		t.Fatalf("reply %+v failures %q", reply, srv.failed)
	}
}

// whisper.cpp condition 7: only WAV reaches the engine; other formats and
// word timestamps read unsupported_feature.
func TestTranscriberUnsupported(t *testing.T) {
	var tr transcriber
	base := wire.TranscriptionRequest{Model: "Whisper-Large-v3-Turbo", MediaType: "audio/wav", Timestamps: wire.TimestampModeSegment}
	for want, change := range map[string]func(*wire.TranscriptionRequest){
		"":                  func(*wire.TranscriptionRequest) {},
		"format:audio/mpeg": func(r *wire.TranscriptionRequest) { r.MediaType = "audio/mpeg" },
		"format:audio/flac": func(r *wire.TranscriptionRequest) { r.MediaType = "audio/flac" },
		"timestamps:word":   func(r *wire.TranscriptionRequest) { r.Timestamps = wire.TimestampModeWord },
		"language:xx":       func(r *wire.TranscriptionRequest) { r.Language = "xx" },
		"model:whisper-1":   func(r *wire.TranscriptionRequest) { r.Model = "whisper-1" },
	} {
		req := base
		change(&req)
		if got := tr.Unsupported(req); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

// The transcriber sends the provider's form and relays whisper-server's
// segments; malformed WAV is invalid before any request.
func TestTranscriberRun(t *testing.T) {
	var contentType string
	var requests atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		contentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"text":"hello","language":"en","duration":0.5,"segments":[{"start":0,"end":0.5,"text":"hello"}]}`))
	}))
	defer engine.Close()
	tr := transcriber{server: &fakeServer{base: engine.URL}, client: engine.Client()}
	wav := append([]byte("RIFF\x28\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00"), 1, 0, 2, 0)
	var segments []wire.TranscriptSegment
	end := tr.Run(context.Background(), "Whisper-Large-v3-Turbo", wire.TranscriptionRequest{Timestamps: wire.TimestampModeSegment, Language: "en"}, wav, func(s wire.TranscriptSegment) { segments = append(segments, s) })
	if end.Outcome != wire.ReplyOutcomeCompleted || len(segments) != 1 || segments[0].Text != "hello" || !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("end %+v segments %+v type %q", end, segments, contentType)
	}
	end = tr.Run(context.Background(), "Whisper-Large-v3-Turbo", wire.TranscriptionRequest{}, []byte("RIFF....WAVEjunk"), func(wire.TranscriptSegment) {})
	if end.Outcome != wire.ReplyOutcomeInvalid || end.Reason != "audio:wav" || requests.Load() != 1 {
		t.Fatalf("malformed WAV %+v, %d requests", end, requests.Load())
	}
}

// Kokoro honours a speaking rate within its own bounds and refuses voice
// instructions and a rate beyond them by name (inference INF-S9).
func TestSpeakerHonoursSpeedAndRefusesInstructions(t *testing.T) {
	with := func(key, value string) wire.SpeechRequest {
		req := speech("Hello.", "af_sky", wire.SpeechFormatWav)
		req.Extensions = map[string]string{key: value}
		return req
	}
	var s speaker
	for want, req := range map[string]wire.SpeechRequest{
		"":                              with(inference.SpeechSpeedExtension, "1.5"),
		"extension:openai/speed":        with(inference.SpeechSpeedExtension, "3"),
		"extension:openai/instructions": with(inference.SpeechInstructionsExtension, "Whisper."),
	} {
		if got := s.Unsupported(req); got != want {
			t.Errorf("%v: %q, want %q", req.Extensions, got, want)
		}
	}
	var body []byte
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "audio/l16;rate=24000;endianness=little-endian")
		w.Write([]byte{1, 0, 2, 0})
	}))
	defer engine.Close()
	reply := speaker{server: &fakeServer{base: engine.URL}, client: engine.Client()}.Run(context.Background(), kokoro.KokoroV1, with(inference.SpeechSpeedExtension, "1.5"))
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil || reply.Outcome != wire.ReplyOutcomeCompleted || sent["speed"] != 1.5 {
		t.Fatalf("reply %+v sent %s", reply, body)
	}
}
