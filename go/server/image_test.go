package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	media "github.com/openabstractions/abstraction-inference/go/mediationcontent"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pngHeader is a PNG signature and IHDR chunk naming width × height; the
// header alone is what the input size check reads.
func pngHeader(width, height uint32) []byte {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8], ihdr[9] = 8, 6
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

var pngInput = pngHeader(512, 512)

type formPart struct {
	name, file, media, encoding string
	data                        []byte
}

func imageForm(t *testing.T, parts ...formPart) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, p := range parts {
		header := make(textproto.MIMEHeader)
		disposition := fmt.Sprintf(`form-data; name=%q`, p.name)
		if p.file != "" {
			disposition += fmt.Sprintf(`; filename=%q`, p.file)
		}
		header.Set("Content-Disposition", disposition)
		if p.media != "" {
			header.Set("Content-Type", p.media)
		}
		if p.encoding != "" {
			header.Set("Content-Transfer-Encoding", p.encoding)
		}
		part, err := w.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), body.Bytes()
}

func editForm(t *testing.T, prompt, size string, extra ...formPart) (string, []byte) {
	parts := []formPart{{name: "model", data: []byte("user.ApprovedKlein")}, {name: "prompt", data: []byte(prompt)},
		{name: "n", data: []byte("1")}, {name: "size", data: []byte(size)}, {name: "response_format", data: []byte("b64_json")},
		{name: "image", file: "image.png", media: "image/png", data: pngInput}}
	return imageForm(t, append(parts, extra...)...)
}

const directive = `<sd_cpp_extra_args>{"lora":[{"path":"\\\\attacker\\share\\x.ckpt","multiplier":1}]}</sd_cpp_extra_args>`

// Stable-diffusion.cpp review addendum, findings for the base review: an
// image request whose text carries sd-server's engine directive, or whose
// size is beyond the bound, never leaves the provider; inference CONTRACT.md
// INF-SD1 and INF-SD2. The refusal comes before the request waits for the
// engine, so this server needs no running Lemonade.
func TestImageRouteRefusesEngineDirectiveAndSize(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:41234", requests: make(chan struct{}, 1), cfg: Config{Models: []string{"user.ApprovedKlein"}}}
	jsonBody := func(prompt string) string {
		return fmt.Sprintf(`{"model":"user.ApprovedKlein","prompt":%q,"n":1,"size":"1024x1024","response_format":"b64_json"}`, prompt)
	}
	type request struct {
		path, contentType, body string
		want                    error
	}
	requests := []request{
		{"/api/v1/images/generations", "application/json", jsonBody("a lighthouse " + directive), media.ErrEngineDirective},
		{"/api/v1/images/generations", "application/json", jsonBody("<SD_CPP_EXTRA_ARGS>{}"), media.ErrEngineDirective},
		{"/api/v1/images/generations", "application/json", jsonBody("\uFF1Csd_cpp_extra_args\uFF1E{}"), media.ErrEngineDirective},
		{"/api/v1/images/generations", "", `{"prompt":"x","model":"<sd_cpp_extra_args>{}"}`, media.ErrEngineDirective},
		{"/api/v1/images/generations", "", `{"prompt":"x","size":"4096x4096"}`, media.ErrImageSize},
		{"/api/v1/images/generations", "", `{"prompt":"x","size":"65536x21846"}`, media.ErrImageSize},
		// Only the fields inference sends are forwarded.
		{"/api/v1/images/generations", "", `{"prompt":"x","negative_prompt":"<sd_cpp_extra_args>{}"}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","extra":{"nested":["<sd_cpp_extra_args"]}}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","<sd_cpp_extra_args>":1}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","width":65536,"height":21846}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","lora":[{"path":"C:\\x.ckpt"}]}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":["x"]}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","n":"1"}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x","n":1.5}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `{"prompt":"x"} {"prompt":"y"}`, media.ErrRequestForm},
		{"/api/v1/images/generations", "", `prompt=x`, media.ErrRequestForm},
	}
	contentType, body := editForm(t, "a lighthouse "+directive, "512x512")
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrEngineDirective})
	contentType, body = editForm(t, "a lighthouse", "4096x4096")
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrImageSize})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "negative_prompt", data: []byte("<sd_cpp_extra_args>")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "prompt", data: []byte("second")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = imageForm(t, formPart{name: "prompt", encoding: "quoted-printable", data: []byte("=3Csd_cpp_extra_args>")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "mask", file: "mask.gif", media: "image/gif", data: []byte("GIF89a....")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	// cpp-httplib and Go's form parser may read a part name holding a quote
	// or a parameter differently; such a name is outside the allow-list.
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "x; filename=a.png", media: "image/png", data: pngInput})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "prompt\"; name=\"image", data: []byte("x")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "prompt", file: "prompt.txt", media: "image/png", data: pngInput})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "mask", data: []byte("text in a file's place")})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrRequestForm})
	// INF-SD2: with size "auto" sd-server takes the size from the input, so
	// the input's header is bounded too; a mask is bounded the same way.
	contentType, body = imageForm(t, formPart{name: "prompt", data: []byte("a lighthouse")}, formPart{name: "size", data: []byte("auto")},
		formPart{name: "image", file: "image.png", media: "image/png", data: pngHeader(16384, 16384)})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrImageSize})
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "mask", file: "mask.png", media: "image/png", data: pngHeader(65536, 21846)})
	requests = append(requests, request{"/api/v1/images/edits", contentType, string(body), media.ErrImageSize})

	for i, r := range requests {
		req, err := http.NewRequest(http.MethodPost, s.base+r.path, strings.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		if r.contentType != "" {
			req.Header.Set("Content-Type", r.contentType)
		}
		resp, err := s.HTTP().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrImageRoute) || !errors.Is(err, r.want) {
			t.Fatalf("request %d (%s): %v, want %v", i, r.path, err, r.want)
		}
		if strings.Contains(err.Error(), "attacker") {
			t.Fatalf("request %d: refusal repeats the text: %v", i, err)
		}
	}

	// A request that passes the check goes on to wait for the engine, which
	// this server has not started.
	contentType, body = editForm(t, "a lighthouse", "512x512", formPart{name: "mask", file: "<sd_cpp_extra_args>.png", media: "image/png",
		data: contractMask(t, 512, 512, func(x, y int) bool { return x < 64 })})
	autoType, autoBody := imageForm(t, formPart{name: "model", data: []byte("user.ApprovedKlein")}, formPart{name: "prompt", data: []byte("a lighthouse")},
		formPart{name: "size", data: []byte("auto")}, formPart{name: "image", file: "image.png", media: "image/png", data: pngHeader(2048, 1024)})
	for _, r := range []request{
		{path: "/api/v1/images/edits", contentType: autoType, body: string(autoBody)},
		{path: "/api/v1/images/generations", contentType: "application/json", body: jsonBody("a lighthouse")},
		{path: "/api/v1/images/edits", contentType: contentType, body: string(body)},
	} {
		req, err := http.NewRequest(http.MethodPost, s.base+r.path, strings.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", r.contentType)
		if _, err := s.HTTP().Do(req); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s: %v", r.path, err)
		}
	}
}

// The forwarded body is the provider's own encoding of the fields it checked.
func TestImageBodyIsForwardedAsChecked(t *testing.T) {
	allows := func(model string) bool { return model == "m" || model == "user.ApprovedKlein" }
	checked, err := checkImageJSON([]byte(`{"prompt":"a <b> lighthouse","n":12345678901234567,"size":"auto","model":"m","prompt":"second"}`), allows)
	if err != nil {
		t.Fatal(err)
	}
	if string(checked) != `{"model":"m","n":12345678901234567,"prompt":"second","size":"auto"}` {
		t.Fatalf("encoded %s", checked)
	}
	mask := contractMask(t, 512, 512, func(x, y int) bool { return y < 32 })
	engine, _, _, err := engineMask(mask)
	if err != nil {
		t.Fatal(err)
	}
	contentType, body := editForm(t, "a lighthouse", "1024x1024", formPart{name: "mask", file: "../../mask.png", media: "image/png", data: mask})
	_, params, _ := mime.ParseMediaType(contentType)
	out, outType, err := checkImageForm(body, params["boundary"], allows)
	if err != nil {
		t.Fatal(err)
	}
	media, outParams, err := mime.ParseMediaType(outType)
	if err != nil || media != "multipart/form-data" || outParams["boundary"] == params["boundary"] {
		t.Fatalf("content type %q", outType)
	}
	reader := multipart.NewReader(bytes.NewReader(out), outParams["boundary"])
	seen := map[string]string{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		seen[part.FormName()+"|"+part.FileName()] = string(data)
	}
	want := map[string]string{"model|": "user.ApprovedKlein", "prompt|": "a lighthouse", "n|": "1", "size|": "1024x1024",
		"response_format|": "b64_json", "image|image.png": string(pngInput), "mask|mask.png": string(engine)}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("forwarded %v", seen)
	}
}

// An image request naming a speech model, an undeclared model or none never
// reaches Lemonade, which would load the model it names and start
// whisper-server or koko outside the audio routes' checks and idle stop.
func TestImageRouteRefusesModelsOtherThanTheImageModels(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:41234", requests: make(chan struct{}, 1),
		cfg: Config{Models: []string{"user.ApprovedKlein"}, Transcription: true, Speech: true, WhisperModel: WhisperLargeV3TurboModel, KokoroModel: KokoroV1Model}}
	refused := func(path, contentType, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, s.base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		if _, err := s.HTTP().Do(req); !errors.Is(err, ErrImageRoute) || !errors.Is(err, media.ErrRequestForm) {
			t.Fatalf("%s %.60s: %v", path, body, err)
		}
	}
	for _, model := range []string{"kokoro-v1", "Whisper-Large-v3-Turbo", "user.Other"} {
		refused("/api/v1/images/generations", "application/json", fmt.Sprintf(`{"model":%q,"prompt":"a lighthouse","n":1,"size":"512x512"}`, model))
		parts := []formPart{{name: "model", data: []byte(model)}, {name: "prompt", data: []byte("a lighthouse")},
			{name: "image", file: "image.png", media: "image/png", data: pngInput}}
		contentType, body := imageForm(t, parts...)
		refused("/api/v1/images/edits", contentType, string(body))
	}
	refused("/api/v1/images/generations", "application/json", `{"prompt":"a lighthouse"}`)
	contentType, body := imageForm(t, formPart{name: "prompt", data: []byte("a lighthouse")}, formPart{name: "image", file: "image.png", media: "image/png", data: pngInput})
	refused("/api/v1/images/edits", contentType, string(body))
}

// The body bound follows the configured MaxImageBytes: two images and the
// text fields.
func TestImageBodyLimitFollowsMaxImageBytes(t *testing.T) {
	if got := (&Server{}).imageBodyLimit(); got != 2*(20<<20)+1<<20 {
		t.Fatalf("default limit %d", got)
	}
	s := &Server{base: "http://127.0.0.1:41234", requests: make(chan struct{}, 1), cfg: Config{MaxImageBytes: 4096}}
	if got := s.imageBodyLimit(); got != 2*4096+1<<20 {
		t.Fatalf("limit %d", got)
	}
	padding := strings.Repeat("a", int(s.imageBodyLimit()))
	req, err := http.NewRequest(http.MethodPost, s.base+"/api/v1/images/generations", strings.NewReader(`{"prompt":"`+padding+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HTTP().Do(req); !errors.Is(err, media.ErrRequestForm) {
		t.Fatalf("oversized body: %v", err)
	}
}

type countingBody struct {
	read chan struct{}
}

func (b *countingBody) Read(p []byte) (int, error) {
	select {
	case b.read <- struct{}{}:
	default:
	}
	return 0, io.EOF
}

func (b *countingBody) Close() error { return nil }

// A request reads its body only once it holds the provider's one request
// slot, so concurrent callers never hold more than one body in memory; a
// refusal gives the slot back.
func TestImageBodyIsReadInsideTheRequestSlot(t *testing.T) {
	s := &Server{base: "http://127.0.0.1:41234", requests: make(chan struct{}, 1)}
	s.requests <- struct{}{} // another request holds the slot
	body := &countingBody{read: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/v1/images/generations", body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.HTTP().Do(req); done <- err }()
	select {
	case <-body.read:
		t.Fatal("body read while another request held the slot")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting request: %v", err)
	}
	<-s.requests
	refused, err := http.NewRequest(http.MethodPost, s.base+"/api/v1/images/generations", strings.NewReader(`{"lora":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HTTP().Do(refused); !errors.Is(err, media.ErrRequestForm) {
		t.Fatal(err)
	}
	select {
	case s.requests <- struct{}{}:
	default:
		t.Fatal("a refused request kept the slot")
	}
}

// A declared model that resolves to a Lemonade collection is refused: an Omni
// collection's tool calls reach sd-server past the image route's checks.
func TestCollectionModelsAreRefused(t *testing.T) {
	config := t.TempDir()
	registry := `{"Omni":{"recipe":"collection.omni","checkpoint":"lemonade-sdk/LMX-Omni-5.5B-Lite"},` +
		`"Router":{"recipe":"collection.router","checkpoint":"x"},` +
		`"ApprovedKlein":{"recipe":"sd-cpp","checkpoint":"repo:model.safetensors"}}`
	if err := os.WriteFile(filepath.Join(config, "user_models.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	// Lemonade also answers every built-in model as builtin.<name>
	// (canonical_id.h, model_manager.cpp).
	for _, model := range []string{"LMX-Omni-5.5B-Lite", "lmx-omni-52b-halo", "RPG-HaloTales-V2", "builtin.LMX-Omni-5.5B-Lite", "BUILTIN.rpg-halotales-v1",
		"Builtin.LMX-Omni-52B-Halo", "user.Omni", "USER.omni", "user.Router", "extra.Anything"} {
		err := checkDeclaredModels(config, []string{"user.ApprovedKlein", model})
		if !errors.Is(err, ErrCollectionModel) || RefusalReason(fmt.Errorf("%w: %w", ErrConfiguration, err)) != "engine:collection-model" {
			t.Fatalf("%s: %v", model, err)
		}
		dir := t.TempDir()
		_, err = New(Config{Program: os.Args[0], CacheDir: filepath.Join(dir, "cache"), ConfigDir: config, Port: 41234, Models: []string{model}})
		if !errors.Is(err, ErrCollectionModel) {
			t.Fatalf("New admitted %s: %v", model, err)
		}
	}
	for _, models := range [][]string{{"user.ApprovedKlein"}, {"user.Absent"}, {"SD-Turbo"}} {
		if err := checkDeclaredModels(config, models); err != nil {
			t.Fatalf("%v refused: %v", models, err)
		}
	}
	if err := checkDeclaredModels(t.TempDir(), []string{"user.ApprovedKlein"}); err != nil {
		t.Fatalf("absent registry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(config, "user_models.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDeclaredModels(config, []string{"user.ApprovedKlein"}); !errors.Is(err, ErrCollectionModel) {
		t.Fatalf("unreadable registry: %v", err)
	}
}
