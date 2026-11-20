package registry

import (
	"context"
	"encoding/json"
	"testing"
)

// refreshGeneralCatalog runs one refresh of the general catalog updater
// against data, as if it had been fetched from source.
func refreshGeneralCatalog(t *testing.T, source string, data []byte) {
	t.Helper()
	u := &catalogUpdater{
		fetch:         func(context.Context, string) ([]byte, error) { return data, nil },
		publish:       generalCatalogUpdater.publish,
		officialGuard: generalCatalogUpdater.officialGuard,
	}
	u.refresh(context.Background(), source, 0)
}

func restoreModelsCatalog(t *testing.T) *staticModelsJSON {
	t.Helper()
	before := getModels()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = before
		modelsCatalogStore.mu.Unlock()
	})
	return before
}

func TestOfficialCatalogRefreshRejectsCatalogWithoutSections(t *testing.T) {
	before := restoreModelsCatalog(t)
	if before == nil || len(before.Gemini) == 0 || len(before.CodexPro) == 0 {
		t.Fatal("embedded catalog should populate gemini and codex-pro")
	}

	refreshGeneralCatalog(t, "", []byte(`{"unrelated":true}`))

	after := getModels()
	if after != before {
		t.Fatalf("re-keyed catalog replaced the current one: claude %d -> %d, gemini %d -> %d",
			len(before.Claude), len(after.Claude), len(before.Gemini), len(after.Gemini))
	}
}

func TestOfficialCatalogRefreshRejectsCatalogMissingOneSection(t *testing.T) {
	before := restoreModelsCatalog(t)

	// Keep every section except claude. Local models still add one Claude
	// model, so the check must look at the fetched section itself.
	trimmed := *before
	trimmed.Claude = nil
	data, errMarshal := json.Marshal(&trimmed)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	refreshGeneralCatalog(t, "", data)

	if getModels() != before {
		t.Fatal("catalog without a claude section replaced the current one")
	}
}

func TestOfficialCatalogRefreshAllowsSectionThatWasAlreadyEmpty(t *testing.T) {
	before := restoreModelsCatalog(t)
	withoutKimi := *before
	withoutKimi.Kimi = nil
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &withoutKimi
	modelsCatalogStore.mu.Unlock()

	next := withoutKimi
	next.XAI = before.XAI[:1]
	data, errMarshal := json.Marshal(&next)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	refreshGeneralCatalog(t, "", data)

	if got := len(getModels().XAI); got != 1 {
		t.Fatalf("catalog with an already-empty section was not published: xai has %d models", got)
	}
}

func TestConfiguredCatalogRefreshPublishesPartialCatalog(t *testing.T) {
	before := restoreModelsCatalog(t)

	// A catalog the operator configured may list only some providers.
	partial := staticModelsJSON{Claude: before.Claude[:1], Gemini: before.Gemini[:1]}
	data, errMarshal := json.Marshal(&partial)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}

	refreshGeneralCatalog(t, "/etc/cliproxy/models.json", data)

	after := getModels()
	if after == before || len(after.Gemini) != 1 {
		t.Fatalf("configured partial catalog was not published: gemini %d", len(after.Gemini))
	}
}
