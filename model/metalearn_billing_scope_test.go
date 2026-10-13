package model

import (
	"errors"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"strings"
	"sync"
	"testing"
)

func TestMetaLearnSwitchAtomicReplaySettlementAndWallet(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.GatewayUserID).Update("quota", 6900000).Error)
	old := MetaLearnPeriodSwitchRequest{MetaLearnPeriodRequest: metalearnTestPeriod(plan.Id), ExpectedQuota: plan.TotalAmount, ScopeKey: strings.Repeat("a", 64)}
	old.EffectiveTimeMS = old.StartTime * 1000
	first, err := SwitchMetaLearnBillingPeriod(old)
	require.NoError(t, err)
	_, err = PreConsumeUserSubscription("before-upgrade", user.GatewayUserID, "mock", 0, 100)
	require.NoError(t, err)
	nextPlan := SubscriptionPlan{Id: plan.Id + 100000, Enabled: true, TotalAmount: 25500000}
	require.NoError(t, DB.Create(&nextPlan).Error)
	next := old
	next.PeriodKey = "upgrade-2"
	next.PlanID = nextPlan.Id
	next.ExpectedQuota = nextPlan.TotalAmount
	next.StartTime++
	next.EffectiveTimeMS += 1000
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := SwitchMetaLearnBillingPeriod(next); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	active, err := GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("scope_write_failure", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*MetaLearnBillingScope); ok {
			tx.AddError(errors.New("injected scope marker failure"))
		}
	}))
	failed := next
	failed.PeriodKey = "marker-failed"
	failed.StartTime++
	failed.EffectiveTimeMS += 1000
	_, err = SwitchMetaLearnBillingPeriod(failed)
	require.Error(t, err)
	require.NoError(t, DB.Callback().Update().Remove("scope_write_failure"))
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, nextPlan.Id, active[0].Subscription.PlanId)
	require.EqualValues(t, 25500000, active[0].Subscription.AmountTotal)
	// A request reserved before the upgrade settles against its original period.
	require.NoError(t, SettleMetaLearnFunding("before-upgrade", user.GatewayUserID, first.SubscriptionID, 150))
	requireMetaLearnUsed(t, first.SubscriptionID, 150)
	requireMetaLearnUsed(t, active[0].Subscription.Id, 0)
	var wallet User
	require.NoError(t, DB.First(&wallet, user.GatewayUserID).Error)
	require.EqualValues(t, 6900000, wallet.Quota)
	_, err = SwitchMetaLearnBillingPeriod(old)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	_, err = SetMetaLearnBillingPeriod(old.MetaLearnPeriodRequest, false)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	// Failed target grant rolls back both the target and retirement of the head.
	invalid := next
	invalid.PeriodKey = "bad-plan"
	invalid.StartTime++
	invalid.EffectiveTimeMS += 1000
	invalid.PlanID = 99999999
	_, err = SwitchMetaLearnBillingPeriod(invalid)
	require.ErrorIs(t, err, ErrMetaLearnPlanNotFound)
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, nextPlan.Id, active[0].Subscription.PlanId)
	wrongQuota := next
	wrongQuota.PeriodKey = "wrong-quota"
	wrongQuota.StartTime++
	wrongQuota.EffectiveTimeMS += 1000
	wrongQuota.ExpectedQuota++
	_, err = SwitchMetaLearnBillingPeriod(wrongQuota)
	require.ErrorIs(t, err, ErrMetaLearnPlanNotEligible)
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	// Neither equal-time ambiguity nor another business/scope may steal a period.
	ambiguous := next
	ambiguous.PeriodKey = "same-time-other-order"
	_, err = SwitchMetaLearnBillingPeriod(ambiguous)
	require.ErrorIs(t, err, ErrMetaLearnPeriodConflict)
	stolen := next
	stolen.ScopeKey = strings.Repeat("b", 64)
	_, err = SwitchMetaLearnBillingPeriod(stolen)
	require.ErrorIs(t, err, ErrMetaLearnPeriodConflict)
	wrong := next
	wrong.BusinessUserID = metalearnOtherUUID
	_, err = SwitchMetaLearnBillingPeriod(wrong)
	require.ErrorIs(t, err, ErrMetaLearnIdentity)
}

func TestMetaLearnSwitchOutOfOrderFutureAndRefund(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	latest := MetaLearnPeriodSwitchRequest{MetaLearnPeriodRequest: metalearnTestPeriod(plan.Id), ExpectedQuota: plan.TotalAmount, ScopeKey: strings.Repeat("c", 64)}
	latest.EffectiveTimeMS = latest.StartTime * 1000
	_, err = SwitchMetaLearnBillingPeriod(latest)
	require.NoError(t, err)
	older := latest
	older.PeriodKey = "late-older"
	older.StartTime--
	older.EffectiveTimeMS -= 1000
	_, err = SwitchMetaLearnBillingPeriod(older)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	future := latest
	future.PeriodKey = "future"
	future.StartTime = GetDBTimestamp() + 3600
	future.EndTime = future.StartTime + 86400
	future.EffectiveTimeMS = future.StartTime * 1000
	_, err = SwitchMetaLearnBillingPeriod(future)
	require.ErrorIs(t, err, ErrMetaLearnPeriodNotStarted)
	future.Revoke = true
	_, err = SwitchMetaLearnBillingPeriod(future)
	require.NoError(t, err)
	active, err := GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	// Refund before grant stays withdrawn while ending the old allowance when
	// the newer paid upgrade is subsequently delivered.
	refunded := latest
	refunded.PeriodKey = "refunded-upgrade"
	refunded.StartTime++
	refunded.EffectiveTimeMS += 1000
	_, err = SetMetaLearnBillingPeriod(refunded.MetaLearnPeriodRequest, true)
	require.NoError(t, err)
	_, err = SwitchMetaLearnBillingPeriod(refunded)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Empty(t, active)
	_, err = SwitchMetaLearnBillingPeriod(latest)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
}

func TestMetaLearnSwitchConcurrentTargetRefundNeverKeepsOldAllowance(t *testing.T) {
	for range 8 {
		plan := setupMetaLearnTest(t)
		_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
		require.NoError(t, err)
		first := MetaLearnPeriodSwitchRequest{MetaLearnPeriodRequest: metalearnTestPeriod(plan.Id), ScopeKey: strings.Repeat("d", 64), ExpectedQuota: plan.TotalAmount}
		first.EffectiveTimeMS = first.StartTime * 1000
		_, err = SwitchMetaLearnBillingPeriod(first)
		require.NoError(t, err)
		next := first
		next.PeriodKey = "refund-race"
		next.StartTime++
		next.EffectiveTimeMS += 1000
		var wg sync.WaitGroup
		var grantErr, refundErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, grantErr = SwitchMetaLearnBillingPeriod(next) }()
		go func() { defer wg.Done(); _, refundErr = SetMetaLearnBillingPeriod(next.MetaLearnPeriodRequest, true) }()
		wg.Wait()
		require.NoError(t, refundErr)
		if grantErr != nil {
			require.ErrorIs(t, grantErr, ErrMetaLearnPeriodRevoked)
		}
		var old MetaLearnBillingPeriod
		require.NoError(t, DB.First(&old, "period_key = ?", first.PeriodKey).Error)
		require.Equal(t, "revoked", old.Status)
		var head MetaLearnBillingScope
		require.NoError(t, DB.First(&head, "scope_key = ?", next.ScopeKey).Error)
		require.Equal(t, next.PeriodKey, head.PeriodKey)
	}
}
