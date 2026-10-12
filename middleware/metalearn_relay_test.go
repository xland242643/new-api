package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMetaLearnRelayTokenRestrictsFundingPaths(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPost, "/v1/chat/completions", true},
		{http.MethodGet, "/v1/models", true},
		{http.MethodGet, "/v1/models/qwen-test", true},
		{http.MethodGet, "/v1beta/models", true},
		{http.MethodGet, "/v1beta/openai/models", true},
		{http.MethodGet, "/v1/realtime", false},
		{http.MethodPost, "/v1/chat/completions/extra", false},
		{http.MethodPost, "/pg/chat/completions", false},
		{http.MethodPost, "/v1/responses", false},
		{http.MethodPost, "/mj/submit/imagine", false},
		{http.MethodPost, "/v1/videos", false},
		{http.MethodPost, "/v1/models/qwen-test:generateContent", false},
	} {
		require.Equal(t, tc.allowed, metaLearnRelayTokenPathAllowed(tc.method, tc.path), "%s %s", tc.method, tc.path)
	}
	priorDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { model.DB = priorDB; _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.MetaLearnBillingUser{}))
	require.NoError(t, db.Create(&model.User{Id: 7, Username: "mapped-user", Password: "unused", Group: "default"}).Error)
	require.NoError(t, db.Create(&model.MetaLearnBillingUser{BusinessUserID: "724a42ec-a8d6-4c27-9282-993773865b40", GatewayUserID: 7}).Error)
	gin.SetMode(gin.TestMode)
	call := func(path string) *httptest.ResponseRecorder {
		out := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(out)
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		c.Set("id", 7)
		if allowMetaLearnRelayToken(c, 7) {
			c.Status(http.StatusNoContent)
			c.Writer.WriteHeaderNow()
		}
		return out
	}
	require.Equal(t, http.StatusNoContent, call("/v1/chat/completions").Code)
	require.Equal(t, http.StatusForbidden, call("/v1/responses").Code)
	// An identity lookup failure cannot re-open any endpoint.
	require.NoError(t, db.Migrator().DropTable(&model.MetaLearnBillingUser{}))
	out := call("/v1/chat/completions")
	require.Equal(t, http.StatusServiceUnavailable, out.Code)
	require.Contains(t, out.Body.String(), "metalearn_billing_identity_unavailable")
	require.NotContains(t, out.Body.String(), "no such table")
}
