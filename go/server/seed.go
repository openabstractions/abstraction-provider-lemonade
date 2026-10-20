package server

// SeedExtension is the image@1 vendor extension that fixes a generation's
// seed on the Lemonade path, so a take can be repeated. Its value is a
// decimal 0..2147483647 without sign or leading zero; Lemonade reads it as
// the integer seed of a generation (sdcpp_server.cpp build_extra_args) or an
// edit (server.cpp parse_int_field) and passes it to sd-server.
const SeedExtension = "lemonade/seed"

// ImageExtensions are the image extensions the provider declares and
// ImageFields carries.
var ImageExtensions = []string{SeedExtension}

// ImageFields maps declared image extensions to fields of the request
// inference sends to Lemonade (inference Config.ImageFields): lemonade/seed
// becomes the integer field seed. It refuses any other key and a seed outside
// the bound, naming the key.
func ImageFields(extensions map[string]string) (map[string]any, string) {
	fields := map[string]any{}
	for key, value := range extensions {
		if key != SeedExtension {
			return nil, key
		}
		seed, ok := seedValue(value)
		if !ok {
			return nil, key
		}
		fields["seed"] = seed
	}
	return fields, ""
}

// seedValue parses 1..10 ASCII digits without a leading zero, at most
// 2147483647: sd-server's seed is a 32-bit integer and Lemonade treats a
// negative one as a request for a random seed.
func seedValue(value string) (int64, bool) {
	if value == "" || len(value) > 10 || len(value) > 1 && value[0] == '0' {
		return 0, false
	}
	var n int64
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n <= 1<<31-1
}
