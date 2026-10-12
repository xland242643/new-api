package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/google/uuid"
)

func metaLearnTokenReport(err error, required bool) string {
	if err != nil {
		return "reported_error"
	}
	// Native token APIs return before Redis writes and batch flushes complete.
	if common.BatchUpdateEnabled || common.RedisEnabled {
		return "queued"
	}
	if !required {
		return "not_required"
	}
	return "reported_ok"
}

func (s *BillingSession) settleMetaLearnLocked(funding *SubscriptionFunding, actualQuota int) error {
	// Even an equal/zero delta must produce a durable funding terminal state.
	// Repeated calls validate the same native actual quota without a second delta.
	if err := model.SettleMetaLearnFunding(funding.requestId, funding.userId, funding.subscriptionId, int64(actualQuota)); err != nil {
		return err
	}
	if s.settled {
		return nil
	}
	s.fundingSettled = true
	delta := actualQuota - s.preConsumedQuota
	var tokenErr error
	if !s.relayInfo.IsPlayground && delta != 0 {
		if delta > 0 {
			tokenErr = model.DecreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
		} else {
			tokenErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		}
	}
	// No repeat of a non-idempotent token API after funding has committed.
	s.settled = true
	s.relayInfo.SubscriptionPostDelta += int64(delta)
	status := metaLearnTokenReport(tokenErr, !s.relayInfo.IsPlayground && delta != 0)
	if err := model.ReportMetaLearnTokenAccounting(funding.requestId, funding.userId, funding.subscriptionId, status); err != nil {
		common.SysLog("metalearn_token_accounting_report_failed")
		if tokenErr == nil {
			return err
		}
	}
	if tokenErr != nil {
		common.SysLog("metalearn_token_accounting_failed")
	}
	return tokenErr
}

func (s *BillingSession) reserveMetaLearnLocked(funding *SubscriptionFunding, delta int) error {
	operationID := uuid.NewString()
	if err := model.ReserveMetaLearnFunding(funding.requestId, funding.userId, funding.subscriptionId, operationID, int64(s.preConsumedQuota), int64(delta)); err != nil {
		return err
	}
	if err := s.reserveToken(delta); err != nil {
		if rollbackErr := model.RollbackMetaLearnFundingReservation(funding.requestId, funding.userId, funding.subscriptionId, operationID); rollbackErr != nil {
			// The durable reservation remains authoritative and refundable. Do not
			// pretend a failed rollback restored the funding balance.
			common.SysLog("metalearn_reservation_rollback_pending")
			return model.ErrMetaLearnFundingInconsistent
		}
		return err
	}
	s.preConsumedQuota += delta
	s.tokenConsumed += delta
	s.extraReserved += delta
	s.syncRelayInfo()
	return nil
}

// Called under s.mu. Persist refund intent before handing it to the native
// async pool. Failure leaves pending funding and does not return token quota.
func (s *BillingSession) refundMetaLearnLocked(funding *SubscriptionFunding) {
	if s.settled || s.refunded || !s.needsRefundLocked() {
		return
	}
	if err := model.MarkMetaLearnRefundPending(funding.requestId, funding.userId, funding.subscriptionId); err != nil {
		common.SysLog("metalearn_refund_intent_failed")
		return
	}
	s.refunded = true
	tokenID, tokenKey, tokenConsumed, playground := s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed, s.relayInfo.IsPlayground
	gopool.Go(func() {
		if err := refundWithRetry(func() error {
			return model.RefundMetaLearnFunding(funding.requestId, funding.userId, funding.subscriptionId)
		}); err != nil {
			common.SysLog("metalearn_funding_refund_pending")
			s.mu.Lock()
			s.refunded = false
			s.mu.Unlock()
			return
		}
		var tokenErr error
		if tokenConsumed > 0 && !playground {
			tokenErr = model.IncreaseTokenQuota(tokenID, tokenKey, tokenConsumed)
		}
		if err := model.ReportMetaLearnTokenAccounting(funding.requestId, funding.userId, funding.subscriptionId, metaLearnTokenReport(tokenErr, tokenConsumed > 0 && !playground)); err != nil {
			common.SysLog("metalearn_token_accounting_report_failed")
		}
		if tokenErr != nil {
			common.SysLog("metalearn_token_accounting_failed")
		}
	})
}
