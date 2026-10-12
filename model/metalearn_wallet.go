package model

// Verified external purchases credit the native user wallet. This narrow seam
// owns order identity/idempotency, not a second balance or pricing algorithm.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrMetaLearnTopUpConflict      = errors.New("topup_conflict")
	ErrMetaLearnTopUpRevoked       = errors.New("topup_revoked")
	ErrMetaLearnWalletOverflow     = errors.New("wallet_overflow")
	ErrMetaLearnWalletInsufficient = errors.New("wallet_insufficient")
)

type MetaLearnWalletTopUp struct {
	ID              int
	Channel         string `gorm:"type:varchar(32);uniqueIndex:idx_metalearn_wallet_order,priority:1"`
	OrderKey        string `gorm:"type:varchar(128);uniqueIndex:idx_metalearn_wallet_order,priority:2"`
	BusinessUserID  string `gorm:"type:varchar(36);index"`
	ProductID       string `gorm:"type:varchar(128)"`
	QuotaAmount     int64
	Currency        string `gorm:"type:varchar(3)"`
	PaidAmountMinor int64
	GatewayUserID   int
	TopUpID         int
	Status          string `gorm:"type:varchar(16)"`
}

type MetaLearnTopUpRequest struct {
	BusinessUserID  string `json:"business_user_id"`
	Channel         string `json:"channel"`
	OrderKey        string `json:"order_key"`
	ProductID       string `json:"product_id"`
	QuotaAmount     int64  `json:"quota_amount"`
	Currency        string `json:"currency"`
	PaidAmountMinor int64  `json:"paid_amount_minor"`
}

type MetaLearnTopUpResult struct {
	MetaLearnTopUpRequest
	Status         string `json:"status"`
	QuotaPerCredit int64  `json:"quota_per_credit"`
	GatewayUserID  int    `json:"gateway_user_id"`
	QuotaBalance   int64  `json:"quota_balance"`
}

type MetaLearnWalletResult struct {
	BusinessUserID string `json:"business_user_id"`
	GatewayUserID  int    `json:"gateway_user_id"`
	QuotaPerCredit int64  `json:"quota_per_credit"`
	QuotaBalance   int64  `json:"quota_balance"`
}

func metaLearnPrintableKey(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func (r MetaLearnTopUpRequest) valid() bool {
	return ValidMetaLearnBusinessUserID(r.BusinessUserID) && metalearnChannel.MatchString(r.Channel) && metaLearnPrintableKey(r.OrderKey) && metaLearnPrintableKey(r.ProductID) && r.QuotaAmount > 0 && r.QuotaAmount%MetaLearnQuotaPerCredit == 0 && r.QuotaAmount <= int64(^uint(0)>>1) && r.Currency == "CNY" && r.PaidAmountMinor > 0
}

func (p MetaLearnWalletTopUp) matches(r MetaLearnTopUpRequest) bool {
	return p.BusinessUserID == r.BusinessUserID && p.Channel == r.Channel && p.OrderKey == r.OrderKey && p.ProductID == r.ProductID && p.QuotaAmount == r.QuotaAmount && p.Currency == r.Currency && p.PaidAmountMinor == r.PaidAmountMinor
}

func lockedMetaLearnWalletUserTx(tx *gorm.DB, businessID string, userID int) (*User, error) {
	mapping, err := metaLearnMappedUser(tx, businessID)
	if err != nil {
		return nil, err
	}
	if mapping.GatewayUserID != userID {
		return nil, ErrMetaLearnIdentity
	}
	var user User
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, userID).Error; err != nil {
		return nil, ErrMetaLearnIdentity
	}
	return &user, nil
}

// Native arithmetic is quota += delta. Unlike the legacy batch queue, the
// dedicated product seam commits it in the same transaction as its evidence.
// Negative balances are retained after full order revocation, never clamped.
func exactMetaLearnWalletDeltaTx(tx *gorm.DB, user *User, delta int64) error {
	before := int64(user.Quota)
	maxInt := int64(^uint(0) >> 1)
	minInt := -maxInt - 1
	if (delta > 0 && before > maxInt-delta) || (delta < 0 && before < minInt-delta) {
		return ErrMetaLearnWalletOverflow
	}
	if delta == 0 {
		return nil
	}
	result := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrMetaLearnIdentity
	}
	user.Quota = int(before + delta)
	return nil
}

func SetMetaLearnWalletTopUp(r MetaLearnTopUpRequest, revoke bool) (*MetaLearnTopUpResult, error) {
	if !r.valid() {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnTopUpResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		seed := MetaLearnWalletTopUp{Channel: r.Channel, OrderKey: r.OrderKey, BusinessUserID: r.BusinessUserID, ProductID: r.ProductID, QuotaAmount: r.QuotaAmount, Currency: r.Currency, PaidAmountMinor: r.PaidAmountMinor, Status: "pending"}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var order MetaLearnWalletTopUp
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("channel = ? AND order_key = ?", r.Channel, r.OrderKey).First(&order).Error; err != nil {
			return err
		}
		if !order.matches(r) {
			return ErrMetaLearnTopUpConflict
		}
		if !revoke && order.Status == "revoked" {
			return ErrMetaLearnTopUpRevoked
		}
		var user *User
		if order.GatewayUserID != 0 {
			var err error
			user, err = lockedMetaLearnWalletUserTx(tx, r.BusinessUserID, order.GatewayUserID)
			if err != nil {
				return err
			}
		}
		if revoke {
			if order.Status == "granted" {
				if user == nil || order.TopUpID == 0 {
					return ErrMetaLearnIdentity
				}
				if err := exactMetaLearnWalletDeltaTx(tx, user, -order.QuotaAmount); err != nil {
					return err
				}
				if err := tx.Model(&TopUp{}).Where("id = ? AND user_id = ?", order.TopUpID, user.Id).Update("status", "refunded").Error; err != nil {
					return err
				}
			}
			order.Status = "revoked"
		} else if order.Status == "pending" {
			mapping, err := metaLearnMappedUser(tx, r.BusinessUserID)
			if err != nil {
				return err
			}
			user, err = lockedMetaLearnWalletUserTx(tx, r.BusinessUserID, mapping.GatewayUserID)
			if err != nil {
				return err
			}
			if err := exactMetaLearnWalletDeltaTx(tx, user, r.QuotaAmount); err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(r.Channel + "\x00" + r.OrderKey))
			now := common.GetTimestamp()
			topUp := TopUp{UserId: user.Id, Amount: r.QuotaAmount, Money: float64(r.PaidAmountMinor) / 100, TradeNo: "metalearn:" + hex.EncodeToString(hash[:]), PaymentMethod: r.Channel, PaymentProvider: "metalearn", CreateTime: now, CompleteTime: now, Status: common.TopUpStatusSuccess}
			if err := tx.Create(&topUp).Error; err != nil {
				return err
			}
			order.TopUpID, order.GatewayUserID, order.Status = topUp.Id, user.Id, "granted"
		}
		if err := tx.Save(&order).Error; err != nil {
			return err
		}
		result = &MetaLearnTopUpResult{MetaLearnTopUpRequest: r, Status: order.Status, QuotaPerCredit: MetaLearnQuotaPerCredit, GatewayUserID: order.GatewayUserID}
		if user != nil {
			result.QuotaBalance = int64(user.Quota)
		}
		return nil
	})
	return result, err
}

func GetMetaLearnWallet(businessID string) (*MetaLearnWalletResult, error) {
	if !ValidMetaLearnBusinessUserID(businessID) {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnWalletResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		mapping, err := metaLearnMappedUser(tx, businessID)
		if err != nil {
			return err
		}
		user, err := lockedMetaLearnWalletUserTx(tx, businessID, mapping.GatewayUserID)
		if err != nil {
			return err
		}
		result = &MetaLearnWalletResult{BusinessUserID: businessID, GatewayUserID: user.Id, QuotaPerCredit: MetaLearnQuotaPerCredit, QuotaBalance: int64(user.Quota)}
		return nil
	})
	return result, err
}

// Check subtraction before negating; the ledger's native integer width is the
// platform int width. SQL migration tests also verify the real dialect type.
func validMetaLearnWalletAmount(amount int64) bool {
	return amount >= 0 && amount <= math.MaxInt64 && amount <= int64(^uint(0)>>1)
}
