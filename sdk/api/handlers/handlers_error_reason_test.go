package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
)

// WriteErrorResponse records the error on the gin context, where the access
// log line picks it up.
func TestWriteErrorResponseRecordsReasonForAccessLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	NewBaseAPIHandlers(nil, nil).WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errors.New("unknown provider for model x")})
	if got := c.Errors.ByType(gin.ErrorTypePrivate).Last(); got == nil || got.Error() != "unknown provider for model x" {
		t.Fatalf("gin errors = %v", c.Errors)
	}
}
