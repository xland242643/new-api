package service

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func fundMetaLearnSessionWallet(t *testing.T, f *metaLearnSessionFixture) int64 {
	t.Helper()
	_, err := model.SetMetaLearnWalletTopUp(model.MetaLearnTopUpRequest{BusinessUserID: metaLearnSessionUUID, Channel: "huawei", OrderKey: "permanent-package", ProductID: "metalearn_credits_6900", QuotaAmount: 6900000, Currency: "CNY", PaidAmountMinor: 3900}, false)
	require.NoError(t, err)
	return 6900000
}

func sessionWalletBalance(t *testing.T, f *metaLearnSessionFixture) int64 {
	t.Helper()
	view, err := model.GetMetaLearnWallet(metaLearnSessionUUID)
	require.NoError(t, err)
	return view.QuotaBalance
}

func TestMetaLearnWalletSessionSubscriptionFirstAndWalletFallback(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	initial := fundMetaLearnSessionWallet(t, f)
	session := f.session(t, 100)
	require.Equal(t, BillingSourceSubscription, session.funding.Source())
	require.NoError(t, session.Settle(60))
	require.Equal(t, initial, sessionWalletBalance(t, f))
	// A whole request falls back to the native wallet when subscription reserve
	// is insufficient; the request never splits money between funding sources.
	require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("id = ?", f.period.SubscriptionID).Update("amount_used", 100000).Error)
	f.info.RequestId = "wallet-fallback"
	wallet, apiErr := NewBillingSession(f.c, f.info, 100)
	require.Nil(t, apiErr)
	require.NotNil(t, wallet)
	funding, ok := wallet.funding.(*WalletFunding)
	require.True(t, ok)
	require.True(t, funding.metalearn)
	require.Equal(t, initial-100, sessionWalletBalance(t, f))
	require.NoError(t, wallet.Settle(60))
	require.Equal(t, initial-60, sessionWalletBalance(t, f))
	r := f.receipt(t)
	require.Equal(t, "wallet", r.FundingSource)
	require.Equal(t, "wallet_funding", r.ReceiptScope)
	require.EqualValues(t, 60, *r.SettledQuota)
	require.EqualValues(t, 100000, f.used(t))
}

func TestMetaLearnWalletSessionNoSubscriptionZeroAndRefundExtra(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pre, actual int
		refund      bool
	}{{"zero", 100, 0, false}, {"equal", 100, 100, false}, {"overage", 100, 160, false}, {"free", 0, 0, false}, {"refund-extra", 100, 0, true}, {"refund-zero", 0, 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMetaLearnSessionFixture(t)
			_, err := model.SetMetaLearnBillingPeriod(f.period.MetaLearnPeriodRequest, true)
			require.NoError(t, err)
			initial := fundMetaLearnSessionWallet(t, f)
			session, apiErr := NewBillingSession(f.c, f.info, tc.pre)
			require.Nil(t, apiErr)
			require.NotNil(t, session)
			require.Equal(t, BillingSourceWallet, session.funding.Source())
			require.False(t, session.trusted)
			if tc.refund {
				if tc.pre > 0 {
					require.NoError(t, session.Reserve(180))
					require.Equal(t, initial-180, sessionWalletBalance(t, f))
				}
				session.Refund(f.c)
				f.waitRefund(t)
				session.Refund(f.c)
				require.Equal(t, initial, sessionWalletBalance(t, f))
				require.Equal(t, model.MetaLearnFundingRefunded, f.receipt(t).FundingStatus)
			} else {
				require.NoError(t, session.Settle(tc.actual))
				require.NoError(t, session.Settle(tc.actual))
				require.ErrorIs(t, session.Settle(tc.actual+1), model.ErrMetaLearnFundingConflict)
				require.Equal(t, initial-int64(tc.actual), sessionWalletBalance(t, f))
				require.Equal(t, model.MetaLearnFundingSettled, f.receipt(t).FundingStatus)
			}
			require.Zero(t, f.info.SubscriptionId)
			require.Zero(t, f.info.SubscriptionPreConsumed)
		})
	}
}

func TestMetaLearnWalletSessionTokenFailureAndReserveRollback(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	_, err := model.SetMetaLearnBillingPeriod(f.period.MetaLearnPeriodRequest, true)
	require.NoError(t, err)
	initial := fundMetaLearnSessionWallet(t, f)
	session, apiErr := NewBillingSession(f.c, f.info, 100)
	require.Nil(t, apiErr)
	callback := "wallet_token_failure"
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "tokens" {
			tx.AddError(errors.New("injected token failure"))
		}
	}))
	require.Error(t, session.Reserve(180))
	require.Equal(t, initial-100, sessionWalletBalance(t, f))
	require.Equal(t, 100, session.GetPreConsumedQuota())
	require.Error(t, session.Settle(160))
	require.Equal(t, initial-160, sessionWalletBalance(t, f))
	require.Equal(t, model.MetaLearnFundingSettled, f.receipt(t).FundingStatus)
	require.Equal(t, "reported_error", f.receipt(t).TokenAccounting)
	require.NoError(t, f.db.Callback().Update().Remove(callback))
	require.NoError(t, session.Settle(160))
	session.Refund(f.c)
	require.Equal(t, initial-160, sessionWalletBalance(t, f))
}
