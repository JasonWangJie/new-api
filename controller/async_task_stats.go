package controller

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// QueryAsyncTaskStats reports persisted tasks owned by the authenticated user,
// including tasks submitted with other API keys or through the workbench.
func QueryAsyncTaskStats(mediaType string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		location := time.Local
		if zone := strings.TrimSpace(os.Getenv("TZ")); zone != "" {
			var err error
			location, err = time.LoadLocation(strings.TrimPrefix(zone, ":"))
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"type": "server_error", "code": "stats_unavailable", "message": "Statistics timezone is unavailable"}})
				return
			}
		}
		now := time.Now().In(location)
		stats, err := model.GetAsyncTaskStats(c.Request.Context(), c.GetInt("id"), mediaType, now)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, model.ErrAsyncTaskStatsUnavailable) {
				status = http.StatusServiceUnavailable
			}
			c.JSON(status, gin.H{"error": gin.H{"type": "server_error", "code": "stats_unavailable", "message": "Async task statistics are unavailable"}})
			return
		}
		// The live quota ledger includes reservations waiting for a batch DB
		// flush. Reading only users.quota would overstate spendable balance.
		quota, err := model.GetAsyncTaskStatsQuota(c.Request.Context(), c.GetInt("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"type": "server_error", "code": "stats_unavailable", "message": "User balance is unavailable"}})
			return
		}
		successRate := float64(0)
		if finished := stats.SuccessCount + stats.FailureCount; finished > 0 {
			successRate = float64(stats.SuccessCount) / float64(finished) * 100
		}
		c.JSON(http.StatusOK, gin.H{
			"object":         "async_" + mediaType + ".stats",
			"date":           now.Format(time.DateOnly),
			"timezone":       location.String(),
			"balance":        float64(quota) / common.QuotaPerUnit,
			"today_requests": stats.TodayRequests,
			"success_count":  stats.SuccessCount,
			"failure_count":  stats.FailureCount,
			"success_rate":   successRate,
		})
	}
}
