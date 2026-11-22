package registry

import (
	"context"

	log "github.com/sirupsen/logrus"
)

const maxDevinModelsSize = 8 << 20

// devinModelsURLs serves the catalog committed to this fork's main branch, so a catalog
// change reaches running proxies only through a reviewed PR.
var devinModelsURLs = []string{
	"https://raw.githubusercontent.com/StivenKN/CLIProxyAPI/main/internal/registry/models/devin_models.json",
}

// StartDevinModelsUpdater starts a background updater that fetches the
// Devin model catalog immediately and refreshes it every 3 hours.
// Safe to call multiple times; only one updater runs.
func StartDevinModelsUpdater(ctx context.Context) {
	devinCatalogUpdater.configure(ctx, "")
}

func tryRefreshDevinModels(ctx context.Context, label string) {
	data, sourceURL := fetchDevinModelsFromRemote(ctx)
	if data == nil {
		log.Warnf("%s: fetch failed from all URLs, keeping current data (embedded or cached fallback)", label)
		return
	}

	changed, err := loadDevinModelsFromBytes(data, sourceURL)
	if err != nil {
		log.Warnf("%s: fetched catalog rejected, keeping current data: %v", label, err)
		return
	}
	if !changed {
		log.Infof("%s completed from %s, no changes detected", label, sourceURL)
		return
	}
	log.Infof("%s completed from %s, catalog updated", label, sourceURL)
}

func fetchDevinModelsFromRemote(ctx context.Context) ([]byte, string) {
	for _, source := range devinModelsURLs {
		data, errRead := readCatalogSource(ctx, source)
		if errRead == nil && validateDevinCatalogBytes(data) == nil {
			return data, source
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, ""
}
