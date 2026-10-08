package registry

import (
	_ "embed"
	"encoding/json"
	"strings"

	log "github.com/sirupsen/logrus"
)

// localModelsJSON holds fork-local models that the official catalog does not
// list yet. They are kept separate from models.json so upstream syncs never
// conflict, and they are merged into every catalog this process publishes.
//
//go:embed models/local_models.json
var localModelsJSON []byte

var localModels = mustParseLocalModels(localModelsJSON)

func mustParseLocalModels(data []byte) *staticModelsJSON {
	var parsed staticModelsJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Warnf("registry: failed to parse local_models.json, ignoring local models: %v", err)
		return &staticModelsJSON{}
	}
	return &parsed
}

// mergeLocalModels appends each local model to its catalog section unless the
// section already defines that ID, so an official definition always wins.
func mergeLocalModels(catalog *staticModelsJSON) {
	if catalog == nil || localModels == nil {
		return
	}
	catalog.Claude = appendMissingModels(catalog.Claude, localModels.Claude)
}

func appendMissingModels(section, extras []*ModelInfo) []*ModelInfo {
	if len(extras) == 0 {
		return section
	}
	seen := make(map[string]struct{}, len(section))
	for _, model := range section {
		if model != nil {
			seen[strings.TrimSpace(model.ID)] = struct{}{}
		}
	}
	for _, model := range extras {
		if model == nil {
			continue
		}
		if _, exists := seen[strings.TrimSpace(model.ID)]; exists {
			continue
		}
		section = append(section, cloneModelInfos([]*ModelInfo{model})...)
	}
	return section
}
