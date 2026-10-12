package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMetaLearnSourceOffersOnlyExactPublicBuildRevision(t *testing.T) {
	old := common.Version
	t.Cleanup(func() { common.Version = old })
	for _, version := range []string{"fork-77fac0557221", "fork-77fac055722165fd088e85e3173d7de68f015d0a", "v0.0.0", "fork-main", "fork-../../secret", "fork-77fac0557221?url=evil"} {
		common.Version = version
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/metalearn/source", nil)
		DownloadMetaLearnSource(ctx)
		if version == "fork-77fac0557221" || version == "fork-77fac055722165fd088e85e3173d7de68f015d0a" {
			require.Equal(t, http.StatusFound, response.Code)
			require.Equal(t, "https://github.com/xland242643/new-api/archive/"+version[5:]+".tar.gz", response.Header().Get("Location"))
		} else {
			require.Equal(t, http.StatusServiceUnavailable, response.Code)
			require.Empty(t, response.Header().Get("Location"))
		}
	}
}
