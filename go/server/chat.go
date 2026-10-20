package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/openabstractions/abstraction-inference/go/providerhost/llamacpp"
)

// chatRoute is the OpenAI-compatible chat call Lemonade 2026.39.1 serves
// through llama-server.
const chatRoute = "/api/v1/chat/completions"

// chatBodyLimit bounds a chat request the provider reads and checks.
// llama-server fetches at most 10 MB per remote medium; base64 data costs a
// third more.
const chatBodyLimit = 32 << 20

// ErrChatRoute reports a chat request the provider refuses. Every refusal is
// a *ChatRefusal naming the unmet condition.
var ErrChatRoute = errors.New("lemonade: chat route refused")

// A ChatRefusal names the llama.cpp b10825 review condition a chat request or
// the running launch does not meet: engine-files (condition 1),
// load-options (2), environment (3), media (6), llama-flags (2's options),
// or model and request for an undeclared model or a malformed body.
type ChatRefusal struct {
	Condition string
	Detail    string
}

func (e *ChatRefusal) Error() string {
	return "lemonade: chat route refused: " + e.Condition + ": " + e.Detail
}

func (e *ChatRefusal) Is(target error) bool { return target == ErrChatRoute }

func chatRefusal(condition, format string, args ...any) error {
	return &ChatRefusal{Condition: condition, Detail: fmt.Sprintf(format, args...)}
}

// chatReady reports the first condition of the running launch that keeps the
// chat route closed. The llama.cpp folder must be the reviewed b10825 one,
// proven and held for this launch (condition 1; Lemonade starts llama-server
// from it and sets it as the working directory on Windows). The locked
// configuration must carry no backend argument and no forbidden option
// (condition 2), and the environment this launch gave Lemonade, which
// llama-server inherits, must pass the allow-list and the forbidden names
// (condition 3). The caller holds s.mu.
func (s *Server) chatReadyLocked() error {
	if !sameManifest(s.launched.LlamaBackend, s.cfg.chatBackend) {
		return chatRefusal("engine-files", "the llama.cpp manifest is %s, not the reviewed %s", s.launched.LlamaBackend.Version, s.cfg.chatBackend.Version)
	}
	if !s.llamaHeld {
		return chatRefusal("engine-files", "no proven llama.cpp %s folder is held for this launch; %s", s.cfg.chatBackend.Version, s.cfg.chatBackend.replaceCommand())
	}
	if err := checkBackendArgs(lockedConfig(s.cfg.ModelsDir)); err != nil {
		if errors.Is(err, ErrForbiddenLlamaFlag) {
			return chatRefusal("llama-flags", "%v", err)
		}
		return chatRefusal("load-options", "%v", err)
	}
	if err := validateEnvironment(s.launchEnv); err != nil {
		return chatRefusal("environment", "%v", err)
	}
	return nil
}

// loadOptionKeys are request fields Lemonade or its Ollama layer turns into
// load options: Server::extract_auto_load_options takes ctx_size from a chat
// body, /load and the Ollama routes take every RecipeOptions key
// (recipe_options.cpp and the llamacpp descriptor) and save_options writes
// them to recipe_options.json. Any `*_args`, `*_bin`, `*_backend` or
// `*_device` field is refused with them.
var loadOptionKeys = map[string]bool{
	"ctx_size": true, "llamacpp_backend": true, "llamacpp_device": true, "llamacpp_args": true, "merge_args": true,
	"auto_evict": true, "evict_idle_timeout": true, "downsize_idle_timeout": true, "evict_weight_factor": true,
	"pinned": true, "auto_update": true, "save_options": true, "recipe_options": true, "options": true, "keep_alive": true,
}

func loadOption(key string) bool {
	if loadOptionKeys[key] {
		return true
	}
	for _, suffix := range []string{"_args", "_bin", "_backend", "_device"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// checkChatBody checks one chat request and returns the bytes to forward: the
// request as the provider decoded it, encoded again, so Lemonade parses
// exactly what was checked (a duplicate key keeps only the value checked) and
// numbers keep their digits. It refuses load options (condition 2), an
// undeclared model, and media other than base64 PNG, JPEG or audio data
// (condition 6): llama-server fetches any URL starting with "http" with no
// host restriction, reads file:// paths under --media-path, and decodes video,
// WebP and any image its stb decoder rejects by spawning FFmpeg.
func checkChatBody(body []byte, allowsModel func(string) bool) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var request map[string]any
	if err := dec.Decode(&request); err != nil || request == nil {
		return nil, chatRefusal("request", "the body is no JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, chatRefusal("request", "the body holds more than one JSON value")
	}
	for key := range request {
		if loadOption(key) {
			return nil, chatRefusal("load-options", "the request names the load option %s", key)
		}
	}
	model, _ := request["model"].(string)
	if model == "" || !allowsModel(model) {
		return nil, chatRefusal("model", "the request names no declared model")
	}
	messages, ok := request["messages"].([]any)
	if !ok {
		return nil, chatRefusal("request", "messages is not a list")
	}
	for _, m := range messages {
		message, ok := m.(map[string]any)
		if !ok {
			return nil, chatRefusal("request", "a message is not an object")
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range content {
			if err := checkChatPart(p); err != nil {
				return nil, err
			}
		}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(request); err != nil {
		return nil, chatRefusal("request", "%v", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// chatMedia is the provider kit's chat media policy (providerhost/llamacpp,
// inference CONTRACT.md INF-LC3) with audio admitted: text, image_url and
// input_audio parts, the types llama-server b10825 reads besides
// input_video (server-common.cpp, oaicompat_chat_params_parse). An image is
// replaced by the provider's own PNG and audio by its own base64; an audio
// part's data and url are both read and both checked.
var chatMedia = llamacpp.Media{Audio: true}

// checkChatPart applies chatMedia to one content part, naming a refusal as
// review condition 6.
func checkChatPart(p any) error {
	err := chatMedia.Part(p)
	var media *llamacpp.MediaError
	if errors.As(err, &media) {
		return chatRefusal("media", "%s", media.Detail)
	}
	return err
}
