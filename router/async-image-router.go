package router

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Only these new public endpoints normalize legacy authentication errors.
// Existing relay and management middleware retain their response contracts.
type asyncImageAuthWriter struct {
	gin.ResponseWriter
	Body bytes.Buffer
	Code int
}

func (writer *asyncImageAuthWriter) Write(data []byte) (int, error) { return writer.Body.Write(data) }
func (writer *asyncImageAuthWriter) WriteString(data string) (int, error) {
	return writer.Body.WriteString(data)
}
func (writer *asyncImageAuthWriter) WriteHeader(code int) {
	if writer.Code == 0 {
		writer.Code = code
	}
}
func (writer *asyncImageAuthWriter) WriteHeaderNow() {}
func (writer *asyncImageAuthWriter) Status() int {
	if writer.Code == 0 {
		return http.StatusOK
	}
	return writer.Code
}
func (writer *asyncImageAuthWriter) Size() int     { return writer.Body.Len() }
func (writer *asyncImageAuthWriter) Written() bool { return writer.Code != 0 || writer.Body.Len() > 0 }

func asyncImagePublicAuth(auth gin.HandlerFunc) gin.HandlerFunc {
	// Quota is evaluated by the image price/funding check. The following
	// policy handler adds expiry, revocation and IP checks for every path.
	return func(c *gin.Context) {
		original := c.Writer
		writer := &asyncImageAuthWriter{ResponseWriter: original}
		c.Writer = writer
		defer func() { c.Writer = original }()
		auth(c)
		body := writer.Body.Bytes()
		if c.IsAborted() && !c.GetBool("async_public_success") {
			var payload struct {
				Message string `json:"message"`
				Error   struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if common.Unmarshal(body, &payload) == nil {
				message := payload.Message
				if message == "" {
					message = payload.Error.Message
				}
				code, kind := "authentication_failed", "authentication_error"
				if writer.Status() != 401 {
					code, kind = "access_denied", "permission_error"
				}
				if writer.Status() >= 500 {
					code, kind = "service_unavailable", "server_error"
				}
				if encoded, err := common.Marshal(gin.H{"error": gin.H{"type": kind, "code": code, "message": message}}); err == nil {
					body = encoded
				}
			}
		}
		original.Header().Set("Cache-Control", "no-store")
		original.WriteHeader(writer.Status())
		_, _ = original.Write(body)
	}
}

func asyncImageTokenPermissions(readOnly bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		var token model.Token
		if err := model.DB.WithContext(c.Request.Context()).Select("tokens.*").Joins("JOIN users ON users.id = tokens.user_id").Where("tokens.id = ? AND tokens.user_id = ? AND users.status = ? AND users.deleted_at IS NULL", c.GetInt("token_id"), c.GetInt("id"), common.UserStatusEnabled).Take(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				controller.AsyncImagePublicError(c, 401, "authentication_failed", "Image Token is unavailable")
			} else {
				controller.AsyncImagePublicError(c, 503, "database_unavailable", "Image Token permissions are unavailable")
			}
			c.Abort()
			return
		}
		validStatus := token.Status == common.TokenStatusEnabled || readOnly && token.Status == common.TokenStatusExhausted
		if !validStatus || token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix() {
			controller.AsyncImagePublicError(c, 401, "authentication_failed", "Image Token is unavailable")
			c.Abort()
			return
		}
		if limits := token.GetIpLimits(); len(limits) > 0 {
			ip := net.ParseIP(c.ClientIP())
			if ip == nil || !common.IsIpInCIDRList(ip, limits) {
				controller.AsyncImagePublicError(c, 403, "access_denied", "Client address is outside Token permissions")
				c.Abort()
				return
			}
		}
		c.Next()
	}
}

func SetAsyncImagePublicRouter(router *gin.Engine) {
	api := router.Group("/v1")
	api.Use(middleware.RouteTag("relay"))
	for _, path := range []string{"/images/generations_oa", "/images/edits_oa", "/chat/completions_gm", "/images/generations_sc", "/images/generations_async", "/images/edits_async"} {
		api.POST(path, asyncImagePublicAuth(middleware.TokenAuthReadOnly()), asyncImageTokenPermissions(false), controller.SubmitAsyncImage)
	}
	api.POST("/videos/generations_async", asyncImagePublicAuth(middleware.TokenAuth()), asyncImageTokenPermissions(false), controller.PrepareAsyncVideo, middleware.PinTaskPluginEndpoint(), controller.FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), controller.RouteAsyncVideo, middleware.Distribute(), controller.AcceptAsyncVideo)
	api.GET("/media/tasks_async/:task_id", asyncImagePublicAuth(middleware.TokenAuthReadOnly()), asyncImageTokenPermissions(true), controller.QueryAsyncMedia)
	api.GET("/media/objects/:object_id", controller.MediaObjectContent)
	api.HEAD("/media/objects/:object_id", controller.MediaObjectContent)
	api.POST("/uploads/images_sc", asyncImagePublicAuth(middleware.TokenAuthReadOnly()), asyncImageTokenPermissions(false), controller.UploadAsyncImageInput)
	api.GET("/images/tasks_async/:task_id", asyncImagePublicAuth(middleware.TokenAuthReadOnly()), asyncImageTokenPermissions(true), controller.QueryAsyncImage)
	api.GET("/tasks_sc/:task_id", asyncImagePublicAuth(middleware.TokenAuthReadOnly()), asyncImageTokenPermissions(true), controller.QueryAsyncImage)
}
