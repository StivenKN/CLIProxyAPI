package management

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetLatestReleaseRequestHeaders(t *testing.T) {
	tests := []struct {
		name              string
		githubToken       string
		wantAuthorization string
	}{
		{
			name:              "sets GitHub authorization",
			githubToken:       "release-token",
			wantAuthorization: "Bearer release-token",
		},
		{
			name: "omits authorization without token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", tt.githubToken)
			if tt.githubToken == "" {
				t.Setenv("github_token", "")
			}
			t.Setenv("GITSTORE_GIT_TOKEN", "")
			t.Setenv("GITSTORE_GIT_URL", "")

			req := httptest.NewRequest(http.MethodGet, latestReleaseURL, nil)
			setLatestReleaseRequestHeaders(req)

			if got := req.Header.Get("Authorization"); got != tt.wantAuthorization {
				t.Fatalf("Authorization = %q, want %q", got, tt.wantAuthorization)
			}
			if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
				t.Fatalf("Accept = %q, want GitHub JSON media type", got)
			}
			if got := req.Header.Get("User-Agent"); got != latestReleaseUserAgent {
				t.Fatalf("User-Agent = %q, want %q", got, latestReleaseUserAgent)
			}
		})
	}
}

// The version check reports this fork's releases, not upstream's.
func TestGetLatestVersionQueriesThisFork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var requested string
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requested = req.URL.String()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v9.9.9"}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/latest-version", nil)
	(&Handler{}).GetLatestVersion(c)

	if want := "https://api.github.com/repos/StivenKN/CLIProxyAPI/releases/latest"; requested != want {
		t.Fatalf("requested %q, want %q", requested, want)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "v9.9.9") {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}
