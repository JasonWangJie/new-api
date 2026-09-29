package router

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	stdlog "log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Every Redis key, including the gateway's existing authentication caches,
// stays inside the test namespace. Unsupported commands fail closed.
type imageRedisTestHook struct{ prefix string }

func (hook imageRedisTestHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	args := cmd.Args()
	positions := []int{}
	switch strings.ToLower(cmd.Name()) {
	case "ping":
	case "get", "set", "setnx", "setex", "hgetall", "hset", "hget", "hincrby", "expire", "pexpire", "ttl", "pttl", "zadd", "zrem", "zrange", "zcard", "zscore", "zremrangebyscore", "incr", "incrby", "exists":
		positions = append(positions, 1)
	case "del", "mget":
		for i := 1; i < len(args); i++ {
			positions = append(positions, i)
		}
	case "eval", "evalsha":
		if len(args) < 3 {
			return ctx, errors.New("invalid test Redis script")
		}
		count, err := strconv.Atoi(toImageRedisString(args[2]))
		if err != nil {
			return ctx, err
		}
		for i := 3; i < 3+count; i++ {
			positions = append(positions, i)
		}
	case "script":
		if len(args) < 2 || strings.ToLower(toImageRedisString(args[1])) != "load" {
			return ctx, errors.New("unsupported test Redis command")
		}
	case "scan":
		if len(args) < 4 {
			return ctx, errors.New("unscoped test Redis scan")
		}
		scoped := false
		for i := 2; i+1 < len(args); i++ {
			if strings.EqualFold(toImageRedisString(args[i]), "match") && strings.HasPrefix(toImageRedisString(args[i+1]), hook.prefix) {
				scoped = true
			}
		}
		if !scoped {
			return ctx, errors.New("unscoped test Redis scan")
		}
	default:
		return ctx, errors.New("unsupported test Redis command: " + cmd.Name())
	}
	for _, position := range positions {
		if position >= len(args) {
			return ctx, errors.New("invalid test Redis key")
		}
		key := toImageRedisString(args[position])
		if !strings.HasPrefix(key, hook.prefix+":") {
			args[position] = hook.prefix + ":" + key
		}
	}
	return ctx, nil
}
func toImageRedisString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	}
	return ""
}
func (hook imageRedisTestHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (hook imageRedisTestHook) BeforeProcessPipeline(ctx context.Context, commands []redis.Cmder) (context.Context, error) {
	for _, command := range commands {
		if _, err := hook.BeforeProcess(ctx, command); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}
func (hook imageRedisTestHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func imageRouterDatabase(t *testing.T, name string) *gorm.DB {
	t.Helper()

	if dsn := os.Getenv("ASYNC_IMAGE_TEST_MYSQL_DSN"); dsn != "" {
		cfg, err := mysqlDriver.ParseDSN(dsn)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(cfg.DBName, "new_api_image_test_"))
		cfg.DBName = ""
		admin, err := sql.Open("mysql", cfg.FormatDSN())
		require.NoError(t, err)
		database := "new_api_image_test_" + strings.ToLower(common.GetUUID())
		_, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+database+"` CHARACTER SET utf8mb4")
		require.NoError(t, err)
		cfg.DBName = database
		db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		t.Cleanup(func() {
			connection, _ := db.DB()
			_ = connection.Close()
			_, err := admin.ExecContext(context.Background(), "DROP DATABASE `"+database+"`")
			assert.NoError(t, err)
			_ = admin.Close()
		})
		return db
	}
	dsn := os.Getenv("ASYNC_IMAGE_TEST_PG_DSN")
	if dsn == "" {
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), name+".db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		require.NoError(t, err)
		t.Cleanup(func() { connection, _ := db.DB(); _ = connection.Close() })
		return db
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(cfg.Database, "new_api_image_test_"))
	schema := "image_api_test_" + strings.ToLower(common.GetUUID())
	admin := stdlib.OpenDB(*cfg)
	_, err = admin.ExecContext(context.Background(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	connection := stdlib.OpenDB(*cfg)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: connection}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = connection.Close()
		_, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
		_ = admin.Close()
	})
	return db
}

func TestAsyncTaskStats(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldDB, oldQuotaUnit, oldRedis, oldLimit := model.DB, common.QuotaPerUnit, common.RedisEnabled, common.GlobalApiRateLimitEnable
	t.Cleanup(func() {
		model.DB, common.QuotaPerUnit, common.RedisEnabled, common.GlobalApiRateLimitEnable = oldDB, oldQuotaUnit, oldRedis, oldLimit
	})
	common.QuotaPerUnit, common.RedisEnabled, common.GlobalApiRateLimitEnable = 500000, false, false
	t.Setenv("TZ", "Asia/Shanghai")
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Now().In(location)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	model.DB = imageRouterDatabase(t, "stats")
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.AsyncImageTask{}, &model.AsyncMediaJob{}, &model.Task{}))
	versionQuery := "SELECT version()"
	if model.DB.Dialector.Name() == "sqlite" {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, model.DB.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database=%s version=%s", model.DB.Dialector.Name(), version)
	user := model.User{Username: "statsowner", AffCode: "statsowner", Status: common.UserStatusEnabled, Quota: 6250000}
	other := model.User{Username: "statsother", AffCode: "statsother", Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&other).Error)
	token := model.Token{UserId: user.Id, Key: strings.Repeat("a", 48), Status: common.TokenStatusExhausted, ExpiredTime: -1}
	second := model.Token{UserId: user.Id, Key: strings.Repeat("b", 48), Status: common.TokenStatusEnabled, ExpiredTime: -1}
	otherToken := model.Token{UserId: other.Id, Key: strings.Repeat("c", 48), Status: common.TokenStatusEnabled, ExpiredTime: -1}
	for _, entry := range []*model.Token{&token, &second, &otherToken} {
		require.NoError(t, model.DB.Create(entry).Error)
	}
	engine := gin.New()
	SetAsyncImagePublicRouter(engine)
	request := func(path, authorization string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		engine.ServeHTTP(recorder, req)
		assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		return recorder
	}
	readStats := func(kind, key string) map[string]any {
		path := "/v1/" + kind + "/tasks_async/stats?user_id=" + strconv.Itoa(other.Id) + "&api_key_id=" + strconv.Itoa(otherToken.Id) + "&timezone=UTC&date=2000-01-01"
		recorder := request(path, "Bearer sk-"+key)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var payload map[string]any
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
		assert.Len(t, payload, 8)
		assert.Equal(t, now.Format(time.DateOnly), payload["date"])
		assert.Equal(t, "Asia/Shanghai", payload["timezone"])
		return payload
	}

	empty := readStats("images", token.Key)
	assert.Equal(t, float64(0), empty["today_requests"])
	assert.Equal(t, float64(0), empty["success_rate"])
	assert.Equal(t, 12.5, empty["balance"])
	imageStatuses := []string{model.ImageTaskQueued, model.ImageTaskInvoking, model.ImageTaskUpstreamSucceeded, model.ImageTaskUploading, model.ImageTaskBillingPending, model.ImageTaskSucceeded, model.ImageTaskSucceeded, model.ImageTaskFailed, model.ImageTaskExecutionUnknown, model.ImageTaskStorageFailed, model.ImageTaskBillingFailed, model.ImageTaskExpired}
	for i, status := range imageStatuses {
		ownerToken := token.Id
		if i%2 != 0 {
			ownerToken = second.Id
		}
		require.NoError(t, model.DB.Create(&model.AsyncImageTask{TaskId: "stats-image-" + strconv.Itoa(i), UserId: user.Id, TokenId: ownerToken, Status: status, CreatedAt: start.Unix() + int64(i)}).Error)
	}
	for i, created := range []int64{start.Unix() - 1, start.AddDate(0, 0, 1).Unix()} {
		require.NoError(t, model.DB.Create(&model.AsyncImageTask{TaskId: "outside-image-" + strconv.Itoa(i), UserId: user.Id, Status: model.ImageTaskSucceeded, CreatedAt: created}).Error)
		require.NoError(t, model.DB.Create(&model.AsyncMediaJob{TaskId: "outside-video-" + strconv.Itoa(i), IdentityHash: "outside-" + strconv.Itoa(i), UserId: user.Id, Status: "succeeded", CreatedAt: created}).Error)
	}
	require.NoError(t, model.DB.Create(&model.AsyncImageTask{TaskId: "other-image", UserId: other.Id, Status: model.ImageTaskSucceeded, CreatedAt: start.Unix()}).Error)
	require.NoError(t, model.DB.Create(&model.AsyncMediaJob{TaskId: "other-video", IdentityHash: "other-video", UserId: other.Id, Status: "succeeded", CreatedAt: start.Unix()}).Error)
	videoCases := []struct{ status, storage, billing, upstream string }{
		{"queued", "pending", "pending", ""},
		{"submitting", "pending", "pending", ""},
		{"submitted", "pending", "reserved", string(model.TaskStatusInProgress)},
		{"submitted", "saving", "settled", string(model.TaskStatusSuccess)},
		{"succeeded", "succeeded", "settled", string(model.TaskStatusSuccess)},
		{"succeeded", "upstream", "settled", string(model.TaskStatusSuccess)},
		{"failed", "pending", "refunded", ""},
		{"execution_unknown", "pending", "unknown", ""},
		{"submitted", "failed", "settled", string(model.TaskStatusSuccess)},
		{"succeeded", "succeeded", "failed", string(model.TaskStatusSuccess)},
		{"expired", "expired", "settled", ""},
		{"submitted", "pending", "reserved", string(model.TaskStatusFailure)},
	}
	for i, entry := range videoCases {
		id := "stats-video-" + strconv.Itoa(i)
		require.NoError(t, model.DB.Create(&model.AsyncMediaJob{TaskId: id, IdentityHash: id, UserId: user.Id, TokenId: second.Id, Status: entry.status, StorageStatus: entry.storage, BillingStatus: entry.billing, CreatedAt: start.Unix() + int64(i)}).Error)
		if entry.upstream != "" {
			// Multiple upstream rows must never multiply the persisted job count.
			for range 2 {
				require.NoError(t, model.DB.Create(&model.Task{TaskID: id, UserId: user.Id, Status: model.TaskStatus(entry.upstream), CreatedAt: start.Unix()}).Error)
			}
		}
	}
	// Neither a generic legacy task nor another owner's colliding ID is this user's failed video.
	require.NoError(t, model.DB.Create(&model.Task{TaskID: "unrelated-generic-task", UserId: user.Id, Status: model.TaskStatusSuccess, CreatedAt: start.Unix()}).Error)
	require.NoError(t, model.DB.Create(&model.Task{TaskID: "stats-video-4", UserId: other.Id, Status: model.TaskStatusFailure, CreatedAt: start.Unix()}).Error)
	for _, entry := range []struct {
		kind, object              string
		requests, success, failed float64
	}{
		{"images", "async_image.stats", 12, 2, 5},
		{"videos", "async_video.stats", 12, 2, 6},
		{"media", "async_media.stats", 24, 4, 11},
	} {
		t.Run(entry.kind, func(t *testing.T) {
			payload := readStats(entry.kind, token.Key)
			assert.Equal(t, entry.object, payload["object"])
			assert.Equal(t, entry.requests, payload["today_requests"])
			assert.Equal(t, entry.success, payload["success_count"])
			assert.Equal(t, entry.failed, payload["failure_count"])
			assert.InDelta(t, entry.success/(entry.success+entry.failed)*100, payload["success_rate"], 1e-10)
			assert.Equal(t, payload, readStats(entry.kind, second.Key))
		})
	}
	require.NoError(t, model.DB.Model(&user).Update("quota", 0).Error)
	assert.Equal(t, float64(0), readStats("images", token.Key)["balance"])

	t.Run("authentication", func(t *testing.T) {
		path := "/v1/images/tasks_async/stats"
		for _, authorization := range []string{"", token.Key, "Basic " + token.Key, "Bearer", "Bearer sk-", "Bearer invalid", "Bearer sk-" + strings.ToUpper(token.Key), "Bearer sk-" + token.Key + "-1", "Bearer sk-" + token.Key + " extra"} {
			recorder := request(path+"?key=sk-"+token.Key+"&api_key=sk-"+token.Key, authorization)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			assert.JSONEq(t, `{"error":{"type":"authentication_error","code":"authentication_error","message":"invalid API key"}}`, recorder.Body.String())
		}
		for _, header := range []string{"x-api-key", "x-goog-api-key", "Cookie", "Sec-WebSocket-Protocol"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set(header, "sk-"+token.Key)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Add("Authorization", "Bearer sk-"+token.Key)
		req.Header.Add("Authorization", "Bearer sk-"+second.Key)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.Equal(t, http.StatusOK, request(path, "bearer sk-"+token.Key).Code)
		for _, changes := range []map[string]any{
			{"status": common.TokenStatusDisabled},
			{"status": common.TokenStatusExpired},
			{"expired_time": time.Now().Unix() - 1},
			{"allow_ips": "203.0.113.0/24"},
			{"deleted_at": time.Now()},
		} {
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(changes).Error)
			assert.Equal(t, http.StatusUnauthorized, request(path, "Bearer sk-"+token.Key).Code)
			require.NoError(t, model.DB.Unscoped().Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"status": common.TokenStatusExhausted, "expired_time": -1, "allow_ips": nil, "deleted_at": nil}).Error)
		}
		for _, changes := range []map[string]any{{"status": common.UserStatusDisabled}, {"deleted_at": time.Now()}} {
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Updates(changes).Error)
			assert.Equal(t, http.StatusUnauthorized, request(path, "Bearer sk-"+token.Key).Code)
			require.NoError(t, model.DB.Unscoped().Model(&model.User{}).Where("id = ?", user.Id).Updates(map[string]any{"status": common.UserStatusEnabled, "deleted_at": nil}).Error)
		}
	})

	t.Run("daylight saving boundaries", func(t *testing.T) {
		zone, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		for _, day := range []string{"2026-03-08", "2026-11-01"} {
			start, err := time.ParseInLocation(time.DateOnly, day, zone)
			require.NoError(t, err)
			end := start.AddDate(0, 0, 1)
			for i, created := range []int64{start.Unix() - 1, start.Unix(), end.Unix() - 1, end.Unix()} {
				id := day + "-" + strconv.Itoa(i)
				require.NoError(t, model.DB.Create(&model.AsyncImageTask{TaskId: id, UserId: user.Id, Status: model.ImageTaskSucceeded, CreatedAt: created}).Error)
				require.NoError(t, model.DB.Create(&model.AsyncMediaJob{TaskId: id, IdentityHash: id, UserId: user.Id, Status: "succeeded", CreatedAt: created}).Error)
			}
			stats, err := model.GetAsyncTaskStats(t.Context(), user.Id, "media", start.Add(12*time.Hour))
			require.NoError(t, err)
			assert.Equal(t, model.AsyncTaskStats{TodayRequests: 4, SuccessCount: 4}, stats)
		}
	})

	t.Run("live balance", func(t *testing.T) {
		server := miniredis.RunT(t)
		oldClient, oldBatch := common.RDB, common.BatchUpdateEnabled
		client := redis.NewClient(&redis.Options{Addr: server.Addr()})
		common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = client, true, true
		t.Cleanup(func() {
			common.RDB, common.RedisEnabled, common.BatchUpdateEnabled = oldClient, false, oldBatch
			assert.NoError(t, client.Close())
			assert.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 0).Error)
		})
		require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("quota", 6250000).Error)
		path := "/v1/images/tasks_async/stats"
		// An uncertain live ledger cannot be replaced with a stale database snapshot.
		recorder := request(path, "Bearer sk-"+token.Key)
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"code":"stats_unavailable"`)
		cacheKey := "user:" + strconv.Itoa(user.Id)
		assert.False(t, server.Exists(cacheKey))
		_, err := model.GetUserCache(user.Id)
		require.NoError(t, err)
		require.NoError(t, client.HSet(t.Context(), cacheKey, "Quota", 1250000).Err())
		assert.Equal(t, 2.5, readStats("images", token.Key)["balance"])
		require.NoError(t, client.HSet(t.Context(), cacheKey, "Quota", 0).Err())
		assert.Equal(t, float64(0), readStats("images", token.Key)["balance"])
		server.SetError("ledger unavailable")
		recorder = request(path, "Bearer sk-"+token.Key)
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"code":"stats_unavailable"`)
		server.SetError("")
		server.Del(cacheKey)
		common.BatchUpdateEnabled = false
		assert.Equal(t, 12.5, readStats("images", token.Key)["balance"])
		assert.False(t, server.Exists(cacheKey), "read-only stats do not hydrate the live quota ledger")
	})

	t.Run("credential logging", func(t *testing.T) {
		var logs bytes.Buffer
		oldWriter, db := gin.DefaultWriter, model.DB
		gin.DefaultWriter = &logs
		model.DB = db.Session(&gorm.Session{Logger: logger.New(stdlog.New(&logs, "", 0), logger.Config{LogLevel: logger.Info})})
		t.Cleanup(func() { gin.DefaultWriter, model.DB = oldWriter, db })
		logEngine := gin.New()
		middleware.SetUpLogger(logEngine)
		SetAsyncImagePublicRouter(logEngine)
		for _, kind := range []string{"images", "videos", "media"} {
			for _, authorization := range []string{"", "Bearer sk-" + token.Key} {
				req := httptest.NewRequest(http.MethodGet, "/v1/"+kind+"/tasks_async/stats?key=sk-"+token.Key, nil)
				if authorization != "" {
					req.Header.Set("Authorization", authorization)
				}
				recorder := httptest.NewRecorder()
				logEngine.ServeHTTP(recorder, req)
			}
		}
		assert.Contains(t, logs.String(), "/tasks_async/stats")
		assert.NotContains(t, logs.String(), token.Key)
		assert.NotContains(t, logs.String(), "?key=")
	})

	t.Run("rate limit", func(t *testing.T) {
		oldNum, oldDuration := common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration
		common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = true, 1, 60
		t.Cleanup(func() {
			common.GlobalApiRateLimitEnable, common.GlobalApiRateLimitNum, common.GlobalApiRateLimitDuration = false, oldNum, oldDuration
		})
		limited := gin.New()
		SetAsyncImagePublicRouter(limited)
		for i := range 2 {
			req := httptest.NewRequest(http.MethodGet, "/v1/images/tasks_async/stats", nil)
			req.RemoteAddr = "198.51.100.231:5000"
			recorder := httptest.NewRecorder()
			limited.ServeHTTP(recorder, req)
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			if i == 0 {
				assert.Equal(t, http.StatusUnauthorized, recorder.Code)
			} else {
				assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
				assert.Equal(t, "60", recorder.Header().Get("Retry-After"))
				assert.Contains(t, recorder.Body.String(), `"code":"rate_limit_exceeded"`)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Setenv("TZ", "invalid/timezone")
		recorder := request("/v1/media/tasks_async/stats", "Bearer sk-"+token.Key)
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"code":"stats_unavailable"`)
		t.Setenv("TZ", "Asia/Shanghai")
		db := model.DB
		model.DB = nil
		recorder = request("/v1/media/tasks_async/stats", "Bearer sk-"+token.Key)
		model.DB = db
		assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"code":"stats_unavailable"`)
		require.NoError(t, model.DB.Migrator().DropTable(&model.AsyncImageTask{}))
		recorder = request("/v1/media/tasks_async/stats", "Bearer sk-"+token.Key)
		assert.Equal(t, http.StatusInternalServerError, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"code":"stats_unavailable"`)
		assert.NotContains(t, recorder.Body.String(), "async_image_tasks")
	})
}

func imageUploadRequest(t *testing.T, engine *gin.Engine, data []byte, filename, key, credential string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/uploads/images_sc", &body)
	request.Header.Set("Authorization", "Bearer sk-"+credential)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Idempotency-Key", key)
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestAsyncImagePublicAdmissionRecoveryAndIsolation(t *testing.T) {
	oldEstimator := service.EstimateAsyncImageQuotaFunc
	service.EstimateAsyncImageQuotaFunc = func(context.Context, model.Token, service.AsyncImageRequest, string) (int, error) { return 1, nil }
	t.Cleanup(func() { service.EstimateAsyncImageQuotaFunc = oldEstimator })
	gin.SetMode(gin.TestMode)
	oldDB, oldLogs, oldRedis, oldEnabled, oldMemory, oldConsume, oldExport, oldExecutor := model.DB, model.LOG_DB, common.RDB, common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled, service.ExecuteAsyncImageFunc
	mainType, logType := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.RDB = oldDB, oldLogs, oldRedis
		common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled = oldEnabled, oldMemory, oldConsume, oldExport
		service.ExecuteAsyncImageFunc = oldExecutor
		common.SetDatabaseTypes(mainType, logType)
	})
	model.DB = imageRouterDatabase(t, "main")
	model.LOG_DB = imageRouterDatabase(t, "logs")
	imageLogs := model.LOG_DB
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	if os.Getenv("ASYNC_IMAGE_TEST_MYSQL_DSN") != "" {
		common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	}
	if os.Getenv("ASYNC_IMAGE_TEST_PG_DSN") != "" {
		common.SetDatabaseTypes(common.DatabaseTypePostgreSQL, common.DatabaseTypePostgreSQL)
	}
	t.Setenv("LOG_SQL_DSN", "")
	master := common.IsMasterNode
	common.IsMasterNode = false
	err := model.InitLogDB()
	common.IsMasterNode = master
	require.NoError(t, err)
	model.LOG_DB = imageLogs
	common.MemoryCacheEnabled, common.LogConsumeEnabled, common.DataExportEnabled = false, true, true
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.QuotaData{}))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	require.NoError(t, model.MigrateImageLogs(model.LOG_DB))
	require.NoError(t, model.MigrateImageModels(model.DB))
	require.NoError(t, model.MigrateImageModels(model.DB))
	prefix := "new-api:image-test:" + common.GetUUID()
	t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", prefix)
	var options *redis.Options
	if url := os.Getenv("ASYNC_IMAGE_TEST_REDIS_URL"); url != "" {
		var err error
		options, err = redis.ParseURL(url)
		require.NoError(t, err)
	} else {
		server := miniredis.RunT(t)
		options = &redis.Options{Addr: server.Addr()}
	}
	client := redis.NewClient(options)
	client.AddHook(imageRedisTestHook{prefix: prefix})
	common.RDB, common.RedisEnabled = client, true
	require.NoError(t, client.Ping(context.Background()).Err())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, prefix+":*", 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				assert.NoError(t, client.Del(ctx, keys...).Err())
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		_ = client.Close()
	})
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	cfg := service.DefaultImageRuntimeConfig()
	cfg.AsyncEnabled = true
	require.NoError(t, service.SaveImageRuntimeConfig(context.Background(), cfg))
	profile, err := service.SaveImageStorageProfile(context.Background(), model.ImageStorageProfile{Class: "temporary", Backend: "local", Provider: "local", Root: t.TempDir(), Active: true}, nil)
	require.NoError(t, err)
	user := model.User{Username: "image-api-user", Group: "default", Status: common.UserStatusEnabled, Quota: 1000}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "imageapitestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000, Group: "default"}
	require.NoError(t, model.DB.Create(&token).Error)
	other := model.Token{UserId: user.Id, Key: "imageapiotherkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000, Group: "default"}
	require.NoError(t, model.DB.Create(&other).Error)
	catalog, err := common.Marshal([]service.ImageModelCapability{{Id: "image-model", MaxOutputImages: 2, MaxReferenceImages: 3, Resolutions: []string{"1K", "2K", "4K"}}})
	require.NoError(t, err)
	policy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "openai"), Group: "default", Platform: "openai", Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: string(catalog), Version: 1}
	require.NoError(t, model.DB.Create(&policy).Error)
	channel := model.Channel{Key: "fake-upstream", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "image-model", Group: "default"}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "image-model", ChannelId: channel.Id, Enabled: true}).Error)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	SetAsyncImagePublicRouter(engine)
	request := func(method, path, body, key, credential string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer sk-"+credential)
		request.Header.Set("Content-Type", "application/json")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		engine.ServeHTTP(recorder, request)
		return recorder
	}
	body := `{"model":"image-model","prompt":"private image prompt","n":2,"resolution":"1K","aspect_ratio":"1:1"}`
	accepted := request("POST", "/v1/images/generations_oa", body, "request-1", token.Key)
	require.Equal(t, 202, accepted.Code, accepted.Body.String())
	assert.Equal(t, "3", accepted.Header().Get("Retry-After"))
	assert.Equal(t, "no-store", accepted.Header().Get("Cache-Control"))
	var payload struct {
		TaskId string `json:"task_id"`
	}
	require.NoError(t, common.Unmarshal(accepted.Body.Bytes(), &payload))
	require.True(t, strings.HasPrefix(payload.TaskId, "asyncimg_"))
	replay := request("POST", "/v1/images/generations_oa", body, "request-1", token.Key)
	require.Equal(t, 202, replay.Code)
	assert.JSONEq(t, accepted.Body.String(), replay.Body.String())
	changed := request("POST", "/v1/images/generations_oa", body+" ", "request-1", token.Key)
	assert.Equal(t, 409, changed.Code)
	assert.Equal(t, 404, request("GET", "/v1/images/tasks_async/"+payload.TaskId, "", "", other.Key).Code)
	assert.Equal(t, 401, request("GET", "/v1/images/tasks_async/"+payload.TaskId, "", "", "invalidkey").Code)
	var invocations int
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	valid, err := service.ValidateImageBytes(bitmap.Bytes(), "image/png", 1<<20, 100)
	require.NoError(t, err)
	service.ExecuteAsyncImageFunc = func(ctx context.Context, task model.AsyncImageTask, _ service.AsyncImageRequest, selected *model.Channel, _ service.ImageRuntimeConfig, dispatch func() error) (*service.AsyncImageOutput, model.AsyncImageBill, error) {
		invocations++
		require.Equal(t, channel.Id, selected.Id)
		if err := dispatch(); err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		require.NoError(t, model.DB.WithContext(ctx).Model(&model.ImageStorageProfile{}).Where("profile_id = ?", profile.ProfileId).Update("active", false).Error)
		log := model.Log{UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Quota: 50, Type: model.LogTypeConsume, RequestId: "async-image:" + task.TaskId, CreatedAt: time.Now().Unix()}
		encoded, err := common.Marshal(log)
		if err != nil {
			return nil, model.AsyncImageBill{}, err
		}
		return &service.AsyncImageOutput{Images: []service.ImageBytes{valid, valid}}, model.AsyncImageBill{TaskId: task.TaskId, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, BillingRequestId: log.RequestId, Fingerprint: "fixed-api-bill", Quota: 50, FundingSource: "wallet", LogPayload: string(encoded)}, nil
	}
	require.NoError(t, service.RunAsyncImageTask(context.Background(), payload.TaskId))
	var task model.AsyncImageTask
	require.NoError(t, model.DB.Where("task_id = ?", payload.TaskId).Take(&task).Error)
	assert.Equal(t, model.ImageTaskStorageFailed, task.Status)
	assert.Equal(t, 1, invocations)
	assert.Empty(t, task.RequestCipher)
	require.NoError(t, model.DB.Model(&model.ImageStorageProfile{}).Where("profile_id = ?", profile.ProfileId).Update("active", true).Error)
	require.NoError(t, model.ResumeAsyncImageTask(context.Background(), task))
	require.NoError(t, service.RunAsyncImageTask(context.Background(), payload.TaskId))
	assert.Equal(t, 1, invocations, "post-processing recovery must never regenerate")
	_ = service.RunAsyncImageTask(context.Background(), payload.TaskId)
	assert.Equal(t, 1, invocations)
	query := request("GET", "/v1/tasks_sc/"+payload.TaskId, "", "", token.Key)
	require.Equal(t, 200, query.Code, query.Body.String())
	var success struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(query.Body.Bytes(), &success))
	assert.Equal(t, "succeeded", success.Status)
	assert.Len(t, success.Data, 2)
	assert.NotContains(t, query.Body.String(), "private image prompt")
	assert.NotContains(t, query.Body.String(), profile.Root)
	require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
	assert.Equal(t, 950, user.Quota)
	require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&token).Error)
	assert.Equal(t, 950, token.RemainQuota)
	var logCount int64
	require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", "async-image:"+payload.TaskId).Count(&logCount).Error)
	assert.EqualValues(t, 1, logCount)
	// Management ownership is broader than the public submitting-token scope.
	management := gin.New()
	management.GET("/own/:task_id", func(c *gin.Context) { c.Set("id", user.Id); controller.GetImageTask(c, false) })
	management.GET("/foreign/:task_id", func(c *gin.Context) { c.Set("id", user.Id+100); controller.GetImageTask(c, false) })
	for path, status := range map[string]int{"/own/": 200, "/foreign/": 404} {
		recorder := httptest.NewRecorder()
		management.ServeHTTP(recorder, httptest.NewRequest("GET", path+payload.TaskId, nil))
		assert.Equal(t, status, recorder.Code)
	}
	geminiPolicy := policy
	geminiPolicy.Id, geminiPolicy.Platform = 0, "gemini"
	geminiPolicy.PolicyKey = service.ImageIdentityHash("default", "gemini")
	geminiPolicy.AsyncEnabled = false
	require.NoError(t, model.DB.Create(&geminiPolicy).Error)
	geminiChannel := channel
	geminiChannel.Id, geminiChannel.Type = 0, constant.ChannelTypeGemini
	geminiMapping := `{"image-model":"gemini-2.5-flash-image"}`
	geminiChannel.ModelMapping = &geminiMapping
	require.NoError(t, model.DB.Create(&geminiChannel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "image-model", ChannelId: geminiChannel.Id, Enabled: true}).Error)
	assert.Equal(t, 403, request("POST", "/v1/uploads/images_sc", "{}", "disabled-upload", token.Key).Code)
	var ticketCount int64
	require.NoError(t, model.DB.Model(&model.ImageUploadAdmissionTicket{}).Where("token_id = ?", token.Id).Count(&ticketCount).Error)
	assert.Zero(t, ticketCount, "disabled policy must not consume an attempt")
	require.NoError(t, model.DB.Model(&model.ImageGroupPolicy{}).Where("id = ?", geminiPolicy.Id).Update("async_enabled", true).Error)
	cfg.UploadsPerMinute = 4
	require.NoError(t, service.SaveImageRuntimeConfig(context.Background(), cfg))
	upload := imageUploadRequest(t, engine, valid.Data, "../original.png", "upload-1", token.Key)
	require.Equal(t, 200, upload.Code, upload.Body.String())
	var uploaded struct {
		URL       string `json:"url"`
		CreatedAt int64  `json:"created_at"`
	}
	require.NoError(t, common.Unmarshal(upload.Body.Bytes(), &uploaded))
	replayUpload := imageUploadRequest(t, engine, valid.Data, "original.png", "upload-1", token.Key)
	require.Equal(t, 200, replayUpload.Code, replayUpload.Body.String())
	var resigned struct {
		URL       string `json:"url"`
		CreatedAt int64  `json:"created_at"`
	}
	require.NoError(t, common.Unmarshal(replayUpload.Body.Bytes(), &resigned))
	assert.Equal(t, uploaded.CreatedAt, resigned.CreatedAt)
	originalURL, err := url.Parse(uploaded.URL)
	require.NoError(t, err)
	resignedURL, err := url.Parse(resigned.URL)
	require.NoError(t, err)
	assert.Equal(t, originalURL.Path, resignedURL.Path, "re-signing preserves stable object identity")
	assert.Equal(t, "true", replayUpload.Header().Get("X-Idempotency-Replayed"))
	assert.Equal(t, 409, imageUploadRequest(t, engine, valid.Data, "different.png", "upload-1", token.Key).Code)
	assert.Equal(t, 400, request("POST", "/v1/uploads/images_sc", "{}", "bad-upload", token.Key).Code)
	limited := request("POST", "/v1/uploads/images_sc", "{}", "limited-upload", token.Key)
	assert.Equal(t, 429, limited.Code)
	assert.Equal(t, "60", limited.Header().Get("Retry-After"))
	scBody, err := common.Marshal(map[string]any{"model": "image-model", "prompt": "Edit the test original", "image_urls": []string{uploaded.URL}, "resolution": "1K", "ratio": "1:1"})
	require.NoError(t, err)
	assert.Equal(t, 202, request("POST", "/v1/images/generations_sc", string(scBody), "sc-generation", token.Key).Code)
	assert.Equal(t, 400, request("POST", "/v1/images/generations_sc", string(scBody), "sc-cross-token", other.Key).Code)
	bbBody, err := common.Marshal(map[string]any{"model": "image-model", "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Edit the test original"}, map[string]any{"type": "image_url", "image_url": map[string]string{"url": uploaded.URL}}}}}})
	require.NoError(t, err)
	assert.Equal(t, 202, request("POST", "/v1/chat/completions_gm", string(bbBody), "bb-generation", token.Key).Code)
	editBody, err := common.Marshal(map[string]any{"model": "image-model", "prompt": "Edit the original", "images": []any{map[string]string{"image_url": uploaded.URL}}})
	require.NoError(t, err)
	assert.Equal(t, 202, request("POST", "/v1/images/edits_oa", string(editBody), "oa-edit", token.Key).Code)
	require.NoError(t, model.DB.Model(&model.ImageInputObject{}).Where("token_id = ?", token.Id).Update("expires_at", time.Now().Unix()-1).Error)
	assert.Equal(t, 400, request("POST", "/v1/images/generations_sc", string(scBody), "sc-tombstone", token.Key).Code)
	cfg.InputBytesPerKey = int64(len(valid.Data)) - 1
	require.NoError(t, service.SaveImageRuntimeConfig(context.Background(), cfg))
	assert.Equal(t, 409, imageUploadRequest(t, engine, valid.Data, "quota.png", "upload-quota", other.Key).Code)
	panicAdmission := request("POST", "/v1/images/generations_oa", body, "panic-after-dispatch", token.Key)
	require.Equal(t, 202, panicAdmission.Code)
	var interrupted struct {
		TaskId string `json:"task_id"`
	}
	require.NoError(t, common.Unmarshal(panicAdmission.Body.Bytes(), &interrupted))
	service.ExecuteAsyncImageFunc = func(_ context.Context, _ model.AsyncImageTask, _ service.AsyncImageRequest, _ *model.Channel, _ service.ImageRuntimeConfig, dispatch func() error) (*service.AsyncImageOutput, model.AsyncImageBill, error) {
		require.NoError(t, dispatch())
		panic("synthetic interruption after dispatch")
	}
	assert.Error(t, service.RunAsyncImageTask(context.Background(), interrupted.TaskId))
	require.NoError(t, model.DB.Model(&model.AsyncImageTask{}).Where("task_id = ?", interrupted.TaskId).Update("lease_expires_at", time.Now().Unix()-1).Error)
	require.NoError(t, model.RecoverAsyncImageTasks(context.Background(), 100, cfg.ExecutionTimeout))
	var unknown model.AsyncImageTask
	require.NoError(t, model.DB.Where("task_id = ?", interrupted.TaskId).Take(&unknown).Error)
	assert.Equal(t, model.ImageTaskExecutionUnknown, unknown.Status)
	assert.Empty(t, unknown.RequestCipher)
	assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), interrupted.TaskId), model.ErrImageConflict)
	for _, tc := range []struct {
		name                      string
		updates                   map[string]any
		queryStatus, submitStatus int
	}{
		{"expired", map[string]any{"expired_time": time.Now().Unix() - 1}, 401, 401},
		{"IP denied", map[string]any{"expired_time": -1, "allow_ips": "127.0.0.1"}, 403, 403},
		{"exhausted read only", map[string]any{"allow_ips": nil, "status": common.TokenStatusExhausted}, 200, 401},
		{"disabled", map[string]any{"status": common.TokenStatusDisabled}, 401, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(tc.updates).Error)
			assert.Equal(t, tc.queryStatus, request("GET", "/v1/images/tasks_async/"+payload.TaskId, "", "", token.Key).Code)
			assert.Equal(t, tc.submitStatus, request("POST", "/v1/images/generations_oa", body, common.GetUUID(), token.Key).Code)
		})
	}
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusEnabled).Error)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("remain_quota", 0).Error)
	require.NoError(t, client.HSet(t.Context(), "token:"+common.GenerateHMAC(token.Key), "RemainQuota", 0).Err())
	require.NoError(t, model.AdjustUserQuotaDurable(user.Id, -user.Quota))
	oldPrices := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { assert.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrices)) })
	priceMap := ratio_setting.GetModelPriceCopy()
	priceMap["image-model"] = 0
	encodedPrices, err := common.Marshal(priceMap)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encodedPrices)))
	countToken := constant.CountToken
	constant.CountToken = false
	t.Cleanup(func() { constant.CountToken = countToken })
	service.EstimateAsyncImageQuotaFunc = relay.EstimateAsyncImageQuota
	free := request("POST", "/v1/images/generations_oa", body, "free-model", token.Key)
	assert.Equal(t, 202, free.Code, free.Body.String())
	priceMap["image-model"] = 0.01
	encodedPrices, err = common.Marshal(priceMap)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encodedPrices)))
	paid := request("POST", "/v1/images/generations_oa", body, "paid-model", token.Key)
	assert.Equal(t, 403, paid.Code, paid.Body.String())
	t.Run("native upstream billing and ambiguous response", func(t *testing.T) {
		service.InitHttpClient()
		var responseBody atomic.Value
		var expectedPath atomic.Value
		var responseRequestID atomic.Value
		var responseStatus atomic.Int64
		var calls atomic.Int64
		expectedPath.Store("/v1/images/generations")
		responseRequestID.Store("")
		responseStatus.Store(http.StatusOK)
		responseBody.Store(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(valid.Data) + `"},{"b64_json":"` + base64.StdEncoding.EncodeToString(valid.Data) + `"}]}`)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, expectedPath.Load().(string), r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			if requestID := responseRequestID.Load().(string); requestID != "" {
				w.Header().Set("X-Goog-Request-Id", requestID)
			}
			w.WriteHeader(int(responseStatus.Load()))
			_, _ = w.Write([]byte(responseBody.Load().(string)))
		}))
		defer upstream.Close()
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("base_url", upstream.URL).Error)
		require.NoError(t, model.AdjustUserQuotaDurable(user.Id, 1000000))
		// This fixture allocates token quota; it is not a usage refund. Keep
		// the existing cache balance aligned without changing UsedQuota.
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("remain_quota", 1000000).Error)
		require.NoError(t, client.HSet(t.Context(), "token:"+common.GenerateHMAC(token.Key), "RemainQuota", 1000000).Err())
		service.ExecuteAsyncImageFunc = func(ctx context.Context, task model.AsyncImageTask, native service.AsyncImageRequest, selected *model.Channel, runtime service.ImageRuntimeConfig, dispatch func() error) (*service.AsyncImageOutput, model.AsyncImageBill, error) {
			images, bill, err := relay.ExecuteAsyncImage(ctx, task, native, selected, runtime, dispatch)
			if err == nil {
				err = model.DB.Model(&model.ImageStorageProfile{}).Where("profile_id = ?", profile.ProfileId).Update("active", false).Error
			}
			return images, bill, err
		}
		realAdmission := request("POST", "/v1/images/generations_oa", body, "native-upstream", token.Key)
		require.Equal(t, 202, realAdmission.Code, realAdmission.Body.String())
		var nativeTask struct {
			TaskId string `json:"task_id"`
		}
		require.NoError(t, common.Unmarshal(realAdmission.Body.Bytes(), &nativeTask))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		task = model.AsyncImageTask{}
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		require.Equal(t, model.ImageTaskStorageFailed, task.Status, "native execution failed: %s %s", task.ErrorCode, task.ErrorMessage)
		var fixed model.AsyncImageBill
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&fixed).Error)
		assert.Equal(t, 10000, fixed.Quota, "two actual images, native upstream reports no token usage")
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		require.Equal(t, model.ImageTaskStorageFailed, task.Status)
		assert.EqualValues(t, 1, calls.Load())
		// A later price change cannot change an already persisted bill.
		priceMap["image-model"] = 0.02
		prices, err := common.Marshal(priceMap)
		require.NoError(t, err)
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(prices)))
		require.NoError(t, model.DB.Model(&model.ImageStorageProfile{}).Where("profile_id = ?", profile.ProfileId).Update("active", true).Error)
		require.NoError(t, model.ResumeAsyncImageTask(context.Background(), task))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
		assert.EqualValues(t, 1, calls.Load(), "postprocessing and duplicate delivery never call upstream again")
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
		require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&token).Error)
		assert.Equal(t, 990000, user.Quota)
		assert.Equal(t, 990000, token.RemainQuota)
		var consume model.Log
		require.NoError(t, model.LOG_DB.Where("request_id = ?", fixed.BillingRequestId).Take(&consume).Error)
		assert.Equal(t, 10000, consume.Quota)
		assert.Zero(t, consume.PromptTokens)
		assert.Zero(t, consume.CompletionTokens)
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", fixed.BillingRequestId).Count(&logCount).Error)
		assert.EqualValues(t, 1, logCount)
		// A broken HTTP 200 body can follow billable generation. Never retry it.
		service.ExecuteAsyncImageFunc = relay.ExecuteAsyncImage
		responseBody.Store(`{"data":[`)
		broken := request("POST", "/v1/images/generations_oa", body, "native-broken-response", token.Key)
		require.Equal(t, 202, broken.Code, broken.Body.String())
		require.NoError(t, common.Unmarshal(broken.Body.Bytes(), &nativeTask))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		task = model.AsyncImageTask{}
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
		assert.Zero(t, task.NextAttemptAt)
		assert.Empty(t, task.RequestCipher)
		assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
		assert.EqualValues(t, 2, calls.Load())
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
		assert.Equal(t, 970000, user.Quota, "ambiguous generation retains its reservation until reconciliation")
		var unknownBill model.AsyncImageBill
		require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&unknownBill).Error)
		assert.Equal(t, "reserved", unknownBill.Status)
		assert.Equal(t, 20000, unknownBill.ReservedQuota)
		assert.ErrorIs(t, model.TerminateAsyncImageTask(t.Context(), task), model.ErrImageConflict)
		require.NoError(t, model.TerminateAsyncImageTaskAfterReconciliation(t.Context(), task))
		// Gemini uses its native protocol and the same durable settlement chain.
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", geminiChannel.Id).Update("base_url", upstream.URL).Error)
		expectedPath.Store("/v1beta/models/gemini-2.5-flash-image:generateContent")
		inline := map[string]any{"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(valid.Data)}}
		geminiResponse, err := common.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{inline, inline}}}}, "usageMetadata": map[string]int{"promptTokenCount": 10, "candidatesTokenCount": 20, "totalTokenCount": 30}})
		require.NoError(t, err)
		responseBody.Store(string(geminiResponse))
		gmBody := `{"model":"image-model","messages":[{"role":"user","content":"Generate test images"}]}`
		gmAdmission := request("POST", "/v1/chat/completions_gm", gmBody, "native-gemini", token.Key)
		require.Equal(t, 202, gmAdmission.Code, gmAdmission.Body.String())
		require.NoError(t, common.Unmarshal(gmAdmission.Body.Bytes(), &nativeTask))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		task = model.AsyncImageTask{}
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		require.Equal(t, model.ImageTaskSucceeded, task.Status, "%s %s", task.ErrorCode, task.ErrorMessage)
		assert.Equal(t, 2, task.ImageCount)
		assert.Equal(t, 20000, task.Quota)
		assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
		assert.EqualValues(t, 3, calls.Load())
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
		require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&token).Error)
		assert.Equal(t, 970000, user.Quota)
		assert.Equal(t, 970000, token.RemainQuota)
		// Gemini failures must be classified from the original provider message,
		// while persisted diagnostics remain safe for public and admin views.
		responseStatus.Store(http.StatusBadRequest)
		responseRequestID.Store("gemini-provider-request-611")
		responseBody.Store(`{"error":{"code":"too_many_reference_images","message":"image: at most 8 images are allowed; Bearer private-key; https://signed.example/path?secret=token","status":"INVALID_ARGUMENT"}}`)
		gmRejected := request("POST", "/v1/chat/completions_gm", gmBody, "native-gemini-rejected", token.Key)
		require.Equal(t, 202, gmRejected.Code, gmRejected.Body.String())
		require.NoError(t, common.Unmarshal(gmRejected.Body.Bytes(), &nativeTask))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		task = model.AsyncImageTask{}
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		assert.Equal(t, model.ImageTaskFailed, task.Status)
		assert.Equal(t, 611, task.PublicErrorCode)
		assert.Contains(t, task.ErrorMessage, "at most 8 images are allowed")
		assert.NotContains(t, task.ErrorMessage, "private-key")
		assert.NotContains(t, task.ErrorMessage, "signed.example")
		var geminiAttempts []service.ImageChannelAttempt
		require.NoError(t, common.UnmarshalJsonStr(task.Attempts, &geminiAttempts))
		require.NotEmpty(t, geminiAttempts)
		geminiAttempt := geminiAttempts[len(geminiAttempts)-1]
		assert.Equal(t, http.StatusBadRequest, geminiAttempt.HTTPStatus)
		assert.Equal(t, "too_many_reference_images", geminiAttempt.ProviderCode)
		assert.Equal(t, "INVALID_ARGUMENT", geminiAttempt.ProviderStatus)
		assert.Equal(t, "gemini-provider-request-611", geminiAttempt.UpstreamRequestID)
		assert.Equal(t, task.ErrorMessage, geminiAttempt.ErrorMessage)
		gmRejectedQuery := request("GET", "/v1/tasks_sc/"+nativeTask.TaskId, "", "", token.Key)
		require.Equal(t, 200, gmRejectedQuery.Code, gmRejectedQuery.Body.String())
		var rejectedResult struct {
			Status     string `json:"status"`
			ErrorCode  int    `json:"error_code"`
			FailReason string `json:"fail_reason"`
		}
		require.NoError(t, common.Unmarshal(gmRejectedQuery.Body.Bytes(), &rejectedResult))
		assert.Equal(t, "failed", rejectedResult.Status)
		assert.Equal(t, 611, rejectedResult.ErrorCode)
		assert.Equal(t, task.ErrorMessage, rejectedResult.FailReason)
		responseStatus.Store(http.StatusOK)
		responseRequestID.Store("")
		responseBody.Store(`{"candidates":[`)
		gmBroken := request("POST", "/v1/chat/completions_gm", gmBody, "native-gemini-broken", token.Key)
		require.Equal(t, 202, gmBroken.Code, gmBroken.Body.String())
		require.NoError(t, common.Unmarshal(gmBroken.Body.Bytes(), &nativeTask))
		require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
		task = model.AsyncImageTask{}
		require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
		assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
		assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
		assert.EqualValues(t, 5, calls.Load())
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
		assert.Equal(t, 960000, user.Quota, "Gemini ambiguous execution keeps its estimated reservation")
		require.NoError(t, model.TerminateAsyncImageTaskAfterReconciliation(t.Context(), task))
		pricing := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
		previousPricing := *pricing
		defer func() { *pricing = previousPricing }()
		*pricing = billing_setting.BillingSetting{BillingMode: map[string]string{"image-model": billing_setting.BillingModeTieredExpr}, BillingExpr: map[string]string{"image-model": `tier("tokens", p * 2 + c * 8)`}}
		for _, platform := range []string{"gemini", "openai"} {
			path, nativeBody := "/v1/chat/completions_gm", gmBody
			responseBody.Store(string(geminiResponse))
			if platform == "openai" {
				path, nativeBody = "/v1/images/generations_oa", body
				expectedPath.Store("/v1/images/generations")
				upstreamBody, err := common.Marshal(map[string]any{"data": []any{map[string]string{"b64_json": base64.StdEncoding.EncodeToString(valid.Data)}, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(valid.Data)}}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 20, "total_tokens": 30}})
				require.NoError(t, err)
				responseBody.Store(string(upstreamBody))
			}
			qualified := request("POST", path, nativeBody, "native-token-priced-"+platform, token.Key)
			require.Equal(t, 202, qualified.Code, qualified.Body.String())
			require.NoError(t, common.Unmarshal(qualified.Body.Bytes(), &nativeTask))
			require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
			task = model.AsyncImageTask{}
			require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
			require.Equal(t, model.ImageTaskSucceeded, task.Status, "%s %s", task.ErrorCode, task.ErrorMessage)
			assert.Equal(t, 90, task.Quota, "actual input/output tokens are charged once across both images")
			assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
			// Fixed image pricing above permits absent usage. Paid token pricing
			// must neither treat it as free nor replace it with a local estimate.
			if platform == "gemini" {
				missing, err := common.Marshal(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{inline, inline}}}}})
				require.NoError(t, err)
				responseBody.Store(string(missing))
			} else {
				responseBody.Store(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(valid.Data) + `"},{"b64_json":"` + base64.StdEncoding.EncodeToString(valid.Data) + `"}]}`)
			}
			missing := request("POST", path, nativeBody, "native-missing-usage-"+platform, token.Key)
			require.Equal(t, 202, missing.Code, missing.Body.String())
			require.NoError(t, common.Unmarshal(missing.Body.Bytes(), &nativeTask))
			require.NoError(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId))
			task = model.AsyncImageTask{}
			require.NoError(t, model.DB.Where("task_id = ?", nativeTask.TaskId).Take(&task).Error)
			assert.Equal(t, model.ImageTaskExecutionUnknown, task.Status)
			assert.ErrorIs(t, service.RunAsyncImageTask(context.Background(), nativeTask.TaskId), model.ErrImageConflict)
			var held model.AsyncImageBill
			require.NoError(t, model.DB.Where("task_id = ?", task.TaskId).Take(&held).Error)
			assert.Equal(t, "reserved", held.Status)
			assert.Positive(t, held.ReservedQuota, "missing usage cannot turn accepted work into a free request")
			require.NoError(t, model.TerminateAsyncImageTaskAfterReconciliation(t.Context(), task))
		}
		assert.EqualValues(t, 9, calls.Load())
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&user).Error)
		require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&token).Error)
		assert.Equal(t, 969820, user.Quota)
		assert.Equal(t, 969820, token.RemainQuota)
	})

	t.Run("async-video-local-recovery-and-range", func(t *testing.T) {
		require.NoError(t, model.DB.AutoMigrate(&model.Task{}, &model.AsyncMediaJob{}, &model.MediaArtifactObject{}))
		previousSubmit, previousPersist, previousAdaptor := service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc, service.GetTaskAdaptorFunc
		service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc = controller.ExecuteAsyncVideo, controller.PersistAsyncVideo
		service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
		t.Cleanup(func() {
			service.SubmitAsyncVideoFunc, service.PersistAsyncVideoFunc, service.GetTaskAdaptorFunc = previousSubmit, previousPersist, previousAdaptor
		})
		originalFetch := *system_setting.GetFetchSetting()
		system_setting.GetFetchSetting().AllowPrivateIp = true
		system_setting.GetFetchSetting().AllowedPorts = []string{"1-65535"}
		t.Cleanup(func() { *system_setting.GetFetchSetting() = originalFetch })
		oldSecret := common.CryptoSecret
		common.CryptoSecret = "video-end-to-end-signing-secret"
		t.Cleanup(func() { common.CryptoSecret = oldSecret })
		mediaCfg := service.DefaultMediaRuntimeConfig()
		mediaCfg.VideoAsyncEnabled, mediaCfg.LocalPath, mediaCfg.MaxFileBytes = true, t.TempDir(), 4096
		require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), mediaCfg))
		box := func(kind string, payload []byte) []byte {
			result := make([]byte, 8+len(payload))
			binary.BigEndian.PutUint32(result, uint32(len(result)))
			copy(result[4:8], kind)
			copy(result[8:], payload)
			return result
		}
		handler := make([]byte, 25)
		copy(handler[8:12], "vide")
		video := box("ftyp", []byte("isom\x00\x00\x00\x00"))
		video = append(video, box("moov", box("trak", box("mdia", box("hdlr", handler))))...)
		video = append(video, box("mdat", []byte("local-video-sample"))...)
		var submits, xaiSubmits, downloads atomic.Int32
		upper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/v1/videos":
				assert.Equal(t, "Bearer frozen-video-key", r.Header.Get("Authorization"))
				submits.Add(1)
				var sent map[string]any
				if !assert.NoError(t, common.DecodeJson(r.Body, &sent)) {
					w.WriteHeader(400)
					return
				}
				assert.Equal(t, "sora-2", sent["model"])
				assert.NotContains(t, sent, "provider")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"upstream-video","status":"queued","model":"sora-2"}`))
			case r.Method == "POST" && r.URL.Path == "/v1/videos/edits":
				assert.Equal(t, "Bearer xai-frozen-video-key", r.Header.Get("Authorization"))
				xaiSubmits.Add(1)
				var sent map[string]any
				if !assert.NoError(t, common.DecodeJson(r.Body, &sent)) {
					w.WriteHeader(400)
					return
				}
				assert.Equal(t, "grok-imagine-video", sent["model"])
				assert.Equal(t, "https://cdn.example/source.mp4", sent["video"].(map[string]any)["url"])
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"request_id":"upstream-xai-edit","status":"pending"}`))
			case r.URL.Path == "/v1/videos/upstream-video":
				assert.Equal(t, "Bearer frozen-video-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"upstream-video","status":"completed","model":"sora-2"}`))
			case r.URL.Path == "/v1/videos/upstream-video/content":
				assert.Equal(t, "Bearer frozen-video-key", r.Header.Get("Authorization"))
				if downloads.Add(1) == 1 {
					_, _ = w.Write([]byte("<html>temporary download failure</html>"))
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(video)
			default:
				w.WriteHeader(404)
			}
		}))
		defer upper.Close()
		base, setting := upper.URL, `{"task_plugin_key":"sora"}`
		videoChannel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "frozen-video-key", BaseURL: &base, Setting: &setting, Status: common.ChannelStatusEnabled, Models: "sora-2", Group: "default"}
		require.NoError(t, model.DB.Create(&videoChannel).Error)
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "sora-2", ChannelId: videoChannel.Id, Enabled: true}).Error)
		xaiCatalog, err := common.Marshal([]service.ImageModelCapability{{Id: "grok-imagine-video", Label: "Grok Imagine Video", MediaType: "video"}})
		require.NoError(t, err)
		xaiPolicy := model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "xai"), Group: "default", Platform: "xai", Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: string(xaiCatalog), Version: 1}
		require.NoError(t, model.DB.Create(&xaiPolicy).Error)
		xaiChannel := model.Channel{Type: constant.ChannelTypeXai, Key: "xai-frozen-video-key", BaseURL: &base, Status: common.ChannelStatusEnabled, Models: "grok-imagine-video", Group: "default"}
		require.NoError(t, model.DB.Create(&xaiChannel).Error)
		require.NoError(t, model.DB.Create(&model.Ability{Group: "default", Model: "grok-imagine-video", ChannelId: xaiChannel.Id, Enabled: true}).Error)
		xaiEditBody := `{"provider":"xai","model":"grok-imagine-video","prompt":"replace the sky","video":"https://cdn.example/source.mp4"}`
		xaiEdit := request("POST", "/v1/videos/edits_async", xaiEditBody, "video-operation-scope", token.Key)
		require.Equal(t, 202, xaiEdit.Code, xaiEdit.Body.String())
		var xaiAccepted struct {
			TaskID string `json:"task_id"`
		}
		require.NoError(t, common.Unmarshal(xaiEdit.Body.Bytes(), &xaiAccepted))
		assert.Equal(t, 202, request("POST", "/v1/videos/edits_async", xaiEditBody, "video-operation-scope", token.Key).Code)
		xaiExtensionBody := `{"provider":"xai","model":"grok-imagine-video","prompt":"continue backward","video":{"file_id":"file-video"},"seconds":6,"extension_direction":"backward"}`
		assert.Equal(t, 409, request("POST", "/v1/videos/extensions_async", xaiExtensionBody, "video-operation-scope", token.Key).Code)
		var xaiJob model.AsyncMediaJob
		require.NoError(t, model.DB.Where("task_id = ?", xaiAccepted.TaskID).Take(&xaiJob).Error)
		xaiFrozenPayload, err := service.DecryptImagePayload(xaiJob.RequestCipher, "media-request:"+xaiJob.TaskId)
		require.NoError(t, err)
		var xaiFrozen service.AsyncVideoRequest
		require.NoError(t, common.Unmarshal(xaiFrozenPayload, &xaiFrozen))
		assert.Equal(t, "edit", xaiFrozen.Operation)
		claimed, err := model.ClaimAsyncMediaJob(t.Context(), &xaiJob, common.GetUUID(), 30)
		require.NoError(t, err)
		require.True(t, claimed)
		require.NoError(t, service.ProcessAsyncMediaJob(t.Context(), xaiJob, mediaCfg))
		assert.EqualValues(t, 1, xaiSubmits.Load())
		var xaiTask model.Task
		require.NoError(t, model.DB.Where("task_id = ?", xaiAccepted.TaskID).Take(&xaiTask).Error)
		assert.Equal(t, "edit_video", xaiTask.Action)
		assert.Equal(t, "upstream-xai-edit", xaiTask.GetUpstreamTaskID())
		var multipartEdit bytes.Buffer
		multipartEditWriter := multipart.NewWriter(&multipartEdit)
		require.NoError(t, multipartEditWriter.WriteField("model", "grok-imagine-video"))
		require.NoError(t, multipartEditWriter.WriteField("prompt", "replace the sky"))
		require.NoError(t, multipartEditWriter.WriteField("video", "https://cdn.example/source.mp4"))
		require.NoError(t, multipartEditWriter.Close())
		multipartEditRequest := httptest.NewRequest("POST", "/v1/videos/edits_async", &multipartEdit)
		multipartEditRequest.Header.Set("Authorization", "Bearer sk-"+token.Key)
		multipartEditRequest.Header.Set("Content-Type", multipartEditWriter.FormDataContentType())
		multipartEditResponse := httptest.NewRecorder()
		engine.ServeHTTP(multipartEditResponse, multipartEditRequest)
		assert.Equal(t, 400, multipartEditResponse.Code)
		historyEditBody := `{"provider":"xai","model":"grok-imagine-video","prompt":"replace the sky","video":"https://cdn.example/source.mp4","source_task_id":"old-task"}`
		historyEdit := request("POST", "/v1/videos/edits_async", historyEditBody, "video-history-source", token.Key)
		assert.Equal(t, 400, historyEdit.Code)
		assert.Contains(t, historyEdit.Body.String(), "source_task_id")
		originalPrices := ratio_setting.GetModelPriceCopy()
		originalPriceJSON, err := common.Marshal(originalPrices)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = ratio_setting.UpdateModelPriceByJSONString(string(originalPriceJSON))
		})
		originalPrices["sora-2"] = 0.001
		encoded, err := common.Marshal(originalPrices)
		require.NoError(t, err)
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
		videoBody := `{"model":"sora-2","provider":"sora","prompt":"a river","seconds":4,"size":"1280x720"}`
		response := request("POST", "/v1/videos/generations_async", videoBody, "video-local-1", token.Key)
		require.Equal(t, 202, response.Code, response.Body.String())
		require.Zero(t, submits.Load(), "HTTP admission must not wait for generation")
		var accepted struct {
			TaskId string `json:"task_id"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &accepted))
		assert.Equal(t, 202, request("POST", "/v1/videos/generations_async", videoBody, "video-local-1", token.Key).Code)
		assert.Equal(t, 409, request("POST", "/v1/videos/generations_async", videoBody+" ", "video-local-1", token.Key).Code)
		assert.Equal(t, 404, request("GET", "/v1/media/tasks_async/"+accepted.TaskId, "", "", other.Key).Code)
		multipartTaskID := ""
		for range 2 {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for _, field := range [][2]string{{"model", "sora-2"}, {"provider", "sora"}, {"prompt", "a river"}, {"seconds", "4"}, {"size", "1280x720"}} {
				require.NoError(t, writer.WriteField(field[0], field[1]))
			}
			require.NoError(t, writer.Close())
			req := httptest.NewRequest("POST", "/v1/videos/generations_async", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer sk-"+token.Key)
			req.Header.Set("Idempotency-Key", "multipart-video-replay")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			require.Equal(t, 202, response.Code, response.Body.String())
			var replay struct {
				TaskID string `json:"task_id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &replay))
			if multipartTaskID == "" {
				multipartTaskID = replay.TaskID
			} else {
				assert.Equal(t, multipartTaskID, replay.TaskID, "multipart boundary changes do not create a new task")
			}
		}
		var job model.AsyncMediaJob
		require.NoError(t, model.DB.Where("task_id = ?", accepted.TaskId).Take(&job).Error)
		frozenRequest, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
		require.NoError(t, err)
		var frozen service.AsyncVideoRequest
		require.NoError(t, common.Unmarshal(frozenRequest, &frozen))
		assert.Equal(t, "create", frozen.Operation)
		originalPrices["sora-2"] = 10
		encoded, err = common.Marshal(originalPrices)
		require.NoError(t, err)
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
		run := func() error {
			require.NoError(t, model.DB.Model(&job).Update("next_attempt_at", 0).Error)
			job = model.AsyncMediaJob{}
			require.NoError(t, model.DB.Where("task_id = ?", accepted.TaskId).Take(&job).Error)
			claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 30)
			require.NoError(t, err)
			require.True(t, claimed)
			return service.ProcessAsyncMediaJob(t.Context(), job, mediaCfg)
		}
		require.NoError(t, run())
		assert.EqualValues(t, 1, submits.Load())
		var generated model.Task
		require.NoError(t, model.DB.Where("task_id = ?", accepted.TaskId).Take(&generated).Error)
		require.NotEmpty(t, generated.GetUpstreamTaskID())
		assert.Equal(t, frozen.Billing.Price.Quota, generated.Quota, "admission freezes the price before a later price change")
		// Changing live credentials cannot redirect a task already submitted.
		require.NoError(t, model.DB.Delete(&videoChannel).Error)
		tasks := map[string]*model.Task{generated.GetUpstreamTaskID(): &generated}
		require.NoError(t, service.UpdateVideoTasks(t.Context(), generated.Platform, map[int][]string{videoChannel.Id: {generated.GetUpstreamTaskID()}}, tasks))
		generated = model.Task{}
		require.NoError(t, model.DB.Where("task_id = ?", accepted.TaskId).Take(&generated).Error)
		require.EqualValues(t, model.TaskStatusSuccess, generated.Status)
		var wallet model.User
		var chargedToken model.Token
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&wallet).Error)
		require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&chargedToken).Error)
		quota, remaining := wallet.Quota, chargedToken.RemainQuota
		require.Error(t, run(), "damaged content must not be published")
		pending := request("GET", "/v1/media/tasks_async/"+accepted.TaskId, "", "", token.Key)
		assert.Contains(t, pending.Body.String(), `"storage_status":"failed"`)
		require.NoError(t, run())
		assert.EqualValues(t, 1, submits.Load(), "storage retry must not submit a new generation")
		require.NoError(t, model.DB.Where("id = ?", user.Id).Take(&wallet).Error)
		require.NoError(t, model.DB.Where("id = ?", token.Id).Take(&chargedToken).Error)
		assert.Equal(t, quota, wallet.Quota)
		assert.Equal(t, remaining, chargedToken.RemainQuota)
		upper.Close()
		complete := request("GET", "/v1/media/tasks_async/"+accepted.TaskId, "", "", token.Key)
		require.Equal(t, 200, complete.Code, complete.Body.String())
		var result struct {
			Status string `json:"status"`
			Data   []struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(complete.Body.Bytes(), &result))
		require.Equal(t, "succeeded", result.Status)
		require.Len(t, result.Data, 1)
		parsed, err := url.Parse(result.Data[0].URL)
		require.NoError(t, err)
		local := httptest.NewRecorder()
		req := httptest.NewRequest("GET", parsed.RequestURI(), nil)
		req.Header.Set("Range", "bytes=8-15")
		engine.ServeHTTP(local, req)
		require.Equal(t, 206, local.Code, local.Body.String())
		assert.Equal(t, video[8:16], local.Body.Bytes())
		head := request("HEAD", parsed.RequestURI(), "", "", "")
		assert.Equal(t, 200, head.Code)
		assert.Empty(t, head.Body.Bytes())
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"expired_time": time.Now().Unix() - 1}).Error)
		assert.Equal(t, 404, request("GET", parsed.RequestURI(), "", "", "").Code)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"expired_time": -1, "allow_ips": "127.0.0.1"}).Error)
		assert.Equal(t, 404, request("GET", parsed.RequestURI(), "", "", "").Code)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("allow_ips", nil).Error)
		mediaCfg.VideoAsyncEnabled = false
		require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), mediaCfg))
		assert.Equal(t, 202, request("POST", "/v1/videos/generations_async", videoBody, "video-local-1", token.Key).Code, "replay does not depend on a deleted channel")
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error)
		assert.Equal(t, 404, request("GET", parsed.RequestURI(), "", "", "").Code)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusEnabled).Error)
	})
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
	assert.Equal(t, 401, request("GET", "/v1/images/tasks_async/"+payload.TaskId, "", "", token.Key).Code)
}
