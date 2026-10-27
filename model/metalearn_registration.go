package model

import (
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Product-owned registration reward: not a payment, subscription or second balance.
// A fixed key and unique business UUID prevent replays across configuration changes.
type MetaLearnRegistrationReward struct {
	BusinessUserID string `gorm:"primaryKey;type:varchar(36)"`
	QuotaAmount    int64
	GatewayUserID  int
	CreatedAt      int64
}
type MetaLearnRegistrationRequest struct {
	BusinessUserID string `json:"business_user_id"`
	RewardKey      string `json:"reward_key"`
	QuotaAmount    int64  `json:"quota_amount"`
}
type MetaLearnRegistrationResult struct {
	MetaLearnRegistrationRequest
	Status         string `json:"status"`
	QuotaPerCredit int64  `json:"quota_per_credit"`
	GatewayUserID  int    `json:"gateway_user_id"`
}

func GrantMetaLearnRegistrationReward(r MetaLearnRegistrationRequest) (*MetaLearnRegistrationResult, error) {
	if !ValidMetaLearnBusinessUserID(r.BusinessUserID) || r.RewardKey != "registration-v1" || r.QuotaAmount < 0 || r.QuotaAmount > 1000000*MetaLearnQuotaPerCredit || r.QuotaAmount%MetaLearnQuotaPerCredit != 0 {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnRegistrationResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		mapping, err := metaLearnMappedUser(tx, r.BusinessUserID)
		if err != nil {
			return err
		}
		user, err := lockedMetaLearnWalletUserTx(tx, r.BusinessUserID, mapping.GatewayUserID)
		if err != nil {
			return err
		}
		seed := MetaLearnRegistrationReward{BusinessUserID: r.BusinessUserID, QuotaAmount: r.QuotaAmount, GatewayUserID: user.Id, CreatedAt: common.GetTimestamp()}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed)
		if created.Error != nil {
			return created.Error
		}
		var stored MetaLearnRegistrationReward
		if err := tx.First(&stored, "business_user_id = ?", r.BusinessUserID).Error; err != nil {
			return err
		}
		if stored.QuotaAmount != r.QuotaAmount || stored.GatewayUserID != user.Id {
			return ErrMetaLearnIdentity
		}
		if created.RowsAffected == 1 {
			if err := exactMetaLearnWalletDeltaTx(tx, user, r.QuotaAmount); err != nil {
				return err
			}
		}
		result = &MetaLearnRegistrationResult{MetaLearnRegistrationRequest: r, Status: "granted", QuotaPerCredit: MetaLearnQuotaPerCredit, GatewayUserID: user.Id}
		return nil
	})
	if err == nil {
		if cacheErr := InvalidateUserCache(result.GatewayUserID); cacheErr != nil {
			common.SysLog("metalearn_registration_cache_invalidation_failed")
		}
	}
	return result, err
}
