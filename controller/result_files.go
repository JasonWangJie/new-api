package controller

import (
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// StoredResultFileContent exposes ordinary generated results as shareable files.
// The filename is not a filesystem path; eligibility is rechecked on every read.
func StoredResultFileContent(c *gin.Context) {
	canvasResponseHeaders(c)
	filename := c.Param("filename")
	ctx, now := c.Request.Context(), time.Now().Unix()
	if strings.HasPrefix(filename, "img_") {
		object, _, err := service.ReadImageResultFile(ctx, filename, now)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		if link := service.ImageResultDeliveryURL(object); link != "" {
			c.Header("Referrer-Policy", "no-referrer")
			c.Redirect(http.StatusTemporaryRedirect, link)
			return
		}
		c.Header("Content-Disposition", `inline; filename="`+filename+`"`)
		serveImageObject(c, object, true)
		return
	}
	object, job, err := service.ReadMediaResultFile(ctx, filename, now)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	// Preserve the existing media download policy for disabled/expired Tokens,
	// disabled owners and IP restrictions, even though the URL has no signature.
	var token model.Token
	if err := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", object.TokenId, object.UserId).Take(&token).Error; err != nil || validateMediaToken(ctx, token, c.ClientIP()) != nil {
		c.Status(http.StatusNotFound)
		return
	}
	serveAuthorizedMediaTaskResult(c, job, object.ArtifactKey)
}
