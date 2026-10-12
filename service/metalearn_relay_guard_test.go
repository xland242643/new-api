package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupMetaLearnRelayGuardTest(t *testing.T) *gorm.DB {
	t.Helper()
	priorDB := model.DB
	priorRedis, priorBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.MetaLearnBillingUser{}))
	for _, user := range []model.User{
		{Id: 11, Username: "mapped", Password: "irrelevant", AffCode: "mapped", Group: "default", Quota: 1000},
		{Id: 12, Username: "reserved", Password: "irrelevant", AffCode: "reserved", Group: model.MetaLearnMonthlyGroup, Quota: 1000},
		{Id: 13, Username: "ordinary", Password: "irrelevant", AffCode: "ordinary", Group: "default", Quota: 1000},
	} {
		require.NoError(t, db.Create(&user).Error)
	}
	require.NoError(t, db.Create(&model.MetaLearnBillingUser{BusinessUserID: "724a42ec-a8d6-4c27-9282-993773865b40", GatewayUserID: 11}).Error)
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 3*time.Second, time.Millisecond)
		model.DB = priorDB
		common.RedisEnabled, common.BatchUpdateEnabled = priorRedis, priorBatch
		_ = sqlDB.Close()
	})
	return db
}

func metaLearnGuardContext(method, path string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(method, path, nil)
	return c
}

func TestMetaLearnRelayAdmissionUsesOriginalIngressAndIdentity(t *testing.T) {
	setupMetaLearnRelayGuardTest(t)
	gin.SetMode(gin.TestMode)
	for _, id := range []int{11, 12} {
		info := &relaycommon.RelayInfo{UserId: id, RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions}
		isMeta, err := RequireMetaLearnStandardChat(metaLearnGuardContext(http.MethodPost, "/v1/chat/completions?client=test"), info)
		require.True(t, isMeta)
		require.NoError(t, err)
		// A mutable upstream URL is not the HTTP admission authority.
		info.RequestURLPath = "/v1/responses"
		isMeta, err = RequireMetaLearnStandardChat(metaLearnGuardContext(http.MethodPost, "/v1/chat/completions"), info)
		require.True(t, isMeta)
		require.NoError(t, err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions/extra"},
		{http.MethodPost, "/pg/chat/completions"},
		{http.MethodPost, "/v1/responses"},
		{http.MethodGet, "/v1/realtime"},
		{http.MethodPost, "/v1/images/generations"},
		{http.MethodPost, "/suno/submit/music"},
	} {
		isMeta, err := CheckMetaLearnRelayEntry(metaLearnGuardContext(tc.method, tc.path), 11, types.RelayFormatOpenAI)
		require.True(t, isMeta)
		require.ErrorIs(t, err, ErrMetaLearnRelayUnsupported)
	}
	for _, mutate := range []func(*relaycommon.RelayInfo){
		func(i *relaycommon.RelayInfo) { i.RelayFormat = types.RelayFormatOpenAIRealtime },
		func(i *relaycommon.RelayInfo) { i.RelayMode = relayconstant.RelayModeResponses },
		func(i *relaycommon.RelayInfo) { i.IsPlayground = true },
		func(i *relaycommon.RelayInfo) { i.IsChannelTest = true },
		func(i *relaycommon.RelayInfo) { i.ClientWs = &websocket.Conn{} },
	} {
		info := &relaycommon.RelayInfo{UserId: 11, RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions}
		mutate(info)
		isMeta, err := RequireMetaLearnStandardChat(metaLearnGuardContext(http.MethodPost, "/v1/chat/completions"), info)
		require.True(t, isMeta)
		require.ErrorIs(t, err, ErrMetaLearnRelayUnsupported)
	}
	isMeta, err := RequireMetaLearnStandardChat(metaLearnGuardContext(http.MethodGet, "/v1/realtime"), &relaycommon.RelayInfo{UserId: 13, RelayFormat: types.RelayFormatOpenAIRealtime})
	require.False(t, isMeta)
	require.NoError(t, err)
}

func TestMetaLearnLegacyFundingRejectsBeforeZeroAndPriceBypasses(t *testing.T) {
	db := setupMetaLearnRelayGuardTest(t)
	c := metaLearnGuardContext(http.MethodPost, "/v1/chat/completions")
	for _, id := range []int{11, 12} {
		info := &relaycommon.RelayInfo{UserId: id, UsePrice: true, IsPlayground: true}
		require.ErrorIs(t, PreWssConsumeQuota(c, info, nil), ErrMetaLearnRelayUnsupported)
		for _, quota := range []int{0, 100, -100} {
			require.ErrorIs(t, PostConsumeQuota(info, quota, 0, false), ErrMetaLearnRelayUnsupported)
			err := PreConsumeQuota(c, quota, info)
			require.ErrorIs(t, err, ErrMetaLearnRelayUnsupported)
			require.Equal(t, http.StatusForbidden, err.StatusCode)
		}
		var user model.User
		require.NoError(t, db.First(&user, id).Error)
		require.Equal(t, 1000, user.Quota)
	}
	// Native ordinary-wallet behavior still applies.
	require.NoError(t, PostConsumeQuota(&relaycommon.RelayInfo{UserId: 13, IsPlayground: true}, 100, 0, false))
	var user model.User
	require.NoError(t, db.First(&user, 13).Error)
	require.Equal(t, 900, user.Quota)
}

func TestMetaLearnRelayGuardUnavailableNeverExposesDatabaseError(t *testing.T) {
	db := setupMetaLearnRelayGuardTest(t)
	require.NoError(t, db.Migrator().DropTable(&model.MetaLearnBillingUser{}))
	isMeta, err := IsMetaLearnBillingRelay(&relaycommon.RelayInfo{UserId: 11})
	require.False(t, isMeta)
	require.ErrorIs(t, err, ErrMetaLearnRelayUnavailable)
	require.Equal(t, "metalearn_billing_identity_unavailable", err.Error())
	apiErr := MetaLearnRelayGuardAPIError(errors.New("SQL password=must-never-leak"))
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	require.Equal(t, "metalearn_billing_identity_unavailable", apiErr.Error())
	require.ErrorIs(t, PostConsumeQuota(&relaycommon.RelayInfo{UserId: 13}, 0, 0, false), ErrMetaLearnRelayUnavailable)
}
