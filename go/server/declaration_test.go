package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	registration "github.com/openabstractions/abstraction-facade/go/abstraction/facade/registration"
)

func TestDeclareEngineFactsMatchesInstalledTypedDeclaration(t *testing.T) {
	path := filepath.Join("testdata", "lemonade-image.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := registration.DecodeRegistrationSpec(raw)
	if err != nil || len(spec.Providers) != 1 {
		t.Fatalf("typed declaration: %v", err)
	}
	declared := spec.Providers[0]
	fresh := registration.ProviderFacet{Models: declared.Models}
	DeclareEngineFacts(&fresh)
	if !reflect.DeepEqual(fresh.Engines, declared.Engines) || !reflect.DeepEqual(fresh.Recipes, declared.Recipes) {
		t.Fatal("installed facts differ from the producer's reviewed recipe table")
	}
}

func TestDeclareEngineFactsUsesWholeExactReviewedRecipe(t *testing.T) {
	model := ImageModels[0]
	facet := &registration.ProviderFacet{Models: []registration.ScopedModel{{Modality: "image", Model: model.ID}}}
	DeclareEngineFacts(facet)
	if len(facet.Engines) != 1 || len(facet.Recipes) != 1 || len(facet.Recipes[0].Objects) != len(model.Files) {
		t.Fatalf("whole recipe: %+v", facet)
	}
	for i, object := range facet.Recipes[0].Objects {
		if object.Role != model.Files[i].Role || object.Digest != "sha256:"+model.Files[i].SHA256 || object.Store != "" || object.Object != "" {
			t.Fatalf("recipe identity: %+v", object)
		}
	}
}

func TestDeclareEngineFactsLeavesUnreviewedModelsUnknown(t *testing.T) {
	for _, model := range []registration.ScopedModel{{Modality: "image", Model: "unreviewed-same-family"}, {Modality: "chat", Model: ImageModels[0].ID}} {
		facet := &registration.ProviderFacet{Models: []registration.ScopedModel{model}}
		DeclareEngineFacts(facet)
		if len(facet.Engines) != 0 || len(facet.Recipes) != 0 {
			t.Fatalf("inferred recipe: %+v", facet)
		}
	}
}
