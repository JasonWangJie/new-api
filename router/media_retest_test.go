package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaRetestSelectionPersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldDB, oldLogs, oldRedis := model.DB, model.LOG_DB, common.RDB
	oldEnabled, oldMemory := common.RedisEnabled, common.MemoryCacheEnabled
	mainType, logType := common.MainDatabaseType(), common.LogDatabaseType()
	oldEstimator := service.EstimateAsyncImageQuotaFunc
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.RDB = oldDB, oldLogs, oldRedis
		common.RedisEnabled, common.MemoryCacheEnabled = oldEnabled, oldMemory
		common.SetDatabaseTypes(mainType, logType)
		service.EstimateAsyncImageQuotaFunc = oldEstimator
	})
	dbType := common.DatabaseTypeSQLite
	if os.Getenv("ASYNC_IMAGE_TEST_MYSQL_DSN") != "" {
		dbType = common.DatabaseTypeMySQL
	} else if os.Getenv("ASYNC_IMAGE_TEST_PG_DSN") != "" {
		dbType = common.DatabaseTypePostgreSQL
	}
	common.SetDatabaseTypes(dbType, dbType)
	model.DB = imageRouterDatabase(t, "retest")
	t.Setenv("LOG_SQL_DSN", "")
	master := common.IsMasterNode
	common.IsMasterNode = false
	err := model.InitLogDB()
	common.IsMasterNode = master
	require.NoError(t, err)
	db := model.DB.WithContext(t.Context())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.CanvasGrant{}, &model.Channel{}, &model.Ability{}, &model.Option{}, &model.SubscriptionPlan{}, &model.UserSubscription{}))
	require.NoError(t, model.MigrateImageModels(db))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	common.RDB, common.RedisEnabled, common.MemoryCacheEnabled = client, true, false
	service.EstimateAsyncImageQuotaFunc = func(context.Context, model.Token, service.AsyncImageRequest, string) (int, error) { return 1, nil }
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	cfg := service.DefaultImageRuntimeConfig()
	cfg.AsyncEnabled = true
	require.NoError(t, service.SaveImageRuntimeConfig(t.Context(), cfg))
	// Admission only creates a client. The test never contacts object storage.
	_, err = service.SaveImageStorageProfile(t.Context(), model.ImageStorageProfile{Class: "temporary", Backend: "s3", Provider: "s3", Endpoint: "https://storage.example.invalid", Bucket: "retest-fixture", Region: "us-east-1", Active: true}, &service.ImageStorageCredentials{AccessKey: "test-access", SecretKey: "test-secret"})
	require.NoError(t, err)
	catalog, err := common.Marshal([]service.ImageModelCapability{{Id: "retest-model", MaxOutputImages: 1, MaxReferenceImages: 1}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.ImageGroupPolicy{PolicyKey: service.ImageIdentityHash("default", "openai"), Group: "default", Platform: "openai", Enabled: true, AsyncEnabled: true, PoolMode: "resolution", Models: string(catalog), Version: 1}).Error)
	channel := model.Channel{Name: "Retest channel", Key: "fake-upstream", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "retest-model", Group: "default"}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "retest-model", ChannelId: channel.Id, Enabled: true}).Error)
	origin := model.User{Username: "retest-origin", AffCode: "retest-origin", Group: "default", Status: common.UserStatusEnabled, Quota: 1000}
	require.NoError(t, db.Create(&origin).Error)
	sourceToken := model.Token{UserId: origin.Id, Key: "retestoriginfixture", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000, Group: "default"}
	require.NoError(t, db.Create(&sourceToken).Error)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	SetAsyncImagePublicRouter(engine)
	SetApiRouter(engine)
	operator, admin := mediaRetestTestOperator(t, engine, "matrix")
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations_oa", strings.NewReader(`{"model":"retest-model","prompt":"keep this prompt","resolution":"1K"}`))
	request.Header.Set("Authorization", "Bearer sk-"+sourceToken.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &accepted))
	var source model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", accepted.TaskID).Take(&source).Error)
	require.NoError(t, model.TerminateAsyncImageTask(t.Context(), source))
	require.NoError(t, db.Model(&source).Update("channel_id", channel.Id).Error)
	path := "/api/admin/media-tasks/" + source.TaskId
	options := fmt.Sprintf("%s/retest-options?token_id=%d", path, operator.Id)
	result := mediaRetestOptionsResponse(t, admin(http.MethodGet, options, ""))
	assert.Equal(t, channel.Id, result.OriginalChannelID)
	assert.Empty(t, result.OriginalUnavailable)
	require.Len(t, result.Channels, 1)
	require.Len(t, result.AutoModels, 1)
	assert.Equal(t, http.StatusForbidden, admin(http.MethodGet, fmt.Sprintf("%s/retest-options?token_id=%d", path, sourceToken.Id), "").Code)
	post := func(channelID int, modelName string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"token_id":%d,"idempotency_key":"matrix-retest-selection","channel_id":%d,"model":%q}`, operator.Id, channelID, modelName)
		return admin(http.MethodPost, path+"/retest", body)
	}
	id := mediaRetestTestTaskID(t, post(channel.Id, source.Model))
	assert.Equal(t, id, mediaRetestTestTaskID(t, post(channel.Id, source.Model)))
	for _, response := range []*httptest.ResponseRecorder{post(0, source.Model), post(channel.Id, "different-model")} {
		assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "idempotency_conflict")
	}
	var task model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", id).Take(&task).Error)
	assert.Equal(t, operator.Id, task.TokenId)
	assert.Equal(t, operator.UserId, task.UserId)
	assert.Equal(t, channel.Id, task.ChannelId)
	data, err := service.DecryptImagePayload(task.RequestCipher, "task:"+id)
	require.NoError(t, err)
	var execution service.AsyncImageRequest
	require.NoError(t, common.Unmarshal(data, &execution))
	assert.Equal(t, channel.Id, execution.RetestChannelID)
	assert.Equal(t, "keep this prompt", execution.Prompt)
	claimed, err := model.ClaimAsyncImageTask(t.Context(), id, common.GetUUID(), 30)
	require.NoError(t, err)
	funding := model.ImageFundingSelection{Source: "wallet"}
	require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), claimed, 1, funding))
	require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), claimed, 1, funding))
	reservation, err := model.GetAsyncImageReservation(t.Context(), claimed)
	require.NoError(t, err)
	require.NotNil(t, reservation)
	assert.Equal(t, operator.UserId, reservation.UserId)
	assert.Equal(t, operator.Id, reservation.TokenId)
	assert.Equal(t, 1, reservation.ReservedQuota)
	var payer model.User
	require.NoError(t, db.Where("id = ?", operator.UserId).Take(&payer).Error)
	assert.Equal(t, operator.RemainQuota-1, payer.Quota)
	var paidToken model.Token
	require.NoError(t, db.Where("id = ?", operator.Id).Take(&paidToken).Error)
	assert.Equal(t, operator.RemainQuota-1, paidToken.RemainQuota)
	var sourceOwner model.User
	require.NoError(t, db.Where("id = ?", origin.Id).Take(&sourceOwner).Error)
	assert.Equal(t, origin.Quota, sourceOwner.Quota)
	var originalToken model.Token
	require.NoError(t, db.Where("id = ?", sourceToken.Id).Take(&originalToken).Error)
	assert.Equal(t, sourceToken.RemainQuota, originalToken.RemainQuota)
	var count int64
	require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("token_id = ?", operator.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	snapshot, err := model.GetRetainedMediaReplaySnapshot(t.Context(), model.MediaReplayImage, source.TaskId, time.Now().Unix())
	require.NoError(t, err)
	assert.EqualValues(t, 86400, snapshot.RetainUntil-snapshot.FailedAt)
	require.NoError(t, db.Model(&snapshot).Update("retain_until", time.Now().Unix()-1).Error)
	assert.Equal(t, http.StatusGone, admin(http.MethodGet, options, "").Code)
	assert.Equal(t, id, mediaRetestTestTaskID(t, post(channel.Id, source.Model)))
}
