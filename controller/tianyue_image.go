package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func TianyueImageReferenceContent(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	image, err := service.ReadTianyueImageReference(c.Request.Context(), c.Param("task_id"), c.Param("index"), c.Query("expires"), c.Query("key"), c.Query("signature"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, image.ContentType, image.Data)
}
