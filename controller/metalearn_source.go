package controller

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

var metaLearnSourceRevision = regexp.MustCompile(`^[a-f0-9]{12,40}$`)

// The existing production workflow embeds fork-<commit> in the binary. Offer
// that build's complete public fork tree, not a mutable branch or private Go
// application's source. Unknown development builds never claim a source match.
func DownloadMetaLearnSource(c *gin.Context) {
	revision, found := strings.CutPrefix(common.Version, "fork-")
	if !found || !metaLearnSourceRevision.MatchString(revision) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "source_revision_unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Redirect(http.StatusFound, "https://github.com/xland242643/new-api/archive/"+revision+".tar.gz")
}
