package middleware

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// Called only after native token validation. The scoped integration credential
// remains entirely separate from relay authentication.
func allowMetaLearnRelayToken(c *gin.Context, userID int) bool {
	isMetaLearn, err := model.IsMetaLearnBillingUser(userID)
	if err != nil {
		abortWithOpenAiMessage(c, http.StatusServiceUnavailable,
			"metalearn_billing_identity_unavailable", types.ErrorCode("metalearn_billing_identity_unavailable"))
		return false
	}
	if !isMetaLearn || metaLearnRelayTokenPathAllowed(c.Request.Method, c.Request.URL.Path) {
		return true
	}
	abortWithOpenAiMessage(c, http.StatusForbidden,
		"metalearn_monthly_requires_standard_chat", types.ErrorCode("metalearn_monthly_requires_standard_chat"))
	return false
}

func metaLearnRelayTokenPathAllowed(method, path string) bool {
	if method == http.MethodPost && path == "/v1/chat/completions" {
		return true
	}
	// Model discovery is read-only and never enters a funding path.
	return method == http.MethodGet && (path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/models/") || path == "/v1beta/models" ||
		path == "/v1beta/openai/models")
}
