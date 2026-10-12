package router

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func TestMetaLearnWalletScopedOrderAndQueryContracts(t *testing.T) {
	r := billingTestRouter(t)
	token := "isolated-integration-test-token"
	user, err := model.EnsureMetaLearnBillingUser(billingUUID)
	require.NoError(t, err)
	path := "/api/metalearn/billing/topups"
	body := fmt.Sprintf(`{"business_user_id":%q,"channel":"huawei","order_key":"verified-order","product_id":"credits_lite","quota_amount":6900000,"currency":"CNY","paid_amount_minor":3900}`, billingUUID)
	for _, path := range []string{path, path + "/revoke"} {
		require.Equal(t, 401, billingCall(r, "POST", path, body, "").Code)
	}
	query := "/api/metalearn/billing/users/" + billingUUID + "/wallet"
	require.Equal(t, 401, billingCall(r, "GET", query, "", "").Code)
	for _, bad := range []string{strings.TrimSuffix(body, "}") + `,"expires_at":123}`, strings.Replace(body, `"CNY"`, `"USD"`, 1), strings.Replace(body, `6900000`, `6900001`, 1), strings.Replace(body, `3900`, `0`, 1)} {
		require.Equal(t, 400, billingCall(r, "POST", path, bad, token).Code)
	}
	for i := 0; i < 2; i++ {
		out := billingCall(r, "POST", path, body, token)
		require.Equal(t, 200, out.Code)
		require.Contains(t, out.Body.String(), `"quota_balance":6900000`)
		require.Contains(t, out.Body.String(), `"status":"granted"`)
	}
	out := billingCall(r, "GET", query, "", token)
	require.Equal(t, 200, out.Code)
	require.Contains(t, out.Body.String(), `"quota_per_credit":1000`)
	for _, secret := range []string{user.RelayToken, "relay_token", "expires_at", "end_time"} {
		require.NotContains(t, out.Body.String(), secret)
	}
	conflict := strings.Replace(body, `"credits_lite"`, `"different-product"`, 1)
	for _, path := range []string{path, path + "/revoke"} {
		out = billingCall(r, "POST", path, conflict, token)
		require.Equal(t, 409, out.Code)
		require.Contains(t, out.Body.String(), "topup_conflict")
	}
	require.NoError(t, model.PreConsumeMetaLearnWallet("wallet-api", user.GatewayUserID, 100))
	require.NoError(t, model.SettleMetaLearnWalletFunding("wallet-api", user.GatewayUserID, 100))
	out = billingCall(r, "GET", "/api/metalearn/billing/users/"+billingUUID+"/requests/wallet-api", "", token)
	require.Equal(t, 200, out.Code)
	require.Contains(t, out.Body.String(), `"receipt_scope":"wallet_funding"`)
	require.Contains(t, out.Body.String(), `"funding_source":"wallet"`)
	require.Contains(t, out.Body.String(), `"subscription_id":0`)
	for i := 0; i < 2; i++ {
		out = billingCall(r, "POST", path+"/revoke", body, token)
		require.Equal(t, 200, out.Code)
		require.Contains(t, out.Body.String(), `"quota_balance":-100`)
	}
	out = billingCall(r, "POST", path, body, token)
	require.Equal(t, 409, out.Code)
	require.Contains(t, out.Body.String(), "topup_revoked")
	body = strings.Replace(body, billingUUID, "724a42ec-a8d6-4c27-9282-993773865b41", 1)
	body = strings.Replace(body, "verified-order", "refund-first", 1)
	require.Equal(t, 200, billingCall(r, "POST", path+"/revoke", body, token).Code)
	require.Equal(t, 409, billingCall(r, "POST", path, body, token).Code)
}
