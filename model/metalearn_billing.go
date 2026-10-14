package model

// MetaLearn's narrow paid-period seam. New API owns quota reservation,
// settlement and refund; this file only authorizes exact verified paid periods.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const MetaLearnQuotaPerCredit int64 = 1000
const MetaLearnSubscriptionSource = "metalearn"
const MetaLearnMonthlyGroup = "metalearn-monthly"

var (
	ErrMetaLearnInvalid         = errors.New("invalid_request")
	ErrMetaLearnIdentity        = errors.New("identity_conflict")
	ErrMetaLearnPeriodConflict  = errors.New("period_conflict")
	ErrMetaLearnPeriodRevoked   = errors.New("period_revoked")
	ErrMetaLearnUserNotFound    = errors.New("user_not_found")
	ErrMetaLearnPlanNotFound    = errors.New("plan_not_found")
	ErrMetaLearnPlanNotEligible = errors.New("plan_not_eligible")
	metalearnChannel            = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// Rows are reserved inside the same transaction as their user/subscription.
// Unique constraints and row locks cover multiple gateway processes.
type MetaLearnBillingUser struct {
	BusinessUserID string `gorm:"primaryKey;type:varchar(36)"`
	GatewayUserID  int    `gorm:"not null;default:0"`
	RelayTokenID   int    `gorm:"not null;default:0"`
}

type MetaLearnBillingPeriod struct {
	ID               int
	Channel          string `gorm:"type:varchar(32);uniqueIndex:idx_metalearn_period,priority:1"`
	PeriodKey        string `gorm:"type:varchar(128);uniqueIndex:idx_metalearn_period,priority:2"`
	BusinessUserID   string `gorm:"type:varchar(36);index"`
	PlanID           int
	StartTime        int64
	EndTime          int64
	Status           string `gorm:"type:varchar(16)"`
	SubscriptionID   int
	BaseDurationMS   int64  `gorm:"not null;default:0"`
	PaidDurationMS   int64  `gorm:"not null;default:0"`
	ScopeKey         string `gorm:"type:varchar(64);not null;default:'';index"`
	QuotaResetPeriod string `gorm:"type:varchar(16)"`
}

type MetaLearnPeriodRequest struct {
	BusinessUserID string `json:"business_user_id"`
	Channel        string `json:"channel"`
	PeriodKey      string `json:"period_key"`
	PlanID         int    `json:"plan_id"`
	StartTime      int64  `json:"start_time"`
	EndTime        int64  `json:"end_time"`
}

type MetaLearnUserResult struct {
	BusinessUserID string `json:"business_user_id"`
	GatewayUserID  int    `json:"gateway_user_id"`
	RelayToken     string `json:"relay_token"`
}

type MetaLearnPeriodResult struct {
	MetaLearnPeriodRequest
	QuotaPerCredit  int64  `json:"quota_per_credit"`
	Status          string `json:"status"`
	SubscriptionID  int    `json:"subscription_id"`
	AmountTotal     int64  `json:"amount_total"`
	AmountUsed      int64  `json:"amount_used"`
	AmountRemaining int64  `json:"amount_remaining"`
}

type MetaLearnSubscriptionsResult struct {
	BusinessUserID string                  `json:"business_user_id"`
	GatewayUserID  int                     `json:"gateway_user_id"`
	QuotaPerCredit int64                   `json:"quota_per_credit"`
	Subscriptions  []MetaLearnPeriodResult `json:"subscriptions"`
}

func ValidMetaLearnBusinessUserID(id string) bool {
	value, err := uuid.Parse(id)
	return err == nil && value != uuid.Nil && value.String() == id
}

func (r MetaLearnPeriodRequest) valid() bool {
	if !ValidMetaLearnBusinessUserID(r.BusinessUserID) || !metalearnChannel.MatchString(r.Channel) || len(r.PeriodKey) < 1 || len(r.PeriodKey) > 128 || r.PlanID <= 0 || r.StartTime <= 0 || r.EndTime <= r.StartTime {
		return false
	}
	for _, ch := range r.PeriodKey {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func metalearnDB() *gorm.DB {
	// Do not let ORM failure logging print relay keys or request identities.
	return DB.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
}

func metalearnTransaction(fn func(*gorm.DB) error) error {
	// SQLite write contention and SQL serialization failures must retry the
	// entire transaction. No process-local financial lock substitutes for DB locks.
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		err = metalearnDB().Transaction(fn)
		if err == nil {
			return nil
		}
		text := strings.ToLower(err.Error())
		// Under READ COMMITTED two preconsume callers can both miss a request
		// anchor before serializing on the subscription row. A duplicate insert
		// aborts PostgreSQL's transaction: replay only in a fresh transaction.
		if !strings.Contains(text, "database is locked") && !strings.Contains(text, "database table is locked") && !strings.Contains(text, "deadlock") && !strings.Contains(text, "serialization failure") && !strings.Contains(text, "could not serialize access") && !strings.Contains(text, "duplicate key") && !strings.Contains(text, "duplicate entry") && !strings.Contains(text, "unique constraint") {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

func metaLearnMappedUser(tx *gorm.DB, id string) (*MetaLearnBillingUser, error) {
	var mapping MetaLearnBillingUser
	if err := tx.Where("business_user_id = ?", id).First(&mapping).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrMetaLearnUserNotFound
		}
		return nil, err
	}
	var user User
	if err := tx.First(&user, mapping.GatewayUserID).Error; err != nil {
		return nil, ErrMetaLearnIdentity
	}
	setting := dto.UserSetting{}
	// Existing mapped subscription-only accounts remain valid until the scoped
	// idempotent provision upgrades them. Their relay still cannot use a wallet.
	if common.UnmarshalJsonStr(user.Setting, &setting) != nil || (setting.BillingPreference != "subscription_first" && setting.BillingPreference != "subscription_only") || user.Status != common.UserStatusEnabled || user.Role != common.RoleCommonUser || user.Group != MetaLearnMonthlyGroup {
		return nil, ErrMetaLearnIdentity
	}
	return &mapping, nil
}

func EnsureMetaLearnBillingUser(id string) (*MetaLearnUserResult, error) {
	if !ValidMetaLearnBusinessUserID(id) {
		return nil, ErrMetaLearnInvalid
	}
	var result MetaLearnUserResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		seed := MetaLearnBillingUser{BusinessUserID: id}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var mapping MetaLearnBillingUser
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("business_user_id = ?", id).First(&mapping).Error; err != nil {
			return err
		}
		if mapping.GatewayUserID == 0 {
			key, err := common.GenerateKey()
			if err != nil {
				return err
			}
			password, err := common.GenerateKey()
			if err != nil {
				return err
			}
			hash, err := common.Password2Hash(password)
			if err != nil {
				return err
			}
			settings, err := common.Marshal(dto.UserSetting{BillingPreference: "subscription_first"})
			if err != nil {
				return err
			}
			usernameHash := sha256.Sum256([]byte(id))
			user := User{Username: "ml_" + hex.EncodeToString(usernameHash[:8]), Password: hash, DisplayName: "MetaLearn", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: MetaLearnMonthlyGroup, Quota: 0, Setting: string(settings), AffCode: common.GetUUID()}
			// Insert/InsertWithTx add registration rewards. This trusted machine
			// provisioning intentionally uses DB.Create and no reward finalizer.
			if err := tx.Create(&user).Error; err != nil {
				return err
			}
			token := Token{UserId: user.Id, Key: key, Name: "MetaLearn server relay", Status: common.TokenStatusEnabled, CreatedTime: common.GetTimestamp(), ExpiredTime: -1, UnlimitedQuota: true, Group: MetaLearnMonthlyGroup}
			if err := tx.Create(&token).Error; err != nil {
				return err
			}
			mapping.GatewayUserID = user.Id
			mapping.RelayTokenID = token.Id
			if err := tx.Save(&mapping).Error; err != nil {
				return err
			}
		}
		// Upgrade only an already dedicated mapped account's previous product
		// preference. Preserve all unrelated user settings and ordinary users.
		var existing User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, mapping.GatewayUserID).Error; err != nil {
			return ErrMetaLearnIdentity
		}
		setting := dto.UserSetting{}
		if common.UnmarshalJsonStr(existing.Setting, &setting) != nil {
			return ErrMetaLearnIdentity
		}
		if setting.BillingPreference == "subscription_only" && existing.Status == common.UserStatusEnabled && existing.Role == common.RoleCommonUser && existing.Group == MetaLearnMonthlyGroup {
			setting.BillingPreference = "subscription_first"
			encoded, err := common.Marshal(setting)
			if err != nil {
				return err
			}
			if err := tx.Model(&existing).Update("setting", string(encoded)).Error; err != nil {
				return err
			}
		}
		if _, err := metaLearnMappedUser(tx, id); err != nil {
			return err
		}
		var token Token
		if err := tx.First(&token, mapping.RelayTokenID).Error; err != nil {
			return ErrMetaLearnIdentity
		}
		if token.UserId != mapping.GatewayUserID || token.Status != common.TokenStatusEnabled || token.Group != MetaLearnMonthlyGroup || !token.UnlimitedQuota || token.Key == "" {
			return ErrMetaLearnIdentity
		}
		result = MetaLearnUserResult{BusinessUserID: id, GatewayUserID: mapping.GatewayUserID, RelayToken: "sk-" + token.Key}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := InvalidateUserCache(result.GatewayUserID); err != nil {
		common.SysLog("metalearn_user_cache_invalidation_failed")
	}
	return &result, nil
}

func (p MetaLearnBillingPeriod) matches(r MetaLearnPeriodRequest) bool {
	return p.BusinessUserID == r.BusinessUserID && p.Channel == r.Channel && p.PeriodKey == r.PeriodKey && p.PlanID == r.PlanID && p.StartTime == r.StartTime && p.EndTime == r.EndTime
}

func metaLearnPeriodResult(tx *gorm.DB, p *MetaLearnBillingPeriod) (*MetaLearnPeriodResult, error) {
	result := &MetaLearnPeriodResult{MetaLearnPeriodRequest: MetaLearnPeriodRequest{BusinessUserID: p.BusinessUserID, Channel: p.Channel, PeriodKey: p.PeriodKey, PlanID: p.PlanID, StartTime: p.StartTime, EndTime: p.EndTime}, QuotaPerCredit: MetaLearnQuotaPerCredit, Status: p.Status, SubscriptionID: p.SubscriptionID}
	if p.SubscriptionID != 0 {
		var sub UserSubscription
		if err := tx.First(&sub, p.SubscriptionID).Error; err != nil {
			return nil, ErrMetaLearnIdentity
		}
		var mapping MetaLearnBillingUser
		if err := tx.Where("business_user_id = ?", p.BusinessUserID).First(&mapping).Error; err != nil {
			return nil, ErrMetaLearnIdentity
		}
		if sub.UserId != mapping.GatewayUserID || sub.PlanId != p.PlanID || sub.Source != MetaLearnSubscriptionSource || sub.StartTime != p.StartTime || sub.EndTime != p.EndTime {
			return nil, ErrMetaLearnIdentity
		}
		if p.Status == "revoked" && sub.Status != "cancelled" {
			return nil, ErrMetaLearnIdentity
		}
		result.AmountTotal = sub.AmountTotal
		result.AmountUsed = sub.AmountUsed
		result.AmountRemaining = max(int64(0), sub.AmountTotal-sub.AmountUsed)
	}
	return result, nil
}

func SetMetaLearnBillingPeriod(r MetaLearnPeriodRequest, revoke bool) (*MetaLearnPeriodResult, error) {
	if !r.valid() {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnPeriodResult
	err := metalearnTransaction(func(tx *gorm.DB) error { return setMetaLearnPeriodTx(tx, r, revoke, &result) })
	if err != nil {
		return nil, err
	}
	return result, nil
}

func setMetaLearnPeriodTx(tx *gorm.DB, r MetaLearnPeriodRequest, revoke bool, result **MetaLearnPeriodResult, quotaOverride ...int64) error {
	seed := MetaLearnBillingPeriod{Channel: r.Channel, PeriodKey: r.PeriodKey, BusinessUserID: r.BusinessUserID, PlanID: r.PlanID, StartTime: r.StartTime, EndTime: r.EndTime, Status: "pending", QuotaResetPeriod: SubscriptionResetNever}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
		return err
	}
	var period MetaLearnBillingPeriod
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("channel = ? AND period_key = ?", r.Channel, r.PeriodKey).First(&period).Error; err != nil {
		return err
	}
	if !period.matches(r) {
		return ErrMetaLearnPeriodConflict
	}
	if revoke {
		if period.SubscriptionID != 0 {
			if _, err := metaLearnPeriodResult(tx, &period); err != nil {
				return err
			}
			if err := tx.Model(&UserSubscription{}).Where("id = ?", period.SubscriptionID).Updates(map[string]any{"status": "cancelled", "updated_at": common.GetTimestamp()}).Error; err != nil {
				return err
			}
		}
		period.Status = "revoked"
		if err := tx.Save(&period).Error; err != nil {
			return err
		}
	} else {
		if period.Status == "revoked" {
			return ErrMetaLearnPeriodRevoked
		}
		if period.Status == "pending" {
			mapping, err := metaLearnMappedUser(tx, r.BusinessUserID)
			if err != nil {
				return err
			}
			var plan SubscriptionPlan
			if err := tx.First(&plan, r.PlanID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrMetaLearnPlanNotFound
				}
				return err
			}
			if !plan.Enabled || plan.TotalAmount <= 0 {
				return ErrMetaLearnPlanNotEligible
			}
			total := plan.TotalAmount
			if len(quotaOverride) > 0 {
				if len(quotaOverride) != 1 || quotaOverride[0] <= 0 {
					return ErrMetaLearnInvalid
				}
				total = quotaOverride[0]
			}
			sub := UserSubscription{UserId: mapping.GatewayUserID, PlanId: plan.Id, AmountTotal: total, StartTime: r.StartTime, EndTime: r.EndTime, Status: "active", Source: MetaLearnSubscriptionSource}
			if err := tx.Create(&sub).Error; err != nil {
				return err
			}
			period.SubscriptionID = sub.Id
			period.Status = "granted"
			if err := tx.Save(&period).Error; err != nil {
				return err
			}
		}
	}
	var err error
	*result, err = metaLearnPeriodResult(tx, &period)
	return err
}

func GetMetaLearnSubscriptions(id string) (*MetaLearnSubscriptionsResult, error) {
	if !ValidMetaLearnBusinessUserID(id) {
		return nil, ErrMetaLearnInvalid
	}
	var result *MetaLearnSubscriptionsResult
	err := metalearnTransaction(func(tx *gorm.DB) error {
		mapping, err := metaLearnMappedUser(tx, id)
		if err != nil {
			return err
		}
		result = &MetaLearnSubscriptionsResult{BusinessUserID: id, GatewayUserID: mapping.GatewayUserID, QuotaPerCredit: MetaLearnQuotaPerCredit, Subscriptions: []MetaLearnPeriodResult{}}
		var periods []MetaLearnBillingPeriod
		if err := tx.Where("business_user_id = ?", id).Order("start_time desc, id desc").Find(&periods).Error; err != nil {
			return err
		}
		for _, period := range periods {
			item, err := metaLearnPeriodResult(tx, &period)
			if err != nil {
				return err
			}
			result.Subscriptions = append(result.Subscriptions, *item)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
