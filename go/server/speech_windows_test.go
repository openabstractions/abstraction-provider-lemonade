//go:build windows

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const fixtureRevision = "0123456789abcdef0123456789abcdef01234567"

// writeSpeechBackend writes a backend folder whose files may be nested, as
// Kokoros keeps espeak-ng-data/ beside koko.exe, and returns its manifest.
func writeSpeechBackend(t *testing.T, cacheDir, family, variant string, files map[string]string) BackendManifest {
	t.Helper()
	m := BackendManifest{Family: family, Variant: variant, Version: "fixture-1"}
	dir := m.Dir(cacheDir)
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, ManifestFile{Name: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Name < m.Files[j].Name })
	if err := os.WriteFile(filepath.Join(dir, "version.txt"), []byte("fixture-1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return m
}

// speechModelPin pins files in the cache layout; write puts them there.
func speechModelPin(t *testing.T, modelsDir, name, folder string, files map[string]string, write bool) SpeechModel {
	t.Helper()
	m := SpeechModel{Name: name, Folder: folder, Revision: fixtureRevision}
	for file, content := range files {
		sum := sha256.Sum256([]byte(content))
		m.Files = append(m.Files, ManifestFile{Name: file, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
		if write {
			p := filepath.Join(modelsDir, folder, "snapshots", fixtureRevision, file)
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if write {
		refs := filepath.Join(modelsDir, folder, "refs")
		if err := os.MkdirAll(refs, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(refs, "main"), []byte(fixtureRevision), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// speechConfig is fakeConfig serving both audio profiles: Lemonade runs from
// a proven package folder, and fixture whisper.cpp and Kokoros folders and
// models stand for the reviewed ones. kokoroModel false leaves the Kokoros
// model unstaged.
func speechConfig(t *testing.T, mode string, kokoroModel bool) Config {
	t.Helper()
	cfg := fakeConfig(t, mode)
	program, err := os.ReadFile(cfg.Program)
	if err != nil {
		t.Fatal(err)
	}
	root, m := writePackageFixture(t, map[string][]byte{"lemond.exe": program, "resources/defaults.json": []byte(`{}`)})
	cfg.Program, cfg.Package = filepath.Join(root, "lemond.exe"), m
	cfg.ModelsDir = filepath.Join(filepath.Dir(cfg.CacheDir), "engine-models")
	cfg.Transcription, cfg.Speech = true, true
	cfg.WhisperBackend = writeSpeechBackend(t, cfg.CacheDir, "whispercpp", "vulkan", map[string]string{"whisper-server.exe": "server", "ggml.dll": "ggml"})
	cfg.KokoroBackend = writeSpeechBackend(t, cfg.CacheDir, "kokoro", "cpu", map[string]string{"koko.exe": "koko", "espeak-ng-data/phondata": "ph"})
	cfg.WhisperModel = speechModelPin(t, cfg.ModelsDir, "Whisper-Large-v3-Turbo", "models--ggerganov--whisper.cpp", map[string]string{"ggml-large-v3-turbo.bin": "weights"}, true)
	cfg.KokoroModel = speechModelPin(t, cfg.ModelsDir, "kokoro-v1", "models--mikkoph--kokoro-onnx",
		map[string]string{"index.json": `{"model":"kokoro-v1.0.onnx","voices":"voices-v1.0.bin"}`, "kokoro-v1.0.onnx": "onnx", "voices-v1.0.bin": "voices"}, kokoroModel)
	return cfg
}

var fixtureWAV = append([]byte("RIFF\x28\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00"), 1, 0, 2, 0)

func transcriptionRequest(t *testing.T, base string, extra ...[2]string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, _ := mw.CreateFormFile("file", `C:\Users\someone\note.wav`)
	part.Write(fixtureWAV)
	mw.WriteField("model", "Whisper-Large-v3-Turbo")
	mw.WriteField("response_format", "verbose_json")
	for _, f := range extra {
		mw.WriteField(f[0], f[1])
	}
	mw.Close()
	req, err := http.NewRequest(http.MethodPost, base+TranscriptionRoute, &b)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func speechRequest(t *testing.T, base, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+SpeechRoute, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// The audio routes reach the engine with the provider's own bodies: the
// transcription form with audio.wav and the provider's WAV, the speech body
// with pcm and stream false. Engine fields never reach it.
func TestAudioRoutesForwardTheProvidersBodies(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CheckAudio("Whisper-Large-v3-Turbo"); err != nil {
		t.Fatal(err)
	}
	resp, err := s.HTTP().Do(transcriptionRequest(t, s.Base(), [2]string{"timestamp_granularities[]", "segment"}))
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(answer, []byte("harbour lighthouse")) {
		t.Fatalf("transcription %d %s", resp.StatusCode, answer)
	}
	seen, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "transcription-body"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(seen, []byte(`filename="audio.wav"`)) || bytes.Contains(seen, []byte("note.wav")) || !bytes.Contains(seen, fixtureWAV) || bytes.Contains(seen, []byte("timestamp_granularities")) {
		t.Fatalf("engine received %q", seen)
	}
	if _, err := s.HTTP().Do(transcriptionRequest(t, s.Base(), [2]string{"translate", "true"})); !errors.Is(err, ErrAudioRoute) {
		t.Fatalf("translate forwarded: %v", err)
	}
	resp, err = s.HTTP().Do(speechRequest(t, s.Base(), `{"model":"kokoro-v1","voice":"af_sky","input":"Ready.","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	pcm, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(pcm, []byte{1, 0, 2, 0, 3, 0}) {
		t.Fatalf("speech answer % x", pcm)
	}
	for _, body := range []string{
		`{"model":"kokoro-v1","voice":"af_sky","input":"Ready.","response_format":"pcm","initial_silence":100000}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Ready.","response_format":"pcm","lang_code":"../../x"}`,
	} {
		if _, err := s.HTTP().Do(speechRequest(t, s.Base(), body)); !errors.Is(err, ErrAudioRoute) {
			t.Fatalf("%s forwarded: %v", body, err)
		}
	}
	bodies, err := os.ReadFile(filepath.Join(evidenceDir(cfg), "speech-bodies"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(bodies)); got != `{"model":"kokoro-v1","input":"Ready.","voice":"af_sky","response_format":"pcm","speed":1,"stream":false}` {
		t.Fatalf("engine received %s", got)
	}
}

// Kokoros condition 2: without the pinned model the speech route stays
// closed, before Lemonade could download it (K4); transcription still runs.
func TestSpeechRouteClosedWithoutTheModel(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", false)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.CheckAudio("kokoro-v1")
	var refusal *AudioRefusal
	if !errors.As(err, &refusal) || refusal.Condition != "model-files" || RefusalReason(err) != "engine:audio:model-files" {
		t.Fatalf("speech without its model: %v", err)
	}
	if _, err := s.HTTP().Do(speechRequest(t, s.Base(), `{"model":"kokoro-v1","voice":"af_sky","input":"Ready.","response_format":"pcm"}`)); !errors.Is(err, ErrAudioRoute) {
		t.Fatalf("speech forwarded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir(cfg), "speech-bodies")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the engine received a speech request")
	}
	if err := s.CheckAudio("Whisper-Large-v3-Turbo"); err != nil {
		t.Fatalf("transcription: %v", err)
	}
}

// Conditions 1, 2 and 6: the speech backend and model files are proven and
// held while the engine runs; a changed model file refuses the next launch.
func TestSpeechFilesAreHeldAndProven(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	weights := filepath.Join(cfg.ModelsDir, filepath.FromSlash(cfg.WhisperModel.Paths()[0]))
	phondata := filepath.Join(cfg.KokoroBackend.Dir(cfg.CacheDir), "espeak-ng-data", "phondata")
	for _, p := range []string{weights, phondata} {
		if err := os.WriteFile(p, []byte("swapped"), 0600); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			s.Close()
			t.Fatalf("write of %s while the engine runs: %v", p, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(weights, []byte("swapped"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrWeightFormat) || s.proc != nil {
		t.Fatalf("a changed model launched: %v", err)
	}
	// The refused NPU backend folder refuses every launch.
	if err := os.WriteFile(weights, []byte("weights"), 0600); err != nil {
		t.Fatal(err)
	}
	npu := filepath.Join(cfg.CacheDir, "bin", "whispercpp", "npu")
	if err := os.MkdirAll(npu, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrBackendManifest) || s.proc != nil {
		t.Fatalf("an NPU folder launched: %v", err)
	}
}

// Condition 9 of both reviews: once an audio request ends and nothing else
// reaches the tree for IdleStop, the tree stops; the next admission starts
// it again.
func TestIdleStopAfterAudio(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	cfg.IdleStop = 300 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := s.HTTP().Do(speechRequest(t, s.Base(), `{"model":"kokoro-v1","voice":"bm_lewis","input":"Done.","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		running := s.proc != nil
		s.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tree still runs after the idle window")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir(cfg), "shutdown-seen")); err != nil {
		t.Fatalf("idle stop without graceful shutdown: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("restart after the idle stop: %v", err)
	}
}

// running reports whether the server holds a launched tree.
func running(s *Server) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc != nil
}

// waitStopped waits for the tree to stop within d.
func waitStopped(t *testing.T, s *Server, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for running(s) {
		if time.Now().After(deadline) {
			t.Fatal("the tree still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The idle stop counts the provider's admitted and running work of every
// profile: an image job still admitted after the last speech request keeps
// the tree running past the idle window, which stops it once that work ends.
func TestIdleStopWaitsForWorkOfAnyProfile(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	cfg.IdleStop = 150 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var imageWork atomic.Bool
	imageWork.Store(true)
	s.BindActivity(func(except string) bool { return except != "image-model" && imageWork.Load() })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := s.HTTP().Do(speechRequest(t, s.Base(), `{"model":"kokoro-v1","voice":"bm_lewis","input":"Done.","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	time.Sleep(5 * cfg.IdleStop)
	if !running(s) {
		t.Fatal("the idle stop ended the tree under admitted image work")
	}
	imageWork.Store(false)
	waitStopped(t, s, 10*time.Second)
}

// An admission that reaches Start while the idle stop reads the provider's
// activity is never answered by a tree the stop then ends: the check and the
// stop are one hold of the server's lock, and the admission's Start runs
// after the stop and launches again. The activity callback runs under that
// lock, so the admission runs beside it, as the provider's admissions do.
// The test checks the tree once that idle check has returned.
func TestIdleStopNeverEndsAnAdmittedStart(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	cfg.IdleStop = 150 * time.Millisecond
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var read atomic.Bool
	var checkedOnce sync.Once
	checked := make(chan struct{})
	s.idleChecked = func() {
		if read.Load() {
			checkedOnce.Do(func() { close(checked) })
		}
	}
	admitted := make(chan error, 1)
	s.BindActivity(func(except string) bool {
		if except == "" && read.CompareAndSwap(false, true) {
			go func() { admitted <- s.Start(context.Background()) }()
			// The admission gets its chance to finish before the answer.
			time.Sleep(200 * time.Millisecond)
		}
		return false
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := s.HTTP().Do(speechRequest(t, s.Base(), `{"model":"kokoro-v1","voice":"bm_lewis","input":"Done.","response_format":"pcm"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	select {
	case <-checked:
	case <-time.After(30 * time.Second):
		t.Fatal("the idle check never read the provider's activity")
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("the admission's Start: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the admission's Start never returned")
	}
	if err := s.CheckAudio("Whisper-Large-v3-Turbo"); err != nil {
		t.Fatalf("the admitted request met a stopped tree: %v", err)
	}
	resp, err = s.HTTP().Do(transcriptionRequest(t, s.Base()))
	if err != nil {
		t.Fatalf("the admitted request failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// A speech crash refuses speech, and speech only, for the backoff. While
// another model has work the tree keeps running; with none, it stops.
func TestSpeechCrashKeepsOtherWork(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", true)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var other atomic.Bool
	other.Store(true)
	s.BindActivity(func(except string) bool { return except == "kokoro-v1" && other.Load() })
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SpeechFailed("sha256:x", errors.New("connection reset")); err == nil {
		t.Fatal("no crash report")
	}
	if !running(s) {
		t.Fatal("a speech crash stopped the tree under other work")
	}
	var refusal *AudioRefusal
	if err := s.CheckAudio("kokoro-v1"); !errors.As(err, &refusal) || refusal.Condition != "backoff" {
		t.Fatalf("speech after a crash: %v", err)
	}
	if err := s.CheckAudio("Whisper-Large-v3-Turbo"); err != nil {
		t.Fatalf("transcription after a speech crash: %v", err)
	}
	other.Store(false)
	_ = s.SpeechFailed("sha256:y", errors.New("again"))
	if running(s) {
		t.Fatal("a speech crash with no other work left the tree running")
	}
}

// An unstaged audio profile is refused by name before anything starts.
func TestAudioStagedRefusesBeforeStart(t *testing.T) {
	cfg := speechConfig(t, "helper-ready", false)
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	condition := func(err error) string {
		var refusal *AudioRefusal
		if errors.As(err, &refusal) {
			return refusal.Condition
		}
		return fmt.Sprint(err)
	}
	if err := s.AudioStaged(context.Background(), "Whisper-Large-v3-Turbo"); err != nil {
		t.Fatalf("staged transcription: %v", err)
	}
	if got := condition(s.AudioStaged(context.Background(), "kokoro-v1")); got != "model-files" {
		t.Fatalf("unstaged speech model: %q", got)
	}
	if got := condition(s.AudioStaged(context.Background(), "tts-1")); got != "disabled" {
		t.Fatalf("undeclared audio model: %q", got)
	}
	if err := os.RemoveAll(cfg.WhisperBackend.Dir(cfg.CacheDir)); err != nil {
		t.Fatal(err)
	}
	if got := condition(s.AudioStaged(context.Background(), "Whisper-Large-v3-Turbo")); got != "engine-files" {
		t.Fatalf("unstaged whisper backend: %q", got)
	}
	if running(s) {
		t.Fatal("a staging check started the tree")
	}
}

// The speech fixture paths are the pins' own.
func TestSpeechFixturePaths(t *testing.T) {
	m := SpeechModel{Folder: "models--a--b", Revision: fixtureRevision, Files: []ManifestFile{{Name: "x.bin"}}}
	if got := m.Paths(); len(got) != 1 || got[0] != path.Join("models--a--b", "snapshots", fixtureRevision, "x.bin") {
		t.Fatal(got)
	}
}
