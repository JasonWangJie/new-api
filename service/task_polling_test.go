package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	taskIDs      []string
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
}

func (a *batchPollingAdaptor) FetchMode() string { return "batch" }
func (a *batchPollingAdaptor) FetchBatchTasks(_ string, _ string, tasks []*model.Task, _ string) (*http.Response, error) {
	a.batchCalls++
	a.batchIDs = a.batchIDs[:0]
	for _, task := range tasks {
		a.batchIDs = append(a.batchIDs, task.GetUpstreamTaskID())
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

func (a *taskPollingFetchAdaptor) FetchTask(_ string, _ string, task *model.Task, _ string) (*http.Response, error) {
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
	statusCode int
	body       []byte
	fetchErr   error
	parse      *relaycommon.TaskInfo
	parseErr   error
}

func (a *scriptedPollingAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *scriptedPollingAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
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
func (a *scriptedBatchPollingAdaptor) ParseBatchResult([]*model.Task, *http.Response, []byte) (map[string]*BatchTaskResult, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	return a.results, nil
}

func TestUpdateVideoSingleTaskPollClassification(t *testing.T) {
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
	}{
		{
			name:          "404 fails immediately and refunds",
			statusCode:    http.StatusNotFound,
			wantStatus:    model.TaskStatusFailure,
			wantRefund:    true,
			wantReason:    "upstream task not found (HTTP 404)",
			wantUnchanged: false,
		},
		{
			name:          "401 increments without changing status",
			statusCode:    http.StatusUnauthorized,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "429 reaches threshold and refunds",
			statusCode:    http.StatusTooManyRequests,
			priorFailures: 2,
			maxFailures:   3,
			wantStatus:    model.TaskStatusFailure,
			wantFailures:  3,
			wantRefund:    true,
			wantReason:    "poll failed: transient (HTTP 429)",
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
	task := &model.Task{
		TaskID:    "task_dynamic_video",
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
	task := &model.Task{
		TaskID:      "task_dynamic_unknown",
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
