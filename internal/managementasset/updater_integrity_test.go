package managementasset

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const panelBody = "<html>panel</html>"

func panelDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// stubPanelHTTP answers release and download requests in-process and records
// every host the updater contacts.
func stubPanelHTTP(t *testing.T, releaseStatus int, releaseJSON string) *[]string {
	t.Helper()
	var mu sync.Mutex
	hosts := []string{}
	previous := newHTTPClient
	newHTTPClient = func(string) *http.Client {
		return &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
			mu.Lock()
			hosts = append(hosts, req.URL.Host)
			mu.Unlock()
			status, body := http.StatusOK, panelBody
			if req.URL.Host == "api.github.com" {
				status, body = releaseStatus, releaseJSON
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})}
	}
	lastUpdateCheckMu.Lock()
	lastUpdateCheckTime = time.Time{}
	lastUpdateCheckMu.Unlock()
	t.Cleanup(func() {
		newHTTPClient = previous
		lastUpdateCheckMu.Lock()
		lastUpdateCheckTime = time.Time{}
		lastUpdateCheckMu.Unlock()
	})
	return &hosts
}

func releaseWithDigest(digest string) string {
	return `{"assets":[{"name":"management.html","browser_download_url":"https://github.com/owner/panel/releases/download/v1/management.html","digest":"` + digest + `"}]}`
}

func TestEnsureLatestManagementHTMLRefusesAssetWithoutDigest(t *testing.T) {
	stubPanelHTTP(t, http.StatusOK, releaseWithDigest(""))
	dir := t.TempDir()

	if EnsureLatestManagementHTML(t.Context(), dir, "", "") {
		t.Fatal("reported a panel as available although the release had no digest")
	}
	if _, errStat := os.Stat(filepath.Join(dir, managementAssetName)); !os.IsNotExist(errStat) {
		t.Fatalf("installed an unverified panel: stat err = %v", errStat)
	}
}

func TestEnsureLatestManagementHTMLRefusesDigestMismatch(t *testing.T) {
	stubPanelHTTP(t, http.StatusOK, releaseWithDigest(panelDigest("something else")))
	dir := t.TempDir()

	if EnsureLatestManagementHTML(t.Context(), dir, "", "") {
		t.Fatal("reported a panel as available although its digest did not match")
	}
}

func TestEnsureLatestManagementHTMLDoesNotUseUnverifiedFallback(t *testing.T) {
	hosts := stubPanelHTTP(t, http.StatusServiceUnavailable, `{}`)
	dir := t.TempDir()

	if EnsureLatestManagementHTML(t.Context(), dir, "", "") {
		t.Fatal("installed a panel although the release lookup failed")
	}
	for _, host := range *hosts {
		if host != "api.github.com" {
			t.Fatalf("contacted %s after the release lookup failed; want only api.github.com", host)
		}
	}
}

func TestEnsureLatestManagementHTMLInstallsVerifiedAsset(t *testing.T) {
	stubPanelHTTP(t, http.StatusOK, releaseWithDigest(panelDigest(panelBody)))
	dir := t.TempDir()

	if !EnsureLatestManagementHTML(t.Context(), dir, "", "") {
		t.Fatal("verified panel was not installed")
	}
	got, errRead := os.ReadFile(filepath.Join(dir, managementAssetName))
	if errRead != nil {
		t.Fatal(errRead)
	}
	if string(got) != panelBody {
		t.Fatalf("installed panel = %q, want %q", got, panelBody)
	}
}
