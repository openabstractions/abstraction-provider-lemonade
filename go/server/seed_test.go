package server

import (
	"bytes"
	"errors"
	media "github.com/openabstractions/abstraction-inference/go/mediationcontent"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

// anyModel admits every model; these tests check fields other than model.
func anyModel(string) bool { return true }

// lemonade/seed carries a non-negative 32-bit decimal seed, which Lemonade
// reads as the integer seed field of a generation or an edit
// (sdcpp_server.cpp build_extra_args; server.cpp parse_int_field). Every other
// spelling is refused before Lemonade sees it.
func TestSeedExtensionBecomesTheEngineSeed(t *testing.T) {
	if len(ImageExtensions) != 1 || ImageExtensions[0] != SeedExtension || SeedExtension != "lemonade/seed" {
		t.Fatalf("declared image extensions %v", ImageExtensions)
	}
	for value, want := range map[string]int64{"0": 0, "7": 7, "20261001": 20261001, "2147483647": 2147483647} {
		fields, refused := ImageFields(map[string]string{SeedExtension: value})
		if refused != "" || len(fields) != 1 || fields["seed"] != want {
			t.Fatalf("%q: fields %v refused %q", value, fields, refused)
		}
	}
	for _, value := range []string{"", "-1", "2147483648", "99999999999999999999", "+7", " 7", "7 ", "07", "0x10", "1e3", "7.0", "٣"} {
		if fields, refused := ImageFields(map[string]string{SeedExtension: value}); refused != SeedExtension || fields != nil {
			t.Fatalf("%q accepted: %v", value, fields)
		}
	}
	if fields, refused := ImageFields(map[string]string{"lemonade/steps": "50"}); refused != "lemonade/steps" || fields != nil {
		t.Fatalf("undeclared key accepted: %v %q", fields, refused)
	}
}

// The route's allow-list admits seed only as lemonade/seed's bound: a JSON
// integer or a form decimal 0..2147483647. Every other spelling is refused as
// a request form Lemonade must not parse.
func TestImageRouteRefusesASeedOutsideTheBound(t *testing.T) {
	for _, seed := range []string{`"7"`, `-1`, `2147483648`, `7.0`, `1e3`, `07`, `null`, `[7]`, `{"seed":7}`, `true`} {
		_, err := checkImageJSON([]byte(`{"model":"user.Z-Image-Turbo","prompt":"a lighthouse","n":1,"size":"512x512","seed":`+seed+`}`), anyModel)
		if !errors.Is(err, ErrImageRoute) || !errors.Is(err, media.ErrRequestForm) {
			t.Fatalf("generation seed %s: %v", seed, err)
		}
	}
	for _, seed := range []string{"", "seven", "-1", "+7", "07", "2147483648", "7 ", "1e3"} {
		contentType, body := editForm(t, "snow", "1024x1024", formPart{name: "seed", data: []byte(seed)})
		_, params, _ := mime.ParseMediaType(contentType)
		if _, _, err := checkImageForm(body, params["boundary"], anyModel); !errors.Is(err, media.ErrRequestForm) {
			t.Fatalf("edit seed %q: %v", seed, err)
		}
	}
	contentType, body := editForm(t, "snow", "1024x1024", formPart{name: "seed", file: "seed.png", media: "image/png", data: pngInput})
	_, params, _ := mime.ParseMediaType(contentType)
	if _, _, err := checkImageForm(body, params["boundary"], anyModel); !errors.Is(err, media.ErrRequestForm) {
		t.Fatalf("a seed file part was accepted: %v", err)
	}
}

// The image route forwards the seed field inference writes for lemonade/seed,
// in a generation body and in an edit form. A field allow-list on this route
// must keep admitting it.
func TestImageRouteForwardsTheSeedField(t *testing.T) {
	checked, err := checkImageJSON([]byte(`{"model":"user.Z-Image-Turbo","prompt":"a lighthouse","n":1,"size":"1024x1024","response_format":"b64_json","seed":20261001}`), anyModel)
	if err != nil || !strings.Contains(string(checked), `"seed":20261001`) {
		t.Fatalf("generation %s, %v", checked, err)
	}
	contentType, body := editForm(t, "snow", "1024x1024", formPart{name: "seed", data: []byte("7")})
	_, params, _ := mime.ParseMediaType(contentType)
	out, outType, err := checkImageForm(body, params["boundary"], anyModel)
	if err != nil {
		t.Fatal(err)
	}
	_, outParams, _ := mime.ParseMediaType(outType)
	reader := multipart.NewReader(bytes.NewReader(out), outParams["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			t.Fatal("the edit form lost the seed field")
		}
		if err != nil {
			t.Fatal(err)
		}
		if part.FormName() == "seed" {
			if data, _ := io.ReadAll(part); string(data) != "7" {
				t.Fatalf("seed %q", data)
			}
			return
		}
	}
}
