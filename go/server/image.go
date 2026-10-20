package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"unicode/utf8"

	inference "github.com/openabstractions/abstraction-inference/go"
	"github.com/openabstractions/abstraction-inference/go/providerhost/sdcpp"
	router "github.com/openabstractions/abstraction-router/go"
)

// ErrImageRoute reports an image request the provider refuses before Lemonade
// sees it. Each refusal also wraps sdcpp.ErrEngineDirective,
// sdcpp.ErrImageSize or sdcpp.ErrRequestForm, which inference turns into the
// reasons prompt:engine-directive, size:engine-bound and request:engine-form.
var ErrImageRoute = errors.New("lemonade: image route refused")

// formRefusal is a refusal for the request's form (sdcpp.ErrRequestForm).
func formRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %w: %s", ErrImageRoute, sdcpp.ErrRequestForm, fmt.Sprintf(format, args...))
}

// policyRefusal wraps a refusal of the sdcpp package.
func policyRefusal(err error) error {
	return fmt.Errorf("%w: %w", ErrImageRoute, err)
}

// imageTextFields are the fields inference's OpenAI-compatible adapter sends
// in a generation body and as text parts of an edit form (runOpenAIImage);
// imageFileParts are the edit form's file parts. No other field or part is
// forwarded: an allow-list of plain names leaves no room for a part name that
// Go's form parser and Lemonade's cpp-httplib parser read differently.
// seed is the field ImageFields writes for lemonade/seed: a JSON integer in a
// generation and a decimal text field in an edit form, 0..2147483647 in both.
var (
	imageTextFields = map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "response_format": true, "seed": true}
	imageFileParts  = map[string]bool{"image": true, "mask": true}
)

// imageInputTypes are the image types inference sends to an edit, each as
// http.DetectContentType names it.
var imageInputTypes = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}

// imageBodyLimit bounds an image request the provider reads and checks: an
// edit carries one input image and one mask of at most MaxImageBytes each,
// the bound inference applies to them, and a few text fields.
func (s *Server) imageBodyLimit() int64 {
	each := s.cfg.MaxImageBytes
	if each <= 0 {
		each = inference.DefaultMaxImageBytes
	}
	return 2*each + 1<<20
}

// readImageBody reads, bounds and checks one image request (inference
// CONTRACT.md INF-SD1..INF-SD3; stable-diffusion.cpp review addendum,
// findings for the base review) and returns the bytes and content type to
// forward. Lemonade passes the prompt to sd-server, whose OpenAI and sdapi
// image routes read generation parameters, LoRA paths included, from an
// `<sd_cpp_extra_args>` block in it, and decode input images against the
// request's size. The provider admits only the fields inference sends,
// refuses that directive in every text value, a size beyond the reviewed
// bound and an input image whose header names one, and forwards its own
// encoding of what it checked, so Lemonade parses exactly the checked fields.
// The model must serve image in the provider's profile table
// (ProfileModels), the table the router host and admission read: Lemonade
// loads the model an image request names, and a speech model named there
// would start whisper-server or koko outside the audio routes' checks and
// idle stop.
func (s *Server) readImageBody(req *http.Request) ([]byte, string, error) {
	imageModel := func(model string) bool { return s.ServesModel(router.ProfileImage, model) }
	if req.Body == nil {
		return nil, "", formRefusal("the request has no body")
	}
	defer req.Body.Close()
	limit := s.imageBodyLimit()
	body, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", formRefusal("the body exceeds %d bytes", limit)
	}
	media, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err == nil && media == "multipart/form-data" {
		return checkImageForm(body, params["boundary"], imageModel)
	}
	checked, err := checkImageJSON(body, imageModel)
	return checked, "application/json", err
}

// checkImageText checks one text field's value.
func checkImageText(name, value string) error {
	if err := sdcpp.CheckText(name, value); err != nil {
		return policyRefusal(err)
	}
	if name == "size" {
		if err := sdcpp.CheckOpenAISize(value); err != nil {
			return policyRefusal(err)
		}
	}
	if name == "seed" {
		if _, ok := seedValue(value); !ok {
			return formRefusal("seed is not a decimal 0..2147483647")
		}
	}
	return nil
}

// imageModelRefusal refuses a body whose model is absent or no declared image
// model. The refusal never repeats the name.
func imageModelRefusal(model string, allows func(string) bool) error {
	if model == "" || !allows(model) {
		return formRefusal("the model is not a declared image model")
	}
	return nil
}

// checkImageJSON checks a generation body: one JSON object holding only
// imageTextFields, n an integer, seed an integer 0..2147483647, every
// other field a string that passes checkImageText, and a model allows
// accepts. It returns the object encoded again.
func checkImageJSON(body []byte, allows func(string) bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var request map[string]any
	if err := dec.Decode(&request); err != nil || request == nil {
		return nil, formRefusal("the body is no JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, formRefusal("the body holds more than one JSON value")
	}
	for key, value := range request {
		if !imageTextFields[key] {
			return nil, formRefusal("field %q is not forwarded", key)
		}
		if key == "n" {
			number, ok := value.(json.Number)
			if _, err := number.Int64(); !ok || err != nil {
				return nil, formRefusal("n is not an integer")
			}
			continue
		}
		if key == "seed" {
			number, ok := value.(json.Number)
			if _, bounded := seedValue(number.String()); !ok || !bounded {
				return nil, formRefusal("seed is not an integer 0..2147483647")
			}
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, formRefusal("field %q is not a string", key)
		}
		if err := checkImageText(key, text); err != nil {
			return nil, err
		}
	}
	model, _ := request["model"].(string)
	if err := imageModelRefusal(model, allows); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(request); err != nil {
		return nil, formRefusal("%v", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// checkImageForm checks an edit body: multipart form data holding text
// fields from imageTextFields and the file parts image and mask, each named
// once and carrying no transfer encoding. Text must be UTF-8 that passes
// checkImageText, and each file a PNG, JPEG or WebP whose bytes are that type
// and whose header names a size within the bound. A mask is a PNG of the
// input image's size, forwarded in sd-server's form (engineMask, inference
// CONTRACT.md INF-I1). The model field must be one allows accepts. It
// returns the parts written again under a new boundary.
func checkImageForm(body []byte, boundary string, allows func(string) bool) ([]byte, string, error) {
	if boundary == "" {
		return nil, "", formRefusal("the form has no boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	seen := map[string]bool{}
	sizes := map[string][2]int{}
	model := ""
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", formRefusal("the form does not parse")
		}
		name, file := part.FormName(), part.FileName()
		if part.Header.Get("Content-Transfer-Encoding") != "" {
			return nil, "", formRefusal("a part carries a transfer encoding")
		}
		if seen[name] {
			return nil, "", formRefusal("a part is named twice")
		}
		seen[name] = true
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, "", formRefusal("the form does not parse")
		}
		switch {
		case imageFileParts[name] && file != "":
			media := part.Header.Get("Content-Type")
			ext, known := imageInputTypes[media]
			if !known || http.DetectContentType(data) != media {
				return nil, "", formRefusal("file part %s is not a PNG, JPEG or WebP image", name)
			}
			if err := sdcpp.CheckImageInput(media, data); err != nil {
				return nil, "", policyRefusal(err)
			}
			width, height, err := sdcpp.InputSize(media, data)
			if err != nil {
				return nil, "", policyRefusal(err)
			}
			if name == "mask" {
				if media != "image/png" {
					return nil, "", formRefusal("the mask is no PNG")
				}
				if data, width, height, err = engineMask(data); err != nil {
					return nil, "", err
				}
			}
			sizes[name] = [2]int{width, height}
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, name, name+ext))
			header.Set("Content-Type", media)
			w, err := writer.CreatePart(header)
			if err == nil {
				_, err = w.Write(data)
			}
			if err != nil {
				return nil, "", formRefusal("%v", err)
			}
		case imageTextFields[name] && file == "":
			if !utf8.Valid(data) {
				return nil, "", formRefusal("field %s is not UTF-8", name)
			}
			if err := checkImageText(name, string(data)); err != nil {
				return nil, "", err
			}
			if name == "model" {
				model = string(data)
			}
			if err := writer.WriteField(name, string(data)); err != nil {
				return nil, "", formRefusal("%v", err)
			}
		default:
			// The refusal never repeats the part's name, which may carry the
			// directive itself.
			return nil, "", formRefusal("a part outside the forwarded fields")
		}
	}
	if mask, ok := sizes["mask"]; ok {
		if input, ok := sizes["image"]; !ok || input != mask {
			return nil, "", formRefusal("the mask's size differs from the input image's")
		}
	}
	if err := imageModelRefusal(model, allows); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", formRefusal("%v", err)
	}
	return out.Bytes(), writer.FormDataContentType(), nil
}
