package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// This credential is intentionally scoped to the dedicated integration group.
// It is never accepted by UserAuth, AdminAuth, RootAuth or relay TokenAuth.
func MetaLearnBillingAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := os.Getenv("METALEARN_BILLING_INTEGRATION_TOKEN")
		header := c.GetHeader("Authorization")
		provided, prefixed := strings.CutPrefix(header, "Bearer ")
		expectedHash := sha256.Sum256([]byte(expected))
		providedHash := sha256.Sum256([]byte(provided))
		if expected == "" || !prefixed || provided == "" || subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "code": "integration_unauthorized"})
			return
		}
		c.Next()
	}
}
