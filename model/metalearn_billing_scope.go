package model

import (
	"errors"
	"regexp"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrMetaLearnPeriodNotStarted = errors.New("period_not_started")
var metaLearnScopeKey = regexp.MustCompile(`^[0-9a-f]{64}$`)

// One effective paid period per verified external store group/account scope.
// This is not a wallet. Store cash proration never enters this service.
type MetaLearnBillingScope struct {
	ScopeKey        string `gorm:"primaryKey;type:varchar(64)"`
	BusinessUserID  string `gorm:"type:varchar(36);not null"`
	Channel         string `gorm:"type:varchar(32);not null"`
	PeriodKey       string `gorm:"type:varchar(128)"`
	EffectiveTimeMS int64
}

type MetaLearnPeriodSwitchRequest struct {
	MetaLearnPeriodRequest
	ScopeKey        string `json:"scope_key"`
	EffectiveTimeMS int64  `json:"effective_time_ms"`
	ExpectedQuota   int64  `json:"expected_quota"`
	Revoke          bool   `json:"revoke"`
}

func SwitchMetaLearnBillingPeriod(r MetaLearnPeriodSwitchRequest) (*MetaLearnPeriodResult, error) {
	if r.ExpectedQuota <= 0 || !r.valid() || !metaLearnScopeKey.MatchString(r.ScopeKey) || r.EffectiveTimeMS <= 0 ||
		(r.EffectiveTimeMS/1000+boolSecond(r.EffectiveTimeMS%1000 != 0)) != r.StartTime {
		return nil, ErrMetaLearnInvalid
	}
	// Do not end today's allowance when a future renewal arrives early.
	now := GetDBTimestamp()
	if !r.Revoke && r.StartTime > now {
		return nil, ErrMetaLearnPeriodNotStarted
	}
	var result *MetaLearnPeriodResult
	var retired bool
	err := metalearnTransaction(func(tx *gorm.DB) error {
		retired = false
		seed := MetaLearnBillingScope{ScopeKey: r.ScopeKey, BusinessUserID: r.BusinessUserID, Channel: r.Channel}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var scope MetaLearnBillingScope
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&scope, "scope_key = ?", r.ScopeKey).Error; err != nil {
			return err
		}
		if scope.BusinessUserID != r.BusinessUserID || scope.Channel != r.Channel {
			return ErrMetaLearnIdentity
		}
		if scope.EffectiveTimeMS == r.EffectiveTimeMS && scope.PeriodKey != r.PeriodKey {
			return ErrMetaLearnPeriodConflict
		}
		var existing MetaLearnBillingPeriod
		lookup := tx.Where("channel = ? AND period_key = ?", r.Channel, r.PeriodKey).First(&existing).Error
		if lookup != nil && !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return lookup
		}
		if lookup == nil && existing.ScopeKey != "" && existing.ScopeKey != r.ScopeKey {
			return ErrMetaLearnPeriodConflict
		}
		if scope.PeriodKey == "" {
			var unscoped int64
			if err := tx.Model(&MetaLearnBillingPeriod{}).Where("business_user_id = ? AND channel = ? AND scope_key = ? AND status = ? AND start_time <= ? AND end_time > ? AND period_key <> ?", r.BusinessUserID, r.Channel, "", "granted", now, now, r.PeriodKey).Count(&unscoped).Error; err != nil {
				return err
			}
			if unscoped > 0 {
				return ErrMetaLearnPeriodConflict
			}
		}
		retired = scope.EffectiveTimeMS > r.EffectiveTimeMS || existing.Status == "revoked"
		if err := setMetaLearnPeriodTx(tx, r.MetaLearnPeriodRequest, r.Revoke || retired, &result); err != nil {
			if !errors.Is(err, ErrMetaLearnPeriodRevoked) {
				return err
			}
			// A refund may obtain the period lock after our initial lookup.
			// Keep its tombstone and still end the prior upgraded allowance.
			retired = true
			if err := setMetaLearnPeriodTx(tx, r.MetaLearnPeriodRequest, true, &result); err != nil {
				return err
			}
		}
		if result.SubscriptionID > 0 && result.AmountTotal != r.ExpectedQuota {
			return ErrMetaLearnPlanNotEligible
		}
		// The period lock is now held. Recheck after waiting on a competing scope.
		if err := tx.Where("channel = ? AND period_key = ?", r.Channel, r.PeriodKey).First(&existing).Error; err != nil {
			return err
		}
		if existing.ScopeKey != "" && existing.ScopeKey != r.ScopeKey {
			return ErrMetaLearnPeriodConflict
		}
		if err := tx.Model(&MetaLearnBillingPeriod{}).Where("channel = ? AND period_key = ?", r.Channel, r.PeriodKey).Update("scope_key", r.ScopeKey).Error; err != nil {
			return err
		}
		if scope.EffectiveTimeMS < r.EffectiveTimeMS && r.StartTime <= now {
			if scope.PeriodKey != "" {
				var old MetaLearnBillingPeriod
				if err := tx.Where("channel = ? AND period_key = ?", scope.Channel, scope.PeriodKey).First(&old).Error; err != nil {
					return err
				}
				if old.ScopeKey != r.ScopeKey || old.BusinessUserID != r.BusinessUserID {
					return ErrMetaLearnIdentity
				}
				var ignored *MetaLearnPeriodResult
				req := MetaLearnPeriodRequest{BusinessUserID: old.BusinessUserID, Channel: old.Channel, PeriodKey: old.PeriodKey, PlanID: old.PlanID, StartTime: old.StartTime, EndTime: old.EndTime}
				if err := setMetaLearnPeriodTx(tx, req, true, &ignored); err != nil {
					return err
				}
			}
			scope.PeriodKey, scope.EffectiveTimeMS = r.PeriodKey, r.EffectiveTimeMS
			if err := tx.Save(&scope).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Late older grants commit a tombstone, then report conflict. They cannot
	// revive on retry, including through the older unscoped grant endpoint.
	if retired && !r.Revoke {
		return nil, ErrMetaLearnPeriodRevoked
	}
	return result, nil
}

func boolSecond(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
