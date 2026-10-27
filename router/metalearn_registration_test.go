package router

import (
	"fmt"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestMetaLearnRegistrationScopedRoute(t *testing.T) {
	r := billingTestRouter(t)
	require.NoError(t, model.DB.AutoMigrate(&model.MetaLearnRegistrationReward{}))
	path := "/api/metalearn/billing/registration-rewards"
	body := fmt.Sprintf(`{"business_user_id":%q,"reward_key":"registration-v1","quota_amount":500000}`, billingUUID)
	token := "isolated-integration-test-token"
	require.Equal(t, 400, billingCall(r, "POST", path, strings.Replace(body, `,"quota_amount":500000`, "", 1), token).Code)
	require.Equal(t, 401, billingCall(r, "POST", path, body, "").Code)
	require.Equal(t, 400, billingCall(r, "POST", path, strings.TrimSuffix(body, "}")+`,"admin":true}`, token).Code)
	require.Equal(t, 200, billingCall(r, "POST", "/api/metalearn/billing/users", fmt.Sprintf(`{"business_user_id":%q}`, billingUUID), token).Code)
	require.Equal(t, 200, billingCall(r, "POST", path, body, token).Code)
	require.Equal(t, 200, billingCall(r, "POST", path, body, token).Code)
	require.Equal(t, 409, billingCall(r, "POST", path, strings.Replace(body, "500000", "1000000", 1), token).Code)
	view, err := model.GetMetaLearnWallet(billingUUID)
	require.NoError(t, err)
	require.EqualValues(t, 500000, view.QuotaBalance)
}
