package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	identity "github.com/openabstractions/abstraction-identity"
)

// ErrWeightFormat reports a model input other than a safetensors or GGUF
// file or a pinned speech model file. sd.cpp also parses PyTorch pickles
// (.ckpt, .pt, .pth), whose loader has had four overflow CVEs
// (stable-diffusion.cpp review, "Model deserialisation"), so those and every
// other format are refused.
var ErrWeightFormat = errors.New("lemonade: weights must be safetensors or GGUF")

const registryLimit = 1 << 20

// WeightFileAllowed reports whether name has a safetensors or GGUF extension.
func WeightFileAllowed(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".safetensors", ".gguf":
		return true
	}
	return false
}

// checkModels walks the private models folder. Weight files must be
// safetensors or GGUF, or one of the pinned speech model files at its exact
// path (whisper.cpp review condition 6, Kokoros condition 2): the legacy
// ggml .bin, the ONNX model, the NumPy voices and index.json are admitted by
// path, never by extension, and their bytes are proven when the launch holds
// the model's tree. The only other files admitted are the Hugging Face
// cache's refs entries and JSON metadata Lemonade keeps beside a snapshot
// (.lemonade_registry.json); neither is loaded as weights. An index.json
// anywhere else is refused, since Lemonade's Kokoros loader follows the
// paths one names. Links and other non-regular entries are refused, since a
// link can name a file elsewhere.
func checkModels(modelsDir string, pinned ...SpeechModel) error {
	if _, err := os.Lstat(modelsDir); os.IsNotExist(err) {
		return nil
	}
	// A pinned file is admitted where it lies, matched by file identity
	// (identity.FileKey): only the file at the pinned path, in whatever
	// spelling the walk reaches it.
	admitted := map[string]bool{}
	for _, m := range pinned {
		for _, p := range m.Paths() {
			if key, err := identity.FileKey(filepath.Join(modelsDir, filepath.FromSlash(p))); err == nil {
				admitted[key] = true
			}
		}
	}
	return filepath.WalkDir(modelsDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %v", ErrWeightFormat, err)
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0 || entry.Type()&fs.ModeIrregular != 0:
			return fmt.Errorf("%w: link or special file %s", ErrWeightFormat, entry.Name())
		case entry.IsDir():
			return nil
		case !entry.Type().IsRegular():
			return fmt.Errorf("%w: special file %s", ErrWeightFormat, entry.Name())
		case admittedFile(admitted, path):
			return nil
		case WeightFileAllowed(entry.Name()):
			return nil
		case identity.SamePathName(entry.Name(), "index.json"):
			return fmt.Errorf("%w: %s outside the pinned Kokoros model", ErrWeightFormat, entry.Name())
		case filepath.Base(filepath.Dir(path)) == "refs" || strings.EqualFold(filepath.Ext(entry.Name()), ".json"):
			return nil
		}
		return fmt.Errorf("%w: %s", ErrWeightFormat, entry.Name())
	})
}

// registryKeys are the model registry fields the provider admits. Any other
// key could carry backend arguments; recipe_options is admitted only as a
// declared model's reviewed engine flags (checkRecipeOptions).
var registryKeys = map[string]bool{
	"source": true, "recipe": true, "size": true, "checkpoint": true, "checkpoints": true, "image_defaults": true, "labels": true,
}

// checkRegistry reads Lemonade's user_models.json in the private config
// folder. Every model must use the sd.cpp or llama.cpp recipe and name each
// checkpoint as `<repo>:<file>` with a safetensors or GGUF file.
func checkRegistry(configDir string) error {
	path := filepath.Join(configDir, "user_models.json")
	data, err := readSmall(path, registryLimit)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: registry: %v", ErrWeightFormat, err)
	}
	var models map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &models); err != nil {
		return fmt.Errorf("%w: registry: %v", ErrWeightFormat, err)
	}
	for name, fields := range models {
		for key, value := range fields {
			if key == "recipe_options" {
				if err := checkRecipeOptions(name, value); err != nil {
					return err
				}
				continue
			}
			if !registryKeys[key] {
				return fmt.Errorf("%w: model %s field %s", ErrWeightFormat, name, key)
			}
		}
		var recipe string
		if err := json.Unmarshal(fields["recipe"], &recipe); err != nil || recipe != "sd-cpp" && recipe != "llamacpp" {
			return fmt.Errorf("%w: model %s recipe", ErrWeightFormat, name)
		}
		var checkpoints []string
		if raw, ok := fields["checkpoint"]; ok {
			var one string
			if err := json.Unmarshal(raw, &one); err != nil {
				return fmt.Errorf("%w: model %s checkpoint", ErrWeightFormat, name)
			}
			checkpoints = append(checkpoints, one)
		}
		if raw, ok := fields["checkpoints"]; ok {
			var many map[string]string
			if err := json.Unmarshal(raw, &many); err != nil {
				return fmt.Errorf("%w: model %s checkpoints", ErrWeightFormat, name)
			}
			for _, one := range many {
				checkpoints = append(checkpoints, one)
			}
		}
		if len(checkpoints) == 0 {
			return fmt.Errorf("%w: model %s names no checkpoint", ErrWeightFormat, name)
		}
		for _, checkpoint := range checkpoints {
			_, file, ok := strings.Cut(checkpoint, ":")
			if !ok || !WeightFileAllowed(file) {
				return fmt.Errorf("%w: model %s checkpoint %s", ErrWeightFormat, name, checkpoint)
			}
		}
	}
	return nil
}

// admittedFile reports whether path is one of the admitted pinned files, by
// file identity.
func admittedFile(admitted map[string]bool, path string) bool {
	key, err := identity.FileKey(path)
	return err == nil && admitted[key]
}
