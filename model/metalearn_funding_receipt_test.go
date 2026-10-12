package model

import (
	"errors"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupMetaLearnFunding(t *testing.T, requestID string, amount int64) (*MetaLearnUserResult, *MetaLearnPeriodResult) {
	t.Helper()
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	period, err := SetMetaLearnBillingPeriod(metalearnTestPeriod(plan.Id), false)
	require.NoError(t, err)
	_, err = PreConsumeUserSubscription(requestID, user.GatewayUserID, "mock", 0, amount)
	require.NoError(t, err)
	return user, period
}

func requireMetaLearnUsed(t *testing.T, id int, used int64) {
	t.Helper()
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, id).Error)
	require.Equal(t, used, sub.AmountUsed)
}

func TestMetaLearnFundingNativeTerminalAndExactMoney(t *testing.T) {
	for _, actual := range []int64{0, 100, 450} {
		t.Run(common.GetRandomString(8), func(t *testing.T) {
			user, period := setupMetaLearnFunding(t, "settle", 100)
			r, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "settle")
			require.NoError(t, err)
			require.Equal(t, MetaLearnFundingReserved, r.FundingStatus)
			require.Nil(t, r.SettledQuota)
			require.EqualValues(t, 1000, r.QuotaPerCredit)
			require.NoError(t, SettleMetaLearnFunding("settle", user.GatewayUserID, period.SubscriptionID, actual))
			require.NoError(t, SettleMetaLearnFunding("settle", user.GatewayUserID, period.SubscriptionID, actual))
			requireMetaLearnUsed(t, period.SubscriptionID, actual)
			r, err = GetMetaLearnFundingReceipt(metalearnTestUUID, "settle")
			require.NoError(t, err)
			require.Equal(t, MetaLearnFundingSettled, r.FundingStatus)
			require.NotNil(t, r.SettledQuota)
			require.Equal(t, actual, *r.SettledQuota)
			require.Equal(t, "pending", r.TokenAccounting)
			require.ErrorIs(t, SettleMetaLearnFunding("settle", user.GatewayUserID, period.SubscriptionID, actual+1), ErrMetaLearnFundingConflict)
			require.ErrorIs(t, RefundMetaLearnFunding("settle", user.GatewayUserID, period.SubscriptionID), ErrMetaLearnFundingConflict)
			require.ErrorIs(t, PostConsumeUserSubscriptionDelta(period.SubscriptionID, 0), ErrMetaLearnFundingConflict)
			require.ErrorIs(t, PostConsumeUserSubscriptionDelta(period.SubscriptionID, 1), ErrMetaLearnFundingConflict)
		})
	}
}

func TestMetaLearnFundingReserveRollbackAndFullRefund(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "reserve", 100)
	op1, op2 := uuid.NewString(), uuid.NewString()
	require.NoError(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op1, 100, 40))
	require.NoError(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op1, 100, 40))
	requireMetaLearnUsed(t, period.SubscriptionID, 140)
	require.ErrorIs(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op1, 100, 41), ErrMetaLearnIdentity)
	require.ErrorIs(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op1, 99, 40), ErrMetaLearnIdentity)
	require.NoError(t, RollbackMetaLearnFundingReservation("reserve", user.GatewayUserID, period.SubscriptionID, op1))
	require.NoError(t, RollbackMetaLearnFundingReservation("reserve", user.GatewayUserID, period.SubscriptionID, op1))
	requireMetaLearnUsed(t, period.SubscriptionID, 100)
	require.ErrorIs(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op1, 100, 40), ErrMetaLearnFundingConflict)
	require.NoError(t, ReserveMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, op2, 100, 75))
	require.NoError(t, MarkMetaLearnRefundPending("reserve", user.GatewayUserID, period.SubscriptionID))
	r, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "reserve")
	require.NoError(t, err)
	require.Equal(t, MetaLearnRefundPending, r.FundingStatus)
	require.Nil(t, r.SettledQuota)
	require.NoError(t, RefundMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID))
	require.NoError(t, RefundMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID))
	requireMetaLearnUsed(t, period.SubscriptionID, 0)
	r, err = GetMetaLearnFundingReceipt(metalearnTestUUID, "reserve")
	require.NoError(t, err)
	require.Equal(t, MetaLearnFundingRefunded, r.FundingStatus)
	require.EqualValues(t, 175, r.ReservedQuota)
	require.NotNil(t, r.SettledQuota)
	require.Zero(t, *r.SettledQuota)
	require.ErrorIs(t, SettleMetaLearnFunding("reserve", user.GatewayUserID, period.SubscriptionID, 0), ErrMetaLearnFundingConflict)
}

func TestMetaLearnFundingIdentityCannotSwap(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "identity", 100)
	require.ErrorIs(t, SettleMetaLearnFunding("identity", user.GatewayUserID+1, period.SubscriptionID, 100), ErrMetaLearnIdentity)
	require.ErrorIs(t, SettleMetaLearnFunding("identity", user.GatewayUserID, period.SubscriptionID+1, 100), ErrMetaLearnIdentity)
	_, err := GetMetaLearnFundingReceipt(metalearnOtherUUID, "identity")
	require.ErrorIs(t, err, ErrMetaLearnRequestNotFound)
	_, err = GetMetaLearnFundingReceipt(metalearnTestUUID, "missing")
	require.ErrorIs(t, err, ErrMetaLearnRequestNotFound)
	_, err = PreConsumeUserSubscription("identity", user.GatewayUserID, "mock", 0, 101)
	require.ErrorIs(t, err, ErrMetaLearnFundingConflict)
	for _, change := range []map[string]any{{"plan_id": period.PlanID + 1}, {"period_key": "other-period"}, {"business_user_id": metalearnOtherUUID}, {"start_time": period.StartTime + 1}} {
		var original MetaLearnFundingReceipt
		require.NoError(t, DB.Where("request_id = ?", "identity").First(&original).Error)
		require.NoError(t, DB.Model(&MetaLearnFundingReceipt{}).Where("request_id = ?", "identity").Updates(change).Error)
		require.ErrorIs(t, SettleMetaLearnFunding("identity", user.GatewayUserID, period.SubscriptionID, 100), ErrMetaLearnIdentity)
		require.NoError(t, DB.Save(&original).Error)
	}
	requireMetaLearnUsed(t, period.SubscriptionID, 100)
}

func TestMetaLearnFundingReceiptWriteFailureRollsBackMoney(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "write-fail", 100)
	const callback = "receipt_write_fail"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*MetaLearnFundingReceipt); ok {
			tx.AddError(errors.New("injected receipt write failure"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove(callback) })
	require.Error(t, SettleMetaLearnFunding("write-fail", user.GatewayUserID, period.SubscriptionID, 40))
	require.Error(t, ReserveMetaLearnFunding("write-fail", user.GatewayUserID, period.SubscriptionID, uuid.NewString(), 100, 80))
	requireMetaLearnUsed(t, period.SubscriptionID, 100)
	var operations int64
	require.NoError(t, DB.Model(&MetaLearnFundingReservation{}).Count(&operations).Error)
	require.Zero(t, operations)
	require.NoError(t, DB.Callback().Update().Remove(callback))
	require.NoError(t, MarkMetaLearnRefundPending("write-fail", user.GatewayUserID, period.SubscriptionID))
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*MetaLearnFundingReceipt); ok {
			tx.AddError(errors.New("injected receipt write failure"))
		}
	}))
	require.Error(t, RefundMetaLearnFunding("write-fail", user.GatewayUserID, period.SubscriptionID))
	requireMetaLearnUsed(t, period.SubscriptionID, 100)
	var anchor SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", "write-fail").First(&anchor).Error)
	require.Equal(t, "consumed", anchor.Status)
	r, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "write-fail")
	require.NoError(t, err)
	require.Equal(t, MetaLearnRefundPending, r.FundingStatus)
	require.Nil(t, r.SettledQuota)
	require.NoError(t, DB.Callback().Update().Remove(callback))
	require.NoError(t, RefundMetaLearnFunding("write-fail", user.GatewayUserID, period.SubscriptionID))
	requireMetaLearnUsed(t, period.SubscriptionID, 0)
}

func TestMetaLearnFundingPreConsumeReceiptFailureIsAtomic(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	period, err := SetMetaLearnBillingPeriod(metalearnTestPeriod(plan.Id), false)
	require.NoError(t, err)
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register("receipt_create_fail", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*MetaLearnFundingReceipt); ok {
			tx.AddError(errors.New("injected receipt create failure"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Create().Remove("receipt_create_fail") })
	_, err = PreConsumeUserSubscription("create-fail", user.GatewayUserID, "mock", 0, 100)
	require.Error(t, err)
	requireMetaLearnUsed(t, period.SubscriptionID, 0)
	var count int64
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).Where("request_id = ?", "create-fail").Count(&count).Error)
	require.Zero(t, count)
}

func TestMetaLearnFundingRefundRejectsInconsistentClamp(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "clamp", 100)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", period.SubscriptionID).Update("amount_used", 30).Error)
	require.ErrorIs(t, RefundMetaLearnFunding("clamp", user.GatewayUserID, period.SubscriptionID), ErrMetaLearnFundingInconsistent)
	requireMetaLearnUsed(t, period.SubscriptionID, 30)
	r, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "clamp")
	require.NoError(t, err)
	require.Equal(t, MetaLearnRefundPending, r.FundingStatus)
	require.Nil(t, r.SettledQuota)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", "clamp").First(&record).Error)
	require.Equal(t, "consumed", record.Status)
}

func TestMetaLearnFundingConcurrentSettleRefundAndPreConsume(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "race", 100)
	var wg sync.WaitGroup
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				errors <- SettleMetaLearnFunding("race", user.GatewayUserID, period.SubscriptionID, 150)
			} else {
				errors <- RefundMetaLearnFunding("race", user.GatewayUserID, period.SubscriptionID)
			}
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			require.ErrorIs(t, err, ErrMetaLearnFundingConflict)
		}
	}
	r, err := GetMetaLearnFundingReceipt(metalearnTestUUID, "race")
	require.NoError(t, err)
	if r.FundingStatus == MetaLearnFundingSettled {
		requireMetaLearnUsed(t, period.SubscriptionID, 150)
	} else {
		require.Equal(t, MetaLearnFundingRefunded, r.FundingStatus)
		requireMetaLearnUsed(t, period.SubscriptionID, 0)
	}
	preErrors := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := PreConsumeUserSubscription("pre-race", user.GatewayUserID, "mock", 0, 100)
			preErrors <- err
		}()
	}
	wg.Wait()
	close(preErrors)
	for err := range preErrors {
		require.NoError(t, err)
	}
	var count int64
	require.NoError(t, DB.Model(&MetaLearnFundingReceipt{}).Where("request_id = ?", "pre-race").Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestMetaLearnFundingCleanupPreservesAnchorsAndReservations(t *testing.T) {
	user, period := setupMetaLearnFunding(t, "cleanup", 100)
	op := uuid.NewString()
	require.NoError(t, ReserveMetaLearnFunding("cleanup", user.GatewayUserID, period.SubscriptionID, op, 100, 20))
	old := GetDBTimestamp() - 86400
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).Where("request_id = ?", "cleanup").UpdateColumn("updated_at", old).Error)
	deleted, err := CleanupSubscriptionPreConsumeRecords(1)
	require.NoError(t, err)
	require.Zero(t, deleted)
	require.NoError(t, RefundMetaLearnFunding("cleanup", user.GatewayUserID, period.SubscriptionID))
	require.NoError(t, DB.Model(&SubscriptionPreConsumeRecord{}).Where("request_id = ?", "cleanup").UpdateColumn("updated_at", old).Error)
	deleted, err = CleanupSubscriptionPreConsumeRecords(1)
	require.NoError(t, err)
	require.Zero(t, deleted)
	var operations int64
	require.NoError(t, DB.Model(&MetaLearnFundingReservation{}).Where("operation_id = ?", op).Count(&operations).Error)
	require.EqualValues(t, 1, operations)
	require.NoError(t, RefundMetaLearnFunding("cleanup", user.GatewayUserID, period.SubscriptionID))
}
