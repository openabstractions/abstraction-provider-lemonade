package server

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"testing"

	"github.com/openabstractions/abstraction-inference/go/providerhost/sdcpp"
)

// contractMask is an [INF-I1] mask: an RGBA PNG whose fully transparent
// pixels mark the area to change. Kept pixels vary in colour and partial
// transparency, the cases a luminance reader would misread.
func contractMask(t *testing.T, width, height int, change func(x, y int) bool) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			switch {
			case change(x, y) && x%2 == 0:
				m.SetNRGBA(x, y, color.NRGBA{R: 0, G: 0, B: 0, A: 0})
			case change(x, y):
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 0})
			case x%3 == 0:
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			case x%3 == 1:
				m.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, B: 255, A: 1})
			default:
				m.SetNRGBA(x, y, color.NRGBA{R: 10, G: 20, B: 30, A: 128})
			}
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// forwardedMask runs an edit form through the route check and returns the
// mask part it forwards.
func forwardedMask(t *testing.T, parts ...formPart) ([]byte, error) {
	t.Helper()
	contentType, body := imageForm(t, append([]formPart{{name: "model", data: []byte("user.ApprovedKlein")}, {name: "prompt", data: []byte("snow")}, {name: "size", data: []byte("auto")}}, parts...)...)
	_, params, _ := mime.ParseMediaType(contentType)
	out, outType, err := checkImageForm(body, params["boundary"], anyModel)
	if err != nil {
		return nil, err
	}
	_, outParams, _ := mime.ParseMediaType(outType)
	reader := multipart.NewReader(bytes.NewReader(out), outParams["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			t.Fatal("no mask forwarded")
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "mask" {
			if part.FileName() != "mask.png" || part.Header.Get("Content-Type") != "image/png" {
				t.Fatalf("mask part %q %q", part.FileName(), part.Header.Get("Content-Type"))
			}
			return io.ReadAll(part)
		}
	}
}

// [INF-I1] against sd-server master-843-462d675: routes_openai.cpp loads the
// edit mask with one channel (load_image_from_memory(..., 1), which stb
// reduces to luminance and drops alpha) and stable-diffusion.cpp
// generate_image treats 1.0 as the area to change ((1 - mask) keeps the
// init image). The provider forwards a grayscale PNG of the same size,
// white exactly where the contract mask is fully transparent.
func TestEditMaskBecomesSdServerLuminance(t *testing.T) {
	change := func(x, y int) bool { return (x+y)%4 == 0 || y == 0 && x < 3 }
	input := contractMask(t, 8, 6, func(int, int) bool { return false })
	for _, order := range [][]formPart{
		{{name: "image", file: "image.png", media: "image/png", data: input}, {name: "mask", file: "mask.png", media: "image/png", data: contractMask(t, 8, 6, change)}},
		{{name: "mask", file: "m.png", media: "image/png", data: contractMask(t, 8, 6, change)}, {name: "image", file: "image.png", media: "image/png", data: input}},
	} {
		data, err := forwardedMask(t, order...)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		gray, ok := decoded.(*image.Gray)
		if !ok || gray.Bounds() != image.Rect(0, 0, 8, 6) {
			t.Fatalf("forwarded mask %T %v", decoded, decoded.Bounds())
		}
		for y := 0; y < 6; y++ {
			for x := 0; x < 8; x++ {
				want := uint8(0)
				if change(x, y) {
					want = 255
				}
				if got := gray.GrayAt(x, y).Y; got != want {
					t.Fatalf("pixel %d,%d = %d, want %d", x, y, got, want)
				}
			}
		}
	}
}

// A mask of another size than its input, a mask that is no PNG, and one whose
// pixels do not decode are refused before Lemonade sees them.
func TestEditMaskRefusals(t *testing.T) {
	input := contractMask(t, 8, 6, func(int, int) bool { return false })
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	for name, mask := range map[string]formPart{
		"other size":      {name: "mask", file: "mask.png", media: "image/png", data: contractMask(t, 6, 8, func(int, int) bool { return true })},
		"jpeg":            {name: "mask", file: "mask.jpg", media: "image/jpeg", data: jpeg},
		"header only":     {name: "mask", file: "mask.png", media: "image/png", data: pngHeader(8, 6)},
		"changes nothing": {name: "mask", file: "mask.png", media: "image/png", data: contractMask(t, 8, 6, func(int, int) bool { return false })},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := forwardedMask(t, formPart{name: "image", file: "image.png", media: "image/png", data: input}, mask)
			if !errors.Is(err, ErrImageRoute) || !errors.Is(err, sdcpp.ErrRequestForm) {
				t.Fatalf("admitted: %v", err)
			}
		})
	}
}
