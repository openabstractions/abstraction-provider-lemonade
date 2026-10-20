package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"strings"

	registration "github.com/openabstractions/abstraction-facade/go/abstraction/facade/registration"
	"github.com/openabstractions/abstraction-provider-kit/go/sdcpp"
)

// DeclareEngineFacts selects exact entries from the reviewed recipe table.
// Unknown models retain unknown engine/object associations.
func DeclareEngineFacts(facet *registration.ProviderFacet) {
	declareImageFacts(facet)
	// The reviewed speech models are installed by hash too: each file of the
	// pinned revision is one recipe object, named by its file stem.
	for _, speech := range []struct {
		modality string
		model    SpeechModel
		engine   registration.EngineFact
	}{
		{"transcription", WhisperLargeV3TurboModel, registration.EngineFact{ID: "whispercpp", Name: "whisper.cpp"}},
		{"speech", KokoroV1Model, registration.EngineFact{ID: "kokoros", Name: "Kokoros"}},
	} {
		if !slices.Contains(facet.Models, registration.ScopedModel{Modality: speech.modality, Model: speech.model.Name}) {
			continue
		}
		recipe := registration.ModelRecipe{Model: speech.model.Name, Engine: speech.engine.ID, Modalities: []string{speech.modality}}
		for _, file := range speech.model.Files {
			recipe.Objects = append(recipe.Objects, registration.RecipeObject{Role: speechRole(file.Name), Digest: "sha256:" + file.SHA256})
		}
		facet.Engines = append(facet.Engines, speech.engine)
		facet.Recipes = append(facet.Recipes, recipe)
	}
}

// speechRole names a speech file's recipe role: the model's weights are
// main, every other file its stem (index, voices).
func speechRole(name string) string {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	switch {
	case strings.HasPrefix(stem, "ggml-") || strings.HasPrefix(stem, "kokoro-"):
		return "main"
	case strings.HasPrefix(stem, "voices"):
		return "voices"
	}
	return stem
}

func declareImageFacts(facet *registration.ProviderFacet) {
	for _, scoped := range facet.Models {
		if scoped.Modality != "image" {
			continue
		}
		for _, model := range ImageModels {
			if model.ID != scoped.Model {
				continue
			}
			recipe := registration.ModelRecipe{Model: model.ID, Engine: "sdcpp", Modalities: []string{"image"}}
			for _, file := range model.Files {
				recipe.Objects = append(recipe.Objects, registration.RecipeObject{Role: file.Role, Digest: "sha256:" + file.SHA256})
			}
			if len(recipe.Objects) == 0 {
				continue
			}
			if len(facet.Recipes) == 0 {
				facet.Engines = append(facet.Engines, registration.EngineFact{ID: "sdcpp", Name: "Lemonade sd.cpp"})
			}
			facet.Recipes = append(facet.Recipes, recipe)
		}
	}
}

// ErrImageModel reports a picture model declaration that is incomplete or
// outside the reviewed bounds. A weight file that is no safetensors or GGUF
// file also wraps ErrWeightFormat.
var ErrImageModel = errors.New("lemonade: invalid picture model declaration")

// ModelFile is one weight file of a picture model, pinned to an exact Hugging
// Face repository revision by the hub's LFS oid (its SHA-256) and size.
// Role is the Lemonade checkpoint role: main, text_encoder or vae.
type ModelFile struct {
	Role       string
	Repository string
	Revision   string
	Path       string
	Bytes      int64
	SHA256     string
}

// CacheFolder is the file's repository folder under Lemonade's models
// folder, in the Hugging Face cache layout Lemonade reads.
func (f ModelFile) CacheFolder() string {
	return "models--" + strings.ReplaceAll(f.Repository, "/", "--")
}

// CachePath is the file's path under Lemonade's models folder.
func (f ModelFile) CachePath() string {
	return filepath.Join(f.CacheFolder(), "snapshots", f.Revision, filepath.FromSlash(f.Path))
}

// ImagePreset is a model's generation defaults, written as the image_defaults
// of its Lemonade registry entry. Lemonade applies each value a request leaves
// out (sdcpp_server.cpp build_extra_args and resolve_size). Sampler and
// FlowShift are omitted from the entry when empty or zero.
//
// EngineFlags are sd-server options for this model alone, written as the
// entry's recipe_options.sdcpp_args, which Lemonade appends to the sd-server
// command line when it loads the model (sdcpp_server.cpp load). Only flags in
// reviewedEngineFlags are admitted, each once, separated by single spaces.
type ImagePreset struct {
	Steps         int
	CFGScale      float64
	Width, Height int
	Sampler       string
	FlowShift     float64
	EngineFlags   string
}

// reviewedEngineFlags are the per-model sd-server options the coordinator
// reviewed on 2026-10-01 (Lemonade's catalogue sets both for Qwen-Image):
// --diffusion-fa selects flash attention in the diffusion model, which
// Lemonade already passes on Vulkan, and --offload-to-cpu keeps weights in
// system memory until use. Both take no value and change only the attention
// kernel and memory placement; neither names a path, address or port, and
// neither is among the listener and model options the stable-diffusion.cpp
// and Lemonade reviews constrain.
var reviewedEngineFlags = map[string]bool{"--diffusion-fa": true, "--offload-to-cpu": true}

func validEngineFlags(flags string) bool {
	if flags == "" {
		return true
	}
	words := strings.Split(flags, " ")
	seen := map[string]bool{}
	for _, word := range words {
		if !reviewedEngineFlags[word] || seen[word] {
			return false
		}
		seen[word] = true
	}
	return true
}

// samplers are stable-diffusion.cpp's sample methods.
var samplers = map[string]bool{
	"euler": true, "euler_a": true, "heun": true, "dpm2": true, "dpm++2s_a": true, "dpm++2m": true, "dpm++2mv2": true,
	"ipndm": true, "ipndm_v": true, "lcm": true, "ddim_trailing": true, "tcd": true,
}

func (p ImagePreset) valid() bool {
	return p.Steps >= 1 && p.Steps <= 150 && p.CFGScale >= 0 && p.CFGScale <= 30 && sdcpp.CheckSize(p.Width, p.Height) == nil &&
		(p.Sampler == "" || samplers[p.Sampler]) && p.FlowShift >= 0 && p.FlowShift <= 20 && validEngineFlags(p.EngineFlags)
}

// ModelLicense is what Setup shows before a model download: the licence's
// SPDX name, its text's address and one sentence on its terms.
type ModelLicense struct {
	Name string
	URL  string
	Text string
}

// ImageModel is one picture model the provider declares for Lemonade's
// sd.cpp recipe: the OA model ID apps name, its pinned weight files, its
// preset, its licence and the model card Setup links. Edit is true for a
// model whose card offers reference-image editing.
type ImageModel struct {
	ID      string
	Title   string
	Source  string
	Files   []ModelFile
	Preset  ImagePreset
	License ModelLicense
	Edit    bool
}

const apacheLicenseURL = "https://www.apache.org/licenses/LICENSE-2.0"

// ImageModels is every picture model this provider build declares. The first
// entry is the starter model Setup stages today. Pins were read on 2026-10-01
// from the Hugging Face API (each file's LFS oid and size at the named
// revision); the files and presets are those of the same model names in
// Lemonade 2026.39.1's resources/server_models.json, except that Klein's main
// weights come from the Comfy-Org repository. Lemonade's per-model sdcpp_args
// for Qwen-Image-2512 become that preset's reviewed EngineFlags. The registry
// key of each model is its ID without the "user." prefix.
var ImageModels = []ImageModel{
	{
		ID: "user.LocalApprovedKlein", Title: "FLUX.2 [klein] 4B", Source: "https://huggingface.co/black-forest-labs/FLUX.2-klein-4B",
		Files: []ModelFile{
			{Role: "main", Repository: "Comfy-Org/vae-text-encorder-for-flux-klein-4b", Revision: "5f526678002e43af5551dadb73ce2e8c91b43afe", Path: "split_files/diffusion_models/flux-2-klein-4b.safetensors", Bytes: 7751105712, SHA256: "ec3d4e733a771f61c052fb4856c48b336c55eaf2c65487c2a1faeb9bbda7a343"},
			{Role: "text_encoder", Repository: "Comfy-Org/vae-text-encorder-for-flux-klein-4b", Revision: "5f526678002e43af5551dadb73ce2e8c91b43afe", Path: "split_files/text_encoders/qwen_3_4b.safetensors", Bytes: 8044982048, SHA256: "6c671498573ac2f7a5501502ccce8d2b08ea6ca2f661c458e708f36b36edfc5a"},
			{Role: "vae", Repository: "Comfy-Org/vae-text-encorder-for-flux-klein-4b", Revision: "5f526678002e43af5551dadb73ce2e8c91b43afe", Path: "split_files/vae/flux2-vae.safetensors", Bytes: 336211292, SHA256: "868fe7b343cc8f3a19dbcfcafbc3d5f888802be3f89bd81b65b3621a066ce8f3"},
		},
		Preset: ImagePreset{Steps: 4, CFGScale: 1, Width: 512, Height: 512},
		License: ModelLicense{Name: "Apache-2.0", URL: apacheLicenseURL,
			Text: "Black Forest Labs publishes FLUX.2 [klein] 4B and the FLUX.2 autoencoder, and Qwen the Qwen3-4B text encoder, each under the Apache License 2.0; Comfy-Org repackages the three files. The licence permits commercial use, changes and redistribution with the licence and notices kept."},
		Edit: true,
	},
	{
		ID: "user.Z-Image-Turbo", Title: "Z-Image-Turbo", Source: "https://huggingface.co/Tongyi-MAI/Z-Image-Turbo",
		Files: []ModelFile{
			{Role: "main", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/diffusion_models/z_image_turbo_bf16.safetensors", Bytes: 12309866400, SHA256: "2407613050b809ffdff18a4ac99af83ea6b95443ecebdf80e064a79c825574a6"},
			{Role: "text_encoder", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/text_encoders/qwen_3_4b.safetensors", Bytes: 8044982048, SHA256: "6c671498573ac2f7a5501502ccce8d2b08ea6ca2f661c458e708f36b36edfc5a"},
			{Role: "vae", Repository: "Comfy-Org/z_image_turbo", Revision: "6fc90a3b1b653e935a0d175e260736de25b84df5", Path: "split_files/vae/ae.safetensors", Bytes: 335304388, SHA256: "afc8e28272cd15db3919bacdb6918ce9c1ed22e96cb12c4d5ed0fba823529e38"},
		},
		Preset: ImagePreset{Steps: 9, CFGScale: 1, Width: 1024, Height: 1024},
		License: ModelLicense{Name: "Apache-2.0", URL: apacheLicenseURL,
			Text: "Tongyi-MAI publishes Z-Image-Turbo, Black Forest Labs the FLUX.1 autoencoder and Qwen the Qwen3-4B text encoder, each under the Apache License 2.0; Comfy-Org repackages the three files. The licence permits commercial use, changes and redistribution with the licence and notices kept."},
	},
	{
		ID: "user.Qwen-Image-2512", Title: "Qwen-Image-2512", Source: "https://huggingface.co/Qwen/Qwen-Image-2512",
		Files: []ModelFile{
			{Role: "main", Repository: "unsloth/Qwen-Image-2512-GGUF", Revision: "1626d7531f84b4d2ea1cd6d2e69f41ec027dd354", Path: "qwen-image-2512-Q5_0.gguf", Bytes: 14400813600, SHA256: "c077d5a3f867b6441235d071d76912ce6575db9832651e8b1769e8329d33d798"},
			{Role: "text_encoder", Repository: "unsloth/Qwen2.5-VL-7B-Instruct-GGUF", Revision: "68bb8bc4b7df5289c143aaec0ab477a7d4051aab", Path: "Qwen2.5-VL-7B-Instruct-UD-Q4_K_XL.gguf", Bytes: 4785159040, SHA256: "8edd10e3ae170ab8b8d114e70763ff63a220f16deec9a56acf9330034efc0bb5"},
			{Role: "vae", Repository: "Comfy-Org/Qwen-Image_ComfyUI", Revision: "1f12b17be14c89b026c51a91d67c32f84bb047bc", Path: "split_files/vae/qwen_image_vae.safetensors", Bytes: 253806246, SHA256: "a70580f0213e67967ee9c95f05bb400e8fb08307e017a924bf3441223e023d1f"},
		},
		Preset: ImagePreset{Steps: 20, CFGScale: 2.5, Width: 512, Height: 512, Sampler: "euler", FlowShift: 3, EngineFlags: "--diffusion-fa --offload-to-cpu"},
		License: ModelLicense{Name: "Apache-2.0", URL: apacheLicenseURL,
			Text: "Qwen publishes Qwen-Image-2512, its autoencoder and the Qwen2.5-VL-7B-Instruct text encoder under the Apache License 2.0; Unsloth quantises the model and the encoder to GGUF, and Comfy-Org repackages the autoencoder. The licence permits commercial use, changes and redistribution with the licence and notices kept."},
	},
}

// FindImageModel returns the declared model whose ID is id.
func FindImageModel(id string) (ImageModel, bool) {
	for _, m := range ImageModels {
		if m.ID == id {
			return m, true
		}
	}
	return ImageModel{}, false
}

// RegistryName is the model's key in Lemonade's user_models.json; Lemonade
// lists it as the model ID.
func (m ImageModel) RegistryName() string { return strings.TrimPrefix(m.ID, "user.") }

// Bytes is the total size of the model's files.
func (m ImageModel) Bytes() int64 {
	var total int64
	for _, f := range m.Files {
		total += f.Bytes
	}
	return total
}

// RegistryEntry is the model's Lemonade user_models.json entry: the sd.cpp
// recipe, each checkpoint as `<repository>:<path>`, the size in gigabytes to
// one decimal, and the preset as image_defaults.
func (m ImageModel) RegistryEntry() map[string]any {
	checkpoints := make(map[string]string, len(m.Files))
	for _, f := range m.Files {
		checkpoints[f.Role] = f.Repository + ":" + f.Path
	}
	defaults := map[string]any{"steps": m.Preset.Steps, "cfg_scale": m.Preset.CFGScale, "width": m.Preset.Width, "height": m.Preset.Height}
	if m.Preset.Sampler != "" {
		defaults["sampling_method"] = m.Preset.Sampler
	}
	if m.Preset.FlowShift != 0 {
		defaults["flow_shift"] = m.Preset.FlowShift
	}
	entry := map[string]any{
		"source": "huggingface", "recipe": "sd-cpp", "size": math.Round(float64(m.Bytes())/1e8) / 10,
		"checkpoints": checkpoints, "image_defaults": defaults,
	}
	if m.Preset.EngineFlags != "" {
		entry["recipe_options"] = map[string]any{"sdcpp_args": m.Preset.EngineFlags}
	}
	return entry
}

// checkRecipeOptions admits a registry entry's recipe_options only as exactly
// {"sdcpp_args": <flags>} for a declared model whose preset names those
// reviewed engine flags.
func checkRecipeOptions(name string, raw json.RawMessage) error {
	var declared ImageModel
	for _, m := range ImageModels {
		if m.RegistryName() == name {
			declared = m
		}
	}
	var options map[string]json.RawMessage
	var flags string
	if declared.Preset.EngineFlags == "" || json.Unmarshal(raw, &options) != nil || len(options) != 1 ||
		json.Unmarshal(options["sdcpp_args"], &flags) != nil || flags != declared.Preset.EngineFlags || !validEngineFlags(flags) {
		return fmt.Errorf("%w: model %s recipe_options", ErrWeightFormat, name)
	}
	return nil
}

var modelRoles = map[string]bool{"main": true, "text_encoder": true, "vae": true}

// Valid refuses a declaration with an ID outside "user.<name>", a weight
// file that is no safetensors or GGUF file, a file without an exact revision,
// SHA-256 and size, a role other than one each of main, text_encoder and vae,
// a preset outside the reviewed bounds, or a licence without its terms.
func (m ImageModel) Valid() error {
	name := m.RegistryName()
	if !strings.HasPrefix(m.ID, "user.") || name == "" || len(m.ID) > 128 || !safeName(name) || m.Title == "" || !strings.HasPrefix(m.Source, "https://") {
		return fmt.Errorf("%w: model ID %q", ErrImageModel, m.ID)
	}
	roles := map[string]bool{}
	for _, f := range m.Files {
		if !WeightFileAllowed(f.Path) {
			return fmt.Errorf("%w: %w: %s", ErrImageModel, ErrWeightFormat, f.Path)
		}
		owner, repo, ok := strings.Cut(f.Repository, "/")
		if !modelRoles[f.Role] || roles[f.Role] || !ok || !safeName(owner) || !safeName(repo) || len(f.Revision) != 40 || !lowerHex(f.Revision) ||
			!hex64(f.SHA256) || f.Bytes < 1 || !fs.ValidPath(f.Path) || strings.ContainsAny(f.Path, `\:`) {
			return fmt.Errorf("%w: %s file %q", ErrImageModel, m.ID, f.Path)
		}
		roles[f.Role] = true
	}
	if !roles["main"] {
		return fmt.Errorf("%w: %s has no main weights", ErrImageModel, m.ID)
	}
	if !m.Preset.valid() {
		return fmt.Errorf("%w: %s preset", ErrImageModel, m.ID)
	}
	if m.License.Name == "" || m.License.Text == "" || !strings.HasPrefix(m.License.URL, "https://") {
		return fmt.Errorf("%w: %s licence", ErrImageModel, m.ID)
	}
	return nil
}

// ValidImageModels refuses an invalid entry or two entries with one ID.
func ValidImageModels(models []ImageModel) error {
	seen := map[string]bool{}
	for _, m := range models {
		if err := m.Valid(); err != nil {
			return err
		}
		if seen[strings.ToLower(m.ID)] {
			return fmt.Errorf("%w: %s declared twice", ErrImageModel, m.ID)
		}
		seen[strings.ToLower(m.ID)] = true
	}
	return nil
}

// safeName admits letters, digits, '.', '_' and '-', starting with a letter
// or digit.
func safeName(s string) bool {
	if s == "" || len(s) > 96 {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func lowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
