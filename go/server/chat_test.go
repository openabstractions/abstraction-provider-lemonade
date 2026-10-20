package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngDataURI(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func dataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// wav is the shortest header llama.cpp's mtmd helper recognises as audio.
var wav = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 28)...)

func chatBody(t *testing.T, extra map[string]any, parts ...any) []byte {
	t.Helper()
	content := any("hello")
	if len(parts) > 0 {
		content = parts
	}
	body := map[string]any{"model": "user.Chat", "messages": []any{map[string]any{"role": "user", "content": content}}, "stream": false, "seed": 12345678901234567}
	for k, v := range extra {
		body[k] = v
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func imagePart(url string) any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func audioPart(key, value string) any {
	return map[string]any{"type": "input_audio", "input_audio": map[string]any{key: value, "format": "wav"}}
}

func allowChat(model string) bool { return model == "user.Chat" }

// A chat request reaches Lemonade only without load options and with media
// carried as base64 PNG, JPEG or audio data. Each refusal names the review
// condition it enforces.
func TestChatBodyRefusals(t *testing.T) {
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 8)...)
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	for name, c := range map[string]struct {
		body      []byte
		condition string
	}{
		"ctx_size":                {chatBody(t, map[string]any{"ctx_size": 131072}), "load-options"},
		"llamacpp_args":           {chatBody(t, map[string]any{"llamacpp_args": "--tools all"}), "load-options"},
		"llamacpp_backend":        {chatBody(t, map[string]any{"llamacpp_backend": "cpu"}), "load-options"},
		"save_options":            {chatBody(t, map[string]any{"save_options": true}), "load-options"},
		"recipe_options":          {chatBody(t, map[string]any{"recipe_options": map[string]any{"llamacpp_args": "--rpc x"}}), "load-options"},
		"sdcpp_args":              {chatBody(t, map[string]any{"sdcpp_args": "--listen-ip 0.0.0.0"}), "load-options"},
		"Ollama options":          {chatBody(t, map[string]any{"options": map[string]any{"num_ctx": 4096}}), "load-options"},
		"http image":              {chatBody(t, nil, imagePart("http://127.0.0.1:8080/x.png")), "media"},
		"https image":             {chatBody(t, nil, imagePart("https://example.invalid/x.png")), "media"},
		"http-prefixed image":     {chatBody(t, nil, imagePart("httpfoo")), "media"},
		"file image":              {chatBody(t, nil, imagePart("file:///C:/Windows/win.ini")), "media"},
		"raw base64 image":        {chatBody(t, nil, imagePart(base64.StdEncoding.EncodeToString(wav))), "media"},
		"https audio":             {chatBody(t, nil, audioPart("url", "https://example.invalid/a.wav")), "media"},
		"https audio beside data": {chatBody(t, nil, audioPart("url", "https://example.invalid/a.wav"), audioPart("data", dataURI("audio/wav", wav))), "media"},
		"video data URI":          {chatBody(t, nil, imagePart(dataURI("video/mp4", []byte("....ftypisom")))), "media"},
		"video part":              {chatBody(t, nil, map[string]any{"type": "input_video", "input_video": map[string]any{"data": dataURI("video/mp4", []byte("x"))}}), "media"},
		"WebP image":              {chatBody(t, nil, imagePart(dataURI("image/webp", webp))), "media"},
		"WebP named as PNG":       {chatBody(t, nil, imagePart(dataURI("image/png", webp))), "media"},
		"JPEG named as PNG":       {chatBody(t, nil, imagePart(strings.Replace(jpegDataURI(t, 4, 4), "image/jpeg", "image/png", 1))), "media"},
		"PNG named as audio":      {chatBody(t, nil, imagePart(strings.Replace(pngDataURI(t), "image/png", "audio/wav", 1))), "media"},
		"GIF image":               {chatBody(t, nil, imagePart(dataURI("image/gif", gif))), "media"},
		"undecodable image":       {chatBody(t, nil, imagePart(dataURI("image/png", []byte("\x89PNG\r\n\x1a\nnot an image")))), "media"},
		"image without an object": {chatBody(t, nil, map[string]any{"type": "image_url", "image_url": "https://example.invalid/x.png"}), "media"},
		"unknown part type":       {chatBody(t, nil, map[string]any{"type": "file", "file": map[string]any{"file_data": "x"}}), "media"},
		"undeclared model":        {bytes.Replace(chatBody(t, nil), []byte("user.Chat"), []byte("user.Other"), 1), "model"},
		"no model":                {[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "model"},
		"not an object":           {[]byte(`[1,2]`), "request"},
		"two values":              {[]byte(`{"model":"user.Chat","messages":[]} {"ctx_size":1}`), "request"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := checkChatBody(c.body, allowChat)
			var refusal *ChatRefusal
			if !errors.As(err, &refusal) || !errors.Is(err, ErrChatRoute) || refusal.Condition != c.condition {
				t.Fatalf("%s: %v, want condition %s", name, err, c.condition)
			}
			if got := RefusalReason(err); got != "engine:chat-route:"+c.condition {
				t.Fatalf("reason %q", got)
			}
		})
	}
}

// Accepted requests are forwarded as the provider re-encoded them, so
// Lemonade parses exactly what was checked: a duplicate key cannot carry a
// second value past the check, and large integers keep their digits.
func TestChatBodyForwardsWhatWasChecked(t *testing.T) {
	for name, body := range map[string][]byte{
		"text":      chatBody(t, nil),
		"PNG image": chatBody(t, nil, map[string]any{"type": "text", "text": "what is this"}, imagePart(pngDataURI(t))),
		"WAV audio": chatBody(t, nil, audioPart("data", dataURI("audio/wav", wav))),
	} {
		out, err := checkChatBody(body, allowChat)
		if err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
		if !bytes.Contains(out, []byte(`"seed":12345678901234567`)) {
			t.Fatalf("%s: integer changed: %s", name, out)
		}
	}
	dup := []byte(`{"model":"user.Chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + pngDataURI(t) + `","url":"https://example.invalid/x.png"}}]}]}`)
	if _, err := checkChatBody(dup, allowChat); !errors.Is(err, ErrChatRoute) {
		t.Fatalf("duplicate url key accepted: %v", err)
	}
	dup = []byte(`{"model":"user.Chat","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/x.png","url":"` + pngDataURI(t) + `"}}]}]}`)
	out, err := checkChatBody(dup, allowChat)
	if err == nil && strings.Contains(string(out), "https://") {
		t.Fatalf("a refused URL was forwarded behind a duplicate key: %s", out)
	}
}

func jpegDataURI(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{G: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return dataURI("image/jpeg", buf.Bytes())
}

// forwardedImage returns the bytes of the one image a forwarded body carries.
func forwardedImage(t *testing.T, body []byte) []byte {
	t.Helper()
	var request struct {
		Messages []struct {
			Content []struct {
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	for _, m := range request.Messages {
		for _, p := range m.Content {
			if p.ImageURL != nil {
				payload, ok := strings.CutPrefix(p.ImageURL.URL, "data:image/png;base64,")
				if !ok {
					t.Fatalf("forwarded image is not a PNG data URI: %.40s", p.ImageURL.URL)
				}
				data, err := base64.StdEncoding.DecodeString(payload)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
		}
	}
	t.Fatal("no image forwarded")
	return nil
}

// Every image is decoded and re-encoded as an 8-bit NRGBA PNG, so llama.cpp's
// stb decoder only parses bytes the provider produced and never hands an
// image to FFmpeg; an image above 16 megapixels is refused.
func TestChatImagesAreReencodedAsProviderPNG(t *testing.T) {
	for name, uri := range map[string]string{"JPEG": jpegDataURI(t, 33, 17), "PNG": pngDataURI(t)} {
		out, err := checkChatBody(chatBody(t, nil, imagePart(uri)), allowChat)
		if err != nil {
			t.Fatalf("%s refused: %v", name, err)
		}
		data := forwardedImage(t, out)
		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil || format != "png" {
			t.Fatalf("%s forwarded as %s: %v", name, format, err)
		}
		// IHDR's bit depth byte: png.Encode writes an 8-bit NRGBA image as
		// 8-bit RGB or RGBA.
		if len(data) < 25 || data[24] != 8 || img.Bounds().Min != (image.Point{}) {
			t.Fatalf("%s forwarded as %T with bit depth %d", name, img, data[24])
		}
		if strings.Contains(string(out), strings.TrimPrefix(uri, "data:image/jpeg;base64,")) && name == "JPEG" {
			t.Fatal("the caller's JPEG bytes were forwarded")
		}
	}
	big := image.NewGray(image.Rect(0, 0, 4097, 4097))
	var buf bytes.Buffer
	if err := png.Encode(&buf, big); err != nil {
		t.Fatal(err)
	}
	_, err := checkChatBody(chatBody(t, nil, imagePart(dataURI("image/png", buf.Bytes()))), allowChat)
	var refusal *ChatRefusal
	if !errors.As(err, &refusal) || refusal.Condition != "media" {
		t.Fatalf("a %d-pixel image was accepted: %v", 4097*4097, err)
	}
}

// input_audio.data is accepted as OpenAI's raw base64 and as a data: URI; the
// bytes must be audio llama.cpp recognises.
func TestChatAudioForms(t *testing.T) {
	for name, part := range map[string]any{
		"raw base64": audioPart("data", base64.StdEncoding.EncodeToString(wav)),
		"data URI":   audioPart("data", dataURI("audio/wav", wav)),
	} {
		if _, err := checkChatBody(chatBody(t, nil, part), allowChat); err != nil {
			t.Fatalf("%s audio refused: %v", name, err)
		}
	}
	// The provider forwards its own standard base64 of the checked bytes:
	// unpadded base64 and a caller's data: URI label never reach Lemonade.
	for name, c := range map[string]struct{ key, given, want string }{
		"unpadded raw": {"data", base64.RawStdEncoding.EncodeToString(wav), base64.StdEncoding.EncodeToString(wav)},
		"x-wav label":  {"url", dataURI("audio/x-wav", wav), "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(wav)},
	} {
		out, err := checkChatBody(chatBody(t, nil, audioPart(c.key, c.given)), allowChat)
		if err != nil {
			t.Fatalf("%s audio refused: %v", name, err)
		}
		if !bytes.Contains(out, []byte(`"`+c.key+`":"`+c.want+`"`)) || bytes.Contains(out, []byte(c.given+`"`)) {
			t.Fatalf("%s audio not forwarded as the provider's base64: %.200s", name, out)
		}
	}
	for name, part := range map[string]any{
		"raw base64 image":       audioPart("data", base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n0000000000"))),
		"data URI text":          audioPart("data", dataURI("audio/wav", []byte("not audio at all"))),
		"raw base64 in the url":  audioPart("url", base64.StdEncoding.EncodeToString(wav)),
		"undecodable raw base64": audioPart("data", "!!not base64!!"),
	} {
		_, err := checkChatBody(chatBody(t, nil, part), allowChat)
		var refusal *ChatRefusal
		if !errors.As(err, &refusal) || refusal.Condition != "media" {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
