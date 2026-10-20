package server

import (
	"github.com/openabstractions/abstraction-inference/go/imagemask"
)

// engineMask converts an edit mask in the image contract's form (inference
// CONTRACT.md INF-I1: a PNG whose fully transparent pixels mark the area to
// change) to the luminance form sd-server reads, and returns it with its
// size. The conversion is the provider kit's (imagemask.Luminance), the one
// inference applies to Stability's route; this route keeps it as a second
// line behind inference, which sends the alpha form that also reads white on
// the area to change.
//
// Lemonade 2026.39.1 forwards the mask part's bytes unchanged
// (server.cpp handle_image_edits, "Extract optional mask file", and
// sdcpp_server.cpp image_edits, the "mask" multipart field). sd-server
// master-843-462d675 loads it with one channel
// (stable-diffusion.cpp/examples/server/routes_openai.cpp, load_image_from_memory(..., 1) for
// mask_bytes), which stb reduces to luminance and drops alpha
// (stable-diffusion.cpp/examples/common/media_io.cpp, stbi_load_from_memory with expected_channel),
// and src/stable-diffusion.cpp generate_image keeps the init image where the
// mask is 0 and changes it where the mask is 1 ((1 - mask) * init, and a
// missing mask is all 1).
//
// The caller has bounded the header with sdcpp.CheckImageInput, so decoding
// allocates at most that bound's pixels.
func engineMask(data []byte) ([]byte, int, int, error) {
	if _, _, changed, err := imagemask.Inspect(data); err != nil || changed == 0 {
		return nil, 0, 0, formRefusal("the mask does not decode as a PNG or marks nothing to change")
	}
	converted, width, height, err := imagemask.Convert(imagemask.Luminance, data)
	if err != nil {
		return nil, 0, 0, formRefusal("the mask does not decode as a PNG")
	}
	return converted, width, height, nil
}
