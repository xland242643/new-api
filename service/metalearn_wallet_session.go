package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/google/uuid"
)

func (s *BillingSession) settleMetaLearnWalletLocked(funding *WalletFunding, actualQuota int) error {
	if err := model.SettleMetaLearnWalletFunding(funding.requestId, funding.userId, int64(actualQuota)); err != nil {
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
	s.settled = true
	status := metaLearnTokenReport(tokenErr, !s.relayInfo.IsPlayground && delta != 0)
	if err := model.ReportMetaLearnTokenAccounting(funding.requestId, funding.userId, 0, status); err != nil {
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

func (s *BillingSession) reserveMetaLearnWalletLocked(funding *WalletFunding, delta int) error {
	operationID := uuid.NewString()
	if err := model.ReserveMetaLearnWalletFunding(funding.requestId, funding.userId, operationID, int64(s.preConsumedQuota), int64(delta)); err != nil {
		return err
	}
	if err := s.reserveToken(delta); err != nil {
		if err := model.RollbackMetaLearnWalletFundingReservation(funding.requestId, funding.userId, operationID); err != nil {
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

// Intent is durable before scheduling. The worker commits native wallet delta
// and terminal receipt together; neither a queued job nor token success proves
// that the wallet refund has committed.
func (s *BillingSession) refundMetaLearnWalletLocked(funding *WalletFunding) {
	if s.settled || s.refunded || s.fundingSettled {
		return
	}
	if err := model.MarkMetaLearnWalletRefundPending(funding.requestId, funding.userId); err != nil {
		common.SysLog("metalearn_refund_intent_failed")
		return
	}
	s.refunded = true
	tokenID, tokenKey, tokenConsumed, playground := s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed, s.relayInfo.IsPlayground
	gopool.Go(func() {
		if err := refundWithRetry(func() error { return model.RefundMetaLearnWalletFunding(funding.requestId, funding.userId) }); err != nil {
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
		if err := model.ReportMetaLearnTokenAccounting(funding.requestId, funding.userId, 0, metaLearnTokenReport(tokenErr, tokenConsumed > 0 && !playground)); err != nil {
			common.SysLog("metalearn_token_accounting_report_failed")
		}
		if tokenErr != nil {
			common.SysLog("metalearn_token_accounting_failed")
		}
	})
}
