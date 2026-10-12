package service

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

var (
	ErrMetaLearnRelayUnsupported = errors.New("metalearn_monthly_requires_standard_chat")
	ErrMetaLearnRelayUnavailable = errors.New("metalearn_billing_identity_unavailable")
)

// IsMetaLearnBillingRelay uses the durable billing identity, rather than the
// request-selected channel group, so a group override cannot enable a bypass.
func IsMetaLearnBillingRelay(info *relaycommon.RelayInfo) (bool, error) {
	if info == nil {
		return false, ErrMetaLearnRelayUnavailable
	}
	return metaLearnRelayUser(info.UserId)
}

func metaLearnRelayUser(userID int) (bool, error) {
	isMetaLearn, err := model.IsMetaLearnBillingUser(userID)
	if err != nil {
		// Never surface ORM errors: they can contain credential-bearing SQL.
		return false, ErrMetaLearnRelayUnavailable
	}
	return isMetaLearn, nil
}

// CheckMetaLearnRelayEntry checks the original HTTP ingress. RequestURLPath can
// be rewritten for playground or an upstream conversion and is not authority.
func CheckMetaLearnRelayEntry(c *gin.Context, userID int, format types.RelayFormat) (bool, error) {
	isMetaLearn, err := metaLearnRelayUser(userID)
	if err != nil || !isMetaLearn {
		return isMetaLearn, err
	}
	if c == nil || c.Request == nil || c.Request.URL == nil ||
		c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/chat/completions" ||
		format != types.RelayFormatOpenAI {
		return true, ErrMetaLearnRelayUnsupported
	}
	return true, nil
}

// RequireMetaLearnStandardChat is the BillingSession admission check. Other
// users retain the native gateway's supported formats and billing preferences.
func RequireMetaLearnStandardChat(c *gin.Context, info *relaycommon.RelayInfo) (bool, error) {
	if info == nil {
		return false, ErrMetaLearnRelayUnavailable
	}
	isMetaLearn, err := CheckMetaLearnRelayEntry(c, info.UserId, info.RelayFormat)
	if err != nil || !isMetaLearn {
		return isMetaLearn, err
	}
	if info.RelayMode != relayconstant.RelayModeChatCompletions || info.IsPlayground ||
		info.IsChannelTest || info.ClientWs != nil {
		return true, ErrMetaLearnRelayUnsupported
	}
	return true, nil
}

// RejectMetaLearnLegacyFunding closes direct wallet, task, WSS and penalty
// paths. Monthly funds may only move through the scoped receipt transactions.
func RejectMetaLearnLegacyFunding(info *relaycommon.RelayInfo) error {
	isMetaLearn, err := IsMetaLearnBillingRelay(info)
	if err != nil {
		return err
	}
	if isMetaLearn {
		return ErrMetaLearnRelayUnsupported
	}
	return nil
}

func MetaLearnRelayGuardAPIError(err error) *types.NewAPIError {
	status := http.StatusServiceUnavailable
	code := types.ErrorCode("metalearn_billing_identity_unavailable")
	message := ErrMetaLearnRelayUnavailable
	if errors.Is(err, ErrMetaLearnRelayUnsupported) {
		status = http.StatusForbidden
		code = types.ErrorCode("metalearn_monthly_requires_standard_chat")
		message = ErrMetaLearnRelayUnsupported
	}
	return types.NewErrorWithStatusCode(message, code, status, types.ErrOptionWithSkipRetry())
}
