package model

// This is evidence of native subscription funding only. Token accounting,
// provider success and consume logs are separate and are never inferred here.
import (
	"errors"
	"math"
	"regexp"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	MetaLearnFundingReserved = "reserved"
	MetaLearnRefundPending   = "refund_pending"
	MetaLearnFundingSettled  = "funding_settled"
	MetaLearnFundingRefunded = "funding_refunded"
)

var (
	ErrMetaLearnRequestNotFound     = errors.New("request_not_found")
	ErrMetaLearnFundingConflict     = errors.New("funding_conflict")
	ErrMetaLearnFundingInconsistent = errors.New("funding_inconsistent")
	metalearnRequestID              = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type MetaLearnFundingReceipt struct {
	RequestID       string `gorm:"primaryKey;type:varchar(64)"`
	BusinessUserID  string `gorm:"type:varchar(36);not null;index"`
	GatewayUserID   int
	FundingSource   string `gorm:"type:varchar(16);not null;default:subscription"`
	SubscriptionID  int
	PeriodID        int
	Channel         string `gorm:"type:varchar(32)"`
	PeriodKey       string `gorm:"type:varchar(128)"`
	PlanID          int
	StartTime       int64
	EndTime         int64
	InitialQuota    int64
	ReservedQuota   int64
	SettledQuota    *int64
	FundingStatus   string `gorm:"type:varchar(32)"`
	TokenAccounting string `gorm:"type:varchar(32)"`
	UpdatedAt       int64
}

// Each extra reservation retains an immutable operation id, including rolled
// back operations, so an uncertain retry cannot apply a delta twice.
type MetaLearnFundingReservation struct {
	OperationID   string `gorm:"primaryKey;type:varchar(36)"`
	RequestID     string `gorm:"type:varchar(64);not null;index"`
	Amount        int64
	ExpectedQuota int64
	Status        string `gorm:"type:varchar(16)"` // reserved/rolled_back
}

type MetaLearnFundingResult struct {
	ReceiptScope    string `json:"receipt_scope"`
	FundingSource   string `json:"funding_source"`
	BusinessUserID  string `json:"business_user_id"`
	GatewayUserID   int    `json:"gateway_user_id"`
	RequestID       string `json:"request_id"`
	SubscriptionID  int    `json:"subscription_id"`
	Channel         string `json:"channel"`
	PeriodKey       string `json:"period_key"`
	PlanID          int    `json:"plan_id"`
	QuotaPerCredit  int64  `json:"quota_per_credit"`
	FundingStatus   string `json:"funding_status"`
	ReservedQuota   int64  `json:"reserved_quota"`
	SettledQuota    *int64 `json:"settled_quota"`
	TokenAccounting string `json:"token_accounting"`
	UpdatedAt       int64  `json:"updated_at"`
}

// Dedicated accounts cannot escape the narrow integration by editing a group.
// A user manually put in the reserved group also fails closed until mapped.
func IsMetaLearnBillingUser(userID int) (bool, error) {
	var count int64
	if err := metalearnDB().Model(&MetaLearnBillingUser{}).Where("gateway_user_id = ?", userID).Count(&count).Error; err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	var user User
	if err := metalearnDB().Select("id", "group").First(&user, userID).Error; err != nil {
		return false, err
	}
	return user.Group == MetaLearnMonthlyGroup, nil
}

func createMetaLearnFundingReceiptTx(tx *gorm.DB, record *SubscriptionPreConsumeRecord, sub *UserSubscription) error {
	if sub.Source != MetaLearnSubscriptionSource {
		return nil
	}
	if !metalearnRequestID.MatchString(record.RequestId) {
		return ErrMetaLearnInvalid
	}
	var period MetaLearnBillingPeriod
	if err := tx.Where("subscription_id = ?", sub.Id).First(&period).Error; err != nil {
		return ErrMetaLearnIdentity
	}
	mapping, err := metaLearnMappedUser(tx, period.BusinessUserID)
	if err != nil || mapping.GatewayUserID != record.UserId || sub.UserId != record.UserId || period.Status != "granted" || period.PlanID != sub.PlanId || period.StartTime != sub.StartTime || period.EndTime != sub.EndTime {
		return ErrMetaLearnIdentity
	}
	r := &MetaLearnFundingReceipt{RequestID: record.RequestId, BusinessUserID: period.BusinessUserID, GatewayUserID: record.UserId, SubscriptionID: sub.Id, PeriodID: period.ID, Channel: period.Channel, PeriodKey: period.PeriodKey, PlanID: period.PlanID, StartTime: period.StartTime, EndTime: period.EndTime, InitialQuota: record.PreConsumed, ReservedQuota: record.PreConsumed, FundingStatus: MetaLearnFundingReserved, TokenAccounting: "pending", UpdatedAt: common.GetTimestamp()}
	return tx.Create(r).Error
}

// Lock order is native request record -> receipt -> subscription for every
// mutation, including refund. Period identity is immutable, even when revoked.
func lockedMetaLearnFundingTx(tx *gorm.DB, requestID string, userID, subID int) (*MetaLearnFundingReceipt, *SubscriptionPreConsumeRecord, *UserSubscription, error) {
	var record SubscriptionPreConsumeRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id = ?", requestID).First(&record).Error; err != nil {
		return nil, nil, nil, ErrMetaLearnRequestNotFound
	}
	var receipt MetaLearnFundingReceipt
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id = ?", requestID).First(&receipt).Error; err != nil {
		return nil, nil, nil, ErrMetaLearnRequestNotFound
	}
	if receipt.FundingSource != "subscription" || receipt.GatewayUserID != userID || record.UserId != userID || record.UserSubscriptionId != subID || receipt.SubscriptionID != subID || receipt.InitialQuota != record.PreConsumed {
		return nil, nil, nil, ErrMetaLearnIdentity
	}
	var sub UserSubscription
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&sub, subID).Error; err != nil {
		return nil, nil, nil, ErrMetaLearnIdentity
	}
	var period MetaLearnBillingPeriod
	if err := tx.First(&period, receipt.PeriodID).Error; err != nil {
		return nil, nil, nil, ErrMetaLearnIdentity
	}
	mapping, err := metaLearnMappedUser(tx, receipt.BusinessUserID)
	if err != nil || mapping.GatewayUserID != userID || sub.Source != MetaLearnSubscriptionSource || sub.UserId != userID || sub.PlanId != receipt.PlanID || sub.StartTime != receipt.StartTime || sub.EndTime != receipt.EndTime || period.BusinessUserID != receipt.BusinessUserID || period.SubscriptionID != subID || period.PlanID != receipt.PlanID || period.StartTime != receipt.StartTime || period.EndTime != receipt.EndTime || period.Channel != receipt.Channel || period.PeriodKey != receipt.PeriodKey {
		return nil, nil, nil, ErrMetaLearnIdentity
	}
	if receipt.ReservedQuota < receipt.InitialQuota || receipt.InitialQuota <= 0 || ((receipt.FundingStatus == MetaLearnFundingRefunded) != (record.Status == "refunded")) {
		return nil, nil, nil, ErrMetaLearnFundingInconsistent
	}
	return &receipt, &record, &sub, nil
}

func saveMetaLearnFundingTx(tx *gorm.DB, r *MetaLearnFundingReceipt) error {
	r.UpdatedAt = common.GetTimestamp()
	return tx.Save(r).Error
}

// Reuse the native delta arithmetic, but reject a clamp/inconsistent refund.
// A receipt may assert an exact delta only when that delta was actually applied.
func exactMetaLearnDeltaTx(tx *gorm.DB, sub *UserSubscription, delta int64) error {
	if sub.AmountUsed < 0 || (sub.AmountTotal > 0 && sub.AmountUsed > sub.AmountTotal) {
		return ErrMetaLearnFundingInconsistent
	}
	if (delta > 0 && sub.AmountUsed > math.MaxInt64-delta) || (delta < 0 && sub.AmountUsed < -delta) {
		return ErrMetaLearnFundingInconsistent
	}
	if delta == 0 {
		return nil
	}
	before := sub.AmountUsed
	if err := postConsumeUserSubscriptionDeltaTx(tx, sub.Id, delta); err != nil {
		return err
	}
	var after UserSubscription
	if err := tx.First(&after, sub.Id).Error; err != nil {
		return err
	}
	if after.AmountUsed-before != delta {
		return ErrMetaLearnFundingInconsistent
	}
	return nil
}

func ReserveMetaLearnFunding(requestID string, userID, subID int, operationID string, expected, delta int64) error {
	if !ValidMetaLearnBusinessUserID(operationID) || delta <= 0 || expected <= 0 || expected > math.MaxInt64-delta {
		return ErrMetaLearnInvalid
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, sub, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		var op MetaLearnFundingReservation
		q := tx.Where("operation_id = ?", operationID).Find(&op)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			if op.RequestID != requestID || op.Amount != delta || op.ExpectedQuota != expected {
				return ErrMetaLearnIdentity
			}
			if op.Status != "reserved" || r.FundingStatus != MetaLearnFundingReserved {
				return ErrMetaLearnFundingConflict
			}
			return nil
		}
		if r.FundingStatus != MetaLearnFundingReserved || r.ReservedQuota != expected {
			return ErrMetaLearnFundingConflict
		}
		if err := exactMetaLearnDeltaTx(tx, sub, delta); err != nil {
			return err
		}
		if err := tx.Create(&MetaLearnFundingReservation{OperationID: operationID, RequestID: requestID, Amount: delta, ExpectedQuota: expected, Status: "reserved"}).Error; err != nil {
			return err
		}
		r.ReservedQuota += delta
		return saveMetaLearnFundingTx(tx, r)
	})
}

func RollbackMetaLearnFundingReservation(requestID string, userID, subID int, operationID string) error {
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, sub, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		var op MetaLearnFundingReservation
		if err := tx.Where("operation_id = ?", operationID).First(&op).Error; err != nil {
			return ErrMetaLearnFundingConflict
		}
		if op.RequestID != requestID {
			return ErrMetaLearnIdentity
		}
		if op.Status == "rolled_back" {
			return nil
		}
		if r.FundingStatus != MetaLearnFundingReserved || op.Status != "reserved" || r.ReservedQuota-op.Amount < r.InitialQuota {
			return ErrMetaLearnFundingConflict
		}
		if err := exactMetaLearnDeltaTx(tx, sub, -op.Amount); err != nil {
			return err
		}
		op.Status = "rolled_back"
		if err := tx.Save(&op).Error; err != nil {
			return err
		}
		r.ReservedQuota -= op.Amount
		return saveMetaLearnFundingTx(tx, r)
	})
}

func SettleMetaLearnFunding(requestID string, userID, subID int, actualQuota int64) error {
	if actualQuota < 0 {
		return ErrMetaLearnInvalid
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, sub, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		if r.FundingStatus == MetaLearnFundingSettled {
			if r.SettledQuota == nil || *r.SettledQuota != actualQuota {
				return ErrMetaLearnFundingConflict
			}
			return nil
		}
		if r.FundingStatus != MetaLearnFundingReserved {
			return ErrMetaLearnFundingConflict
		}
		if err := exactMetaLearnDeltaTx(tx, sub, actualQuota-r.ReservedQuota); err != nil {
			return err
		}
		r.FundingStatus, r.SettledQuota = MetaLearnFundingSettled, &actualQuota
		return saveMetaLearnFundingTx(tx, r)
	})
}

func MarkMetaLearnRefundPending(requestID string, userID, subID int) error {
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, _, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		if r.FundingStatus == MetaLearnFundingRefunded || r.FundingStatus == MetaLearnRefundPending {
			return nil
		}
		if r.FundingStatus != MetaLearnFundingReserved {
			return ErrMetaLearnFundingConflict
		}
		r.FundingStatus = MetaLearnRefundPending
		return saveMetaLearnFundingTx(tx, r)
	})
}

func RefundMetaLearnFunding(requestID string, userID, subID int) error {
	// A failed refund leaves durable intent for operator/retry reconciliation.
	if err := MarkMetaLearnRefundPending(requestID, userID, subID); err != nil {
		return err
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, record, sub, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		if r.FundingStatus == MetaLearnFundingRefunded {
			return nil
		}
		if r.FundingStatus != MetaLearnRefundPending {
			return ErrMetaLearnFundingConflict
		}
		if err := exactMetaLearnDeltaTx(tx, sub, -r.ReservedQuota); err != nil {
			return err
		}
		record.Status = "refunded"
		if err := tx.Save(record).Error; err != nil {
			return err
		}
		zero := int64(0)
		r.FundingStatus, r.SettledQuota = MetaLearnFundingRefunded, &zero
		return saveMetaLearnFundingTx(tx, r)
	})
}

// This records what the separate native token API reported. Even reported_ok
// is not proof of Redis/batch durability or overall request success.
func ReportMetaLearnTokenAccounting(requestID string, userID, subID int, status string) error {
	if status != "reported_ok" && status != "reported_error" && status != "queued" && status != "not_required" {
		return ErrMetaLearnInvalid
	}
	if subID == 0 {
		return reportMetaLearnWalletTokenAccounting(requestID, userID, status)
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, _, err := lockedMetaLearnFundingTx(tx, requestID, userID, subID)
		if err != nil {
			return err
		}
		if r.FundingStatus != MetaLearnFundingSettled && r.FundingStatus != MetaLearnFundingRefunded {
			return ErrMetaLearnFundingConflict
		}
		if r.TokenAccounting != "pending" {
			if r.TokenAccounting != status {
				return ErrMetaLearnFundingConflict
			}
			return nil
		}
		r.TokenAccounting = status
		return saveMetaLearnFundingTx(tx, r)
	})
}

func GetMetaLearnFundingReceipt(businessID, requestID string) (*MetaLearnFundingResult, error) {
	if !ValidMetaLearnBusinessUserID(businessID) || !metalearnRequestID.MatchString(requestID) {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnFundingResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		var r MetaLearnFundingReceipt
		if err := tx.Where("request_id = ? AND business_user_id = ?", requestID, businessID).First(&r).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMetaLearnRequestNotFound
			}
			return err
		}
		var verified *MetaLearnFundingReceipt
		var err error
		scope := "subscription_funding"
		if r.FundingSource == "wallet" {
			verified, _, err = lockedMetaLearnWalletFundingTx(tx, requestID, r.GatewayUserID)
			scope = "wallet_funding"
		} else {
			verified, _, _, err = lockedMetaLearnFundingTx(tx, requestID, r.GatewayUserID, r.SubscriptionID)
		}
		if err != nil {
			return err
		}
		result = &MetaLearnFundingResult{ReceiptScope: scope, FundingSource: verified.FundingSource, BusinessUserID: verified.BusinessUserID, GatewayUserID: verified.GatewayUserID, RequestID: verified.RequestID, SubscriptionID: verified.SubscriptionID, Channel: verified.Channel, PeriodKey: verified.PeriodKey, PlanID: verified.PlanID, QuotaPerCredit: MetaLearnQuotaPerCredit, FundingStatus: verified.FundingStatus, ReservedQuota: verified.ReservedQuota, SettledQuota: verified.SettledQuota, TokenAccounting: verified.TokenAccounting, UpdatedAt: verified.UpdatedAt}
		return nil
	})
	return result, err
}
