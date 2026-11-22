package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Clients must not be able to switch a request into the internal compact mode.
func TestGetAltIgnoresClientCompactMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for query, want := range map[string]string{
		"alt=responses/compact":  "",
		"$alt=responses/compact": "",
		"alt=sse":                "",
		"alt=json":               "json",
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions?"+query, nil)
		if got := (&BaseAPIHandler{}).GetAlt(c); got != want {
			t.Errorf("GetAlt(%q) = %q, want %q", query, got, want)
		}
	}
}
