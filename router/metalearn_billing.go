package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func setMetaLearnBillingRouter(api *gin.RouterGroup) {
	api.GET("/metalearn/source", controller.DownloadMetaLearnSource)
	integration := api.Group("/metalearn/billing", middleware.MetaLearnBillingAuth())
	integration.POST("/users", controller.ProvisionMetaLearnUser)
	integration.POST("/periods", controller.GrantMetaLearnPeriod)
	integration.POST("/periods/revoke", controller.RevokeMetaLearnPeriod)
	integration.GET("/users/:business_user_id/subscriptions", controller.QueryMetaLearnSubscriptions)
	integration.GET("/users/:business_user_id/requests/:request_id", controller.QueryMetaLearnFundingReceipt)
}
