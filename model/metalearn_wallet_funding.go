package model

import (
	"errors"
	"math"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Wallet requests use the existing receipt/reservation table, but have no paid
// subscription period. Their lock order is receipt -> native user wallet.
func lockedMetaLearnWalletFundingTx(tx *gorm.DB, requestID string, userID int) (*MetaLearnFundingReceipt, *User, error) {
	var r MetaLearnFundingReceipt
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id = ?", requestID).First(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, ErrMetaLearnRequestNotFound
		}
		return nil, nil, err
	}
	if r.FundingSource != "wallet" || r.GatewayUserID != userID || r.SubscriptionID != 0 || r.PeriodID != 0 || r.PlanID != 0 || r.Channel != "" || r.PeriodKey != "" || r.StartTime != 0 || r.EndTime != 0 {
		return nil, nil, ErrMetaLearnIdentity
	}
	if r.InitialQuota < 0 || r.ReservedQuota < r.InitialQuota {
		return nil, nil, ErrMetaLearnFundingInconsistent
	}
	user, err := lockedMetaLearnWalletUserTx(tx, r.BusinessUserID, userID)
	return &r, user, err
}

func PreConsumeMetaLearnWallet(requestID string, userID int, amount int64) error {
	if !metalearnRequestID.MatchString(requestID) || !validMetaLearnWalletAmount(amount) {
		return ErrMetaLearnInvalid
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		var r MetaLearnFundingReceipt
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id = ?", requestID).Find(&r)
		if q.Error != nil {
			return q.Error
		}
		if q.RowsAffected > 0 {
			verified, _, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
			if err != nil {
				return err
			}
			if verified.InitialQuota != amount {
				return ErrMetaLearnIdentity
			}
			if verified.FundingStatus != MetaLearnFundingReserved {
				return ErrMetaLearnFundingConflict
			}
			return nil
		}
		var mapping MetaLearnBillingUser
		if err := tx.Where("gateway_user_id = ?", userID).First(&mapping).Error; err != nil {
			return ErrMetaLearnIdentity
		}
		user, err := lockedMetaLearnWalletUserTx(tx, mapping.BusinessUserID, userID)
		if err != nil {
			return err
		}
		// Every concurrent request checks the durable wallet under the same lock.
		if int64(user.Quota) < amount || user.Quota <= 0 {
			return ErrMetaLearnWalletInsufficient
		}
		if err := exactMetaLearnWalletDeltaTx(tx, user, -amount); err != nil {
			return err
		}
		return tx.Create(&MetaLearnFundingReceipt{RequestID: requestID, BusinessUserID: mapping.BusinessUserID, GatewayUserID: userID, FundingSource: "wallet", InitialQuota: amount, ReservedQuota: amount, FundingStatus: MetaLearnFundingReserved, TokenAccounting: "pending", UpdatedAt: common.GetTimestamp()}).Error
	})
}

func ReserveMetaLearnWalletFunding(requestID string, userID int, operationID string, expected, delta int64) error {
	if !ValidMetaLearnBusinessUserID(operationID) || expected < 0 || delta <= 0 || expected > math.MaxInt64-delta {
		return ErrMetaLearnInvalid
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, user, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
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
		if err := exactMetaLearnWalletDeltaTx(tx, user, -delta); err != nil {
			return err
		}
		if err := tx.Create(&MetaLearnFundingReservation{OperationID: operationID, RequestID: requestID, Amount: delta, ExpectedQuota: expected, Status: "reserved"}).Error; err != nil {
			return err
		}
		r.ReservedQuota += delta
		return saveMetaLearnFundingTx(tx, r)
	})
}

func RollbackMetaLearnWalletFundingReservation(requestID string, userID int, operationID string) error {
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, user, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
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
		if err := exactMetaLearnWalletDeltaTx(tx, user, op.Amount); err != nil {
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

func SettleMetaLearnWalletFunding(requestID string, userID int, actualQuota int64) error {
	if !validMetaLearnWalletAmount(actualQuota) {
		return ErrMetaLearnInvalid
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, user, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
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
		// Native settle deducts actual minus all initial/extra reservations. The
		// original wallet algorithm allows a final overage to leave a debt.
		if err := exactMetaLearnWalletDeltaTx(tx, user, r.ReservedQuota-actualQuota); err != nil {
			return err
		}
		r.FundingStatus, r.SettledQuota = MetaLearnFundingSettled, &actualQuota
		return saveMetaLearnFundingTx(tx, r)
	})
}

func MarkMetaLearnWalletRefundPending(requestID string, userID int) error {
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
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

func RefundMetaLearnWalletFunding(requestID string, userID int) error {
	if err := MarkMetaLearnWalletRefundPending(requestID, userID); err != nil {
		return err
	}
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, user, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
		if err != nil {
			return err
		}
		if r.FundingStatus == MetaLearnFundingRefunded {
			return nil
		}
		if r.FundingStatus != MetaLearnRefundPending {
			return ErrMetaLearnFundingConflict
		}
		if err := exactMetaLearnWalletDeltaTx(tx, user, r.ReservedQuota); err != nil {
			return err
		}
		zero := int64(0)
		r.FundingStatus, r.SettledQuota = MetaLearnFundingRefunded, &zero
		return saveMetaLearnFundingTx(tx, r)
	})
}

func reportMetaLearnWalletTokenAccounting(requestID string, userID int, status string) error {
	return metalearnTransaction(func(tx *gorm.DB) error {
		r, _, err := lockedMetaLearnWalletFundingTx(tx, requestID, userID)
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
