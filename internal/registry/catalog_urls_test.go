package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The default remote catalogs must be the files committed to this fork's main
// branch, so catalog changes reach running proxies only through our own PRs.
func TestDefaultCatalogURLsServeThisForksCommittedFiles(t *testing.T) {
	const forkMain = "https://raw.githubusercontent.com/StivenKN/CLIProxyAPI/main/"
	for name, urls := range map[string][]string{
		"models":              modelsURLs,
		"codex_client_models": codexClientModelsURLs,
		"devin_models":        devinModelsURLs,
	} {
		if len(urls) == 0 {
			t.Errorf("%s: no default URL", name)
		}
		for _, rawURL := range urls {
			repoPath, ok := strings.CutPrefix(rawURL, forkMain)
			if !ok {
				t.Errorf("%s: %s is not served from %s", name, rawURL, forkMain)
				continue
			}
			if _, errStat := os.Stat(filepath.Join("..", "..", filepath.FromSlash(repoPath))); errStat != nil {
				t.Errorf("%s: %s does not match a committed file: %v", name, rawURL, errStat)
			}
		}
	}
}
