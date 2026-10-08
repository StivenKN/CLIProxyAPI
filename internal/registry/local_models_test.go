package registry

import "testing"

func TestMergeLocalModelsAddsMissingClaudeModel(t *testing.T) {
	catalog := &staticModelsJSON{Claude: []*ModelInfo{{ID: "claude-opus-5-5"}}}
	mergeLocalModels(catalog)

	if !catalogHasModel(catalog.Claude, "claude-haiku-5-5") {
		t.Fatalf("expected claude-haiku-5-5 to be merged, got %d models", len(catalog.Claude))
	}
	if errValidate := validateModelSection("claude", catalog.Claude); errValidate != nil {
		t.Fatalf("merged section invalid: %v", errValidate)
	}
}

func TestMergeLocalModelsKeepsOfficialDefinition(t *testing.T) {
	official := &ModelInfo{ID: "claude-haiku-5-5", DisplayName: "Official"}
	catalog := &staticModelsJSON{Claude: []*ModelInfo{official}}
	mergeLocalModels(catalog)

	if len(catalog.Claude) != 1 || catalog.Claude[0].DisplayName != "Official" {
		t.Fatalf("official definition should win, got %+v", catalog.Claude)
	}
}

func TestPublishedCatalogIncludesLocalModels(t *testing.T) {
	if !catalogHasModel(GetClaudeModels(), "claude-haiku-5-5") {
		t.Fatal("embedded catalog should expose claude-haiku-5-5")
	}
}

func catalogHasModel(models []*ModelInfo, id string) bool {
	for _, model := range models {
		if model != nil && model.ID == id {
			return true
		}
	}
	return false
}
