package controller

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GrantMetaLearnRegistrationReward(c *gin.Context) {
	var req struct {
		BusinessUserID string `json:"business_user_id"`
		RewardKey      string `json:"reward_key"`
		QuotaAmount    *int64 `json:"quota_amount"`
	}
	if err := decodeMetaLearnRequest(c, &req, "business_user_id", "reward_key", "quota_amount"); err != nil {
		metaLearnError(c, err)
		return
	}
	if req.QuotaAmount == nil {
		metaLearnError(c, model.ErrMetaLearnInvalid)
		return
	}
	result, err := model.GrantMetaLearnRegistrationReward(model.MetaLearnRegistrationRequest{BusinessUserID: req.BusinessUserID, RewardKey: req.RewardKey, QuotaAmount: *req.QuotaAmount})
	if err != nil {
		metaLearnError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}
