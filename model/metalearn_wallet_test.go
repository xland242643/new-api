package model

import (
	"errors"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func metaLearnTestTopUp() MetaLearnTopUpRequest {
	return MetaLearnTopUpRequest{BusinessUserID: metalearnTestUUID, Channel: "huawei", OrderKey: "verified-permanent-order", ProductID: "metalearn_credits_6900", QuotaAmount: 6900000, Currency: "CNY", PaidAmountMinor: 3900}
}

func TestMetaLearnWalletTopUpConcurrentIdentityAndNoExpiry(t *testing.T) {
	setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metaLearnTestTopUp()
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := SetMetaLearnWalletTopUp(r, false); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Equal(t, r.QuotaAmount, view.QuotaBalance)
	require.Equal(t, user.GatewayUserID, view.GatewayUserID)
	var count int64
	require.NoError(t, DB.Model(&TopUp{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, DB.Model(&MetaLearnWalletTopUp{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	for _, mutate := range []func(*MetaLearnTopUpRequest){
		func(r *MetaLearnTopUpRequest) { r.BusinessUserID = metalearnOtherUUID },
		func(r *MetaLearnTopUpRequest) { r.ProductID = "other-product" },
		func(r *MetaLearnTopUpRequest) { r.QuotaAmount += 1000 },
		func(r *MetaLearnTopUpRequest) { r.PaidAmountMinor++ },
	} {
		bad := r
		mutate(&bad)
		_, err := SetMetaLearnWalletTopUp(bad, false)
		require.ErrorIs(t, err, ErrMetaLearnTopUpConflict)
	}
	// Creation/query have no expiry or subscription dependency. Permanent funds
	// use the native wallet even when every monthly term is absent or expired.
	var periods int64
	require.NoError(t, DB.Model(&UserSubscription{}).Count(&periods).Error)
	require.Zero(t, periods)
	data, err := common.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(data), "end_time")
	require.NotContains(t, string(data), "expire")
}

func TestMetaLearnWalletTopUpFullRefundDebtAndTombstone(t *testing.T) {
	setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metaLearnTestTopUp()
	_, err = SetMetaLearnWalletTopUp(r, false)
	require.NoError(t, err)
	require.NoError(t, PreConsumeMetaLearnWallet("wallet-spend", user.GatewayUserID, 1000))
	require.NoError(t, SettleMetaLearnWalletFunding("wallet-spend", user.GatewayUserID, 3000))
	for i := 0; i < 2; i++ {
		result, err := SetMetaLearnWalletTopUp(r, true)
		require.NoError(t, err)
		require.Equal(t, "revoked", result.Status)
		require.EqualValues(t, -3000, result.QuotaBalance)
	}
	_, err = SetMetaLearnWalletTopUp(r, false)
	require.ErrorIs(t, err, ErrMetaLearnTopUpRevoked)
	require.ErrorIs(t, PreConsumeMetaLearnWallet("debt-denied", user.GatewayUserID, 1), ErrMetaLearnWalletInsufficient)
	next := r
	next.OrderKey = "next-order"
	result, err := SetMetaLearnWalletTopUp(next, false)
	require.NoError(t, err)
	require.Equal(t, r.QuotaAmount-3000, result.QuotaBalance)
	before := r
	before.BusinessUserID = metalearnOtherUUID
	before.OrderKey = "refund-before-grant"
	result, err = SetMetaLearnWalletTopUp(before, true)
	require.NoError(t, err)
	require.Equal(t, "revoked", result.Status)
	require.Zero(t, result.GatewayUserID)
	_, err = SetMetaLearnWalletTopUp(before, false)
	require.ErrorIs(t, err, ErrMetaLearnTopUpRevoked)
}

func TestMetaLearnWalletGrantRevokeRaceAndRollback(t *testing.T) {
	setupMetaLearnTest(t)
	_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metaLearnTestTopUp()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(revoke bool) {
			defer wg.Done()
			_, err := SetMetaLearnWalletTopUp(r, revoke)
			if revoke || !errors.Is(err, ErrMetaLearnTopUpRevoked) {
				errs <- err
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Zero(t, view.QuotaBalance)
	_, err = SetMetaLearnWalletTopUp(r, false)
	require.ErrorIs(t, err, ErrMetaLearnTopUpRevoked)
	// A native TopUp write failure must roll back its wallet delta and order row.
	callback := "wallet_topup_failure"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "top_ups" {
			tx.AddError(errors.New("injected native topup failure"))
		}
	}))
	next := r
	next.OrderKey = "rollback-order"
	_, err = SetMetaLearnWalletTopUp(next, false)
	require.Error(t, err)
	require.NoError(t, DB.Callback().Create().Remove(callback))
	view, err = GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Zero(t, view.QuotaBalance)
	_, err = SetMetaLearnWalletTopUp(next, false)
	require.NoError(t, err)
}

func TestMetaLearnWalletFundingNativeAmountsAndReceiptAtomicity(t *testing.T) {
	setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	_, err = SetMetaLearnWalletTopUp(metaLearnTestTopUp(), false)
	require.NoError(t, err)
	for _, tc := range []struct {
		request     string
		pre, actual int64
	}{{"zero", 100, 0}, {"equal", 100, 100}, {"overage", 100, 160}, {"free", 0, 0}} {
		before, err := GetMetaLearnWallet(metalearnTestUUID)
		require.NoError(t, err)
		require.NoError(t, PreConsumeMetaLearnWallet(tc.request, user.GatewayUserID, tc.pre))
		require.NoError(t, PreConsumeMetaLearnWallet(tc.request, user.GatewayUserID, tc.pre))
		require.NoError(t, SettleMetaLearnWalletFunding(tc.request, user.GatewayUserID, tc.actual))
		require.NoError(t, SettleMetaLearnWalletFunding(tc.request, user.GatewayUserID, tc.actual))
		require.ErrorIs(t, SettleMetaLearnWalletFunding(tc.request, user.GatewayUserID, tc.actual+1), ErrMetaLearnFundingConflict)
		after, err := GetMetaLearnWallet(metalearnTestUUID)
		require.NoError(t, err)
		require.Equal(t, before.QuotaBalance-tc.actual, after.QuotaBalance)
		receipt, err := GetMetaLearnFundingReceipt(metalearnTestUUID, tc.request)
		require.NoError(t, err)
		require.Equal(t, "wallet_funding", receipt.ReceiptScope)
		require.Equal(t, "wallet", receipt.FundingSource)
		require.Equal(t, tc.actual, *receipt.SettledQuota)
		require.Zero(t, receipt.SubscriptionID)
		require.Zero(t, receipt.PlanID)
		require.Empty(t, receipt.Channel)
		require.Empty(t, receipt.PeriodKey)
		require.ErrorIs(t, RefundMetaLearnWalletFunding(tc.request, user.GatewayUserID), ErrMetaLearnFundingConflict)
	}
	callback := "wallet_receipt_create_failure"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "meta_learn_funding_receipts" {
			tx.AddError(errors.New("injected receipt failure"))
		}
	}))
	before, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Error(t, PreConsumeMetaLearnWallet("write-failure", user.GatewayUserID, 100))
	require.NoError(t, DB.Callback().Create().Remove(callback))
	after, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Equal(t, before.QuotaBalance, after.QuotaBalance)
	_, err = GetMetaLearnFundingReceipt(metalearnTestUUID, "write-failure")
	require.ErrorIs(t, err, ErrMetaLearnRequestNotFound)
}

func TestMetaLearnWalletFundingExtraReserveRollbackFullRefundAndRace(t *testing.T) {
	setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	_, err = SetMetaLearnWalletTopUp(metaLearnTestTopUp(), false)
	require.NoError(t, err)
	require.NoError(t, PreConsumeMetaLearnWallet("reserve-refund", user.GatewayUserID, 100))
	op := uuid.NewString()
	for i := 0; i < 2; i++ {
		require.NoError(t, ReserveMetaLearnWalletFunding("reserve-refund", user.GatewayUserID, op, 100, 50))
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, RollbackMetaLearnWalletFundingReservation("reserve-refund", user.GatewayUserID, op))
	}
	require.NoError(t, ReserveMetaLearnWalletFunding("reserve-refund", user.GatewayUserID, uuid.NewString(), 100, 80))
	for i := 0; i < 2; i++ {
		require.NoError(t, RefundMetaLearnWalletFunding("reserve-refund", user.GatewayUserID))
	}
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Equal(t, metaLearnTestTopUp().QuotaAmount, view.QuotaBalance)
	receipt, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "reserve-refund")
	require.NoError(t, err)
	require.Equal(t, MetaLearnFundingRefunded, receipt.FundingStatus)
	require.Zero(t, *receipt.SettledQuota)
	require.NoError(t, PreConsumeMetaLearnWallet("race", user.GatewayUserID, 100))
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(refund bool) {
			defer wg.Done()
			var err error
			if refund {
				err = RefundMetaLearnWalletFunding("race", user.GatewayUserID)
			} else {
				err = SettleMetaLearnWalletFunding("race", user.GatewayUserID, 60)
			}
			if !errors.Is(err, ErrMetaLearnFundingConflict) {
				errs <- err
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	receipt, err = GetMetaLearnFundingReceipt(metalearnTestUUID, "race")
	require.NoError(t, err)
	view, err = GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	if receipt.FundingStatus == MetaLearnFundingSettled {
		require.EqualValues(t, 60, *receipt.SettledQuota)
		require.Equal(t, metaLearnTestTopUp().QuotaAmount-60, view.QuotaBalance)
	} else {
		require.Equal(t, MetaLearnFundingRefunded, receipt.FundingStatus)
		require.Equal(t, metaLearnTestTopUp().QuotaAmount, view.QuotaBalance)
	}
}

func TestMetaLearnWalletOldMappedPreferenceUpgradeAndOverflow(t *testing.T) {
	setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	settings, err := common.Marshal(dto.UserSetting{BillingPreference: "subscription_only"})
	require.NoError(t, err)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.GatewayUserID).Update("setting", string(settings)).Error)
	// The previous subscription-only mapping remains readable before upgrade.
	_, err = GetMetaLearnSubscriptions(metalearnTestUUID)
	require.NoError(t, err)
	again, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	require.Equal(t, user, again)
	var native User
	require.NoError(t, DB.First(&native, user.GatewayUserID).Error)
	require.Equal(t, "subscription_first", native.GetSetting().BillingPreference)
	maxInt := int64(^uint(0) >> 1)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.GatewayUserID).Update("quota", maxInt-10).Error)
	_, err = SetMetaLearnWalletTopUp(metaLearnTestTopUp(), false)
	require.ErrorIs(t, err, ErrMetaLearnWalletOverflow)
	view, err := GetMetaLearnWallet(metalearnTestUUID)
	require.NoError(t, err)
	require.Equal(t, maxInt-10, view.QuotaBalance)
}
