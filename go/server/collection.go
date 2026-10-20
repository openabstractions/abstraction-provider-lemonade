package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrCollectionModel reports a declared model that resolves to a Lemonade
// collection. An Omni collection (recipe collection.omni) runs an LLM whose
// tool calls generate_image and edit_image Lemonade's collection orchestrator
// sends to sd-server itself (collection_orchestrator.cpp), past the image
// route's checks (inference CONTRACT.md INF-SD1, INF-SD2); a router
// collection (collection.router) forwards to models the provider never
// declared.
var ErrCollectionModel = errors.New("lemonade: declared model is a collection")

// builtinCollections are the models whose recipe is collection.omni in the
// pinned package's resources/server_models.json (Lemonade 2026.39.1, SHA-256
// df9f6a619d03ac01c88d0ff35279fa84b0c1d4c97306fbc0ca73b0d19751848a).
var builtinCollections = []string{"LMX-Omni-52B-Halo", "LMX-Omni-5.5B-Lite", "RPG-HaloTales-V1", "RPG-HaloTales-V2"}

// userModelPrefix and extraModelPrefix are the name prefixes Lemonade gives
// models from user_models.json and from extra_models_dir; builtinModelPrefix
// is the alias it registers for every built-in model (canonical_id.h,
// model_manager.cpp), so builtin.LMX-Omni-5.5B-Lite names that collection.
const (
	userModelPrefix    = "user."
	extraModelPrefix   = "extra."
	builtinModelPrefix = "builtin."
)

// checkDeclaredModels refuses a declared model that resolves, or may
// resolve, to a collection: a built-in collection, by its name or its
// builtin.<name> alias, in any letter case; a
// model from extra_models_dir, whose recipe comes from files the provider
// does not pin; and a user model whose entry in the private config folder's
// user_models.json names a collection.* recipe. An absent registry resolves
// no user model.
func checkDeclaredModels(configDir string, models []string) error {
	var registry map[string]map[string]json.RawMessage
	data, err := readSmall(filepath.Join(configDir, "user_models.json"), registryLimit)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &registry); err != nil {
			return fmt.Errorf("%w: registry: %v", ErrCollectionModel, err)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("%w: registry: %v", ErrCollectionModel, err)
	}
	for _, model := range models {
		bare := model
		if strings.HasPrefix(strings.ToLower(model), builtinModelPrefix) {
			bare = model[len(builtinModelPrefix):]
		}
		for _, collection := range builtinCollections {
			if strings.EqualFold(bare, collection) {
				return fmt.Errorf("%w: %s is a built-in Omni collection", ErrCollectionModel, model)
			}
		}
		lower := strings.ToLower(model)
		if strings.HasPrefix(lower, extraModelPrefix) {
			return fmt.Errorf("%w: %s comes from extra_models_dir", ErrCollectionModel, model)
		}
		if !strings.HasPrefix(lower, userModelPrefix) {
			continue
		}
		for key, fields := range registry {
			if !strings.EqualFold(key, model[len(userModelPrefix):]) {
				continue
			}
			var recipe string
			if err := json.Unmarshal(fields["recipe"], &recipe); err != nil || strings.HasPrefix(strings.ToLower(recipe), "collection") {
				return fmt.Errorf("%w: %s has recipe %q", ErrCollectionModel, model, recipe)
			}
		}
	}
	return nil
}
