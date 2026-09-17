package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
)

func setImageManagementRouter(api *gin.RouterGroup) {
	api.GET("/image-plaza/:publication_id/content", controller.ImagePlazaContent)
	plaza := api.Group("/image-plaza", middleware.UserAuth(), middleware.DisableCache())
	plaza.GET("", controller.ListImagePlaza)
	plaza.POST("/:publication_id/reports", controller.ReportImagePublication)
	library := api.Group("/user/image-library", middleware.UserAuth(), middleware.DisableCache())
	library.GET("", controller.ListImageLibrary)
	library.POST("/import", controller.ImportImageLibrary)
	library.POST("/import-url", controller.ImportImageLibraryURL)
	library.POST("/from-task", controller.ImageLibraryFromTask)
	library.GET("/submission-requests", controller.ListDeferredImageSubmissions)
	library.POST("/submission-requests", controller.CreateDeferredImageSubmission)
	library.POST("/submission-requests/:request_id/sync", controller.SyncDeferredImageSubmission)
	library.DELETE("/submission-requests/:request_id", controller.WithdrawDeferredImageSubmission)
	library.GET("/:asset_id", controller.GetImageLibraryItem)
	library.PATCH("/:asset_id", controller.PatchImageLibraryItem)
	library.DELETE("/:asset_id", controller.DeleteImageLibraryItem)
	library.GET("/:asset_id/view", controller.ViewImageLibraryItem)
	library.POST("/:asset_id/publications", controller.CreateImageLibraryPublication)
	library.DELETE("/:asset_id/publication", controller.WithdrawImageLibraryPublication)
	admin := api.Group("/admin", middleware.AdminAuth(), middleware.DisableCache())
	read, manage := middleware.RequirePermission(authz.ImageModerationRead), middleware.RequirePermission(authz.ImageModerationManage)
	admin.GET("/image-plaza/publications", read, controller.ListAdminImagePublications)
	admin.GET("/image-plaza/publications/:publication_id/view", read, controller.ViewAdminImagePublication)
	admin.POST("/image-plaza/publications/batch", manage, controller.BatchModerateImagePublications)
	admin.POST("/image-plaza/publications/:publication_id/:action", manage, controller.ModerateImagePublication)
	admin.GET("/image-plaza/submission-requests", read, controller.ListAdminDeferredImageSubmissions)
	admin.POST("/image-plaza/submission-requests/:request_id/:action", manage, controller.ModerateDeferredImageSubmission)
	admin.GET("/image-plaza/reports", read, controller.ListAdminImageReports)
	admin.POST("/image-plaza/reports/:report_id/resolve", manage, controller.ResolveImageReport)
	admin.GET("/image-library", read, controller.ListAdminImageLibrary)
	admin.GET("/image-library/stats", read, controller.ImageLibraryStats)
	admin.GET("/image-library/:asset_id/view", read, controller.ViewAdminImageLibraryItem)
	admin.GET("/image-library/migration", read, controller.ImageLibraryMigration)
	admin.GET("/image-library/cleanup-jobs", read, controller.ListImageCleanupJobs)
	admin.POST("/image-library/cleanup-jobs/preview", manage, controller.PreviewImageCleanup)
	admin.POST("/image-library/cleanup-jobs", manage, controller.CreateImageCleanup)
}
