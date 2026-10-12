package controller

import (
	"encoding/json" // RawMessage types only; decoding uses common wrappers.
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func metaLearnError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "integration_unavailable"
	switch {
	case errors.Is(err, model.ErrMetaLearnInvalid):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, model.ErrMetaLearnIdentity):
		status, code = http.StatusConflict, "identity_conflict"
	case errors.Is(err, model.ErrMetaLearnPeriodConflict):
		status, code = http.StatusConflict, "period_conflict"
	case errors.Is(err, model.ErrMetaLearnPeriodRevoked):
		status, code = http.StatusConflict, "period_revoked"
	case errors.Is(err, model.ErrMetaLearnPlanNotEligible):
		status, code = http.StatusConflict, "plan_not_eligible"
	case errors.Is(err, model.ErrMetaLearnPlanNotFound):
		status, code = http.StatusNotFound, "plan_not_found"
	case errors.Is(err, model.ErrMetaLearnUserNotFound):
		status, code = http.StatusNotFound, "user_not_found"
	}
	c.JSON(status, gin.H{"success": false, "code": code})
}

// Reject unknown fields, including attempted quota, group or reward injection.
func decodeMetaLearnRequest(c *gin.Context, into any, allowed ...string) error {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	if err != nil {
		return model.ErrMetaLearnInvalid
	}
	fields := map[string]json.RawMessage{}
	if common.Unmarshal(body, &fields) != nil || fields == nil {
		return model.ErrMetaLearnInvalid
	}
	for key := range fields {
		known := false
		for _, name := range allowed {
			if name == key {
				known = true
				break
			}
		}
		if !known {
			return model.ErrMetaLearnInvalid
		}
	}
	if common.Unmarshal(body, into) != nil {
		return model.ErrMetaLearnInvalid
	}
	return nil
}

func ProvisionMetaLearnUser(c *gin.Context) {
	var req struct {
		BusinessUserID string `json:"business_user_id"`
	}
	if err := decodeMetaLearnRequest(c, &req, "business_user_id"); err != nil {
		metaLearnError(c, err)
		return
	}
	result, err := model.EnsureMetaLearnBillingUser(req.BusinessUserID)
	if err != nil {
		metaLearnError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

func setMetaLearnPeriod(c *gin.Context, revoke bool) {
	var req model.MetaLearnPeriodRequest
	if err := decodeMetaLearnRequest(c, &req, "business_user_id", "channel", "period_key", "plan_id", "start_time", "end_time"); err != nil {
		metaLearnError(c, err)
		return
	}
	result, err := model.SetMetaLearnBillingPeriod(req, revoke)
	if err != nil {
		metaLearnError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

func GrantMetaLearnPeriod(c *gin.Context)  { setMetaLearnPeriod(c, false) }
func RevokeMetaLearnPeriod(c *gin.Context) { setMetaLearnPeriod(c, true) }
func QueryMetaLearnSubscriptions(c *gin.Context) {
	result, err := model.GetMetaLearnSubscriptions(c.Param("business_user_id"))
	if err != nil {
		metaLearnError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}
