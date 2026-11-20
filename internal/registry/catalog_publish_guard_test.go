package registry

import (
	"encoding/json"
	"testing"
)

func TestPublishCatalogBytesRejectsCatalogThatEmptiesSections(t *testing.T) {
	before := getModels()
	if before == nil || len(before.Gemini) == 0 || len(before.CodexPro) == 0 {
		t.Fatal("embedded catalog should populate gemini and codex-pro")
	}
	claudeBefore := len(before.Claude)
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = before
		modelsCatalogStore.mu.Unlock()
	}()

	changed, errPublish := publishCatalogBytes([]byte(`{"unrelated":true}`))
	if errPublish == nil {
		t.Fatalf("expected a catalog without any model sections to be rejected, changed providers: %v", changed)
	}
	after := getModels()
	if after != before || len(after.Claude) != claudeBefore {
		t.Fatalf("rejected catalog replaced the current one: claude %d -> %d, gemini %d", claudeBefore, len(after.Claude), len(after.Gemini))
	}
}

func TestPublishCatalogBytesRejectsCatalogMissingOneSection(t *testing.T) {
	before := getModels()
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = before
		modelsCatalogStore.mu.Unlock()
	}()

	// Keep every section except claude. Local models still add one Claude
	// model, so the check must look at the fetched section itself.
	trimmed := *before
	trimmed.Claude = nil
	data, errMarshal := json.Marshal(&trimmed)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errPublish := publishCatalogBytes(data); errPublish == nil {
		t.Fatal("expected a catalog that drops the claude section to be rejected")
	}
	if getModels() != before {
		t.Fatal("rejected catalog replaced the current one")
	}
}

func TestPublishCatalogBytesAllowsSectionThatWasAlreadyEmpty(t *testing.T) {
	before := getModels()
	defer func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = before
		modelsCatalogStore.mu.Unlock()
	}()

	withoutKimi := *before
	withoutKimi.Kimi = nil
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &withoutKimi
	modelsCatalogStore.mu.Unlock()

	next := withoutKimi
	next.Claude = append([]*ModelInfo(nil), before.Claude...)
	next.XAI = before.XAI[:1]
	data, errMarshal := json.Marshal(&next)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errPublish := publishCatalogBytes(data); errPublish != nil {
		t.Fatalf("catalog with a section that was already empty should publish: %v", errPublish)
	}
	if got := len(getModels().XAI); got != 1 {
		t.Fatalf("expected the new catalog to be published, xai has %d models", got)
	}
}
