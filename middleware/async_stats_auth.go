package middleware

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// AsyncTaskStatsAuth authenticates an API key without requiring remaining quota.
// Revocation, expiry, user status and IP restrictions still apply to every read.
func AsyncTaskStatsAuth() gin.HandlerFunc {
	var rateLimit gin.HandlerFunc
	if common.GlobalApiRateLimitEnable {
		rateLimit = rateLimitFactory(common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration, "AST")
	}
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if rateLimit != nil {
			rateLimit(c)
			if c.IsAborted() {
				abortAsyncTaskStatsAuth(c, c.Writer.Status(), "rate_limit_rejected")
				return
			}
		}
		headers := c.Request.Header.Values("Authorization")
		if len(headers) != 1 {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "invalid_authorization")
			return
		}
		parts := strings.Fields(headers[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "invalid_authorization")
			return
		}
		key := strings.TrimPrefix(parts[1], "sk-")
		if key == "" || len(key) > 128 || strings.ContainsAny(key, ",\r\n") {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "invalid_authorization")
			return
		}
		if model.DB == nil {
			abortAsyncTaskStatsAuth(c, http.StatusServiceUnavailable, "database_unavailable")
			return
		}

		// Bypass identity caches so revocation applies immediately. The query
		// contains a credential, so never log its SQL, including in debug mode.
		db := model.DB.WithContext(c.Request.Context()).Session(&gorm.Session{Logger: gormlogger.Discard})
		var token model.Token
		err := db.Where(&model.Token{Key: key}).First(&token).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "invalid_token")
			} else {
				abortAsyncTaskStatsAuth(c, http.StatusInternalServerError, "token_lookup_failed")
			}
			return
		}
		// Some database collations compare text without case sensitivity.
		if subtle.ConstantTimeCompare([]byte(token.Key), []byte(key)) != 1 {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "invalid_token")
			return
		}
		c.Set("token_id", token.Id)
		if token.Status != common.TokenStatusEnabled && token.Status != common.TokenStatusExhausted {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "token_unavailable")
			return
		}
		if token.ExpiredTime != -1 && token.ExpiredTime <= common.GetTimestamp() {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "token_expired")
			return
		}
		if allowIPs := token.GetIpLimits(); len(allowIPs) > 0 {
			ip := net.ParseIP(c.ClientIP())
			if ip == nil || !common.IsIpInCIDRList(ip, allowIPs) {
				abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "ip_denied")
				return
			}
		}

		var user model.User
		err = db.Select("id", "status").First(&user, "id = ?", token.UserId).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "user_unavailable")
			} else {
				abortAsyncTaskStatsAuth(c, http.StatusInternalServerError, "user_lookup_failed")
			}
			return
		}
		if user.Status != common.UserStatusEnabled {
			abortAsyncTaskStatsAuth(c, http.StatusUnauthorized, "user_unavailable")
			return
		}
		c.Set("id", user.Id)
		logger.LogInfo(c, fmt.Sprintf("async_task_stats_auth outcome=success user_id=%d token_id=%d route=%q ip=%q", user.Id, token.Id, c.FullPath(), c.ClientIP()))
		c.Next()
	}
}

func abortAsyncTaskStatsAuth(c *gin.Context, status int, reason string) {
	logger.LogWarn(c, "async_task_stats_auth outcome=failure reason=%s token_id=%d route=%q ip=%q", reason, c.GetInt("token_id"), c.FullPath(), c.ClientIP())
	errorType, code, message := "server_error", "stats_unavailable", "statistics unavailable"
	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", "Bearer")
		errorType, code, message = "authentication_error", "authentication_error", "invalid API key"
	} else if status == http.StatusTooManyRequests {
		errorType, code, message = "rate_limit_error", "rate_limit_exceeded", "too many requests"
	}
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{"type": errorType, "code": code, "message": message},
	})
}
