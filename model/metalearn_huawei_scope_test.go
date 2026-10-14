package model

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestMetaLearnHuaweiConvertedAllowanceAtomicSnapshotReplay(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	old := MetaLearnPeriodSwitchRequest{MetaLearnPeriodRequest: metalearnTestPeriod(plan.Id), ScopeKey: strings.Repeat("e", 64), ExpectedQuota: plan.TotalAmount}
	old.Channel = "huawei"
	old.EffectiveTimeMS = old.StartTime * 1000
	// Existing unchanged unscoped paid period adopts scope without resetting usage.
	first, err := SetMetaLearnBillingPeriod(old.MetaLearnPeriodRequest, false)
	require.NoError(t, err)
	_, err = PreConsumeUserSubscription("hw-old-reserve", user.GatewayUserID, "mock", 0, 100)
	require.NoError(t, err)
	_, err = SwitchMetaLearnBillingPeriod(old)
	require.NoError(t, err)
	requireMetaLearnUsed(t, first.SubscriptionID, 100)
	next := old
	next.PeriodKey = "huawei-upgrade"
	next.StartTime++
	next.EffectiveTimeMS += 1000
	next.BaseDurationMS = 300000
	next.PaidDurationMS = 350000
	next.EndTime = (next.EffectiveTimeMS + next.PaidDurationMS) / 1000
	next.ExpectedQuota = plan.TotalAmount * 7 / 6
	result, err := SwitchMetaLearnBillingPeriod(next)
	require.NoError(t, err)
	require.Equal(t, next.ExpectedQuota, result.AmountTotal)
	require.NoError(t, SettleMetaLearnFunding("hw-old-reserve", user.GatewayUserID, first.SubscriptionID, 150))
	requireMetaLearnUsed(t, first.SubscriptionID, 150)
	again, err := SwitchMetaLearnBillingPeriod(next)
	require.NoError(t, err)
	require.Equal(t, result.SubscriptionID, again.SubscriptionID)
	active, err := GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	bad := next
	bad.BaseDurationMS++
	_, err = SwitchMetaLearnBillingPeriod(bad)
	require.ErrorIs(t, err, ErrMetaLearnPeriodConflict)
	bad = next
	bad.Channel = "revenuecat"
	_, err = SwitchMetaLearnBillingPeriod(bad)
	require.ErrorIs(t, err, ErrMetaLearnInvalid)
	bad = next
	bad.PeriodKey = "bad-huawei-quota"
	bad.StartTime++
	bad.EffectiveTimeMS += 1000
	bad.EndTime++
	bad.ExpectedQuota++
	_, err = SwitchMetaLearnBillingPeriod(bad)
	require.ErrorIs(t, err, ErrMetaLearnPlanNotEligible)
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Update("total_amount", plan.TotalAmount+100).Error)
	again, err = SwitchMetaLearnBillingPeriod(next)
	require.NoError(t, err)
	require.Equal(t, result.AmountTotal, again.AmountTotal)
	next.Revoke = true
	_, err = SwitchMetaLearnBillingPeriod(next)
	require.NoError(t, err)
	active, err = GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Empty(t, active)
	_, err = SwitchMetaLearnBillingPeriod(old)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
}
