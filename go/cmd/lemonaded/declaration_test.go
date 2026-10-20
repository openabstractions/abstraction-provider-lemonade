package main

import (
	"bytes"
	"strings"
	"testing"

	registration "github.com/openabstractions/abstraction-facade/go/abstraction/facade/registration"
)

func TestDeclareRefreshesTypedFactsWithoutSetup(t *testing.T) {
	input := &registration.RegistrationSpec{Program: "untrusted-input", Providers: []registration.ProviderFacet{{Name: "any", Endpoint: "any", Activation: registration.ActivationOnDemand,
		Services: []string{"abstraction.inference/image@1"}, Models: []registration.ScopedModel{{Modality: "image", Model: "user.LocalApprovedKlein"}, {Modality: "image", Model: "unknown"}},
		Engines: []registration.EngineFact{{ID: "invented", Name: "invented"}}}}}
	var output bytes.Buffer
	if err := writeDeclaration(bytes.NewReader(registration.EncodeRegistrationSpec(input)), &output); err != nil {
		t.Fatal(err)
	}
	spec, err := registration.DecodeRegistrationSpec(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	facet := spec.Providers[0]
	if spec.Program == input.Program || len(facet.Engines) != 1 || facet.Engines[0].ID != "sdcpp" || len(facet.Recipes) != 1 || len(facet.Recipes[0].Objects) != 3 {
		t.Fatalf("producer facts: %+v", spec)
	}
}

func TestDeclareRejectsMalformedAndOversizedInput(t *testing.T) {
	for _, raw := range []string{`{}`, `{"unknown":true}`, strings.Repeat(" ", 262145)} {
		var output bytes.Buffer
		if err := writeDeclaration(strings.NewReader(raw), &output); err == nil || output.Len() != 0 {
			t.Fatalf("invalid input emitted a declaration: %v", err)
		}
	}
}

func TestDeclareDispatchPrecedesRootOptionEffects(t *testing.T) {
	for _, args := range [][]string{{"declare"}, {"declare", "--state", "absent"}, {"--state", "absent", "declare"}, {"--dir=absent", "declare"}} {
		if !declarationInvocation(args) {
			t.Fatalf("declaration would reach effectful root-option parsing: %v", args)
		}
	}
	if declarationInvocation([]string{"--models", "declare"}) {
		t.Fatal("ordinary model value became declaration dispatch")
	}
}

func TestDeclareHelpAndRootOptionsAreInert(t *testing.T) {
	var output bytes.Buffer
	if err := declarationCommand([]string{"declare", "--help"}, nil, &output); err != nil || !strings.Contains(output.String(), "RegistrationSpec") {
		t.Fatalf("declaration help: %s %v", &output, err)
	}
	output.Reset()
	if err := declarationCommand([]string{"--state", "absent", "declare"}, nil, &output); err == nil || output.Len() != 0 {
		t.Fatalf("root options reached declaration input: %v", err)
	}
}
