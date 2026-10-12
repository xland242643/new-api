package service

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var metaLearnSessionPlanSequence atomic.Int64

const metaLearnSessionUUID = "724a42ec-a8d6-4c27-9282-993773865b40"

type metaLearnSessionFixture struct {
	db     *gorm.DB
	c      *gin.Context
	info   *relaycommon.RelayInfo
	period *model.MetaLearnPeriodResult
}

func newMetaLearnSessionFixture(t *testing.T) *metaLearnSessionFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	priorDB := model.DB
	priorRedis, priorBatch := common.RedisEnabled, common.BatchUpdateEnabled
	priorSQLite, priorMySQL, priorPG := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = true, false, false
	// Use the native startup to initialize its dialect-dependent quoted column
	// names. A raw gorm.Open fixture alone leaves commonKeyCol unset. This
	// isolated initialization skips migrations and never reads a real DSN.
	t.Setenv("SQL_DSN", "local")
	priorMaster, priorPath := common.IsMasterNode, common.SQLitePath
	common.IsMasterNode, common.SQLitePath = false, ":memory:"
	err := model.InitDB()
	common.IsMasterNode, common.SQLitePath = priorMaster, priorPath
	require.NoError(t, err)
	initialDB, err := model.DB.DB()
	require.NoError(t, err)
	require.NoError(t, initialDB.Close())
	db, err := gorm.Open(sqlite.Open("file:"+filepath.Join(t.TempDir(), "session.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.MetaLearnBillingUser{}, &model.MetaLearnBillingPeriod{}, &model.MetaLearnFundingReceipt{}, &model.MetaLearnFundingReservation{}, &model.TopUp{}, &model.MetaLearnWalletTopUp{}))
	planID := 49000 + int(metaLearnSessionPlanSequence.Add(1))
	require.NoError(t, db.Create(&model.SubscriptionPlan{Id: planID, Title: "paid monthly", Enabled: true, TotalAmount: 100000, QuotaResetPeriod: model.SubscriptionResetNever}).Error)
	t.Cleanup(func() {
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 3*time.Second, time.Millisecond)
		model.InvalidateSubscriptionPlanCache(planID)
		_ = sqlDB.Close()
		model.DB = priorDB
		common.RedisEnabled, common.BatchUpdateEnabled = priorRedis, priorBatch
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = priorSQLite, priorMySQL, priorPG
	})
	user, err := model.EnsureMetaLearnBillingUser(metaLearnSessionUUID)
	require.NoError(t, err)
	now := model.GetDBTimestamp()
	period, err := model.SetMetaLearnBillingPeriod(model.MetaLearnPeriodRequest{BusinessUserID: metaLearnSessionUUID, Channel: "web", PeriodKey: "paid-period", PlanID: planID, StartTime: now - 10, EndTime: now + 86400}, false)
	require.NoError(t, err)
	var mapping model.MetaLearnBillingUser
	require.NoError(t, db.First(&mapping, "business_user_id = ?", metaLearnSessionUUID).Error)
	return &metaLearnSessionFixture{db: db, c: metaLearnGuardContext(http.MethodPost, "/v1/chat/completions"), period: period, info: &relaycommon.RelayInfo{
		RequestId: "session-request", UserId: user.GatewayUserID, UserGroup: model.MetaLearnMonthlyGroup,
		TokenId: mapping.RelayTokenID, TokenKey: strings.TrimPrefix(user.RelayToken, "sk-"), TokenUnlimited: true,
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		OriginModelName: "qwen-native-pricing", UserSetting: dto.UserSetting{BillingPreference: "subscription_first"},
	}}
}

func (f *metaLearnSessionFixture) session(t *testing.T, quota int) *BillingSession {
	t.Helper()
	session, err := NewBillingSession(f.c, f.info, quota)
	require.Nil(t, err)
	require.NotNil(t, session)
	funding, ok := session.funding.(*SubscriptionFunding)
	require.True(t, ok)
	require.True(t, funding.metalearn)
	f.info.Billing = session
	return session
}

func (f *metaLearnSessionFixture) receipt(t *testing.T) *model.MetaLearnFundingResult {
	t.Helper()
	receipt, err := model.GetMetaLearnFundingReceipt(metaLearnSessionUUID, f.info.RequestId)
	require.NoError(t, err)
	return receipt
}

func (f *metaLearnSessionFixture) used(t *testing.T) int64 {
	t.Helper()
	var sub model.UserSubscription
	require.NoError(t, f.db.First(&sub, f.period.SubscriptionID).Error)
	return sub.AmountUsed
}

func (f *metaLearnSessionFixture) tokenUsed(t *testing.T) int {
	t.Helper()
	var token model.Token
	require.NoError(t, f.db.First(&token, f.info.TokenId).Error)
	return token.UsedQuota
}

func (f *metaLearnSessionFixture) waitRefund(t *testing.T) {
	t.Helper()
	// Token reporting is the last database operation in the async refund. Wait
	// for it before restoring the shared DB; a funding state alone is too early.
	require.Eventually(t, func() bool {
		var receipt model.MetaLearnFundingReceipt
		err := f.db.First(&receipt, "request_id = ?", f.info.RequestId).Error
		return err == nil && receipt.FundingStatus == model.MetaLearnFundingRefunded && receipt.TokenAccounting != "pending"
	}, 3*time.Second, 10*time.Millisecond)
}

func TestMetaLearnBillingSessionZeroEqualAndOverageAreTerminal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pre, actual int
	}{
		{"actual_zero", 100, 0},
		{"equal_delta", 100, 100},
		{"overage", 100, 160},
		{"free_zero", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMetaLearnSessionFixture(t)
			session := f.session(t, tc.pre)
			pre := f.receipt(t)
			require.Equal(t, model.MetaLearnFundingReserved, pre.FundingStatus)
			require.Nil(t, pre.SettledQuota)
			require.Greater(t, pre.ReservedQuota, int64(0))
			require.NoError(t, session.Settle(tc.actual))
			result := f.receipt(t)
			require.Equal(t, model.MetaLearnFundingSettled, result.FundingStatus)
			require.NotNil(t, result.SettledQuota)
			require.Equal(t, int64(tc.actual), *result.SettledQuota)
			require.Equal(t, int64(tc.actual), f.used(t))
			require.Equal(t, tc.actual, f.tokenUsed(t))
			require.NoError(t, session.Settle(tc.actual))
			require.Equal(t, int64(tc.actual), f.used(t))
			require.Equal(t, tc.actual, f.tokenUsed(t))
			require.ErrorIs(t, session.Settle(tc.actual+1), model.ErrMetaLearnFundingConflict)
			require.False(t, session.NeedsRefund())
			session.Refund(f.c)
			require.Equal(t, model.MetaLearnFundingSettled, f.receipt(t).FundingStatus)
		})
	}
}

func TestMetaLearnBillingSessionRejectsAlternateFundingAdmission(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	f.info.UserSetting.BillingPreference = "wallet_first"
	session, err := NewBillingSession(f.c, f.info, 100)
	require.Nil(t, session)
	require.ErrorIs(t, err, ErrMetaLearnRelayUnsupported)
	f.info.UserSetting.BillingPreference = "subscription_first"
	f.c = metaLearnGuardContext(http.MethodPost, "/pg/chat/completions")
	session, err = NewBillingSession(f.c, f.info, 100)
	require.Nil(t, session)
	require.ErrorIs(t, err, ErrMetaLearnRelayUnsupported)
	require.Equal(t, int64(0), f.used(t))
	require.Equal(t, 0, f.tokenUsed(t))
}

func TestMetaLearnBillingSessionTokenErrorDoesNotUndoFunding(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	session := f.session(t, 100)
	failure := errors.New("injected_token_update_failure")
	var attempts atomic.Int64
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("metalearn_test_token_failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "tokens" {
			attempts.Add(1)
			tx.AddError(failure)
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Update().Remove("metalearn_test_token_failure") })
	require.ErrorIs(t, session.Settle(160), failure)
	result := f.receipt(t)
	require.Equal(t, model.MetaLearnFundingSettled, result.FundingStatus)
	require.Equal(t, "reported_error", result.TokenAccounting)
	require.Equal(t, int64(160), *result.SettledQuota)
	require.Equal(t, int64(160), f.used(t))
	require.Equal(t, 100, f.tokenUsed(t))
	require.NoError(t, session.Settle(160))
	require.Equal(t, int64(1), attempts.Load())
	session.Refund(f.c)
	require.Equal(t, int64(160), f.used(t))
	require.False(t, session.NeedsRefund())
}

func TestMetaLearnBillingSessionQueuedTokenIsSeparateFromFunding(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	session := f.session(t, 100)
	common.BatchUpdateEnabled = true
	// Balance the isolated native in-memory batch entry after the assertion so
	// it cannot affect a later fixture if its updater is explicitly exercised.
	t.Cleanup(func() {
		common.BatchUpdateEnabled = true
		_ = model.IncreaseTokenQuota(f.info.TokenId, f.info.TokenKey, 60)
		common.BatchUpdateEnabled = false
	})
	require.NoError(t, session.Settle(160))
	result := f.receipt(t)
	require.Equal(t, model.MetaLearnFundingSettled, result.FundingStatus)
	require.Equal(t, "queued", result.TokenAccounting)
	require.Equal(t, int64(160), f.used(t))
	require.Equal(t, 100, f.tokenUsed(t))
	common.BatchUpdateEnabled = false
	common.RedisEnabled = true
	// Classification only; no Redis-backed API is invoked by this check.
	require.Equal(t, "queued", metaLearnTokenReport(nil, true))
	common.RedisEnabled = false
}

func TestMetaLearnBillingSessionReserveFailureRollsBackAndExtraRefunds(t *testing.T) {
	t.Run("token_failure_rolls_back", func(t *testing.T) {
		f := newMetaLearnSessionFixture(t)
		session := f.session(t, 100)
		failure := errors.New("injected_reserve_token_failure")
		require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("metalearn_reserve_token_failure", func(tx *gorm.DB) {
			if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "tokens" {
				tx.AddError(failure)
			}
		}))
		require.ErrorIs(t, session.Reserve(160), failure)
		require.NoError(t, f.db.Callback().Update().Remove("metalearn_reserve_token_failure"))
		require.Equal(t, int64(100), f.used(t))
		require.Equal(t, int64(100), f.receipt(t).ReservedQuota)
		require.Equal(t, 100, session.GetPreConsumedQuota())
		var op model.MetaLearnFundingReservation
		require.NoError(t, f.db.First(&op).Error)
		require.Equal(t, "rolled_back", op.Status)
		session.Refund(f.c)
		f.waitRefund(t)
		require.Equal(t, int64(0), f.used(t))
		require.Equal(t, 0, f.tokenUsed(t))
	})
	t.Run("extra_reservation_refunded_once", func(t *testing.T) {
		f := newMetaLearnSessionFixture(t)
		session := f.session(t, 100)
		require.NoError(t, session.Reserve(160))
		require.NoError(t, session.Reserve(160))
		require.Equal(t, int64(160), f.used(t))
		require.Equal(t, 160, f.tokenUsed(t))
		require.Equal(t, int64(160), f.receipt(t).ReservedQuota)
		session.Refund(f.c)
		session.Refund(f.c)
		f.waitRefund(t)
		result := f.receipt(t)
		require.Equal(t, int64(0), *result.SettledQuota)
		require.Equal(t, int64(0), f.used(t))
		require.Equal(t, 0, f.tokenUsed(t))
		session.Refund(f.c)
		require.Equal(t, 0, f.tokenUsed(t))
		require.ErrorIs(t, session.Settle(0), model.ErrMetaLearnFundingConflict)
	})
	t.Run("failed_rollback_retains_durable_extra", func(t *testing.T) {
		f := newMetaLearnSessionFixture(t)
		session := f.session(t, 100)
		failure := errors.New("injected_reserve_rollback_failure")
		var fundingWrites atomic.Int64
		require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("metalearn_reserve_rollback_failure", func(tx *gorm.DB) {
			if tx.Statement.Schema == nil {
				return
			}
			if tx.Statement.Schema.Table == "tokens" {
				tx.AddError(failure)
			}
			if tx.Statement.Schema.Table == "user_subscriptions" && fundingWrites.Add(1) == 2 {
				tx.AddError(failure)
			}
		}))
		require.ErrorIs(t, session.Reserve(160), model.ErrMetaLearnFundingInconsistent)
		require.NoError(t, f.db.Callback().Update().Remove("metalearn_reserve_rollback_failure"))
		require.Equal(t, int64(160), f.used(t))
		require.Equal(t, int64(160), f.receipt(t).ReservedQuota)
		require.Equal(t, 100, session.GetPreConsumedQuota())
		require.Equal(t, 100, f.tokenUsed(t))
		// Refund uses the durable funding reservation, not the stale in-memory
		// successful-reserve counter. Token refund covers only its native debit.
		session.Refund(f.c)
		f.waitRefund(t)
		require.Equal(t, int64(0), f.used(t))
		require.Equal(t, 0, f.tokenUsed(t))
	})
}

func TestMetaLearnBillingSessionInconsistentRefundRemainsPendingAndRetries(t *testing.T) {
	f := newMetaLearnSessionFixture(t)
	session := f.session(t, 100)
	// A corrupted native amount must not be clamped to zero and then presented
	// as a full 100-quota refund. Intent and the native debit remain unresolved.
	require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("id = ?", f.period.SubscriptionID).Update("amount_used", 10).Error)
	session.Refund(f.c)
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 3*time.Second, time.Millisecond)
	result := f.receipt(t)
	require.Equal(t, model.MetaLearnRefundPending, result.FundingStatus)
	require.Nil(t, result.SettledQuota)
	require.Equal(t, "pending", result.TokenAccounting)
	require.Equal(t, int64(10), f.used(t))
	require.Equal(t, 100, f.tokenUsed(t))
	require.True(t, session.NeedsRefund())
	// After the fixture's native inconsistency is repaired, retry uses the same
	// durable request anchor and returns the original reservation exactly once.
	require.NoError(t, f.db.Model(&model.UserSubscription{}).Where("id = ?", f.period.SubscriptionID).Update("amount_used", 100).Error)
	session.Refund(f.c)
	f.waitRefund(t)
	require.Equal(t, int64(0), f.used(t))
	require.Equal(t, 0, f.tokenUsed(t))
}
