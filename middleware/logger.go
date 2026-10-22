package middleware

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

const RouteTagKey = "route_tag"

func RouteTag(tag string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(RouteTagKey, tag)
		c.Next()
	}
}

func SetUpLogger(server *gin.Engine) {
	server.Use(redactTaskArtifactAccessQuery())
	server.Use(gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		var requestID string
		if param.Keys != nil {
			requestID, _ = param.Keys[common.RequestIdKey].(string)
		}
		tag, _ := param.Keys[RouteTagKey].(string)
		if tag == "" {
			tag = "web"
		}
		path := param.Path
		// Callbacks and image reference URLs carry credentials in the query string.
		// Redact the log value only; the handler still needs the original query.
		if strings.HasPrefix(path, "/api/oauth/") || strings.HasPrefix(path, "/oauth/") || strings.HasPrefix(path, "/api/integrations/canvas/") || strings.HasPrefix(path, "/canvas/") || strings.HasPrefix(path, "/api/image-task-references/") {
			path, _, _ = strings.Cut(path, "?")
		}
		// Stats never accept query credentials; avoid recording rejected keys.
		requestPath, _, _ := strings.Cut(path, "?")
		if requestPath == "/v1/models" || strings.HasPrefix(requestPath, "/v1beta/models") || strings.HasPrefix(requestPath, "/v1beta/openai/models") {
			// These compatibility routes accept ?key= credentials, including
			// rejected managed keys. Never copy their query into access logs.
			path = requestPath
		}
		switch requestPath {
		case "/v1/images/tasks_async/stats", "/v1/videos/tasks_async/stats", "/v1/media/tasks_async/stats":
			path = requestPath
		}
		return fmt.Sprintf("[GIN] %s | %s | %s | %3d | %13v | %15s | %7s %s\n",
			param.TimeStamp.Format("2006/01/02 - 15:04:05"),
			tag,
			requestID,
			param.StatusCode,
			param.Latency,
			param.ClientIP,
			param.Method,
			path,
		)
	}))
}
