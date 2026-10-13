package router

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const billingUUID = "724a42ec-a8d6-4c27-9282-993773865b40"

func billingTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	priorDB := model.DB
	priorRedis, priorBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.MetaLearnBillingUser{}, &model.MetaLearnBillingPeriod{}, &model.MetaLearnBillingScope{}, &model.SubscriptionPreConsumeRecord{}, &model.MetaLearnFundingReceipt{}, &model.MetaLearnFundingReservation{}, &model.TopUp{}, &model.MetaLearnWalletTopUp{}))
	require.NoError(t, db.Create(&model.SubscriptionPlan{Id: 991, Title: "fixed server snapshot", Enabled: true, TotalAmount: 11500000}).Error)
	t.Cleanup(func() {
		model.DB = priorDB
		common.RedisEnabled, common.BatchUpdateEnabled = priorRedis, priorBatch
		sqlDB.Close()
	})
	t.Setenv("METALEARN_BILLING_INTEGRATION_TOKEN", "isolated-integration-test-token")
	r := gin.New()
	setMetaLearnBillingRouter(r.Group("/api"))
	return r
}

func billingCall(r http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	out := httptest.NewRecorder()
	r.ServeHTTP(out, req)
	return out
}

func TestMetaLearnScopedSwitchRejectsInjectionAndHonorsExactPlanQuota(t *testing.T) {
	r := billingTestRouter(t)
	path := "/api/metalearn/billing/periods/switch"
	start := model.GetDBTimestamp() - 10
	body := fmt.Sprintf(`{"business_user_id":%q,"channel":"revenuecat","period_key":"store-paid-1","plan_id":991,"start_time":%d,"end_time":%d,"scope_key":%q,"effective_time_ms":%d,"expected_quota":11500000,"revoke":false}`, billingUUID, start, start+86400, strings.Repeat("a", 64), start*1000)
	require.Equal(t, 401, billingCall(r, "POST", path, body, "").Code)
	require.Equal(t, 400, billingCall(r, "POST", path, strings.TrimSuffix(body, "}")+`,"amount_total":99999999}`, "isolated-integration-test-token").Code)
	require.Equal(t, 200, billingCall(r, "POST", "/api/metalearn/billing/users", fmt.Sprintf(`{"business_user_id":%q}`, billingUUID), "isolated-integration-test-token").Code)
	require.Equal(t, 200, billingCall(r, "POST", path, body, "isolated-integration-test-token").Code)
	require.Equal(t, 200, billingCall(r, "POST", path, body, "isolated-integration-test-token").Code)
	bad := strings.Replace(body, `"expected_quota":11500000`, `"expected_quota":99999999`, 1)
	require.Equal(t, 409, billingCall(r, "POST", path, bad, "isolated-integration-test-token").Code)
	var count int64
	require.NoError(t, model.DB.Model(&model.UserSubscription{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestMetaLearnScopedRouteRejectsMissingCredentialAndUnknownFields(t *testing.T) {
	r := billingTestRouter(t)
	path := "/api/metalearn/billing/users"
	body := fmt.Sprintf(`{"business_user_id":%q}`, billingUUID)
	for _, token := range []string{"", "wrong"} {
		out := billingCall(r, "POST", path, body, token)
		require.Equal(t, 401, out.Code)
		require.NotContains(t, out.Body.String(), os.Getenv("METALEARN_BILLING_INTEGRATION_TOKEN"))
	}
	t.Setenv("METALEARN_BILLING_INTEGRATION_TOKEN", "")
	require.Equal(t, 401, billingCall(r, "POST", path, body, "isolated-integration-test-token").Code)
	t.Setenv("METALEARN_BILLING_INTEGRATION_TOKEN", "isolated-integration-test-token")
	for _, body := range []string{`{"business_user_id":"not-uuid"}`, fmt.Sprintf(`{"business_user_id":%q,"quota":999}`, billingUUID), fmt.Sprintf(`{"business_user_id":%q,"group":"admin"}`, billingUUID)} {
		require.Equal(t, 400, billingCall(r, "POST", path, body, "isolated-integration-test-token").Code)
	}
	var count int64
	require.NoError(t, model.DB.Model(&model.User{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestMetaLearnFundingReceiptScopedIdentityAndExplicitZero(t *testing.T) {
	r := billingTestRouter(t)
	token := "isolated-integration-test-token"
	user, err := model.EnsureMetaLearnBillingUser(billingUUID)
	require.NoError(t, err)
	now := model.GetDBTimestamp()
	period, err := model.SetMetaLearnBillingPeriod(model.MetaLearnPeriodRequest{BusinessUserID: billingUUID, Channel: "huawei", PeriodKey: "native-request-period", PlanID: 991, StartTime: now - 10, EndTime: now + 86400}, false)
	require.NoError(t, err)
	_, err = model.PreConsumeUserSubscription("gateway-request-1", user.GatewayUserID, "mock-model", 0, 100)
	require.NoError(t, err)
	path := "/api/metalearn/billing/users/" + billingUUID + "/requests/gateway-request-1"
	require.Equal(t, 401, billingCall(r, "GET", path, "", "").Code)
	for _, uuid := range []string{"724a42ec-a8d6-4c27-9282-993773865b41", billingUUID} {
		request := "gateway-request-1"
		if uuid == billingUUID {
			request = "unknown-request"
		}
		out := billingCall(r, "GET", "/api/metalearn/billing/users/"+uuid+"/requests/"+request, "", token)
		require.Equal(t, 404, out.Code)
		require.Contains(t, out.Body.String(), "request_not_found")
		require.NotContains(t, out.Body.String(), user.RelayToken)
	}
	out := billingCall(r, "GET", path, "", token)
	require.Equal(t, 200, out.Code)
	require.Contains(t, out.Body.String(), `"settled_quota":null`)
	var response struct {
		Data model.MetaLearnFundingResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(out.Body.Bytes(), &response))
	require.Equal(t, "subscription_funding", response.Data.ReceiptScope)
	require.Equal(t, user.GatewayUserID, response.Data.GatewayUserID)
	require.Equal(t, billingUUID, response.Data.BusinessUserID)
	require.NoError(t, model.SettleMetaLearnFunding("gateway-request-1", user.GatewayUserID, period.SubscriptionID, 0))
	out = billingCall(r, "GET", path, "", token)
	require.Equal(t, 200, out.Code)
	require.Contains(t, out.Body.String(), `"settled_quota":0`)
	require.Contains(t, out.Body.String(), `"funding_status":"funding_settled"`)
	for _, secret := range []string{user.RelayToken, "relay_token", "token_key", "log_content", "prompt", "completion"} {
		require.NotContains(t, out.Body.String(), secret)
	}
	require.Equal(t, 400, billingCall(r, "GET", "/api/metalearn/billing/users/not-uuid/requests/gateway-request-1", "", token).Code)
	require.Equal(t, 400, billingCall(r, "GET", "/api/metalearn/billing/users/"+billingUUID+"/requests/invalid.request", "", token).Code)
}

func TestMetaLearnScopedRouterIdempotencyConflictsAndNoSecretQuery(t *testing.T) {
	r := billingTestRouter(t)
	token := "isolated-integration-test-token"
	query := "/api/metalearn/billing/users/" + billingUUID + "/subscriptions"
	require.Equal(t, 404, billingCall(r, "GET", query, "", token).Code)
	out := billingCall(r, "POST", "/api/metalearn/billing/users", fmt.Sprintf(`{"business_user_id":%q}`, billingUUID), token)
	require.Equal(t, 200, out.Code)
	var provision struct {
		Data model.MetaLearnUserResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(out.Body.Bytes(), &provision))
	require.NotEmpty(t, provision.Data.RelayToken)
	body := fmt.Sprintf(`{"business_user_id":%q,"channel":"apple","period_key":"api-period","plan_id":991,"start_time":1234567890,"end_time":3234567890}`, billingUUID)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- billingCall(r, "POST", "/api/metalearn/billing/periods", body, token).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		require.Equal(t, 200, code)
	}
	for _, bad := range []string{strings.Replace(body, billingUUID, "724a42ec-a8d6-4c27-9282-993773865b41", 1), strings.Replace(body, `"plan_id":991`, `"plan_id":992`, 1), strings.Replace(body, `1234567890`, `1234567891`, 1), strings.Replace(body, `3234567890`, `3234567891`, 1)} {
		require.Equal(t, 409, billingCall(r, "POST", "/api/metalearn/billing/periods", bad, token).Code)
	}
	require.Equal(t, 400, billingCall(r, "POST", "/api/metalearn/billing/periods", strings.TrimSuffix(body, "}")+`,"total_amount":999}`, token).Code)
	out = billingCall(r, "GET", query, "", token)
	require.Equal(t, 200, out.Code)
	require.NotContains(t, out.Body.String(), "relay_token")
	require.NotContains(t, out.Body.String(), provision.Data.RelayToken)
	var view struct {
		Data model.MetaLearnSubscriptionsResult `json:"data"`
	}
	require.NoError(t, common.Unmarshal(out.Body.Bytes(), &view))
	require.Equal(t, billingUUID, view.Data.BusinessUserID)
	require.Equal(t, provision.Data.GatewayUserID, view.Data.GatewayUserID)
	require.EqualValues(t, 1000, view.Data.QuotaPerCredit)
	require.Len(t, view.Data.Subscriptions, 1)
	require.EqualValues(t, 11500000, view.Data.Subscriptions[0].AmountTotal)
	require.Equal(t, 200, billingCall(r, "POST", "/api/metalearn/billing/periods/revoke", body, token).Code)
	require.Equal(t, 200, billingCall(r, "POST", "/api/metalearn/billing/periods/revoke", body, token).Code)
	require.Equal(t, 409, billingCall(r, "POST", "/api/metalearn/billing/periods", body, token).Code)
	// Refund-before-grant does not require a user or active plan; it prevents later resurrection.
	body = strings.Replace(body, billingUUID, "724a42ec-a8d6-4c27-9282-993773865b42", 1)
	body = strings.Replace(body, "api-period", "refund-first", 1)
	require.Equal(t, 200, billingCall(r, "POST", "/api/metalearn/billing/periods/revoke", body, token).Code)
	require.Equal(t, 409, billingCall(r, "POST", "/api/metalearn/billing/periods", body, token).Code)
}
