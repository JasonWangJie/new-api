package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func MediaInputContent(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	object, err := service.ReadMediaInputObject(c.Request.Context(), c.Param("object_id"), c.Query("expires"), c.Query("access"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	if err := service.ServeMediaInputObject(c, object); err != nil && !c.Writer.Written() {
		c.Status(http.StatusNotFound)
	}
}
