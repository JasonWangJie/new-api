package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	taskIDs      []string
	publicIDs    []string
	keys         []string
	fetched      chan string
	blockTaskID  string
	blockStarted chan struct{}
	releaseBlock chan struct{}
	blockOnce    sync.Once
}

type batchPollingAdaptor struct {
	taskPollingFetchAdaptor
	batchCalls int
	batchIDs   []string
	results    map[string]*BatchTaskResult
	batchKeys  []string
	publicIDs  []string
}

func (a *batchPollingAdaptor) FetchMode() string { return "batch" }
func (a *batchPollingAdaptor) FetchBatchTasks(_ string, key string, tasks []*model.Task, _ string) (*http.Response, error) {
	a.batchCalls++
	a.batchKeys = append(a.batchKeys, key)
	a.batchIDs = a.batchIDs[:0]
	for _, task := range tasks {
		a.batchIDs = append(a.batchIDs, task.GetUpstreamTaskID())
		a.publicIDs = append(a.publicIDs, task.TaskID)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{}`)))}, nil
}
func (a *batchPollingAdaptor) ParseBatchResult(_ []*model.Task, _ *http.Response, _ []byte) (map[string]*BatchTaskResult, error) {
	if a.results != nil {
		return a.results, nil
	}
	results := make(map[string]*BatchTaskResult, len(a.batchIDs))
	for _, taskID := range a.batchIDs {
		results[taskID] = &BatchTaskResult{TaskInfo: relaycommon.TaskInfo{TaskID: taskID, Status: model.TaskStatusInProgress, Url: "https://example.com/result"}}
	}
	return results, nil
}

func (a *taskPollingFetchAdaptor) Init(_ *relaycommon.RelayInfo) {}

func (a *taskPollingFetchAdaptor) FetchTask(_ string, key string, task *model.Task, _ string) (*http.Response, error) {
	taskID := ""
	if task != nil {
		taskID = task.GetUpstreamTaskID()
	}
	if taskID == a.blockTaskID && a.releaseBlock != nil {
		a.blockOnce.Do(func() {
			if a.blockStarted != nil {
				close(a.blockStarted)
			}
		})
		<-a.releaseBlock
	}

	a.mu.Lock()
	a.taskIDs = append(a.taskIDs, taskID)
	if task != nil {
		a.publicIDs = append(a.publicIDs, task.TaskID)
	}
	a.keys = append(a.keys, key)
	a.mu.Unlock()
	if a.fetched != nil {
		select {
		case a.fetched <- taskID:
		default:
		}
	}

	response := taskdto.TaskResponse[model.Task]{
		Code: taskdto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	}
	responseBody, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *taskPollingFetchAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *taskPollingFetchAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *taskPollingFetchAdaptor) fetchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskIDs)
}

func (a *taskPollingFetchAdaptor) fetchedTaskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.taskIDs...)
}

func TestRedactVideoResponseBodyPreservesPollingPayloadShape(t *testing.T) {
	rawVideo := strings.Repeat("a", 300)
	body, err := common.Marshal(map[string]any{
		"done": true,
		"name": "operations/provider-task",
		"response": map[string]any{
			"bytesBase64Encoded": "secret-bytes",
			"video":              rawVideo,
			"videos": []any{
				map[string]any{
					"bytesBase64Encoded": "other-secret-bytes",
					"mimeType":           "video/mp4",
					"uri":                "https://media.example/video.mp4",
				},
			},
		},
	})
	require.NoError(t, err)

	var stored map[string]any
	require.NoError(t, common.Unmarshal(redactVideoResponseBody(body), &stored))
	assert.Equal(t, true, stored["done"])
	assert.Equal(t, "operations/provider-task", stored["name"])
	response, ok := stored["response"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, response, "bytesBase64Encoded")
	assert.Equal(t, strings.Repeat("a", 256)+"...", response["video"])
	videos, ok := response["videos"].([]any)
	require.True(t, ok)
	require.Len(t, videos, 1)
	video, ok := videos[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, video, "bytesBase64Encoded")
	assert.Equal(t, "video/mp4", video["mimeType"])
	assert.Equal(t, "https://media.example/video.mp4", video["uri"])
}

func seedTaskPollingChannel(t *testing.T, id int, disableSleep bool) {
	t.Helper()
	ch := &model.Channel{
		Id:     id,
		Type:   constant.ChannelTypeKling,
		Name:   "polling_channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
	}
	if disableSleep {
		ch.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedPollingTask(t *testing.T, channelID int, publicID string, upstreamID string) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicID,
		Platform:  constant.TaskPlatform("kling"),
		UserId:    1,
		ChannelId: channelID,
		Action:    constant.TaskActionImageToVideo,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: upstreamID,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

func setupTaskPollingDatabase(t *testing.T) {
	t.Helper()
	var driver gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "polling.db"))
	if dsn := os.Getenv("ASYNC_IMAGE_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_test", "use a disposable image test database")
		driver = mysql.Open(dsn)
	} else if dsn := os.Getenv("ASYNC_IMAGE_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_test") || strings.Contains(dsn, "/new_api_image_test"), "use a disposable image test database")
		driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("polling_%d_", time.Now().UnixNano())}})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	if db.Dialector.Name() == "sqlite" {
		connection.SetMaxOpenConns(1)
	}
	previousDB, previousLogDB, previousCache := model.DB, model.LOG_DB, common.MemoryCacheEnabled
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousPollingCursor, previousTimeoutCursor := taskPollingCursor.Swap(0), taskTimeoutCursor.Swap(0)
	model.DB, model.LOG_DB, common.MemoryCacheEnabled = db, db, false
	common.SetDatabaseTypes(common.DatabaseType(db.Dialector.Name()), common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.MemoryCacheEnabled = previousDB, previousLogDB, previousCache
		common.SetDatabaseTypes(previousType, previousLogType)
		taskPollingCursor.Store(previousPollingCursor)
		taskTimeoutCursor.Store(previousTimeoutCursor)
		assert.NoError(t, db.Migrator().DropTable(&model.Task{}, &model.Channel{}, &model.User{}, &model.Log{}))
		assert.NoError(t, connection.Close())
	})
	for range 2 {
		require.NoError(t, db.AutoMigrate(&model.Task{}, &model.Channel{}, &model.User{}, &model.Log{}))
	}
}

func TestRunTaskPollingOnceKeepsProviderIDsScopedToChannelAndCredential(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			setupTaskPollingDatabase(t)
			previousFactory, previousLimit := GetTaskAdaptorFunc, constant.TaskQueryLimit
			t.Cleanup(func() {
				GetTaskAdaptorFunc, constant.TaskQueryLimit = previousFactory, previousLimit
			})
			constant.TaskQueryLimit = 100
			for _, id := range []int{1201, 1202} {
				seedTaskPollingChannel(t, id, true)
			}
			publicIDs := []string{"task_account_a", "task_account_b", "task_channel_b"}
			for index, id := range []int{1201, 1201, 1202} {
				task := seedPollingTask(t, id, publicIDs[index], "same-provider-id")
				task.PrivateData.Key = fmt.Sprintf("credential-%d", index)
				task.SubmitTime = time.Now().Unix()
				require.NoError(t, task.Update())
			}
			perTask := &taskPollingFetchAdaptor{}
			batchAdaptor := &batchPollingAdaptor{}
			GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
				if batch {
					return batchAdaptor
				}
				return perTask
			}
			summary := RunTaskPollingOnce(t.Context(), nil)
			assert.Equal(t, 3, summary.UnfinishedTasks)
			if batch {
				assert.ElementsMatch(t, publicIDs, batchAdaptor.publicIDs)
				assert.ElementsMatch(t, []string{"credential-0", "credential-1", "credential-2"}, batchAdaptor.batchKeys)
			} else {
				assert.ElementsMatch(t, publicIDs, perTask.publicIDs)
				assert.ElementsMatch(t, []string{"credential-0", "credential-1", "credential-2"}, perTask.keys)
			}
		})
	}
}

func TestTaskPollingUsesPersistedSubmissionCredential(t *testing.T) {
	setupTaskPollingDatabase(t)
	channel := &model.Channel{
		Id: 1210, Type: constant.ChannelTypeSora, Name: "multi-key", Status: common.ChannelStatusEnabled,
		Key: "account-a\naccount-b", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, model.DB.Create(channel).Error)
	info := &relaycommon.RelayInfo{
		UserId: 1, TaskRelayInfo: &relaycommon.TaskRelayInfo{},
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type, ApiKey: "account-b"},
	}
	task := model.InitTask("sora", info)
	task.PrivateData.UpstreamTaskID = "account-b-video"
	require.NoError(t, task.Insert())
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })
	require.NoError(t, UpdateVideoTasks(t.Context(), "sora", map[int][]string{channel.Id: {"task-ref"}}, map[string]*model.Task{"task-ref": &persisted}))
	assert.Equal(t, []string{"account-b"}, adaptor.keys)
}

func TestTaskPollingKeepsAmbiguousLegacyAccountsPending(t *testing.T) {
	for _, keys := range []string{"account-a\naccount-b", `[{"account":"a"},{"account":"b"}]`} {
		t.Run(keys, func(t *testing.T) {
			setupTaskPollingDatabase(t)
			previousTimeout := constant.TaskTimeoutMinutes
			constant.TaskTimeoutMinutes = 1
			t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })
			channel := &model.Channel{
				Id: 1220, Type: constant.ChannelTypeSora, Name: "legacy-accounts", Key: keys,
				Status: common.ChannelStatusEnabled, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
			}
			require.NoError(t, model.DB.Create(channel).Error)
			task := seedPollingTask(t, channel.Id, "task_legacy_no_account", "provider-id")
			task.Quota = 50
			task.SubmitTime = time.Now().Add(-2 * time.Minute).Unix()
			require.NoError(t, task.Update())
			before := task.Snapshot()
			perTask := &taskPollingFetchAdaptor{}
			err := updateVideoSingleTask(t.Context(), perTask, channel, "task-ref", map[string]*model.Task{"task-ref": task})
			require.ErrorContains(t, err, "reconcile its original credential")
			batch := &batchPollingAdaptor{}
			require.NoError(t, updateBatchTasks(t.Context(), batch, channel.Id, []string{"task-ref"}, map[string]*model.Task{"task-ref": task}))
			sweepTimedOutTasks(t.Context())
			assert.Zero(t, perTask.fetchCount())
			assert.Zero(t, batch.batchCalls)
			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.True(t, before.Equal(persisted.Snapshot()))
			assert.Equal(t, 50, persisted.Quota)
		})
	}
}

func TestTaskPollingAdvancesPastHeldLegacyAccounts(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%t", timeout), func(t *testing.T) {
			setupTaskPollingDatabase(t)
			previousFactory, previousLimit, previousTimeout := GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes
			t.Cleanup(func() {
				GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes = previousFactory, previousLimit, previousTimeout
			})
			constant.TaskQueryLimit, constant.TaskTimeoutMinutes = 100, 0
			if timeout {
				constant.TaskTimeoutMinutes = 1
			}
			channel := &model.Channel{
				Id: 1230, Type: constant.ChannelTypeSora, Name: "held-legacy-backlog", Key: "account-a\naccount-b",
				Status: common.ChannelStatusEnabled, UsedQuota: 5050, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
			}
			channel.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
			require.NoError(t, model.DB.Create(channel).Error)
			require.NoError(t, model.DB.Create(&model.User{Id: 1, Username: "backlog-user", Quota: 10000, UsedQuota: 5050, RequestCount: 101}).Error)
			for index := range 101 {
				task := seedPollingTask(t, channel.Id, fmt.Sprintf("task_backlog_%d", index), fmt.Sprintf("provider-%d", index))
				task.Quota = 50
				task.SubmitTime = time.Now().Add(-2 * time.Minute).Unix()
				if index == 100 {
					task.PrivateData.Key = "account-a"
					task.PrivateData.SubmitAccountingRecorded = true
				}
				require.NoError(t, task.Update())
			}
			adaptor := &taskPollingFetchAdaptor{}
			GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
			for range 3 {
				if timeout {
					sweepTimedOutTasks(t.Context())
				} else {
					RunTaskPollingOnce(t.Context(), nil)
				}
			}
			var known model.Task
			require.NoError(t, model.DB.Where("task_id = ?", "task_backlog_100").First(&known).Error)
			var user model.User
			require.NoError(t, model.DB.First(&user, 1).Error)
			if timeout {
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), known.Status)
				assert.Zero(t, known.Quota)
				assert.Equal(t, 10050, user.Quota)
				assert.Equal(t, 5000, user.UsedQuota)
				assert.Zero(t, adaptor.fetchCount())
			} else {
				assert.Equal(t, []string{"task_backlog_100"}, adaptor.publicIDs)
				assert.Equal(t, []string{"account-a"}, adaptor.keys)
				assert.Equal(t, 50, known.Quota)
				assert.Equal(t, 10000, user.Quota)
				assert.Equal(t, 5050, user.UsedQuota)
			}
			var held []*model.Task
			require.NoError(t, model.DB.Where("task_id != ?", known.TaskID).Order("id").Find(&held).Error)
			require.Len(t, held, 100)
			for _, task := range held {
				assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), task.Status)
				assert.Equal(t, "30%", task.Progress)
				assert.Equal(t, 50, task.Quota)
				assert.Zero(t, task.FinishTime)
				assert.Zero(t, task.PrivateData.PollFailures)
			}
		})
	}
}

func TestUpdateVideoTasksDefaultSleepWaitsBetweenTasks(t *testing.T) {
	truncate(t)

	const channelID = 101
	seedTaskPollingChannel(t, channelID, false)
	first := seedPollingTask(t, channelID, "task_public_1", "upstream_1")
	second := seedPollingTask(t, channelID, "task_public_2", "upstream_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, adaptor.fetchCount())
}

func TestDispatchPlatformUpdateUsesFetchMode(t *testing.T) {
	truncate(t)
	const channelID = 109
	seedTaskPollingChannel(t, channelID, true)
	task := seedPollingTask(t, channelID, "task_batch", "upstream_batch")
	taskChannels := map[int][]string{channelID: {task.GetUpstreamTaskID()}}
	tasks := map[string]*model.Task{task.GetUpstreamTaskID(): task}

	batch := &batchPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return batch }
	DispatchPlatformUpdate(context.Background(), "batch-plugin", taskChannels, tasks)
	assert.Equal(t, 1, batch.batchCalls)
	assert.Equal(t, 0, batch.fetchCount())
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, "https://example.com/result", persisted.GetResultURL())

	perTask := &taskPollingFetchAdaptor{}
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return perTask }
	DispatchPlatformUpdate(context.Background(), "per-task-plugin", taskChannels, tasks)
	assert.Equal(t, 1, perTask.fetchCount())

	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return nil }
	assert.NotPanics(t, func() { DispatchPlatformUpdate(context.Background(), "missing-plugin", taskChannels, tasks) })
	GetTaskAdaptorFunc = previousFactory
}

func TestUpdateBatchTasksSettlesTieredUsageForTerminalStates(t *testing.T) {
	testCases := []struct {
		name        string
		status      model.TaskStatus
		units       float64
		actualQuota int
	}{
		{name: "success with usage", status: model.TaskStatusSuccess, units: 3, actualQuota: 3_000},
		{name: "failure with usage", status: model.TaskStatusFailure, units: 3, actualQuota: 0},
		{name: "success with zero usage", status: model.TaskStatusSuccess, units: 0, actualQuota: 0},
		{name: "failure with zero usage", status: model.TaskStatusFailure, units: 0, actualQuota: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)

			const userID, tokenID, channelID = 41, 41, 141
			const initialQuota, preConsumedQuota = 10_000, 5_000
			const tokenRemain = 8_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-batch-tiered", tokenRemain)
			seedTaskPollingChannel(t, channelID, true)

			expression := `tier("actual", u("units"))`
			task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_batch_tiered_" + string(testCase.status)
			task.Platform = "batch-plugin"
			task.PrivateData.UpstreamTaskID = "upstream_batch_tiered_" + string(testCase.status)
			task.SetData(map[string]any{"provider_payload": "must-be-preserved"})
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
				ExprString:       expression,
				ExprHash:         billingexpr.ExprHashString(expression),
				GroupRatio:       1,
				QuotaPerUnit:     1_000,
				ExprVersion:      1,
				TaskUsageBilling: true,
			}
			require.NoError(t, model.DB.Create(task).Error)

			upstreamID := task.GetUpstreamTaskID()
			reason := ""
			if testCase.status == model.TaskStatusFailure {
				reason = "upstream failed"
			}
			result := &BatchTaskResult{TaskInfo: relaycommon.TaskInfo{
				TaskID:     upstreamID,
				Status:     string(testCase.status),
				Reason:     reason,
				UsageFacts: map[string]any{"units": testCase.units},
			}}
			adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{upstreamID: result}}
			taskIDs := []string{upstreamID}
			taskMap := map[string]*model.Task{upstreamID: task}

			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: taskIDs}, taskMap))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.Equal(t, testCase.status, persisted.Status)
			assert.Equal(t, testCase.actualQuota, persisted.Quota)
			var persistedData map[string]any
			require.NoError(t, common.Unmarshal(persisted.Data, &persistedData))
			assert.Equal(t, "must-be-preserved", persistedData["provider_payload"])
			assert.Equal(t, initialQuota+(preConsumedQuota-testCase.actualQuota), getUserQuota(t, userID))
			assert.Equal(t, tokenRemain+(preConsumedQuota-testCase.actualQuota), getTokenRemainQuota(t, tokenID))
			assert.Equal(t, int64(1), countLogs(t))
			if testCase.status == model.TaskStatusFailure {
				log := getLastLog(t)
				require.NotNil(t, log)
				assert.Equal(t, model.LogTypeRefund, log.Type)
			}

			// A duplicate terminal response must not settle the same task twice.
			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: taskIDs}, taskMap))
			assert.Equal(t, initialQuota+(preConsumedQuota-testCase.actualQuota), getUserQuota(t, userID))
			assert.Equal(t, int64(1), countLogs(t))
		})
	}
}

func TestUpdateBatchTasksRefundsFailedTieredTask(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 43, 43, 143
	const initialQuota, preConsumedQuota, tokenRemain = 10_000, 5_000, 8_000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "sk-batch-tiered-refund", tokenRemain)
	seedTaskPollingChannel(t, channelID, true)

	expression := `tier("actual", u("units"))`
	task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = "task_batch_tiered_refund"
	task.Platform = "batch-plugin"
	task.PrivateData.UpstreamTaskID = "upstream_batch_tiered_refund"
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:       expression,
		ExprHash:         billingexpr.ExprHashString(expression),
		GroupRatio:       1,
		QuotaPerUnit:     1_000,
		ExprVersion:      1,
		TaskUsageBilling: true,
		UsageFacts:       map[string]any{"units": float64(5)},
		EstimatedTier:    "actual",
	}
	require.NoError(t, model.DB.Create(task).Error)

	upstreamID := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamID: {TaskInfo: relaycommon.TaskInfo{
			TaskID:     upstreamID,
			Status:     model.TaskStatusFailure,
			Reason:     "upstream failed",
			UsageFacts: map[string]any{"units": float64(5)},
		}},
	}}
	require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, persisted.Status)
	assert.Zero(t, persisted.Quota)
	assert.Equal(t, initialQuota+preConsumedQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumedQuota, getTokenRemainQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumedQuota, log.Quota)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, "actual", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"units": float64(5)}, facts)
}

func TestUpdateBatchTasksRefundsFailedTaskWithoutUsageSettlement(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 42, 42, 142
	const initialQuota, preConsumedQuota, tokenRemain = 10_000, 4_000, 7_000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "sk-batch-refund", tokenRemain)
	seedTaskPollingChannel(t, channelID, true)

	task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = "task_batch_refund"
	task.Platform = "batch-plugin"
	task.Properties.OriginModelName = "missing-batch-token-price"
	task.PrivateData.UpstreamTaskID = "upstream_batch_refund"
	task.PrivateData.BillingContext.OriginModelName = "missing-batch-token-price"
	require.NoError(t, model.DB.Create(task).Error)

	upstreamID := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamID: {TaskInfo: relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusFailure, Reason: "upstream failed", TotalTokens: 123}},
	}}
	require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

	assert.Equal(t, initialQuota+preConsumedQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumedQuota, getTokenRemainQuota(t, tokenID))
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestUpdateVideoTasksCanSkipPollingSleepPerChannel(t *testing.T) {
	truncate(t)

	const channelID = 102
	seedTaskPollingChannel(t, channelID, true)
	first := seedPollingTask(t, channelID, "task_public_3", "upstream_3")
	second := seedPollingTask(t, channelID, "task_public_4", "upstream_4")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, adaptor.fetchCount())
}

func TestUpdateVideoTasksDefaultSleepDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const firstChannelID = 201
	const secondChannelID = 202
	seedTaskPollingChannel(t, firstChannelID, false)
	seedTaskPollingChannel(t, secondChannelID, false)
	firstChannelFirst := seedPollingTask(t, firstChannelID, "task_public_5", "upstream_a_1")
	firstChannelSecond := seedPollingTask(t, firstChannelID, "task_public_6", "upstream_a_2")
	secondChannelFirst := seedPollingTask(t, secondChannelID, "task_public_7", "upstream_b_1")
	secondChannelSecond := seedPollingTask(t, secondChannelID, "task_public_8", "upstream_b_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		firstChannelID: {
			firstChannelFirst.GetUpstreamTaskID(),
			firstChannelSecond.GetUpstreamTaskID(),
		},
		secondChannelID: {
			secondChannelFirst.GetUpstreamTaskID(),
			secondChannelSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		firstChannelFirst.GetUpstreamTaskID():   firstChannelFirst,
		firstChannelSecond.GetUpstreamTaskID():  firstChannelSecond,
		secondChannelFirst.GetUpstreamTaskID():  secondChannelFirst,
		secondChannelSecond.GetUpstreamTaskID(): secondChannelSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_a_1", "upstream_b_1"}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksSlowChannelDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const slowChannelID = 251
	const fastChannelID = 252
	seedTaskPollingChannel(t, slowChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	slowTask := seedPollingTask(t, slowChannelID, "task_public_slow", "upstream_slow_1")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_fast_1", "upstream_fast_parallel_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_fast_2", "upstream_fast_parallel_2")

	adaptor := &taskPollingFetchAdaptor{
		fetched:      make(chan string, 4),
		blockTaskID:  slowTask.GetUpstreamTaskID(),
		blockStarted: make(chan struct{}),
		releaseBlock: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseBlockedTask := func() {
		releaseOnce.Do(func() {
			close(adaptor.releaseBlock)
		})
	}
	t.Cleanup(releaseBlockedTask)
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	errCh := make(chan error, 1)
	gopool.Go(func() {
		errCh <- UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), map[int][]string{
			slowChannelID: {
				slowTask.GetUpstreamTaskID(),
			},
			fastChannelID: {
				fastFirst.GetUpstreamTaskID(),
				fastSecond.GetUpstreamTaskID(),
			},
		}, map[string]*model.Task{
			slowTask.GetUpstreamTaskID():   slowTask,
			fastFirst.GetUpstreamTaskID():  fastFirst,
			fastSecond.GetUpstreamTaskID(): fastSecond,
		})
	})

	select {
	case <-adaptor.blockStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow channel did not start blocking")
	}

	require.Eventually(t, func() bool {
		fetchedTaskIDs := adaptor.fetchedTaskIDs()
		return len(fetchedTaskIDs) == 2 &&
			fetchedTaskIDs[0] == fastFirst.GetUpstreamTaskID() &&
			fetchedTaskIDs[1] == fastSecond.GetUpstreamTaskID()
	}, 500*time.Millisecond, 10*time.Millisecond)

	releaseBlockedTask()
	require.NoError(t, <-errCh)
	assert.ElementsMatch(t, []string{
		slowTask.GetUpstreamTaskID(),
		fastFirst.GetUpstreamTaskID(),
		fastSecond.GetUpstreamTaskID(),
	}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksMixedChannelSleepSettings(t *testing.T) {
	truncate(t)

	const sleepyChannelID = 301
	const fastChannelID = 302
	seedTaskPollingChannel(t, sleepyChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	sleepyFirst := seedPollingTask(t, sleepyChannelID, "task_public_9", "upstream_sleepy_1")
	sleepySecond := seedPollingTask(t, sleepyChannelID, "task_public_10", "upstream_sleepy_2")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_11", "upstream_fast_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_12", "upstream_fast_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		sleepyChannelID: {
			sleepyFirst.GetUpstreamTaskID(),
			sleepySecond.GetUpstreamTaskID(),
		},
		fastChannelID: {
			fastFirst.GetUpstreamTaskID(),
			fastSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		sleepyFirst.GetUpstreamTaskID():  sleepyFirst,
		sleepySecond.GetUpstreamTaskID(): sleepySecond,
		fastFirst.GetUpstreamTaskID():    fastFirst,
		fastSecond.GetUpstreamTaskID():   fastSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_sleepy_1", "upstream_fast_1", "upstream_fast_2"}, adaptor.fetchedTaskIDs())
}

func TestUpdateSunoTasksStalePollsRefundExactlyOnce(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 401, 401, 401
	const initialUserQuota, initialTokenQuota, taskQuota = 10_000, 6_000, 2_500
	const publicTaskID, upstreamTaskID = "suno_public_refund_once", "suno_upstream_refund_once"

	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-suno-refund-once", initialTokenQuota)
	baseURL := "https://suno.invalid"
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:      channelID,
		Type:    constant.ChannelTypeSunoAPI,
		Name:    "suno_refund_once",
		Key:     "sk-suno-channel",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error)

	task := makeTask(userID, channelID, taskQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = publicTaskID
	task.Platform = constant.TaskPlatformSuno
	task.Status = model.TaskStatusInProgress
	task.Progress = "50%"
	task.SubmitTime = time.Now().Unix()
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	require.NoError(t, model.DB.Create(task).Error)

	var firstPollTask model.Task
	var staleSecondPollTask model.Task
	require.NoError(t, model.DB.First(&firstPollTask, task.ID).Error)
	require.NoError(t, model.DB.First(&staleSecondPollTask, task.ID).Error)

	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamTaskID: {TaskInfo: relaycommon.TaskInfo{
			TaskID: upstreamTaskID,
			Status: model.TaskStatusFailure,
			Reason: "upstream failed",
		}},
	}}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &firstPollTask,
	}))
	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &staleSecondPollTask,
	}))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)
	assert.Equal(t, initialUserQuota+taskQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota+taskQuota, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestRunTaskPollingOnceDoesNotRefundHistoricalFailedTask(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 402, 10_000, 1_200
	seedUser(t, userID, initialQuota)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	task.TaskID = "historical_failed_already_refunded"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = time.Now().Add(-90 * 24 * time.Hour).Unix()
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &taskPollingFetchAdaptor{}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)

	assert.Zero(t, summary.UnfinishedTasks)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, taskQuota, getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSweepTimedOutTasksHonorsRefundRolloutBoundary(t *testing.T) {
	truncate(t)

	const (
		userID          = 403
		initialQuota    = 10_000
		legacyTaskQuota = 1_800
		modernTaskQuota = 1_200
	)
	seedUser(t, userID, initialQuota)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_timeout_without_refund"
	legacyTask.Progress = "50%"
	legacyTask.SubmitTime = 1771718399 // 2026-02-21 23:59:59 UTC
	require.NoError(t, model.DB.Create(legacyTask).Error)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_timeout_with_refund"
	modernTask.Progress = "50%"
	modernTask.SubmitTime = 1771718400 // 2026-02-22 00:00:00 UTC
	require.NoError(t, model.DB.Create(modernTask).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedLegacy model.Task
	var reloadedModern model.Task
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedLegacy.Status)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedModern.Status)
	assert.Zero(t, reloadedLegacy.Quota)
	assert.Zero(t, reloadedModern.Quota)
	assert.Contains(t, reloadedLegacy.FailReason, "旧系统遗留任务")
	assert.Contains(t, reloadedModern.FailReason, "任务超时")
	assert.Equal(t, initialQuota+modernTaskQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(1), countLogs(t))
}

type scriptedPollingAdaptor struct {
	fetchCalls int
	statusCode int
	body       []byte
	fetchErr   error
	parse      *relaycommon.TaskInfo
	parseErr   error
}

func (a *scriptedPollingAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *scriptedPollingAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
	a.fetchCalls++
	if a.fetchErr != nil {
		return nil, a.fetchErr
	}
	code := a.statusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := a.body
	if body == nil {
		body = []byte(`{}`)
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body))}, nil
}
func (a *scriptedPollingAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	if a.parse != nil {
		return a.parse, nil
	}
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}
func (a *scriptedPollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return 0
}

type scriptedCompletionUsageAdaptor struct {
	scriptedPollingAdaptor
	usageFacts map[string]any
}

func (a *scriptedCompletionUsageAdaptor) ApplyCompletionUsage(_ *model.Task, _ *http.Response, _ []byte, result *relaycommon.TaskInfo) error {
	result.UsageFacts = a.usageFacts
	return nil
}

type scriptedBatchPollingAdaptor struct {
	scriptedPollingAdaptor
	results map[string]*BatchTaskResult
}

func (a *scriptedBatchPollingAdaptor) FetchMode() string { return "batch" }
func (a *scriptedBatchPollingAdaptor) FetchBatchTasks(string, string, []*model.Task, string) (*http.Response, error) {
	return a.FetchTask("", "", nil, "")
}
func (a *scriptedBatchPollingAdaptor) FetchBatchTasksContext(ctx context.Context, base, key string, tasks []*model.Task, proxy string) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a.FetchBatchTasks(base, key, tasks, proxy)
}
func (a *scriptedBatchPollingAdaptor) ParseBatchResult([]*model.Task, *http.Response, []byte) (map[string]*BatchTaskResult, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	return a.results, nil
}

func TestMediaPollingTimeoutConfiguration(t *testing.T) {
	setupTaskPollingDatabase(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	t.Cleanup(func() { assert.NoError(t, model.DB.Migrator().DropTable(&model.Option{})) })
	legacy := `{"local_path":"data/media","retention_days":90,"signed_url_expiry_seconds":3600,"max_file_bytes":1073741824,"download_timeout_seconds":900,"download_concurrency":2,"storage_retry_attempts":5}`
	option := model.Option{Key: "media_runtime_config", Value: legacy}
	require.NoError(t, model.DB.Create(&option).Error)
	cfg, err := GetMediaRuntimeConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 30, cfg.PollTimeout, "pre-upgrade settings use the independent polling default")
	assert.Equal(t, 1440, cfg.ReconciliationTimeout)
	assert.Equal(t, 60, cfg.ReconciliationPollInterval)
	assert.Equal(t, 900, cfg.DownloadTimeout)
	for _, timeout := range []int{0, -1, 301, 1, 300} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			cfg.PollTimeout = timeout
			err := SaveMediaRuntimeConfig(t.Context(), cfg)
			if timeout < 1 || timeout > 300 {
				require.Error(t, err)
				// Explicit invalid persisted values must not silently use a default.
				encoded, marshalErr := common.Marshal(cfg)
				require.NoError(t, marshalErr)
				require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": option.Key}).Update("value", string(encoded)).Error)
				_, err = GetMediaRuntimeConfig(t.Context())
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			loaded, err := GetMediaRuntimeConfig(t.Context())
			require.NoError(t, err)
			assert.Equal(t, timeout, loaded.PollTimeout)
			assert.Equal(t, 900, loaded.DownloadTimeout)
		})
	}
	for _, tc := range []struct {
		name             string
		window, interval int
		valid            bool
	}{
		{name: "zero window", window: 0, interval: 60},
		{name: "oversized window", window: 10081, interval: 60},
		{name: "zero interval", window: 1440, interval: 0},
		{name: "short interval", window: 1440, interval: 4},
		{name: "oversized interval", window: 1440, interval: 3601},
		{name: "minimum reconciliation limits", window: 1, interval: 5, valid: true},
		{name: "maximum reconciliation limits", window: 10080, interval: 3600, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := DefaultMediaRuntimeConfig()
			candidate.ReconciliationTimeout, candidate.ReconciliationPollInterval = tc.window, tc.interval
			err := SaveMediaRuntimeConfig(t.Context(), candidate)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				encoded, err := common.Marshal(candidate)
				require.NoError(t, err)
				require.NoError(t, model.DB.Model(&model.Option{}).Where(map[string]any{"key": option.Key}).Update("value", string(encoded)).Error)
			}
			loaded, err := GetMediaRuntimeConfig(t.Context())
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.window, loaded.ReconciliationTimeout)
				assert.Equal(t, tc.interval, loaded.ReconciliationPollInterval)
			} else {
				require.Error(t, err, "explicit invalid fields never use legacy defaults")
			}
		})
	}
}

type boundedVideoPollingAdaptor struct {
	scriptedBatchPollingAdaptor
	fetch func(context.Context) (*http.Response, error)
}

func (a *boundedVideoPollingAdaptor) FetchTaskContext(ctx context.Context, _, _ string, _ *model.Task, _ string) (*http.Response, error) {
	return a.fetch(ctx)
}

func (a *boundedVideoPollingAdaptor) FetchBatchTasksContext(ctx context.Context, _, _ string, _ []*model.Task, _ string) (*http.Response, error) {
	return a.fetch(ctx)
}

type stalledVideoPollBody struct{ ctx context.Context }

func (body stalledVideoPollBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (stalledVideoPollBody) Close() error { return nil }

func TestAsyncVideoPollTimeoutIncludesResponseBodyAndPreservesDownloadBudget(t *testing.T) {
	for _, mode := range []string{"plugin", "override", "batch"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Driver watchers must share the bubble with request contexts;
				// opening a MySQL connection outside it crosses synctest channels.
				setupTaskPollingDatabase(t)
				entities := []any{&model.Option{}, &model.AsyncMediaJob{}}
				require.NoError(t, model.DB.AutoMigrate(entities...))
				t.Cleanup(func() { assert.NoError(t, model.DB.Migrator().DropTable(entities...)) })
				cfg := DefaultMediaRuntimeConfig()
				cfg.PollTimeout, cfg.DownloadTimeout = 45, 900
				require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
				seedTaskPollingChannel(t, 1, true)
				channel, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				task := seedPollingTask(t, 1, "poll-timeout", "upstream-timeout")
				task.PrivateData.AsyncMedia = true
				if mode == "override" {
					task.PrivateData.UpstreamAsync = upstreamAsyncTestProfile(t, "video")
				}
				require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
				previousClient, previousFailureLimit := httpClient, constant.TaskPollMaxFailures
				constant.TaskPollMaxFailures = 20
				t.Cleanup(func() { httpClient, constant.TaskPollMaxFailures = previousClient, previousFailureLimit })
				// The upstream transport is fake; database connections remain real.
				// Time advances when the response body blocks, without real sleeps.
				started := time.Now()
				parent, cancel := context.WithTimeout(t.Context(), time.Duration(cfg.DownloadTimeout)*time.Second)
				defer cancel()
				var requestContext context.Context
				adaptor := &boundedVideoPollingAdaptor{fetch: func(ctx context.Context) (*http.Response, error) {
					requestContext = ctx
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					assert.Equal(t, started.Add(45*time.Second), deadline)
					return &http.Response{StatusCode: http.StatusOK, Body: stalledVideoPollBody{ctx: ctx}}, nil
				}}
				httpClient = &http.Client{Transport: mediaInputRoundTripper(func(request *http.Request) (*http.Response, error) {
					return adaptor.fetch(request.Context())
				})}
				if mode == "batch" {
					require.NoError(t, updateBatchTaskGroup(parent, adaptor, channel, channel.Key, []*model.Task{task}))
				} else {
					require.NoError(t, updateVideoSingleTask(parent, adaptor, channel, task.TaskID, map[string]*model.Task{task.TaskID: task}))
				}
				require.NotNil(t, requestContext)
				assert.ErrorIs(t, requestContext.Err(), context.DeadlineExceeded)
				assert.NoError(t, parent.Err(), "poll expiry must leave the download budget available")
				var stored model.Task
				require.NoError(t, model.DB.First(&stored, task.ID).Error)
				assert.EqualValues(t, model.TaskStatusInProgress, stored.Status)
				assert.Equal(t, 1, stored.PrivateData.PollFailures)
			})
		})
	}
}

func TestUpdateVideoSingleTaskPollClassification(t *testing.T) {
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}))
	testCases := []struct {
		name          string
		statusCode    int
		fetchErr      error
		parse         *relaycommon.TaskInfo
		parseErr      error
		priorFailures int
		priorState    string
		maxFailures   int
		wantStatus    model.TaskStatus
		wantFailures  int
		wantRefund    bool
		wantReason    string
		wantState     string
		wantUnchanged bool
		wantConfirmed bool
	}{
		{
			name:          "confirmed upstream failure is distinguished from a polling cutoff",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "provider generation failed"},
			wantStatus:    model.TaskStatusFailure,
			wantRefund:    true,
			wantReason:    "provider generation failed",
			wantConfirmed: true,
		},
		{
			name:          "404 preserves reservation for reconciliation",
			statusCode:    http.StatusNotFound,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "401 increments without changing status",
			statusCode:    http.StatusUnauthorized,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "429 threshold retains reservation",
			statusCode:    http.StatusTooManyRequests,
			priorFailures: 2,
			maxFailures:   3,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  3,
			wantUnchanged: true,
		},
		{
			name:          "UNKNOWN increments",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusUnknown, Reason: "weird"},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "valid 2xx resets the failure counter",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorFailures: 5,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  0,
		},
		{
			name:       "omit state preserves previous plugin state",
			statusCode: http.StatusOK,
			parse:      &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorState: `{"req_key":"keep"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"keep"}`,
		},
		{
			name:       "returned state replaces plugin state",
			statusCode: http.StatusOK,
			parse: &relaycommon.TaskInfo{
				Status:      model.TaskStatusInProgress,
				PluginState: []byte(`{"req_key":"new"}`),
			},
			priorState: `{"req_key":"old"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"new"}`,
		},
		{
			name:          "other 4xx non-terminal is unrecognized",
			statusCode:    http.StatusBadRequest,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 510, 510, 510
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-poll-class", tokenRemain)
			ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Name: "poll", Key: "sk-test", Status: common.ChannelStatusEnabled}

			if testCase.maxFailures > 0 {
				previous := constant.TaskPollMaxFailures
				constant.TaskPollMaxFailures = testCase.maxFailures
				t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
			}

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_poll_class"
			task.PrivateData.UpstreamTaskID = "upstream_poll_class"
			task.PrivateData.AsyncMedia = true
			task.PrivateData.PollFailures = testCase.priorFailures
			if testCase.priorState != "" {
				task.PrivateData.PluginState = []byte(testCase.priorState)
			}
			require.NoError(t, model.DB.Create(task).Error)

			adaptor := &scriptedPollingAdaptor{statusCode: testCase.statusCode, fetchErr: testCase.fetchErr, parse: testCase.parse, parseErr: testCase.parseErr}
			require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{
				task.GetUpstreamTaskID(): task,
			}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			assert.Equal(t, testCase.wantConfirmed, persisted.PrivateData.VideoTerminalConfirmed)
			if testCase.wantUnchanged {
				assert.Empty(t, persisted.FailReason)
			}
			if testCase.wantReason != "" {
				assert.Contains(t, persisted.FailReason, testCase.wantReason)
			}
			if testCase.wantState != "" {
				assert.JSONEq(t, testCase.wantState, string(persisted.PrivateData.PluginState))
			}
			if testCase.wantRefund {
				assert.Equal(t, initialQuota+preConsumed, getUserQuota(t, userID))
				assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
				assert.Zero(t, persisted.Quota)
				log := getLastLog(t)
				require.NotNil(t, log)
				assert.Equal(t, model.LogTypeRefund, log.Type)
			} else {
				assert.Equal(t, initialQuota, getUserQuota(t, userID))
				assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
			}
		})
	}
}

func TestUpdateBatchTasksPollClassification(t *testing.T) {
	testCases := []struct {
		name         string
		statusCode   int
		resultStatus model.TaskStatus
		wantStatus   model.TaskStatus
		wantFailures int
		wantRefund   bool
		wantReason   string
	}{
		{
			name:       "404 fails the batch and refunds",
			statusCode: http.StatusNotFound,
			wantStatus: model.TaskStatusFailure,
			wantRefund: true,
			wantReason: "upstream task not found (HTTP 404)",
		},
		{
			name:         "401 increments every task",
			statusCode:   http.StatusUnauthorized,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
		{
			name:         "UNKNOWN increments",
			statusCode:   http.StatusOK,
			resultStatus: model.TaskStatusUnknown,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 610, 610, 610
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-batch-class", tokenRemain)
			seedTaskPollingChannel(t, channelID, true)

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_batch_class"
			task.PrivateData.UpstreamTaskID = "upstream_batch_class"
			require.NoError(t, model.DB.Create(task).Error)
			upstreamID := task.GetUpstreamTaskID()

			adaptor := &scriptedBatchPollingAdaptor{
				scriptedPollingAdaptor: scriptedPollingAdaptor{statusCode: testCase.statusCode},
			}
			if testCase.resultStatus != "" {
				adaptor.results = map[string]*BatchTaskResult{
					upstreamID: {TaskInfo: relaycommon.TaskInfo{TaskID: upstreamID, Status: string(testCase.resultStatus), Reason: "weird"}},
				}
			}

			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			if testCase.wantReason != "" {
				assert.Contains(t, persisted.FailReason, testCase.wantReason)
			}
			if testCase.wantRefund {
				assert.Equal(t, initialQuota+preConsumed, getUserQuota(t, userID))
				assert.Zero(t, persisted.Quota)
			} else {
				assert.Equal(t, initialQuota, getUserQuota(t, userID))
			}
		})
	}
}

func setupVideoReconciliationTask(t *testing.T) (*model.Task, *model.Channel) {
	t.Helper()
	setupMeasuredTaskBillingDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	require.NoError(t, model.DB.Where(map[string]any{"key": "media_runtime_config"}).Delete(&model.Option{}).Error)
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where(map[string]any{"key": "media_runtime_config"}).Delete(&model.Option{}).Error)
	})
	const userID, tokenID, channelID = 683, 683, 683
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "reconcile-token", 7000)
	seedTaskPollingChannel(t, channelID, true)
	channel, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	task := makeTask(userID, channelID, 4000, tokenID, BillingSourceWallet, 0)
	task.TaskID, task.SubmitTime = "task_reconciliation", time.Now().Unix()
	task.PrivateData.AsyncMedia, task.PrivateData.UpstreamTaskID, task.PrivateData.Key = true, "native-reconciliation", channel.Key
	require.NoError(t, model.DB.Create(task).Error)
	return task, channel
}

func TestAsyncVideoReconciliationPreservesReservation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		fetchErr   error
		status     string
		batch      bool
	}{
		{name: "transport", fetchErr: io.ErrUnexpectedEOF},
		{name: "rate limited", statusCode: http.StatusTooManyRequests},
		{name: "server failure", statusCode: http.StatusServiceUnavailable},
		{name: "unknown", status: model.TaskStatusUnknown},
		{name: "not found", statusCode: http.StatusNotFound},
		{name: "gone", statusCode: http.StatusGone},
		{name: "non-success HTTP cannot confirm failure", statusCode: http.StatusBadRequest, status: model.TaskStatusFailure},
		{name: "batch not found", statusCode: http.StatusNotFound, batch: true},
		{name: "batch unknown", status: model.TaskStatusUnknown, batch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, channel := setupVideoReconciliationTask(t)
			previous := constant.TaskPollMaxFailures
			constant.TaskPollMaxFailures = 1
			t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
			adaptor := &scriptedBatchPollingAdaptor{
				scriptedPollingAdaptor: scriptedPollingAdaptor{statusCode: tc.statusCode, fetchErr: tc.fetchErr, parse: &relaycommon.TaskInfo{Status: tc.status}},
				results:                map[string]*BatchTaskResult{task.GetUpstreamTaskID(): {TaskInfo: relaycommon.TaskInfo{Status: tc.status}}},
			}
			poll := func(current *model.Task) error {
				if tc.batch {
					return updateBatchTaskGroup(t.Context(), adaptor, channel, channel.Key, []*model.Task{current})
				}
				return updateVideoSingleTask(t.Context(), adaptor, channel, current.TaskID, map[string]*model.Task{current.TaskID: current})
			}
			require.NoError(t, poll(task))
			var stored model.Task
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			require.NotNil(t, stored.PrivateData.VideoReconciliation)
			assert.False(t, stored.PrivateData.VideoReconciliation.ManualRequired)
			assert.Equal(t, int64(86400), stored.PrivateData.VideoReconciliation.DeadlineAt-stored.PrivateData.VideoReconciliation.StartedAt)
			assert.Equal(t, 1, stored.PrivateData.PollFailures)
			assert.EqualValues(t, model.TaskStatusInProgress, stored.Status)
			assert.Zero(t, stored.FinishTime)
			assert.Equal(t, 4000, stored.Quota)
			assert.Equal(t, 10000, getUserQuota(t, task.UserId))
			assert.Equal(t, 7000, getTokenRemainQuota(t, task.PrivateData.TokenId))
			assert.Zero(t, countLogs(t))
			// A fresh database read models restart: it must honor the persisted
			// next query time rather than immediately spend another poll attempt.
			require.NoError(t, poll(&stored))
			assert.Equal(t, 1, adaptor.fetchCalls)
		})
	}
}

func TestAsyncVideoReconciliationResumeAndSettlement(t *testing.T) {
	for _, terminal := range []string{model.TaskStatusSuccess, model.TaskStatusFailure} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%v", terminal, batch), func(t *testing.T) {
				task, channel := setupVideoReconciliationTask(t)
				task.Platform = "drama"
				require.NoError(t, task.Update())
				adaptor := &scriptedBatchPollingAdaptor{scriptedPollingAdaptor: scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusUnknown}}, results: map[string]*BatchTaskResult{task.GetUpstreamTaskID(): {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusUnknown}}}}
				poll := func(current *model.Task) error {
					if batch {
						return updateBatchTaskGroup(t.Context(), adaptor, channel, channel.Key, []*model.Task{current})
					}
					return updateVideoSingleTask(t.Context(), adaptor, channel, current.TaskID, map[string]*model.Task{current.TaskID: current})
				}
				require.NoError(t, poll(task))
				require.NoError(t, model.DB.First(task, task.ID).Error)
				deadline := task.PrivateData.VideoReconciliation.DeadlineAt
				task.PrivateData.VideoReconciliation.NextPollAt = 0
				require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
				adaptor.parse = &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Reason: "still generating"}
				adaptor.results[task.GetUpstreamTaskID()].TaskInfo = *adaptor.parse
				require.NoError(t, poll(task))
				require.NoError(t, model.DB.First(task, task.ID).Error)
				assert.Equal(t, deadline, task.PrivateData.VideoReconciliation.DeadlineAt, "pending does not renew the window")
				assert.EqualValues(t, model.TaskStatusInProgress, task.Status, "nonterminal reason text is not authoritative failure")
				assert.Zero(t, task.PrivateData.PollFailures)
				task.PrivateData.VideoReconciliation.DeadlineAt = time.Now().Unix() - 1
				require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
				calls := adaptor.fetchCalls
				require.NoError(t, poll(task))
				require.NoError(t, model.DB.First(task, task.ID).Error)
				require.True(t, task.PrivateData.VideoReconciliation.ManualRequired)
				assert.Equal(t, calls, adaptor.fetchCalls, "expired windows stop automatic queries")
				assert.Equal(t, 4000, task.Quota)
				assert.Zero(t, countLogs(t))
				stale := *task
				before := stale.Snapshot()
				resumed, won, err := model.ResumeAsyncVideoReconciliation(t.Context(), task.TaskID, task.UserId, 60)
				require.NoError(t, err)
				require.True(t, won)
				assert.Equal(t, int64(60), resumed.PrivateData.VideoReconciliation.DeadlineAt-resumed.PrivateData.VideoReconciliation.StartedAt)
				assert.Equal(t, task.PrivateData.UpstreamTaskID, resumed.PrivateData.UpstreamTaskID)
				assert.Equal(t, task.AttemptGeneration, resumed.AttemptGeneration)
				stale.PrivateData.PollFailures = 999
				won, err = stale.UpdatePollingState(t.Context(), before)
				require.NoError(t, err)
				assert.False(t, won, "an old poll cannot undo operator resume")
				adaptor.parse = &relaycommon.TaskInfo{Status: terminal, Url: "https://cdn.example/result.mp4"}
				if terminal == model.TaskStatusFailure {
					adaptor.parse.Url = ""
					adaptor.parse.Reason = "reference video download failed"
					adaptor.body = []byte(`{"status":"failed","id":"native-reconciliation"}`)
				}
				adaptor.results[task.GetUpstreamTaskID()].TaskInfo = *adaptor.parse
				late := *resumed
				require.NoError(t, poll(resumed))
				logs := countLogs(t)
				require.NoError(t, poll(&late))
				assert.Equal(t, logs, countLogs(t), "duplicate terminal replies do not settle twice")
				require.NoError(t, model.DB.First(task, task.ID).Error)
				assert.EqualValues(t, terminal, task.Status)
				assert.Nil(t, task.PrivateData.VideoReconciliation)
				assert.True(t, task.PrivateData.VideoTerminalConfirmed)
				assert.Equal(t, int64(0), task.AttemptGeneration, "reconciliation never generates another video")
				assert.Equal(t, "native-reconciliation", task.PrivateData.UpstreamTaskID)
				if terminal == model.TaskStatusFailure {
					assert.Zero(t, task.Quota)
					assert.Equal(t, 14000, getUserQuota(t, task.UserId))
					assert.Equal(t, 11000, getTokenRemainQuota(t, task.PrivateData.TokenId))
				} else {
					assert.Equal(t, 4000, task.Quota)
					assert.Equal(t, 10000, getUserQuota(t, task.UserId))
				}
				_, won, err = model.ResumeAsyncVideoReconciliation(t.Context(), task.TaskID, task.UserId, 60)
				require.NoError(t, err)
				assert.False(t, won, "terminal tasks cannot be resumed")
				if !batch {
					require.NoError(t, model.DB.AutoMigrate(&model.MediaInputObject{}))
					job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: task.TaskID, UserId: task.UserId, ChannelId: task.ChannelId, TokenId: task.PrivateData.TokenId, Status: "reconciling", StorageStatus: "pending", BillingStatus: "reserved", Quota: 4000, ExpiresAt: time.Now().Unix() + 3600}
					require.NoError(t, model.DB.Create(&job).Error)
					jobs, err := model.GetDueAsyncMediaJobsForPhase(t.Context(), time.Now().Unix(), 10, "status")
					require.NoError(t, err)
					require.Len(t, jobs, 1)
					claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "terminal-sync", 30)
					require.NoError(t, err)
					require.True(t, claimed)
					require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, DefaultMediaRuntimeConfig()))
					require.NoError(t, model.DB.First(&job, job.ID).Error)
					if terminal == model.TaskStatusSuccess {
						assert.Equal(t, "submitted", job.Status)
						assert.Equal(t, "ready", job.StorageStatus)
						jobs, err = model.GetDueAsyncMediaJobsForPhase(t.Context(), time.Now().Unix(), 10, "download")
						require.NoError(t, err)
						assert.Len(t, jobs, 1, "completed reconciliation moves into the download pool")
					} else {
						assert.Equal(t, "failed", job.Status)
						assert.Equal(t, "refunded", job.BillingStatus)
					}
				}
			})
		}
	}
}

func TestAsyncVideoReconciliationConcurrentPollingPreservesAccounting(t *testing.T) {
	task, _ := setupVideoReconciliationTask(t)
	var first, second model.Task
	require.NoError(t, model.DB.First(&first, task.ID).Error)
	require.NoError(t, model.DB.First(&second, task.ID).Error)
	before := first.Snapshot()
	first.PrivateData.PollFailures, second.PrivateData.PollFailures = 1, 2
	// Accounting confirmation does not change the upstream polling state.
	task.PrivateData.SubmitAccountingRecorded = true
	require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
	type outcome struct {
		won bool
		err error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for _, contender := range []*model.Task{&first, &second} {
		go func() {
			<-start
			won, err := contender.UpdatePollingState(t.Context(), before)
			results <- outcome{won, err}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		if result.won {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.True(t, task.PrivateData.SubmitAccountingRecorded)
	assert.Equal(t, 4000, task.Quota)
	// A confirmed failure query can have started before another poll entered
	// reconciliation. The recovery transaction must re-check the locked row.
	task.Platform, task.PrivateData.DurableBilling = "drama", true
	require.NoError(t, task.Update())
	stale := *task
	job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: task.TaskID, UserId: task.UserId, ChannelId: task.ChannelId, TokenId: task.PrivateData.TokenId, Provider: "drama", Status: "submitted", BillingStatus: "reserved", Quota: task.Quota, RequestCipher: []byte("request"), ExpiresAt: time.Now().Unix() + 3600}
	require.NoError(t, model.DB.Create(&job).Error)
	require.NoError(t, reconcileAsyncVideoTask(t.Context(), task, task.Snapshot(), pollClassTransport, 0))
	require.ErrorIs(t, model.UpdateAsyncMediaJob(t.Context(), job, map[string]any{"status": "expired", "channel_cipher": nil}), model.ErrImageConflict, "a pre-reconciliation lease cannot erase the new projection")
	won, err := model.ScheduleAsyncVideoReferenceRecovery(t.Context(), stale, job, "reference video download failed", true)
	require.NoError(t, err)
	assert.False(t, won, "an old response cannot start generation after reconciliation begins")
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.Zero(t, task.AttemptGeneration)
	assert.Equal(t, "native-reconciliation", task.PrivateData.UpstreamTaskID)
}

func TestAsyncVideoReconciliationMissingChannelRetainsHold(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			task, channel := setupVideoReconciliationTask(t)
			require.NoError(t, model.DB.Delete(channel).Error)
			tasks := map[string]*model.Task{task.TaskID: task}
			if batch {
				_ = updateBatchTasks(t.Context(), &scriptedBatchPollingAdaptor{}, channel.Id, []string{task.TaskID}, tasks)
			} else {
				_ = updateVideoTasks(t.Context(), task.Platform, channel.Id, []string{task.TaskID}, tasks)
			}
			require.NoError(t, model.DB.First(task, task.ID).Error)
			require.NotNil(t, task.PrivateData.VideoReconciliation)
			assert.Contains(t, task.PrivateData.VideoReconciliation.Reason, "channel_unavailable")
			assert.EqualValues(t, model.TaskStatusInProgress, task.Status)
			assert.Equal(t, 4000, task.Quota)
			assert.Equal(t, 10000, getUserQuota(t, task.UserId))
			assert.Zero(t, countLogs(t))
		})
	}
}

func TestAsyncVideoReconciliationLateTerminalAfterPause(t *testing.T) {
	task, channel := setupVideoReconciliationTask(t)
	now := time.Now().Unix()
	task.PrivateData.VideoReconciliation = &model.VideoTaskReconciliation{StartedAt: now - 30, DeadlineAt: now + 60, Revision: 1}
	require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
	adaptor := &boundedVideoPollingAdaptor{fetch: func(context.Context) (*http.Response, error) {
		var paused model.Task
		require.NoError(t, model.DB.First(&paused, task.ID).Error)
		paused.PrivateData.VideoReconciliation.DeadlineAt = now - 1
		require.NoError(t, model.DB.Model(&paused).Update("private_data", paused.PrivateData).Error)
		ready, err := asyncVideoPollReady(t.Context(), &paused)
		require.NoError(t, err)
		require.False(t, ready)
		require.True(t, paused.PrivateData.VideoReconciliation.ManualRequired)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}}
	adaptor.parse = &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, Url: "https://cdn.example/result.mp4"}
	require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, task.TaskID, map[string]*model.Task{task.TaskID: task}))
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusSuccess, task.Status)
	assert.Nil(t, task.PrivateData.VideoReconciliation)
	assert.Equal(t, 4000, task.Quota)
}

func TestAsyncVideoReconciliationTimeoutAndRetention(t *testing.T) {
	task, _ := setupVideoReconciliationTask(t)
	previous := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previous })
	task.SubmitTime = time.Now().Unix() - 120
	require.NoError(t, task.Update())
	sweepTimedOutTasks(t.Context())
	require.NoError(t, model.DB.First(task, task.ID).Error)
	require.NotNil(t, task.PrivateData.VideoReconciliation)
	deadline := task.PrivateData.VideoReconciliation.DeadlineAt
	sweepTimedOutTasks(t.Context())
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.Equal(t, deadline, task.PrivateData.VideoReconciliation.DeadlineAt)
	assert.Zero(t, task.PrivateData.VideoReconciliation.NextPollAt, "sweep leaves the first reconciliation query eligible")
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: task.TaskID, UserId: task.UserId, ChannelId: task.ChannelId, TokenId: task.PrivateData.TokenId, IdentityHash: "reconciliation-retention", Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: task.Quota, CreatedAt: now - 3600, ExpiresAt: now - 1, LeaseToken: "retention-lease", LeaseExpiresAt: now + 30, RequestCipher: []byte("keep-request"), ChannelCipher: []byte("keep-channel")}
	require.NoError(t, model.DB.Create(&job).Error)
	require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, DefaultMediaRuntimeConfig()))
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "reconciling", job.Status)
	assert.Equal(t, "reserved", job.BillingStatus)
	assert.Equal(t, []byte("keep-request"), job.RequestCipher)
	assert.Equal(t, []byte("keep-channel"), job.ChannelCipher)
	require.ErrorIs(t, model.ExpireAsyncMediaJob(t.Context(), job, now), model.ErrImageConflict)
	task.PrivateData.VideoReconciliation.DeadlineAt = now - 1
	require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
	sweepTimedOutTasks(t.Context())
	require.NoError(t, model.DB.First(task, task.ID).Error)
	assert.True(t, task.PrivateData.VideoReconciliation.ManualRequired)
	assert.Equal(t, 4000, task.Quota)
	assert.Equal(t, 10000, getUserQuota(t, task.UserId))
	assert.Zero(t, countLogs(t))
	// Recover an already expired pre-upgrade job without changing artifact TTL
	// or submitting the same generation again.
	require.NoError(t, model.DB.Model(&job).Update("status", "expired").Error)
	_, won, err := model.ResumeAsyncVideoReconciliation(t.Context(), task.TaskID, task.UserId, 60)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "reconciling", job.Status)
	assert.Equal(t, now-1, job.ExpiresAt)
	assert.Equal(t, "native-reconciliation", task.GetUpstreamTaskID())
}

func TestAsyncVideoReconciliationCleanupRetainsPendingAndRetriesFiles(t *testing.T) {
	task, _ := setupVideoReconciliationTask(t)
	require.NoError(t, model.DB.AutoMigrate(&model.MediaArtifactObject{}))
	now := time.Now().Unix()
	root := t.TempDir()
	pendingPath := filepath.Join(root, "pending.bin")
	require.NoError(t, os.WriteFile(pendingPath, []byte("pending output"), 0600))
	var pendingJobs []model.AsyncMediaJob
	// One full cleanup page of unresolved operations must not hide a later
	// terminal job. These records exercise the actual 100-row cleanup bound.
	for i := range 100 {
		pending := *task
		pending.ID, pending.TaskID = 0, fmt.Sprintf("cleanup-pending-%d", i)
		pending.PrivateData.VideoReconciliation = &model.VideoTaskReconciliation{StartedAt: now - 60, DeadlineAt: now, ManualRequired: true, Revision: 1}
		require.NoError(t, model.DB.Create(&pending).Error)
		job := model.AsyncMediaJob{TaskId: pending.TaskID, IdentityHash: pending.TaskID, UserId: task.UserId, Status: "reconciling", StorageStatus: "pending", BillingStatus: "reserved", ExpiresAt: now - 1, RequestCipher: []byte("keep-request"), ChannelCipher: []byte("keep-channel")}
		if i == 0 {
			job.OutputRootPath, job.OutputObjectKey = root, "pending.bin"
		}
		pendingJobs = append(pendingJobs, job)
	}
	require.NoError(t, model.DB.Create(&pendingJobs).Error)
	finished := *task
	finished.ID, finished.TaskID, finished.Status = 0, "cleanup-finished", model.TaskStatusSuccess
	require.NoError(t, model.DB.Create(&finished).Error)
	blockedPath := filepath.Join(root, "blocked")
	require.NoError(t, os.Mkdir(blockedPath, 0700))
	childPath := filepath.Join(blockedPath, "child")
	require.NoError(t, os.WriteFile(childPath, []byte("busy"), 0600))
	job := model.AsyncMediaJob{TaskId: finished.TaskID, IdentityHash: finished.TaskID, UserId: task.UserId, Status: "succeeded", StorageStatus: "succeeded", BillingStatus: "settled", ExpiresAt: now - 1, RequestCipher: []byte("expire-request"), ChannelCipher: []byte("expire-channel"), OutputRootPath: root, OutputObjectKey: "blocked"}
	require.NoError(t, model.DB.Create(&job).Error)
	require.Error(t, CleanupMediaObjects(t.Context()), "a nonempty directory deterministically prevents output removal")
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "expired", job.Status)
	assert.Empty(t, job.RequestCipher)
	assert.Empty(t, job.ChannelCipher)
	assert.Equal(t, "blocked", job.OutputObjectKey, "failed filesystem cleanup retains a retry pointer")
	require.NoError(t, os.Remove(childPath))
	require.NoError(t, CleanupMediaObjects(t.Context()))
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Empty(t, job.OutputObjectKey)
	assert.Empty(t, job.OutputRootPath)
	_, err := os.Stat(blockedPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(pendingPath)
	require.NoError(t, err, "cleanup must not remove output before its task eligibility check")
	var kept model.AsyncMediaJob
	require.NoError(t, model.DB.First(&kept, pendingJobs[0].ID).Error)
	assert.Equal(t, "reconciling", kept.Status)
	assert.Equal(t, []byte("keep-channel"), kept.ChannelCipher)
}

func TestAsyncVideoReconciliationAcceptedTaskExpiry(t *testing.T) {
	task, _ := setupVideoReconciliationTask(t)
	require.NoError(t, model.DB.AutoMigrate(&model.MediaArtifactObject{}))
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: task.TaskID, IdentityHash: task.TaskID, UserId: task.UserId, ChannelId: task.ChannelId, TokenId: task.PrivateData.TokenId, Status: "submitted", StorageStatus: "pending", BillingStatus: "reserved", Quota: task.Quota, ExpiresAt: now - 1, RequestCipher: []byte("keep-request"), ChannelCipher: []byte("keep-channel")}
	require.NoError(t, model.DB.Create(&job).Error)
	require.Nil(t, task.PrivateData.VideoReconciliation)
	// Cleanup may run before the status worker first observes retention expiry.
	require.ErrorIs(t, model.ExpireAsyncMediaJob(t.Context(), job, now), model.ErrImageConflict)
	require.NoError(t, CleanupMediaObjects(t.Context()))
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, []byte("keep-channel"), job.ChannelCipher)
	claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "expired-accepted", 30)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, DefaultMediaRuntimeConfig()))
	require.NoError(t, model.DB.First(task, task.ID).Error)
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	require.NotNil(t, task.PrivateData.VideoReconciliation)
	assert.Contains(t, task.PrivateData.VideoReconciliation.Reason, "retention_expired")
	assert.Equal(t, "reconciling", job.Status)
	assert.Empty(t, job.LeaseToken, "the worker releases its lease after its own status projection changes")
	assert.Equal(t, []byte("keep-request"), job.RequestCipher)
	assert.Equal(t, []byte("keep-channel"), job.ChannelCipher)
	assert.Equal(t, 4000, task.Quota)
	assert.Zero(t, countLogs(t))
}

func TestPendingTaskAtFullProgressRemainsPollableAndTimesOutOnce(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID = 680, 680, 680
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "full-progress-token", 7000)
	seedTaskPollingChannel(t, channelID, true)
	task := makeTask(userID, channelID, 4000, tokenID, BillingSourceWallet, 0)
	task.TaskID, task.Progress = "task_full_progress", "100%"
	task.SubmitTime = time.Now().Add(-2 * time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)
	assert.True(t, model.HasUnfinishedSyncTasks())
	require.Len(t, model.GetAllUnFinishSyncTasks(10), 1)
	require.Len(t, model.GetTimedOutUnfinishedTasks(time.Now().Add(-time.Minute).Unix(), 10), 1)
	page, err := model.GetUnfinishedSyncTaskPage(t.Context(), 0, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 1)
	ch, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	adaptor := &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Progress: "100%"}}
	require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, ch, task.TaskID, map[string]*model.Task{task.TaskID: task}))
	var pending model.Task
	require.NoError(t, model.DB.First(&pending, task.ID).Error)
	assert.Equal(t, "99%", pending.Progress)
	previousTimeout, previousCursor := constant.TaskTimeoutMinutes, taskTimeoutCursor.Swap(0)
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout; taskTimeoutCursor.Store(previousCursor) })
	for range 2 {
		sweepTimedOutTasks(t.Context())
	}
	var finished model.Task
	require.NoError(t, model.DB.First(&finished, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, finished.Status)
	assert.Zero(t, finished.Quota)
	assert.Equal(t, 14000, getUserQuota(t, userID))
	assert.Equal(t, 11000, getTokenRemainQuota(t, tokenID))
	assert.False(t, model.HasUnfinishedSyncTasks())
}

func TestBatchMissingTaskUsesFailureBudgetWithoutAffectingOtherTasks(t *testing.T) {
	setupMeasuredTaskBillingDB(t)
	const userID, tokenID, channelID = 681, 681, 681
	seedUser(t, userID, 10000)
	seedToken(t, tokenID, userID, "batch-missing-token", 7000)
	seedTaskPollingChannel(t, channelID, true)
	previousLimit := constant.TaskPollMaxFailures
	constant.TaskPollMaxFailures = 2
	t.Cleanup(func() { constant.TaskPollMaxFailures = previousLimit })
	missing := makeTask(userID, channelID, 4000, tokenID, BillingSourceWallet, 0)
	missing.TaskID, missing.PrivateData.UpstreamTaskID = "task_missing", "upstream_missing"
	require.NoError(t, model.DB.Create(missing).Error)
	present := seedPollingTask(t, channelID, "task_present", "upstream_present")
	present.PrivateData.PollFailures = 1
	require.NoError(t, present.Update())
	adaptor := &scriptedBatchPollingAdaptor{results: map[string]*BatchTaskResult{
		present.GetUpstreamTaskID(): {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Progress: "100%"}},
	}}
	ch, err := model.GetChannelById(channelID, true)
	require.NoError(t, err)
	require.NoError(t, updateBatchTaskGroup(t.Context(), adaptor, ch, ch.Key, []*model.Task{missing, present}))
	assert.Equal(t, 1, missing.PrivateData.PollFailures)
	assert.EqualValues(t, model.TaskStatusInProgress, missing.Status)
	assert.Zero(t, present.PrivateData.PollFailures)
	assert.Equal(t, "99%", present.Progress)
	stale := *missing
	require.NoError(t, updateBatchTaskGroup(t.Context(), adaptor, ch, ch.Key, []*model.Task{missing, present}))
	// A poll started from the same pre-terminal row cannot issue a second refund.
	require.NoError(t, updateBatchTaskGroup(t.Context(), adaptor, ch, ch.Key, []*model.Task{&stale}))
	var finished model.Task
	require.NoError(t, model.DB.First(&finished, missing.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, finished.Status)
	assert.Equal(t, 2, finished.PrivateData.PollFailures)
	assert.Contains(t, finished.FailReason, "batch response omitted the requested task")
	assert.Zero(t, finished.Quota)
	assert.Equal(t, 14000, getUserQuota(t, userID))
	assert.Equal(t, 11000, getTokenRemainQuota(t, tokenID))
	require.NoError(t, model.DB.First(present, present.ID).Error)
	assert.EqualValues(t, model.TaskStatusInProgress, present.Status)
	assert.Zero(t, present.PrivateData.PollFailures)
}

func upstreamAsyncTestProfile(t *testing.T, mediaType string) *dto.UpstreamAsyncProfile {
	t.Helper()
	configJSON := fmt.Sprintf(`{"profiles":[{"id":"dynamic","media_type":%q,"models":["mapped"],"operations":["generate"],"submit":{"task_id_path":"job.identifier"},"poll":{"request":{"method":"POST","path":"/tasks/{task_id}","query":{"model":"{upstream_model}"},"headers":{"Authorization":"Bearer {api_key}"},"body":{"task":"{task_id}","model":"{model}"}},"response":{"status_path":"job.status","status_values":{"queued":["queued",1],"in_progress":["running"],"succeeded":["done",true],"failed":["failed",false]},"progress_path":"job.progress","failure_reason_path":"job.error.message","result_path":"job.output","usage_paths":{"prompt_tokens":"usage.input","completion_tokens":"usage.output","total_tokens":"usage.total"},"download_headers":{"Authorization":"Bearer {api_key}"}}}}]}`, mediaType)
	var config dto.UpstreamAsyncConfig
	require.NoError(t, common.Unmarshal([]byte(configJSON), &config))
	require.NoError(t, config.Validate())
	profile, matched := config.Match(mediaType, "mapped", "generate")
	require.True(t, matched)
	return profile
}

func TestUpstreamAsyncRequestAndSubmitParsing(t *testing.T) {
	profile := upstreamAsyncTestProfile(t, "video")
	request, err := BuildUpstreamAsyncPollRequest(t.Context(), "https://vendor.example/api", profile, UpstreamAsyncTemplateContext{
		TaskID:        "job/id",
		Model:         "client-model",
		UpstreamModel: "mapped",
		APIKey:        "secret",
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, request.Method)
	assert.Equal(t, "https://vendor.example/api/tasks/job%2Fid?model=mapped", request.URL.String())
	assert.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
	body, err := io.ReadAll(request.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"task":"job/id","model":"client-model"}`, string(body))

	_, err = BuildUpstreamAsyncPollRequest(t.Context(), "https://vendor.example/api", profile, UpstreamAsyncTemplateContext{TaskID: "job", APIKey: "secret\r\nX-Injected: yes"})
	require.ErrorContains(t, err, "header is invalid")

	taskID, err := ParseUpstreamAsyncTaskID(profile, []byte(`{"job":{"identifier":987654321}}`))
	require.NoError(t, err)
	assert.Equal(t, "987654321", taskID)
	_, err = ParseUpstreamAsyncTaskID(profile, []byte(`{"job":{}}`))
	require.Error(t, err)

	request, err = BuildUpstreamAsyncPollRequest(t.Context(), "https://vendor.example/api", profile, UpstreamAsyncTemplateContext{TaskID: "{model}", Model: "{task_id}"})
	require.NoError(t, err)
	assert.Equal(t, "/api/tasks/{model}", request.URL.Path)
	body, err = io.ReadAll(request.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"task":"{model}","model":"{task_id}"}`, string(body), "template substitutions must preserve opaque task IDs and model values")
}

func TestUpstreamAsyncCustomSubmitRequest(t *testing.T) {
	var config dto.UpstreamAsyncConfig
	require.NoError(t, common.Unmarshal([]byte(`{"profiles":[{"id":"mai-image","media_type":"image","models":["seedream-5-pro"],"operations":["generate"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/v1/videos","headers":{"Authorization":"Bearer {api_key}"},"body":{"model":"{upstream_model}","prompt":"{request.prompt}","aspect_ratio":"{request.aspect_ratio}","quality":"1440p","image_urls":"{request.image_urls}","create_count":"{request.create_count}"}}},"poll":{"request":{"method":"GET","path":"/v1/videos/{task_id}","headers":{"Authorization":"Bearer {api_key}"}},"response":{"status_path":"status","status_values":{"queued":["queued"],"in_progress":["in_progress"],"succeeded":["completed"],"failed":["failed","cancelled"]},"result_path":"video_url"}}}]}`), &config))
	require.NoError(t, config.Validate())
	profile, found := config.Match("image", "seedream-5-pro", "generate")
	require.True(t, found)
	profile.Submit.Request.Query = map[string]string{"client": "{model}"}
	require.NoError(t, (&dto.UpstreamAsyncConfig{Profiles: []dto.UpstreamAsyncProfile{*profile}}).Validate())
	source := map[string]any{"prompt": "panda", "aspect_ratio": "16:9", "image_urls": []string{"https://cdn.example/ref.jpg"}, "create_count": 1}
	request, err := BuildUpstreamAsyncSubmitRequest(t.Context(), "https://api.mai-token.com", profile, UpstreamAsyncTemplateContext{Model: "public-model", UpstreamModel: "seedream-5-pro", APIKey: "secret"}, source, strings.NewReader(`{"wrong":true}`), "application/json")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, request.Method)
	assert.Equal(t, "https://api.mai-token.com/v1/videos?client=public-model", request.URL.String())
	assert.Equal(t, "Bearer secret", request.Header.Get("Authorization"))
	assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
	body, err := io.ReadAll(request.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"seedream-5-pro","prompt":"panda","aspect_ratio":"16:9","quality":"1440p","image_urls":["https://cdn.example/ref.jpg"],"create_count":1}`, string(body))
	require.NoError(t, ValidateUpstreamAsyncImageSubmitCount(request, 1))
	taskID, err := ParseUpstreamAsyncTaskID(profile, []byte(`{"id":"task_123","status":"queued"}`))
	require.NoError(t, err)
	assert.Equal(t, "task_123", taskID)

	profile.Submit.Request.Body = map[string]any{"create_count": float64(2)}
	request, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://api.mai-token.com", profile, UpstreamAsyncTemplateContext{}, source, nil, "")
	require.NoError(t, err)
	require.ErrorContains(t, ValidateUpstreamAsyncImageSubmitCount(request, 1), "validated image count")

	profile.Submit.Request.Body = map[string]any{"image_urls": "{request.image_urls}", "optional": "{request.missing}"}
	request, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://api.mai-token.com", profile, UpstreamAsyncTemplateContext{}, source, nil, "")
	require.NoError(t, err)
	body, err = io.ReadAll(request.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"image_urls":["https://cdn.example/ref.jpg"]}`, string(body))
}

func TestUpstreamAsyncCustomSubmitRejectsUnsafeTemplates(t *testing.T) {
	profile := upstreamAsyncTestProfile(t, "video")
	profile.Submit.Request = &dto.UpstreamAsyncSubmitRequest{Method: http.MethodPost, Path: "/v1/videos", Body: map[string]any{"seconds": "{request.seconds}"}}
	require.NoError(t, (&dto.UpstreamAsyncConfig{Profiles: []dto.UpstreamAsyncProfile{*profile}}).Validate())
	source := map[string]any{"seconds": 4}
	request, err := BuildUpstreamAsyncSubmitRequest(t.Context(), "https://vendor.example", profile, UpstreamAsyncTemplateContext{}, source, nil, "application/json")
	require.NoError(t, err)
	require.NoError(t, ValidateUpstreamAsyncVideoSubmitDuration(request, source, 3600))
	for _, duration := range []any{float64(4), "4"} {
		for _, sourceDuration := range []any{4, "4"} {
			profile.Submit.Request.Body = map[string]any{"seconds": duration}
			stringSource := map[string]any{"seconds": sourceDuration}
			request, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://vendor.example", profile, UpstreamAsyncTemplateContext{}, stringSource, nil, "application/json")
			require.NoError(t, err)
			require.NoError(t, ValidateUpstreamAsyncVideoSubmitDuration(request, stringSource, 3600), "valid numeric string durations must match the validated duration")
		}
	}
	profile.Submit.Request.Body = map[string]any{"seconds": float64(3601)}
	request, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://vendor.example", profile, UpstreamAsyncTemplateContext{}, source, nil, "application/json")
	require.NoError(t, err)
	require.ErrorContains(t, ValidateUpstreamAsyncVideoSubmitDuration(request, source, 3600), "validated video duration")
	for _, invalidDuration := range []any{"0", "-1", "3601", "NaN", "+Inf", "not a duration", true} {
		profile.Submit.Request.Body = map[string]any{"seconds": invalidDuration}
		request, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://vendor.example", profile, UpstreamAsyncTemplateContext{}, source, nil, "application/json")
		require.NoError(t, err)
		require.ErrorContains(t, ValidateUpstreamAsyncVideoSubmitDuration(request, source, 3600), "validated video duration")
	}
	_, err = BuildUpstreamAsyncSubmitRequest(t.Context(), "https://vendor.example", profile, UpstreamAsyncTemplateContext{}, source, nil, "multipart/form-data; boundary=test")
	require.ErrorContains(t, err, "cannot replace multipart input")

	for _, invalid := range []*dto.UpstreamAsyncSubmitRequest{
		{Method: http.MethodPost, Path: "https://evil.example/submit"},
		{Method: http.MethodPost, Path: "/v1/videos?"},
		{Method: http.MethodPost, Path: "/v1/videos/{task_id}"},
		{Method: http.MethodPost, Path: "/v1/videos", Query: map[string]string{"id": "{task_id}"}},
		{Method: http.MethodPost, Path: "/v1/videos", Headers: map[string]string{"X-Task-ID": "{task_id}"}},
		{Method: http.MethodGet, Path: "/v1/videos"},
		{Method: http.MethodPost, Path: "/v1/videos", Body: map[string]any{"prompt": "prefix {request.prompt}"}},
		{Method: http.MethodPost, Path: "/v1/videos", Headers: map[string]string{"Host": "evil.example"}},
	} {
		profile.Submit.Request = invalid
		require.Error(t, (&dto.UpstreamAsyncConfig{Profiles: []dto.UpstreamAsyncProfile{*profile}}).Validate())
	}
}

func TestUpstreamAsyncPollParsingStatusResultsAndUsage(t *testing.T) {
	imageProfile := upstreamAsyncTestProfile(t, "image")
	imageProfile.Poll.Response.UsagePaths["prompt_tokens_details.cached_tokens_details.image_tokens"] = "usage.cached_image"
	result, err := ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":"queued","progress":25}}`))
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), result.Status)
	assert.Equal(t, "25%", result.Progress)

	result, err = ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":"missing"}}`))
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusUnknown), result.Status)

	result, err = ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":"done","output":["https://cdn.example/a.png","https://cdn.example/b.png"]},"usage":{"input":10,"output":4,"total":14,"cached_image":2}}`))
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), result.Status)
	assert.Equal(t, []string{"https://cdn.example/a.png", "https://cdn.example/b.png"}, result.URLs)
	require.NotNil(t, result.Usage)
	assert.Equal(t, 10, result.Usage.PromptTokens)
	assert.Equal(t, 4, result.Usage.CompletionTokens)
	assert.Equal(t, 14, result.Usage.TotalTokens)
	require.NotNil(t, result.Usage.PromptTokensDetails.CachedTokensDetails)
	require.NotNil(t, result.Usage.PromptTokensDetails.CachedTokensDetails.ImageTokens)
	assert.Equal(t, 2, *result.Usage.PromptTokensDetails.CachedTokensDetails.ImageTokens)

	result, err = ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":"failed","error":{"message":"Bearer private-token request failed"}}}`))
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), result.Status)
	assert.NotContains(t, result.Reason, "private-token")

	videoProfile := upstreamAsyncTestProfile(t, "video")
	_, err = ParseUpstreamAsyncPollResponse(videoProfile, []byte(`{"job":{"status":true,"output":["https://cdn.example/a.mp4","https://cdn.example/b.mp4"]}}`))
	require.Error(t, err)
	_, err = ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":true,"output":"https://cdn.example/a.png"},"usage":{"total":-1}}`))
	require.ErrorIs(t, err, ErrUpstreamAsyncBillingUsage)
	_, err = ParseUpstreamAsyncPollResponse(imageProfile, []byte(`{"job":{"status":true,"output":"https://cdn.example/a.png"},"usage":{"input":2147483647,"output":1,"total":1}}`))
	require.ErrorIs(t, err, ErrUpstreamAsyncBillingUsage)
}

func TestUpstreamAsyncPollActualVideoSeconds(t *testing.T) {
	profile := upstreamAsyncTestProfile(t, "video")
	completed := `{"job":{"status":"done","output":"https://cdn.example/video.mp4","duration":3.25}}`

	result, err := ParseUpstreamAsyncPollResponse(profile, []byte(completed))
	require.NoError(t, err)
	assert.Nil(t, result.ActualSeconds, "existing profiles must keep their original billing behavior")

	profile.Poll.Response.ActualSecondsPath = "job.duration"
	result, err = ParseUpstreamAsyncPollResponse(profile, []byte(completed))
	require.NoError(t, err)
	require.NotNil(t, result.ActualSeconds)
	assert.Equal(t, 3.25, *result.ActualSeconds)

	result, err = ParseUpstreamAsyncPollResponse(profile, []byte(`{"job":{"status":"running","duration":"bad"}}`))
	require.NoError(t, err)
	assert.Nil(t, result.ActualSeconds)

	for _, testCase := range []struct {
		name     string
		duration string
	}{
		{name: "missing"},
		{name: "null", duration: `,"duration":null`},
		{name: "string", duration: `,"duration":"3.25"`},
		{name: "boolean", duration: `,"duration":true`},
		{name: "zero", duration: `,"duration":0`},
		{name: "negative", duration: `,"duration":-1`},
		{name: "fraction above maximum", duration: `,"duration":3600.0000000000000001`},
		{name: "above maximum", duration: `,"duration":3601`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := `{"job":{"status":"done","output":"https://cdn.example/video.mp4"` + testCase.duration + `}}`
			_, err := ParseUpstreamAsyncPollResponse(profile, []byte(body))
			require.ErrorIs(t, err, ErrUpstreamAsyncBillingUsage)
		})
	}

	result, err = ParseUpstreamAsyncPollResponse(profile, []byte(`{"job":{"status":"done","output":"https://cdn.example/video.mp4","duration":3600}}`))
	require.NoError(t, err)
	require.NotNil(t, result.ActualSeconds)
	assert.Equal(t, float64(3600), *result.ActualSeconds)
}

func TestUpstreamAsyncDownloadHeadersRequireSameOrigin(t *testing.T) {
	profile := upstreamAsyncTestProfile(t, "video")
	headers, credentialless, err := BuildUpstreamAsyncDownloadHeaders(profile, "https://vendor.example/output.mp4", "https://vendor.example/api", UpstreamAsyncTemplateContext{APIKey: "secret"})
	require.NoError(t, err)
	assert.False(t, credentialless)
	assert.Equal(t, "Bearer secret", headers["Authorization"])

	_, _, err = BuildUpstreamAsyncDownloadHeaders(profile, "https://vendor.example/output.mp4", "https://vendor.example/api", UpstreamAsyncTemplateContext{APIKey: "secret\r\nX-Injected: yes"})
	require.ErrorContains(t, err, "header is invalid")

	_, _, err = BuildUpstreamAsyncDownloadHeaders(profile, "https://cdn.example/output.mp4", "https://vendor.example/api", UpstreamAsyncTemplateContext{APIKey: "secret"})
	require.Error(t, err)

	profile.Poll.Response.DownloadHeaders = nil
	headers, credentialless, err = BuildUpstreamAsyncDownloadHeaders(profile, "https://cdn.example/output.mp4", "https://vendor.example/api", UpstreamAsyncTemplateContext{APIKey: "secret"})
	require.NoError(t, err)
	assert.True(t, credentialless)
	assert.Empty(t, headers)
}

func TestUpstreamAsyncPollRedirectsKeepCredentialsAtOrigin(t *testing.T) {
	targetRequests := make(chan http.Header, 1)
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetRequests <- request.Header.Clone()
		_, _ = io.WriteString(writer, `{"job":{"status":"queued"}}`)
	}))
	defer target.Close()
	previousClient := httpClient
	httpClient = target.Client()
	t.Cleanup(func() {
		httpClient.CloseIdleConnections()
		httpClient = previousClient
	})

	for _, external := range []bool{true, false} {
		t.Run(map[bool]string{true: "different port", false: "same origin"}[external], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "Bearer channel-secret", request.Header.Get("Authorization"))
				assert.Equal(t, "channel-secret", request.Header.Get("X-API-Key"))
				if request.URL.Path == "/tasks/job" {
					location := "/result"
					if external {
						location = target.URL + "/result"
					}
					http.Redirect(writer, request, location, http.StatusTemporaryRedirect)
					return
				}
				assert.Equal(t, "/result", request.URL.Path)
				_, _ = io.WriteString(writer, `{"job":{"status":"queued"}}`)
			}))
			defer upstream.Close()
			profile := upstreamAsyncTestProfile(t, "video")
			profile.Poll.Request.Headers["X-API-Key"] = "{api_key}"
			response, err := PollUpstreamAsync(t.Context(), upstream.URL, "", profile, UpstreamAsyncTemplateContext{TaskID: "job", APIKey: "channel-secret"})
			require.NoError(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			if external {
				assert.Equal(t, http.StatusTemporaryRedirect, response.StatusCode)
				assert.Empty(t, targetRequests, "poll redirects must not send channel credentials to another origin")
			} else {
				assert.Equal(t, http.StatusOK, response.StatusCode)
			}
		})
	}
}

func TestDynamicUpstreamAsyncVideoPollingUsesFrozenProfile(t *testing.T) {
	truncate(t)
	seedUser(t, 701, 1000)
	profile := upstreamAsyncTestProfile(t, "video")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		assert.Equal(t, "/tasks/upstream-frozen", request.URL.Path)
		assert.Equal(t, "mapped", request.URL.Query().Get("model"))
		assert.Equal(t, "Bearer frozen-key", request.Header.Get("Authorization"))
		_, err := io.WriteString(writer, `{"job":{"status":"done","output":"https://cdn.example/result.mp4"},"usage":{"input":2,"output":3,"total":5}}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	channel := &model.Channel{Id: 701, Type: constant.ChannelTypeKling, Name: "dynamic", Key: "live-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(channel).Error)
	task := &model.Task{
		TaskID:    "task_dynamic_video",
		UserId:    701,
		ChannelId: channel.Id,
		Platform:  constant.TaskPlatform("kling"),
		Status:    model.TaskStatusInProgress,
		Properties: model.Properties{
			OriginModelName:   "client-model",
			UpstreamModelName: "mapped",
		},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-frozen", Key: "frozen-key", UpstreamAsync: profile},
	}
	require.NoError(t, model.DB.Create(task).Error)

	adaptor := &scriptedPollingAdaptor{}
	require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, "https://cdn.example/result.mp4", persisted.PrivateData.ResultURL)
	assert.Empty(t, persisted.Data)
	assert.JSONEq(t, `{"job":{"status":"done","output":"https://cdn.example/result.mp4"},"usage":{"input":2,"output":3,"total":5}}`, string(persisted.PrivateData.UpstreamAsyncResponse))
	require.NotNil(t, persisted.PrivateData.UpstreamAsync)
	assert.Equal(t, "dynamic", persisted.PrivateData.UpstreamAsync.ID)
}

func TestDynamicUpstreamAsyncVideoUsageSettlement(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		durationField  string
		retry          bool
		failSettlement bool
		costExceeds    bool
		upstreamFailed bool
		missingChannel bool
		pluginSeconds  bool
		timedOut       bool
		wantStatus     model.TaskStatus
		wantQuota      int
		wantReason     string
		wantURL        string
	}{
		{name: "plugin seconds settle without configured duration override", durationField: `,"duration":4`, pluginSeconds: true, wantStatus: model.TaskStatusSuccess, wantQuota: 3_250, wantURL: "https://cdn.example/result.mp4"},
		{name: "upstream seconds override plugin and refund difference", durationField: `,"duration":3.25`, wantStatus: model.TaskStatusSuccess, wantQuota: 3_250, wantURL: "https://cdn.example/result.mp4"},
		{name: "failed settlement transaction can retry", durationField: `,"duration":3.25`, failSettlement: true, wantStatus: model.TaskStatusSuccess, wantQuota: 3_250, wantURL: "https://cdn.example/result.mp4"},
		{name: "duration above reserved limit fails and refunds", durationField: `,"duration":6`, wantStatus: model.TaskStatusFailure, wantQuota: 0, wantReason: "exceeded reserved billing limit"},
		{name: "completion cost above reservation fails and refunds", durationField: `,"duration":3.25`, costExceeds: true, wantStatus: model.TaskStatusFailure, wantQuota: 0, wantReason: "cost exceeded reserved billing limit"},
		{name: "upstream failure refunds", upstreamFailed: true, wantStatus: model.TaskStatusFailure, wantQuota: 0, wantReason: "vendor failed"},
		{name: "deleted channel refunds", missingChannel: true, wantStatus: model.TaskStatusFailure, wantQuota: 0, wantReason: "Failed to get channel info"},
		{name: "timed out measured task refunds once", timedOut: true, wantStatus: model.TaskStatusFailure, wantQuota: 0, wantReason: "任务超时"},
		{name: "missing duration consumes retry budget", retry: true, wantStatus: model.TaskStatusFailure, wantReason: "actual_seconds_path"},
		{name: "string duration consumes retry budget", durationField: `,"duration":"3.25"`, retry: true, wantStatus: model.TaskStatusFailure, wantReason: "actual_seconds_path"},
		{name: "duration above host maximum consumes retry budget", durationField: `,"duration":3601`, retry: true, wantStatus: model.TaskStatusFailure, wantReason: "actual_seconds_path"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			previousFailures := constant.TaskPollMaxFailures
			constant.TaskPollMaxFailures = 2
			t.Cleanup(func() { constant.TaskPollMaxFailures = previousFailures })
			previousTimeout := constant.TaskTimeoutMinutes
			constant.TaskTimeoutMinutes = 60
			t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })
			const userID, tokenID, channelID = 711, 711, 711
			const userQuota, tokenQuota, reservedQuota = 10_000, 8_000, 5_000
			seedUser(t, userID, userQuota)
			seedToken(t, tokenID, userID, "sk-measured-video", tokenQuota)
			profile := upstreamAsyncTestProfile(t, "video")
			if !testCase.pluginSeconds {
				profile.Poll.Response.ActualSecondsPath = "job.duration"
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				status := "done"
				if testCase.upstreamFailed {
					status = "failed"
				}
				_, err := io.WriteString(writer, `{"job":{"status":"`+status+`","output":"https://cdn.example/result.mp4","error":{"message":"vendor failed"}`+testCase.durationField+`}}`)
				assert.NoError(t, err)
			}))
			defer server.Close()

			channel := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Name: "measured-video", Key: "key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
			require.NoError(t, model.DB.Create(channel).Error)
			seedChargedAccounting(t, userID, channelID, tokenID, reservedQuota, 1)

			expression := `tier("seconds", u("seconds"))`
			estimatedFacts := map[string]any{"seconds": float64(5)}
			completionFacts := map[string]any{"seconds": float64(99)}
			if testCase.pluginSeconds {
				completionFacts["seconds"] = 3.25
			}
			if testCase.costExceeds {
				expression = `tier("seconds", u("seconds") + u("extra"))`
				estimatedFacts["extra"] = float64(0)
				completionFacts["extra"] = float64(5)
			}
			task := makeTask(userID, channelID, reservedQuota, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_measured_video"
			task.Platform = constant.TaskPlatform("kling")
			task.SubmitTime = time.Now().Unix()
			if testCase.timedOut {
				task.SubmitTime -= int64(constant.TaskTimeoutMinutes)*60 + 1
			}
			task.PrivateData.UpstreamTaskID = "upstream-measured"
			task.PrivateData.UpstreamAsync = profile
			task.PrivateData.SubmitAccountingRecorded = true
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
				ExprString:       expression,
				ExprHash:         billingexpr.ExprHashString(expression),
				GroupRatio:       1,
				QuotaPerUnit:     1_000,
				ExprVersion:      1,
				TaskUsageBilling: true,
				UsageFacts:       estimatedFacts,
				EstimatedTier:    "seconds",
			}
			require.NoError(t, model.DB.Create(task).Error)
			if testCase.missingChannel {
				require.NoError(t, model.DB.Delete(channel).Error)
			}
			var staleTask model.Task
			require.NoError(t, model.DB.First(&staleTask, task.ID).Error)

			adaptor := &scriptedCompletionUsageAdaptor{usageFacts: completionFacts}
			const callbackName = "test:fail_measured_video_token_refund"
			callbackRegistered := false
			failureInjected := false
			if testCase.failSettlement {
				require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
					if tx.Statement.Table == "tokens" && !failureInjected {
						failureInjected = true
						tx.AddError(fmt.Errorf("forced measured video token refund failure"))
					}
				}))
				callbackRegistered = true
				t.Cleanup(func() {
					if callbackRegistered {
						_ = model.DB.Callback().Update().Remove(callbackName)
					}
				})
			}
			poll := func(current *model.Task) error {
				if testCase.timedOut {
					sweepTimedOutTasks(t.Context())
					return nil
				}
				if testCase.missingChannel {
					_ = updateVideoTasks(t.Context(), current.Platform, channelID, []string{current.GetUpstreamTaskID()}, map[string]*model.Task{current.GetUpstreamTaskID(): current})
					return nil
				}
				return updateVideoSingleTask(t.Context(), adaptor, channel, current.GetUpstreamTaskID(), map[string]*model.Task{current.GetUpstreamTaskID(): current})
			}
			firstErr := poll(task)
			if testCase.failSettlement {
				require.ErrorContains(t, firstErr, "forced measured video token refund failure")
				require.True(t, failureInjected)
				require.NoError(t, model.DB.Callback().Update().Remove(callbackName))
				callbackRegistered = false
				var pending model.Task
				require.NoError(t, model.DB.First(&pending, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), pending.Status)
				assert.Equal(t, reservedQuota, pending.Quota)
				assert.Equal(t, userQuota, getUserQuota(t, userID))
				assert.Equal(t, tokenQuota, getTokenRemainQuota(t, tokenID))
				assert.Zero(t, countLogs(t))
				require.NoError(t, poll(&pending))
			} else {
				require.NoError(t, firstErr)
			}
			if testCase.retry {
				var pending model.Task
				require.NoError(t, model.DB.First(&pending, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), pending.Status)
				assert.Equal(t, 1, pending.PrivateData.PollFailures)
				assert.Equal(t, reservedQuota, pending.Quota)
				assert.Equal(t, userQuota, getUserQuota(t, userID))
				assert.Zero(t, countLogs(t))
				require.NoError(t, poll(&pending))
			}

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.Equal(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantQuota, persisted.Quota)
			assert.Equal(t, testCase.wantURL, persisted.PrivateData.ResultURL)
			if testCase.wantReason != "" {
				assert.Contains(t, persisted.FailReason, testCase.wantReason)
			}
			if testCase.retry {
				assert.Equal(t, 2, persisted.PrivateData.PollFailures)
			}
			assert.Equal(t, userQuota+reservedQuota-testCase.wantQuota, getUserQuota(t, userID))
			assert.Equal(t, tokenQuota+reservedQuota-testCase.wantQuota, getTokenRemainQuota(t, tokenID))
			usedQuota, _ := getUserUsageAccounting(t, userID)
			assert.Equal(t, testCase.wantQuota, usedQuota)
			assert.Equal(t, int64(1), countLogs(t))
			log := getLastLog(t)
			require.NotNil(t, log)
			assert.Equal(t, model.LogTypeRefund, log.Type)
			assert.Equal(t, reservedQuota-testCase.wantQuota, log.Quota)
			if testCase.wantStatus == model.TaskStatusSuccess {
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				facts, ok := other["usage_facts"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, 3.25, facts["seconds"])
			}

			// A stale terminal poll cannot settle the same reservation twice.
			require.NoError(t, poll(&staleTask))
			assert.Equal(t, userQuota+reservedQuota-testCase.wantQuota, getUserQuota(t, userID))
			assert.Equal(t, int64(1), countLogs(t))
		})
	}
}

func TestDynamicUpstreamAsyncUnknownStatusDoesNotExposeResponseBody(t *testing.T) {
	truncate(t)
	seedUser(t, 702, 1000)
	previous := constant.TaskPollMaxFailures
	constant.TaskPollMaxFailures = 1
	t.Cleanup(func() { constant.TaskPollMaxFailures = previous })

	profile := upstreamAsyncTestProfile(t, "video")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, err := io.WriteString(writer, `{"job":{"status":"vendor-new-status","output":"https://private.example/result.mp4?signature=secret"}}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	channel := &model.Channel{Id: 702, Type: constant.ChannelTypeKling, Name: "dynamic", Key: "key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(channel).Error)
	task := &model.Task{
		TaskID:      "task_dynamic_unknown",
		UserId:      702,
		ChannelId:   channel.Id,
		Platform:    constant.TaskPlatform("kling"),
		Status:      model.TaskStatusInProgress,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-unknown", Key: "key", UpstreamAsync: profile},
	}
	require.NoError(t, model.DB.Create(task).Error)

	adaptor := &scriptedPollingAdaptor{}
	require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Contains(t, persisted.FailReason, "configured upstream status did not match")
	assert.NotContains(t, persisted.FailReason, "private.example")
	assert.NotContains(t, persisted.FailReason, "signature")
}
