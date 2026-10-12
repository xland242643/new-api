package model

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var metalearnTestPlanID atomic.Int64

func setupMetaLearnTest(t *testing.T) *SubscriptionPlan {
	t.Helper()
	previous := DB
	previousPG, previousSQLite, previousMySQL := common.UsingPostgreSQL, common.UsingSQLite, common.UsingMySQL
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "billing.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	DB = db
	common.UsingPostgreSQL, common.UsingSQLite, common.UsingMySQL = false, true, false
	initCol()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}, &MetaLearnBillingUser{}, &MetaLearnBillingPeriod{}))
	plan := &SubscriptionPlan{Id: 10000 + int(metalearnTestPlanID.Add(1)), Title: "test paid plan", Enabled: true, TotalAmount: 11500000, QuotaResetPeriod: SubscriptionResetNever}
	require.NoError(t, db.Create(plan).Error)
	t.Cleanup(func() {
		InvalidateSubscriptionPlanCache(plan.Id)
		sqlDB.Close()
		DB = previous
		common.UsingPostgreSQL, common.UsingSQLite, common.UsingMySQL = previousPG, previousSQLite, previousMySQL
		initCol()
	})
	return plan
}

const metalearnTestUUID = "724a42ec-a8d6-4c27-9282-993773865b40"
const metalearnOtherUUID = "724a42ec-a8d6-4c27-9282-993773865b41"

func metalearnTestPeriod(planID int) MetaLearnPeriodRequest {
	now := GetDBTimestamp()
	return MetaLearnPeriodRequest{BusinessUserID: metalearnTestUUID, Channel: "apple", PeriodKey: "paid-transaction-1", PlanID: planID, StartTime: now - 10, EndTime: now + 86400}
}

func TestMetaLearnProvisionConcurrentNoReward(t *testing.T) {
	setupMetaLearnTest(t)
	oldReward := common.QuotaForNewUser
	common.QuotaForNewUser = 123456
	t.Cleanup(func() { common.QuotaForNewUser = oldReward })
	const workers = 12
	results := make(chan *MetaLearnUserResult, workers)
	errors := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
			results <- result
			errors <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var first *MetaLearnUserResult
	for result := range results {
		if first == nil {
			first = result
		}
		require.Equal(t, first, result)
	}
	var user User
	require.NoError(t, DB.First(&user, first.GatewayUserID).Error)
	require.Zero(t, user.Quota)
	require.Zero(t, user.AffQuota)
	require.Nil(t, user.AccessToken)
	require.Equal(t, common.RoleCommonUser, user.Role)
	accessUser, accessErr := ValidateAccessToken("Bearer " + first.RelayToken)
	require.NoError(t, accessErr)
	require.Nil(t, accessUser)
	require.Equal(t, "subscription_only", user.GetSetting().BillingPreference)
	require.Equal(t, MetaLearnMonthlyGroup, user.Group)
	var count int64
	require.NoError(t, DB.Model(&User{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.NoError(t, DB.Model(&Token{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	// Dedicated user/token mismatch is rejected, never silently recreated.
	require.NoError(t, DB.Model(&Token{}).Where("user_id = ?", user.Id).Update("user_id", user.Id+1).Error)
	_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.ErrorIs(t, err, ErrMetaLearnIdentity)
}

func TestMetaLearnGrantConcurrentConflictAndImmutableSnapshot(t *testing.T) {
	plan := setupMetaLearnTest(t)
	_, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metalearnTestPeriod(plan.Id)
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	ids := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := SetMetaLearnBillingPeriod(r, false)
			errors <- err
			if res != nil {
				ids <- res.SubscriptionID
			}
		}()
	}
	wg.Wait()
	close(errors)
	close(ids)
	for err := range errors {
		require.NoError(t, err)
	}
	id := 0
	for value := range ids {
		if id == 0 {
			id = value
		}
		require.Equal(t, id, value)
	}
	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	for _, mutate := range []func(*MetaLearnPeriodRequest){func(p *MetaLearnPeriodRequest) { p.BusinessUserID = metalearnOtherUUID }, func(p *MetaLearnPeriodRequest) { p.PlanID++ }, func(p *MetaLearnPeriodRequest) { p.StartTime++ }, func(p *MetaLearnPeriodRequest) { p.EndTime++ }} {
		copy := r
		mutate(&copy)
		_, err := SetMetaLearnBillingPeriod(copy, false)
		require.ErrorIs(t, err, ErrMetaLearnPeriodConflict)
	}
	// Editing a shared plan neither adds quota on replay nor resets paid usage.
	require.NoError(t, DB.Model(&SubscriptionPlan{}).Where("id = ?", plan.Id).Updates(map[string]any{"total_amount": 99999999, "quota_reset_period": SubscriptionResetDaily}).Error)
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", id).Update("amount_used", 4000).Error)
	InvalidateSubscriptionPlanCache(plan.Id)
	res, err := SetMetaLearnBillingPeriod(r, false)
	require.NoError(t, err)
	require.EqualValues(t, 11500000, res.AmountTotal)
	require.EqualValues(t, 4000, res.AmountUsed)
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, id).Error)
	plan.QuotaResetPeriod = SubscriptionResetDaily
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		return maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, r.StartTime+86401)
	}))
	require.EqualValues(t, 4000, sub.AmountUsed)
	view, err := GetMetaLearnSubscriptions(r.BusinessUserID)
	require.NoError(t, err)
	require.EqualValues(t, 1000, view.QuotaPerCredit)
	require.Len(t, view.Subscriptions, 1)
}

func TestMetaLearnFuturePeriodNotSpendable(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metalearnTestPeriod(plan.Id)
	r.StartTime = GetDBTimestamp() + 3600
	r.EndTime = r.StartTime + 86400
	_, err = SetMetaLearnBillingPeriod(r, false)
	require.NoError(t, err)
	active, err := HasActiveUserSubscription(user.GatewayUserID)
	require.NoError(t, err)
	require.False(t, active)
	list, err := GetAllActiveUserSubscriptions(user.GatewayUserID)
	require.NoError(t, err)
	require.Empty(t, list)
	_, err = PreConsumeUserSubscription("future-request", user.GatewayUserID, "mock-model", 0, 1)
	require.Error(t, err)
	// Transition to started time succeeds through the unchanged native funding.
	require.NoError(t, DB.Model(&UserSubscription{}).Where("user_id = ?", user.GatewayUserID).Update("start_time", GetDBTimestamp()-1).Error)
	active, err = HasActiveUserSubscription(user.GatewayUserID)
	require.NoError(t, err)
	require.True(t, active)
	_, err = PreConsumeUserSubscription("started-request", user.GatewayUserID, "mock-model", 0, 1)
	require.NoError(t, err)
}

func TestMetaLearnRevokeBeforeGrantAndRepeated(t *testing.T) {
	plan := setupMetaLearnTest(t)
	r := metalearnTestPeriod(plan.Id)
	first, err := SetMetaLearnBillingPeriod(r, true)
	require.NoError(t, err)
	require.Equal(t, "revoked", first.Status)
	require.Zero(t, first.SubscriptionID)
	second, err := SetMetaLearnBillingPeriod(r, true)
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, err = EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	_, err = SetMetaLearnBillingPeriod(r, false)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	r.BusinessUserID = metalearnOtherUUID
	_, err = SetMetaLearnBillingPeriod(r, true)
	require.ErrorIs(t, err, ErrMetaLearnPeriodConflict)
	var count int64
	require.NoError(t, DB.Model(&UserSubscription{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestMetaLearnGrantRevokeRaceCannotRevive(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metalearnTestPeriod(plan.Id)
	var wg sync.WaitGroup
	var grantErr, revokeErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, grantErr = SetMetaLearnBillingPeriod(r, false) }()
	go func() { defer wg.Done(); _, revokeErr = SetMetaLearnBillingPeriod(r, true) }()
	wg.Wait()
	require.NoError(t, revokeErr)
	if grantErr != nil {
		require.ErrorIs(t, grantErr, ErrMetaLearnPeriodRevoked)
	}
	_, err = SetMetaLearnBillingPeriod(r, false)
	require.ErrorIs(t, err, ErrMetaLearnPeriodRevoked)
	active, err := HasActiveUserSubscription(user.GatewayUserID)
	require.NoError(t, err)
	require.False(t, active)
}

func TestMetaLearnNativeRefundAtomic(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	r := metalearnTestPeriod(plan.Id)
	grant, err := SetMetaLearnBillingPeriod(r, false)
	require.NoError(t, err)
	sqlDB, err := DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	reservation, err := PreConsumeUserSubscription("native-refund", user.GatewayUserID, "mock", 0, 100)
	require.NoError(t, err)
	require.EqualValues(t, 100, reservation.PreConsumed)
	// A single-connection DB reproduces the upstream nested transaction deadlock.
	require.NoError(t, RefundSubscriptionPreConsume("native-refund"))
	require.NoError(t, RefundSubscriptionPreConsume("native-refund"))
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, grant.SubscriptionID).Error)
	require.Zero(t, sub.AmountUsed)
	_, err = PreConsumeUserSubscription("native-refund", user.GatewayUserID, "mock", 0, 100)
	require.Error(t, err)
	// Native settlement and failure refund use the same subscription snapshot.
	_, err = PreConsumeUserSubscription("native-settle", user.GatewayUserID, "mock", 0, 100)
	require.NoError(t, err)
	require.NoError(t, PostConsumeUserSubscriptionDelta(grant.SubscriptionID, 50))
	require.NoError(t, DB.First(&sub, grant.SubscriptionID).Error)
	require.EqualValues(t, 150, sub.AmountUsed)
}

func TestMetaLearnNativeRefundRollbackIfMarkerWriteFails(t *testing.T) {
	plan := setupMetaLearnTest(t)
	user, err := EnsureMetaLearnBillingUser(metalearnTestUUID)
	require.NoError(t, err)
	grant, err := SetMetaLearnBillingPeriod(metalearnTestPeriod(plan.Id), false)
	require.NoError(t, err)
	_, err = PreConsumeUserSubscription("refund-marker-fail", user.GatewayUserID, "mock", 0, 100)
	require.NoError(t, err)
	// Force the idempotency marker write to fail after quota adjustment.
	// Both writes must roll back together, rather than release quota twice on retry.
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("test_refund_marker_error", func(tx *gorm.DB) {
		if _, ok := tx.Statement.Dest.(*SubscriptionPreConsumeRecord); ok {
			tx.AddError(errors.New("injected marker write failure"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove("test_refund_marker_error") })
	require.Error(t, RefundSubscriptionPreConsume("refund-marker-fail"))
	var sub UserSubscription
	require.NoError(t, DB.First(&sub, grant.SubscriptionID).Error)
	require.EqualValues(t, 100, sub.AmountUsed)
	var record SubscriptionPreConsumeRecord
	require.NoError(t, DB.Where("request_id = ?", "refund-marker-fail").First(&record).Error)
	require.Equal(t, "consumed", record.Status)
	require.NoError(t, DB.Callback().Update().Remove("test_refund_marker_error"))
	require.NoError(t, RefundSubscriptionPreConsume("refund-marker-fail"))
	require.NoError(t, DB.First(&sub, grant.SubscriptionID).Error)
	require.Zero(t, sub.AmountUsed)
}
