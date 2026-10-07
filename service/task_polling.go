package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/samber/lo"
)

// TaskPollingAdaptor 定义轮询所需的最小适配器接口，避免 service -> relay 的循环依赖
type TaskPollingAdaptor interface {
	Init(info *relaycommon.RelayInfo)
	FetchTask(baseURL string, key string, task *model.Task, proxy string) (*http.Response, error)
	ParseTaskResult(task *model.Task, resp *http.Response, body []byte) (*relaycommon.TaskInfo, error)
	// AdjustBillingOnComplete 在任务到达终态（成功/失败）时由轮询循环调用。
	// 返回正数触发差额结算（补扣/退还），返回 0 保持预扣费金额不变。
	AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int
}

type TaskContextPollingAdaptor interface {
	FetchTaskContext(context.Context, string, string, *model.Task, string) (*http.Response, error)
}

type TaskCompletionUsageAdaptor interface {
	ApplyCompletionUsage(*model.Task, *http.Response, []byte, *relaycommon.TaskInfo) error
}

type BatchTaskPollingAdaptor interface {
	TaskPollingAdaptor
	FetchMode() string
	FetchBatchTasks(baseURL, key string, tasks []*model.Task, proxy string) (*http.Response, error)
	ParseBatchResult(tasks []*model.Task, resp *http.Response, body []byte) (map[string]*BatchTaskResult, error)
}

const (
	pollClassOK           = "ok"
	pollClassOtherClient  = "other_client"
	pollClassNotFound     = "not_found"
	pollClassAuth         = "auth"
	pollClassTransient    = "transient"
	pollClassUnrecognized = "unrecognized"
	pollClassHookError    = "hook_error"
	pollClassTransport    = "transport_error"
)

type BatchTaskResult struct {
	TaskInfo   relaycommon.TaskInfo
	Action     string
	SubmitTime int64
	StartTime  int64
	FinishTime int64
	Data       any
}

// GetTaskAdaptorFunc 由 main 包注入，用于获取指定平台的任务适配器。
// 打破 service -> relay -> relay/channel -> service 的循环依赖。
var GetTaskAdaptorFunc func(platform constant.TaskPlatform) TaskPollingAdaptor

var taskPollingCursor, taskTimeoutCursor atomic.Int64

// nextUnfinishedTaskPage rotates bounded pages so tasks held for manual
// reconciliation cannot permanently hide later tasks from polling or timeout.
func nextUnfinishedTaskPage(ctx context.Context, cursor *atomic.Int64, cutoff int64, limit int) ([]*model.Task, error) {
	limit = max(limit, 1)
	afterID := cursor.Load()
	tasks, err := model.GetUnfinishedSyncTaskPage(ctx, afterID, cutoff, limit)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 && afterID != 0 {
		tasks, err = model.GetUnfinishedSyncTaskPage(ctx, 0, cutoff, limit)
		if err != nil {
			return nil, err
		}
	}
	var nextID int64
	if len(tasks) >= limit {
		nextID = tasks[len(tasks)-1].ID
	}
	// An overlapping pass may already have advanced the cursor. Its progress
	// must not be overwritten by this pass's older page.
	cursor.CompareAndSwap(afterID, nextID)
	return tasks, nil
}

// sweepTimedOutTasks 在主轮询之前独立清理超时任务。
// 每次最多处理 100 条，剩余的下个周期继续处理。
// 使用 per-task CAS (UpdateWithStatus) 防止覆盖被正常轮询已推进的任务。
func sweepTimedOutTasks(ctx context.Context) {
	if constant.TaskTimeoutMinutes <= 0 {
		return
	}
	cutoff := time.Now().Unix() - int64(constant.TaskTimeoutMinutes)*60
	tasks, err := nextUnfinishedTaskPage(ctx, &taskTimeoutCursor, cutoff, 100)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("sweepTimedOutTasks query error: %v", err))
		return
	}
	if len(tasks) == 0 {
		return
	}

	reason := fmt.Sprintf("任务超时（%d分钟）", constant.TaskTimeoutMinutes)
	legacyReason := "任务超时（旧系统遗留任务，不进行退款，请联系管理员）"
	now := time.Now().Unix()
	timedOutCount := 0

	for _, task := range tasks {
		if task.PrivateData.Key == "" {
			if channel, err := model.CacheGetChannel(task.ChannelId); err == nil {
				if _, credentialErr := taskPollingCredential(task, channel); credentialErr != nil {
					logger.LogWarn(ctx, credentialErr.Error())
					continue
				}
			}
		}
		isLegacy := task.SubmitTime > 0 && task.SubmitTime < model.TaskRefundLegacyCutoff
		if !isLegacy {
			if err := ensureMeasuredTaskSubmitAccounting(ctx, task); err != nil {
				logger.LogError(ctx, fmt.Sprintf("sweepTimedOutTasks submit accounting error for task %s: %v", task.TaskID, err))
				continue
			}
		}

		oldStatus := task.Status
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.FinishTime = now
		if isLegacy {
			task.FailReason = legacyReason
			// 旧系统任务明确不退款，随终态 CAS 一并清掉 quota，
			// 避免留下可再次退款的计费状态。
			task.Quota = 0
		} else {
			task.FailReason = reason
		}
		if !isLegacy {
			won, err := SettleTaskBillingOnComplete(ctx, nil, task, oldStatus, relaycommon.FailTaskInfo(reason))
			if err != nil {
				logger.LogError(ctx, fmt.Sprintf("sweepTimedOutTasks settlement error for task %s: %v", task.TaskID, err))
				continue
			}
			if won {
				timedOutCount++
			}
			continue
		}

		won, err := task.UpdateWithStatus(oldStatus)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("sweepTimedOutTasks CAS update error for task %s: %v", task.TaskID, err))
			continue
		}
		if !won {
			logger.LogInfo(ctx, fmt.Sprintf("sweepTimedOutTasks: task %s already transitioned, skip", task.TaskID))
			continue
		}
		timedOutCount++
		if !isLegacy && task.Quota != 0 {
			RefundTaskQuota(ctx, task, reason)
		}
	}

	if timedOutCount > 0 {
		logger.LogInfo(ctx, fmt.Sprintf("sweepTimedOutTasks: timed out %d tasks", timedOutCount))
	}
}

// TaskPollSummary is the result recorded on an async_task_poll system task row,
// summarizing one polling pass.
type TaskPollSummary struct {
	UnfinishedTasks  int `json:"unfinished_tasks"`
	PlatformsScanned int `json:"platforms_scanned"`
	NullTasksFailed  int `json:"null_tasks_failed"`
}

// RunTaskPollingOnce performs one async-task (Suno/video) polling pass
// synchronously. It honors ctx cancellation (the system-task runner cancels it
// when the lease is lost) and, when report is non-nil, reports progress as
// (processedPlatforms, totalPlatforms). It returns immediately if the task
// adaptor factory has not been wired yet, to avoid a nil call during startup.
func RunTaskPollingOnce(ctx context.Context, report func(processed, total int)) TaskPollSummary {
	summary := TaskPollSummary{}
	if GetTaskAdaptorFunc == nil {
		return summary
	}
	if ctx == nil {
		ctx = context.Background()
	}

	common.SysLog("任务进度轮询开始")
	sweepTimedOutTasks(ctx)
	queryLimit := constant.TaskQueryLimit
	if queryLimit <= 0 {
		queryLimit = 1000
	}
	allTasks, err := nextUnfinishedTaskPage(ctx, &taskPollingCursor, 0, queryLimit)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("task polling query error: %v", err))
		return summary
	}
	summary.UnfinishedTasks = len(allTasks)
	platformTask := make(map[constant.TaskPlatform][]*model.Task)
	for _, t := range allTasks {
		platformTask[t.Platform] = append(platformTask[t.Platform], t)
	}

	totalPlatforms := len(platformTask)
	processedPlatforms := 0
	for platform, tasks := range platformTask {
		if ctx.Err() != nil {
			break
		}
		if report != nil {
			report(processedPlatforms, totalPlatforms)
		}
		processedPlatforms++
		if len(tasks) == 0 {
			continue
		}
		summary.PlatformsScanned++
		taskChannelM := make(map[int][]string)
		taskM := make(map[string]*model.Task)
		nullTaskIds := make([]int64, 0)
		for _, task := range tasks {
			upstreamID := task.GetUpstreamTaskID()
			if upstreamID == "" {
				if task.PrivateData.DurableBilling || taskUsesMeasuredVideoBilling(task) {
					if accountingErr := ensureMeasuredTaskSubmitAccounting(ctx, task); accountingErr != nil {
						logger.LogError(ctx, fmt.Sprintf("record task %s submission accounting: %v", task.TaskID, accountingErr))
						continue
					}
					oldStatus := task.Status
					task.Status = model.TaskStatusFailure
					task.Progress = taskcommon.ProgressComplete
					task.FinishTime = time.Now().Unix()
					task.FailReason = "upstream task ID is missing"
					won, settleErr := SettleTaskBillingOnComplete(ctx, nil, task, oldStatus, relaycommon.FailTaskInfo(task.FailReason))
					if settleErr != nil {
						logger.LogError(ctx, fmt.Sprintf("settle task %s with missing upstream ID: %v", task.TaskID, settleErr))
					} else if won {
						summary.NullTasksFailed++
					}
					continue
				}
				// 统计失败的未完成任务
				nullTaskIds = append(nullTaskIds, task.ID)
				continue
			}
			// Provider identifiers are scoped to their account, not globally
			// unique across channels or credentials. Keep persisted rows distinct.
			taskRef := fmt.Sprint(task.ID)
			taskM[taskRef] = task
			taskChannelM[task.ChannelId] = append(taskChannelM[task.ChannelId], taskRef)
		}
		if len(nullTaskIds) > 0 {
			summary.NullTasksFailed += len(nullTaskIds)
			err := model.TaskBulkUpdateByID(nullTaskIds, map[string]any{
				"status":   "FAILURE",
				"progress": "100%",
			})
			if err != nil {
				logger.LogError(ctx, fmt.Sprintf("Fix null task_id task error: %v", err))
			} else {
				logger.LogInfo(ctx, fmt.Sprintf("Fix null task_id task success: %v", nullTaskIds))
			}
		}
		if len(taskChannelM) == 0 {
			continue
		}

		DispatchPlatformUpdate(ctx, platform, taskChannelM, taskM)
	}
	if report != nil && ctx.Err() == nil {
		report(totalPlatforms, totalPlatforms)
	}
	common.SysLog("任务进度轮询完成")
	return summary
}

// DispatchPlatformUpdate 按平台分发轮询更新
func DispatchPlatformUpdate(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) {
	if ctx == nil {
		ctx = context.Background()
	}
	if platform == constant.TaskPlatformMidjourney {
		// MJ 轮询由其自身处理，这里预留入口
		return
	}
	adaptor := GetTaskAdaptorFunc(platform)
	if batchAdaptor, ok := adaptor.(BatchTaskPollingAdaptor); ok && batchAdaptor.FetchMode() == "batch" {
		dynamicChannels := make(map[int][]string)
		dynamicTasks := make(map[string]*model.Task)
		batchChannels := make(map[int][]string)
		batchTasks := make(map[string]*model.Task)
		for channelID, upstreamIDs := range taskChannelM {
			for _, upstreamID := range upstreamIDs {
				task := taskM[upstreamID]
				if task != nil && task.PrivateData.UpstreamAsync != nil {
					dynamicChannels[channelID] = append(dynamicChannels[channelID], upstreamID)
					dynamicTasks[upstreamID] = task
					continue
				}
				batchChannels[channelID] = append(batchChannels[channelID], upstreamID)
				batchTasks[upstreamID] = task
			}
		}
		if len(batchChannels) > 0 {
			if err := UpdateBatchTasks(ctx, batchAdaptor, batchChannels, batchTasks); err != nil {
				common.SysLog(fmt.Sprintf("UpdateBatchTasks fail: %s", err))
			}
		}
		if len(dynamicChannels) > 0 {
			if err := UpdateVideoTasks(ctx, platform, dynamicChannels, dynamicTasks); err != nil {
				common.SysLog(fmt.Sprintf("UpdateVideoTasks fail: %s", err))
			}
		}
		return
	}
	if err := UpdateVideoTasks(ctx, platform, taskChannelM, taskM); err != nil {
		common.SysLog(fmt.Sprintf("UpdateVideoTasks fail: %s", err))
	}
}

func UpdateBatchTasks(ctx context.Context, adaptor BatchTaskPollingAdaptor, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	for channelId, taskIds := range taskChannelM {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := updateBatchTasks(ctx, adaptor, channelId, taskIds, taskM)
		if err != nil {
			logger.LogError(ctx, fmt.Sprintf("渠道 #%d 更新异步任务失败: %s", channelId, err.Error()))
		}
	}
	return nil
}

func updateBatchTasks(ctx context.Context, adaptor BatchTaskPollingAdaptor, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, fmt.Sprintf("渠道 #%d 未完成的任务有: %d", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	ch, err := model.CacheGetChannel(channelId)
	if err != nil {
		common.SysLog(fmt.Sprintf("CacheGetChannel: %v", err))
		// Collect DB primary key IDs for bulk update (taskIds are upstream IDs, not task_id column values)
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				if t.PrivateData.DurableBilling || taskUsesMeasuredVideoBilling(t) {
					if accountingErr := ensureMeasuredTaskSubmitAccounting(ctx, t); accountingErr != nil {
						logger.LogError(ctx, fmt.Sprintf("record task %s submission accounting: %v", t.TaskID, accountingErr))
						continue
					}
					reason := fmt.Sprintf("获取渠道信息失败，请联系管理员，渠道ID：%d", channelId)
					if settleErr := failTaskFromPoll(ctx, adaptor, t, t.Status, reason); settleErr != nil {
						logger.LogError(ctx, fmt.Sprintf("settle task %s after missing channel: %v", t.TaskID, settleErr))
					}
					continue
				}
				failedIDs = append(failedIDs, t.ID)
			}
		}
		if len(failedIDs) > 0 {
			err = model.TaskBulkUpdateByID(failedIDs, map[string]any{
				"fail_reason": fmt.Sprintf("获取渠道信息失败，请联系管理员，渠道ID：%d", channelId),
				"status":      "FAILURE",
				"progress":    "100%",
			})
			if err != nil {
				common.SysLog(fmt.Sprintf("UpdateSunoTask error: %v", err))
			}
		}
		return err
	}
	tasksByKey := make(map[string][]*model.Task)
	for _, upstreamID := range taskIds {
		if task := taskM[upstreamID]; task != nil {
			key, keyErr := taskPollingCredential(task, ch)
			if keyErr != nil {
				logger.LogWarn(ctx, keyErr.Error())
				continue
			}
			if err := ensureMeasuredTaskSubmitAccounting(ctx, task); err != nil {
				return err
			}
			tasksByKey[key] = append(tasksByKey[key], task)
		}
	}
	for key, tasks := range tasksByKey {
		if err := updateBatchTaskGroup(ctx, adaptor, ch, key, tasks); err != nil {
			logger.LogError(ctx, fmt.Sprintf("渠道 #%d 更新异步任务失败: %s", channelId, err.Error()))
		}
	}
	return ctx.Err()
}

// Batch endpoints authenticate once for the whole request, so tasks belonging
// to different credentials must never share a query or response lookup.
func updateBatchTaskGroup(ctx context.Context, adaptor BatchTaskPollingAdaptor, ch *model.Channel, key string, tasks []*model.Task) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	baseURL := ch.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(ch.Type)
	}
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:          ch.Type,
		ChannelBaseUrl:       baseURL,
		ApiKey:               key,
		ChannelSetting:       ch.GetSetting(),
		ChannelOtherSettings: ch.GetOtherSettings(),
	}})
	resp, err := adaptor.FetchBatchTasks(baseURL, key, tasks, ch.GetSetting().Proxy)
	if err != nil {
		common.SysLog(fmt.Sprintf("Get Task Do req error: %v", err))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransport, 0, err.Error())
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		common.SysLog(fmt.Sprintf("Get Suno Task parse body error: %v", err))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransport, resp.StatusCode, err.Error())
	}
	switch classifyPollHTTP(resp.StatusCode) {
	case pollClassNotFound:
		return failTasksFromPoll(ctx, adaptor, tasks, fmt.Sprintf("upstream task not found (HTTP %d)", resp.StatusCode))
	case pollClassAuth:
		logger.LogWarn(ctx, fmt.Sprintf("task poll auth failure channel_id=%d http=%d", ch.Id, resp.StatusCode))
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassAuth, resp.StatusCode, "")
	case pollClassTransient:
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassTransient, resp.StatusCode, "")
	}
	responseItems, err := adaptor.ParseBatchResult(tasks, resp, responseBody)
	if err != nil {
		return recordPollFailureForTasks(ctx, adaptor, tasks, pollClassHookError, resp.StatusCode, err.Error())
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		responseItem := responseItems[task.GetUpstreamTaskID()]
		if responseItem == nil {
			continue
		}
		snap := task.Snapshot()
		httpClass := classifyPollHTTP(resp.StatusCode)
		parsedStatus := model.TaskStatus(responseItem.TaskInfo.Status)
		if parsedStatus == model.TaskStatusUnknown || parsedStatus == "" || !knownPollStatus(parsedStatus) {
			if err := recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode, responseItem.TaskInfo.Reason); err != nil {
				common.SysLog("UpdateSunoTask task error: " + err.Error())
			}
			continue
		}
		if httpClass == pollClassOtherClient && isNonTerminalPollStatus(parsedStatus) {
			if err := recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode, responseItem.TaskInfo.Reason); err != nil {
				common.SysLog("UpdateSunoTask task error: " + err.Error())
			}
			continue
		}
		if isNonTerminalPollStatus(parsedStatus) {
			task.PrivateData.PollFailures = 0
		}
		if len(responseItem.TaskInfo.PluginState) > 0 {
			task.PrivateData.PluginState = responseItem.TaskInfo.PluginState
		}
		task.Status = lo.If(parsedStatus != "", parsedStatus).Else(task.Status)
		task.FailReason = lo.If(responseItem.TaskInfo.Reason != "", responseItem.TaskInfo.Reason).Else(task.FailReason)
		task.SubmitTime = lo.If(responseItem.SubmitTime != 0, responseItem.SubmitTime).Else(task.SubmitTime)
		task.StartTime = lo.If(responseItem.StartTime != 0, responseItem.StartTime).Else(task.StartTime)
		task.FinishTime = lo.If(responseItem.FinishTime != 0, responseItem.FinishTime).Else(task.FinishTime)
		if responseItem.TaskInfo.Progress != "" {
			task.Progress = responseItem.TaskInfo.Progress
		}
		if responseItem.TaskInfo.Reason != "" || task.Status == model.TaskStatusFailure {
			logger.LogInfo(ctx, task.TaskID+" 构建失败，"+task.FailReason)
			task.Status = model.TaskStatusFailure
			task.Progress = "100%"
		}
		if responseItem.TaskInfo.Status == model.TaskStatusSuccess {
			task.Progress = "100%"
		}
		if responseItem.Data != nil {
			task.SetData(responseItem.Data)
		} else if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			logger.LogWarn(ctx, fmt.Sprintf(
				"Batch task %s reached terminal status without data; preserving existing task data",
				task.TaskID,
			))
		}
		if responseItem.TaskInfo.Url != "" {
			task.PrivateData.ResultURL = responseItem.TaskInfo.Url
		}
		if task.Status == model.TaskStatusSuccess {
			captured, captureErr := CaptureAsyncMediaOutput(ctx, task)
			if captureErr != nil {
				logger.LogWarn(ctx, "Async media result spooling failed; storage will retry the completed result")
			}
			if captured {
				task.Data = RedactAsyncMediaData(task.Data)
				task.PrivateData.PluginState = RedactAsyncMediaData(task.PrivateData.PluginState)
				if strings.HasPrefix(task.PrivateData.ResultURL, "data:") {
					task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
				}
			}
		}

		isDone := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
		terminalTransition := isDone && snap.Status != task.Status
		if terminalTransition {
			if _, err := SettleTaskBillingOnComplete(ctx, adaptor, task, snap.Status, &responseItem.TaskInfo); err != nil {
				logger.LogError(ctx, fmt.Sprintf("settle batch task %s: %v", task.TaskID, err))
			}
			continue
		}
		won, updateErr := task.UpdateWithStatus(snap.Status)
		if updateErr != nil {
			common.SysLog("UpdateSunoTask task error: " + updateErr.Error())
			continue
		}
		if !won {
			logger.LogWarn(ctx, fmt.Sprintf("Batch task %s already transitioned by another process, skip billing", task.TaskID))
			continue
		}
	}
	return nil
}

// UpdateVideoTasks 按渠道更新所有视频任务
func UpdateVideoTasks(ctx context.Context, platform constant.TaskPlatform, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	channelIDs := make([]int, 0, len(taskChannelM))
	for channelID := range taskChannelM {
		channelIDs = append(channelIDs, channelID)
	}
	sort.Ints(channelIDs)

	var wg sync.WaitGroup
	for _, channelId := range channelIDs {
		taskIds := taskChannelM[channelId]
		if len(taskIds) == 0 {
			continue
		}
		taskIds = append([]string(nil), taskIds...)

		wg.Add(1)
		gopool.Go(func() {
			defer wg.Done()
			if err := updateVideoTasks(ctx, platform, channelId, taskIds, taskM); err != nil {
				logger.LogError(ctx, fmt.Sprintf("Channel #%d failed to update video async tasks: %s", channelId, err.Error()))
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func updateVideoTasks(ctx context.Context, platform constant.TaskPlatform, channelId int, taskIds []string, taskM map[string]*model.Task) error {
	logger.LogInfo(ctx, fmt.Sprintf("Channel #%d pending video tasks: %d", channelId, len(taskIds)))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(taskIds) == 0 {
		return nil
	}
	cacheGetChannel, err := model.CacheGetChannel(channelId)
	if err != nil {
		// Accepted media tasks retain their channel credentials independently of
		// later channel deletion. Only legacy tasks require a live channel here.
		for _, upstreamID := range taskIds {
			if task := taskM[upstreamID]; task != nil && task.PrivateData.AsyncMedia {
				frozen, freezeErr := FrozenAsyncMediaChannel(ctx, task)
				if freezeErr != nil {
					return freezeErr
				}
				cacheGetChannel, err = frozen, nil
				break
			}
		}
	}
	if err != nil {
		// Collect DB primary key IDs for bulk update (taskIds are upstream IDs, not task_id column values)
		var failedIDs []int64
		for _, upstreamID := range taskIds {
			if t, ok := taskM[upstreamID]; ok {
				if t.PrivateData.DurableBilling || taskUsesMeasuredVideoBilling(t) {
					if accountingErr := ensureMeasuredTaskSubmitAccounting(ctx, t); accountingErr != nil {
						logger.LogError(ctx, fmt.Sprintf("record task %s submission accounting: %v", t.TaskID, accountingErr))
						continue
					}
					reason := fmt.Sprintf("Failed to get channel info, channel ID: %d", channelId)
					if settleErr := failTaskFromPoll(ctx, nil, t, t.Status, reason); settleErr != nil {
						logger.LogError(ctx, fmt.Sprintf("settle task %s after missing channel: %v", t.TaskID, settleErr))
					}
					continue
				}
				failedIDs = append(failedIDs, t.ID)
			}
		}
		if len(failedIDs) > 0 {
			errUpdate := model.TaskBulkUpdateByID(failedIDs, map[string]any{
				"fail_reason": fmt.Sprintf("Failed to get channel info, channel ID: %d", channelId),
				"status":      "FAILURE",
				"progress":    "100%",
			})
			if errUpdate != nil {
				common.SysLog(fmt.Sprintf("UpdateVideoTask error: %v", errUpdate))
			}
		}
		return fmt.Errorf("CacheGetChannel failed: %w", err)
	}
	adaptor := GetTaskAdaptorFunc(platform)
	if adaptor == nil {
		return fmt.Errorf("video adaptor not found")
	}
	info := &relaycommon.RelayInfo{}
	info.ChannelMeta = &relaycommon.ChannelMeta{
		ChannelType:          cacheGetChannel.Type,
		ChannelBaseUrl:       cacheGetChannel.GetBaseURL(),
		ChannelSetting:       cacheGetChannel.GetSetting(),
		ChannelOtherSettings: cacheGetChannel.GetOtherSettings(),
	}
	info.ApiKey = cacheGetChannel.Key
	adaptor.Init(info)
	disablePollingSleep := cacheGetChannel.GetOtherSettings().DisableTaskPollingSleep
	for i, taskId := range taskIds {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if task := taskM[taskId]; task != nil && !task.PrivateData.AsyncMedia {
			if _, err := model.CacheGetChannel(channelId); err != nil {
				continue
			}
		}
		if err := updateVideoSingleTask(ctx, adaptor, cacheGetChannel, taskId, taskM); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to update video task %s: %s", taskId, err.Error()))
		}
		if disablePollingSleep || i == len(taskIds)-1 {
			continue
		}

		// sleep 1 second between tasks for this channel only.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return nil
}

func updateVideoSingleTask(ctx context.Context, adaptor TaskPollingAdaptor, ch *model.Channel, taskId string, taskM map[string]*model.Task) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if task := taskM[taskId]; task != nil {
		frozen, err := FrozenAsyncMediaChannel(ctx, task)
		if err != nil {
			return err
		}
		if frozen != nil {
			ch = frozen
		}
	}
	baseURL := constant.GetChannelBaseURL(ch.Type)
	if ch.GetBaseURL() != "" {
		baseURL = ch.GetBaseURL()
	}
	proxy := ch.GetSetting().Proxy

	task := taskM[taskId]
	if task == nil {
		logger.LogError(ctx, fmt.Sprintf("Task %s not found in taskM", taskId))
		return fmt.Errorf("task %s not found", taskId)
	}
	key, keyErr := taskPollingCredential(task, ch)
	if keyErr != nil {
		return keyErr
	}
	if err := ensureMeasuredTaskSubmitAccounting(ctx, task); err != nil {
		return err
	}
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType:          ch.Type,
		ChannelBaseUrl:       baseURL,
		ApiKey:               key,
		ChannelSetting:       ch.GetSetting(),
		ChannelOtherSettings: ch.GetOtherSettings(),
	}})
	snap := task.Snapshot()
	var resp *http.Response
	var err error
	responseLimit := int64(64 << 20)
	if task.PrivateData.UpstreamAsync != nil {
		pollContext := ctx
		var cancel context.CancelFunc
		if task.PrivateData.AsyncMedia {
			cfg, configErr := GetMediaRuntimeConfig(ctx)
			if configErr != nil {
				return configErr
			}
			pollContext, cancel = context.WithTimeout(ctx, time.Duration(cfg.DownloadTimeout)*time.Second)
			defer cancel()
			responseLimit = min(maxUpstreamAsyncResponseBytes, cfg.MaxFileBytes*4+(1<<20))
		}
		resp, err = PollUpstreamAsync(pollContext, baseURL, proxy, task.PrivateData.UpstreamAsync, UpstreamAsyncTemplateContext{
			TaskID:        task.GetUpstreamTaskID(),
			Model:         task.Properties.OriginModelName,
			UpstreamModel: task.Properties.UpstreamModelName,
			APIKey:        key,
		})
	} else if task.PrivateData.AsyncMedia {
		cfg, configErr := GetMediaRuntimeConfig(ctx)
		if configErr != nil {
			return configErr
		}
		pollContext, cancel := context.WithTimeout(ctx, time.Duration(cfg.DownloadTimeout)*time.Second)
		defer cancel()
		poller, ok := adaptor.(TaskContextPollingAdaptor)
		if !ok {
			return errors.New("video provider does not support bounded polling")
		}
		responseLimit = cfg.MaxFileBytes*4 + (1 << 20)
		resp, err = poller.FetchTaskContext(pollContext, baseURL, key, task, proxy)
	} else {
		resp, err = adaptor.FetchTask(baseURL, key, task, proxy)
	}
	if err != nil {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransport, 0, err.Error())
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransport, resp.StatusCode, err.Error())
	}
	if int64(len(responseBody)) > responseLimit {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, "video result metadata byte limit exceeded")
	}

	if !task.PrivateData.AsyncMedia {
		logger.LogDebug(ctx, "updateVideoSingleTask response: %s", responseBody)
	}

	switch classifyPollHTTP(resp.StatusCode) {
	case pollClassNotFound:
		return failTaskFromPoll(ctx, adaptor, task, snap.Status, fmt.Sprintf("upstream task not found (HTTP %d)", resp.StatusCode))
	case pollClassAuth:
		logger.LogWarn(ctx, fmt.Sprintf("task poll auth failure channel_id=%d task=%s http=%d", ch.Id, task.TaskID, resp.StatusCode))
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassAuth, resp.StatusCode, "")
	case pollClassTransient:
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassTransient, resp.StatusCode, "")
	}

	measuredVideo := taskUsesMeasuredVideoBilling(task)
	var actualSeconds *float64
	taskResult := &relaycommon.TaskInfo{}
	if task.PrivateData.UpstreamAsync != nil {
		parsed, parseErr := ParseUpstreamAsyncPollResponse(task.PrivateData.UpstreamAsync, responseBody)
		if parseErr != nil {
			return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, parseErr.Error())
		}
		taskResult = &relaycommon.TaskInfo{Status: string(parsed.Status), Progress: parsed.Progress, Reason: parsed.Reason}
		if len(parsed.URLs) > 0 {
			taskResult.Url = parsed.URLs[0]
		}
		if parsed.Usage != nil {
			taskResult.TotalTokens = parsed.Usage.TotalTokens
			taskResult.CompletionTokens = parsed.Usage.CompletionTokens
		}
		actualSeconds = parsed.ActualSeconds
		if usageAdaptor, ok := adaptor.(TaskCompletionUsageAdaptor); ok {
			if usageErr := usageAdaptor.ApplyCompletionUsage(task, resp, responseBody, taskResult); usageErr != nil {
				logger.LogWarn(ctx, fmt.Sprintf("task %s completion usage hook failed: %v", task.TaskID, usageErr))
			}
		}
		if actualSeconds != nil {
			if taskResult.UsageFacts == nil {
				taskResult.UsageFacts = make(map[string]any)
			}
			// The channel's frozen response rule is authoritative for measured
			// seconds; the plugin may still contribute other completion facts.
			taskResult.UsageFacts["seconds"] = *actualSeconds
		}
	} else {
		// try parse as New API response format
		var responseItems taskdto.TaskResponse[model.Task]
		if err = common.Unmarshal(responseBody, &responseItems); err == nil && responseItems.IsSuccess() {
			logger.LogDebug(ctx, "updateVideoSingleTask parsed as new api response format: %+v", responseItems)
			t := responseItems.Data
			taskResult.TaskID = t.TaskID
			taskResult.Status = string(t.Status)
			taskResult.Url = t.GetResultURL()
			taskResult.Progress = t.Progress
			taskResult.Reason = t.FailReason
			task.Data = t.Data
		} else if taskResult, err = adaptor.ParseTaskResult(task, resp, responseBody); err != nil {
			return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, err.Error())
		}
	}

	if !task.PrivateData.AsyncMedia {
		logger.LogDebug(ctx, "updateVideoSingleTask taskResult: %+v", taskResult)
	}

	parsedStatus := model.TaskStatus(taskResult.Status)
	measuredActualQuota := 0
	var measuredClamp *common.QuotaClamp
	if measuredVideo && parsedStatus == model.TaskStatusSuccess {
		billingContext := task.PrivateData.BillingContext
		if actualSeconds == nil || billingContext == nil || billingContext.TieredSnapshot == nil {
			return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, "measured video billing facts are unavailable")
		}
		limit, ok := billingContext.TieredSnapshot.UsageFacts["seconds"].(float64)
		if !ok || limit <= 0 {
			return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, "measured video precharge limit is unavailable")
		}
		if *actualSeconds > limit {
			logger.LogWarn(ctx, fmt.Sprintf("task %s measured video seconds %.6f exceed reserved limit %.6f", task.TaskID, *actualSeconds, limit))
			parsedStatus = model.TaskStatusFailure
			taskResult.Status = string(parsedStatus)
			taskResult.Url = ""
			taskResult.Reason = "upstream video duration exceeded reserved billing limit"
		} else {
			result, _, billingErr := EvaluateTaskCompletionUsage(billingContext.TieredSnapshot, taskResult.UsageFacts)
			if billingErr != nil {
				return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassHookError, resp.StatusCode, "measured video expression could not be evaluated")
			}
			measuredActualQuota, measuredClamp = result.ActualQuotaAfterGroup, result.Clamp
			if measuredClamp != nil || measuredActualQuota > task.Quota {
				logger.LogWarn(ctx, fmt.Sprintf("task %s measured video final quota %d exceeds reserved quota %d (clamp=%v)", task.TaskID, measuredActualQuota, task.Quota, measuredClamp))
				parsedStatus = model.TaskStatusFailure
				taskResult.Status = string(parsedStatus)
				taskResult.Url = ""
				taskResult.Reason = "upstream video cost exceeded reserved billing limit"
				measuredActualQuota = 0
			}
		}
	}
	var unrecognizedDetail string
	if task.PrivateData.UpstreamAsync != nil {
		unrecognizedDetail = strings.TrimSpace(taskResult.Reason)
		if unrecognizedDetail == "" {
			unrecognizedDetail = "configured upstream status did not match"
		}
	} else {
		unrecognizedDetail = unrecognizedPollDetail(taskResult.Reason, responseBody)
	}
	if parsedStatus == model.TaskStatusUnknown || parsedStatus == "" || !knownPollStatus(parsedStatus) {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode, unrecognizedDetail)
	}
	if classifyPollHTTP(resp.StatusCode) == pollClassOtherClient && isNonTerminalPollStatus(parsedStatus) {
		return recordPollFailure(ctx, adaptor, task, snap.Status, pollClassUnrecognized, resp.StatusCode, unrecognizedDetail)
	}

	if task.PrivateData.UpstreamAsync != nil {
		task.PrivateData.UpstreamAsyncResponse = append(task.PrivateData.UpstreamAsyncResponse[:0], responseBody...)
		task.Data = nil
	} else {
		task.Data = responseBody
	}
	if len(taskResult.PluginState) > 0 {
		task.PrivateData.PluginState = taskResult.PluginState
	}
	if isNonTerminalPollStatus(parsedStatus) {
		task.PrivateData.PollFailures = 0
	}

	now := time.Now().Unix()

	task.Status = parsedStatus
	switch parsedStatus {
	case model.TaskStatusSubmitted:
		task.Progress = taskcommon.ProgressSubmitted
	case model.TaskStatusQueued:
		task.Progress = taskcommon.ProgressQueued
	case model.TaskStatusInProgress:
		task.Progress = taskcommon.ProgressInProgress
		if task.StartTime == 0 {
			task.StartTime = now
		}
	case model.TaskStatusSuccess:
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		if strings.HasPrefix(taskResult.Url, "data:") {
			// data: URI (e.g. Vertex base64 encoded video) — keep in Data, not in ResultURL
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		} else if taskResult.Url != "" {
			// Direct upstream URL (e.g. Kling, Ali, Doubao, etc.)
			task.PrivateData.ResultURL = taskResult.Url
		} else {
			// No URL from adaptor — construct proxy URL using public task ID
			task.PrivateData.ResultURL = taskcommon.BuildProxyURL(task.TaskID)
		}
	case model.TaskStatusFailure:
		logger.LogJson(ctx, fmt.Sprintf("Task %s failed", taskId), task)
		task.Status = model.TaskStatusFailure
		task.Progress = taskcommon.ProgressComplete
		if task.FinishTime == 0 {
			task.FinishTime = now
		}
		task.FailReason = taskResult.Reason
		logger.LogInfo(ctx, fmt.Sprintf("Task %s failed: %s", task.TaskID, task.FailReason))
		taskResult.Progress = taskcommon.ProgressComplete
	}
	if taskResult.Progress != "" {
		task.Progress = taskResult.Progress
	}
	if task.Status == model.TaskStatusSuccess {
		captured, captureErr := CaptureAsyncMediaOutput(ctx, task)
		if captureErr != nil {
			logger.LogWarn(ctx, "Async media output spooling failed; storage will fetch the completed result again")
		}
		if captured {
			task.Data = RedactAsyncMediaData(task.Data)
			task.PrivateData.PluginState = RedactAsyncMediaData(task.PrivateData.PluginState)
		}
	}
	task.Data = redactVideoResponseBody(task.Data)

	isDone := task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure
	if measuredVideo && isDone {
		won, settleErr := SettleMeasuredVideoTask(ctx, task, snap.Status, measuredActualQuota, taskResult.UsageFacts, measuredClamp)
		if settleErr != nil {
			return settleErr
		}
		if !won {
			logger.LogInfo(ctx, fmt.Sprintf("task %s was already settled by another poller", task.TaskID))
		}
		return nil
	}
	if isDone {
		_, err := SettleTaskBillingOnComplete(ctx, adaptor, task, snap.Status, taskResult)
		return err
	}
	if !snap.Equal(task.Snapshot()) {
		if _, err := task.UpdateWithStatus(snap.Status); err != nil {
			logger.LogError(ctx, fmt.Sprintf("Failed to update task %s: %s", task.TaskID, err.Error()))
		}
	} else {
		// No changes, skip update
		logger.LogDebug(ctx, "No update needed for task %s", task.TaskID)
	}

	return nil
}

func taskPollingCredential(task *model.Task, channel *model.Channel) (string, error) {
	if task.PrivateData.Key != "" {
		return task.PrivateData.Key, nil
	}
	if channel.ChannelInfo.IsMultiKey {
		keys := channel.GetKeys()
		if len(keys) != 1 {
			return "", fmt.Errorf("task %s has no stored upstream account; reconcile its original credential before polling", task.TaskID)
		}
		return keys[0], nil
	}
	return channel.Key, nil
}

func redactVideoResponseBody(body []byte) []byte {
	var m map[string]any
	if err := common.Unmarshal(body, &m); err != nil {
		return body
	}
	resp, _ := m["response"].(map[string]any)
	if resp != nil {
		delete(resp, "bytesBase64Encoded")
		if v, ok := resp["video"].(string); ok {
			resp["video"] = truncateBase64(v)
		}
		if vs, ok := resp["videos"].([]any); ok {
			for i := range vs {
				if vm, ok := vs[i].(map[string]any); ok {
					delete(vm, "bytesBase64Encoded")
				}
			}
		}
	}
	b, err := common.Marshal(m)
	if err != nil {
		return body
	}
	return b
}

func truncateBase64(s string) string {
	const maxKeep = 256
	if len(s) <= maxKeep {
		return s
	}
	return s[:maxKeep] + "..."
}

func classifyPollHTTP(statusCode int) string {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return pollClassOK
	case statusCode == http.StatusNotFound || statusCode == http.StatusGone:
		return pollClassNotFound
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return pollClassAuth
	case statusCode == http.StatusTooManyRequests || statusCode >= 500:
		return pollClassTransient
	case statusCode >= 400 && statusCode < 500:
		return pollClassOtherClient
	default:
		return pollClassTransient
	}
}

func knownPollStatus(status model.TaskStatus) bool {
	switch status {
	case model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress, model.TaskStatusSuccess, model.TaskStatusFailure:
		return true
	default:
		return false
	}
}

func isNonTerminalPollStatus(status model.TaskStatus) bool {
	switch status {
	case model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued, model.TaskStatusInProgress:
		return true
	default:
		return false
	}
}

func pollFailureReason(class string, statusCode int, detail string) string {
	reason := fmt.Sprintf("poll failed: %s", class)
	if statusCode > 0 {
		reason = fmt.Sprintf("poll failed: %s (HTTP %d)", class, statusCode)
	}
	if detail != "" {
		reason = reason + ": " + detail
	}
	return reason
}

// unrecognizedPollDetail pairs the plugin's reason with a bounded copy of the
// upstream body so the WARN line is enough to diagnose a parser gap.
func unrecognizedPollDetail(reason string, body []byte) string {
	const maxBodyChars = 512
	redacted := string(redactVideoResponseBody(body))
	if len(redacted) > maxBodyChars {
		redacted = redacted[:maxBodyChars] + "…"
	}
	if strings.TrimSpace(reason) == "" {
		return "body=" + redacted
	}
	return reason + "; body=" + redacted
}

func recordPollFailure(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, fromStatus model.TaskStatus, class string, statusCode int, detail string) error {
	task.PrivateData.PollFailures++
	if class == pollClassUnrecognized || class == pollClassHookError {
		// The redacted body is intentionally not persisted to Task.Data on these
		// paths, so the WARN line is the only operator-visible copy of what the
		// plugin could not interpret.
		logger.LogWarn(ctx, fmt.Sprintf("task %s poll %s (failures=%d, http=%d): %s", task.TaskID, class, task.PrivateData.PollFailures, statusCode, detail))
	}
	// TASK_POLL_MAX_FAILURES <= 0 disables the consecutive-failure cutoff, matching
	// TASK_TIMEOUT_MINUTES semantics; the 24h sweep remains the only backstop.
	if constant.TaskPollMaxFailures > 0 && task.PrivateData.PollFailures >= constant.TaskPollMaxFailures {
		return failTaskFromPoll(ctx, adaptor, task, fromStatus, pollFailureReason(class, statusCode, detail))
	}
	if _, err := task.UpdateWithStatus(fromStatus); err != nil {
		return err
	}
	return nil
}

func recordPollFailureForTasks(ctx context.Context, adaptor TaskPollingAdaptor, tasks []*model.Task, class string, statusCode int, detail string) error {
	var firstErr error
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if err := recordPollFailure(ctx, adaptor, task, task.Status, class, statusCode, detail); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func failTaskFromPoll(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, fromStatus model.TaskStatus, reason string) error {
	now := time.Now().Unix()
	task.Status = model.TaskStatusFailure
	task.Progress = taskcommon.ProgressComplete
	if task.FinishTime == 0 {
		task.FinishTime = now
	}
	task.FailReason = reason
	taskResult := relaycommon.FailTaskInfo(reason)
	_, err := SettleTaskBillingOnComplete(ctx, adaptor, task, fromStatus, taskResult)
	return err
}

func taskUsesMeasuredVideoBilling(task *model.Task) bool {
	return task != nil && task.PrivateData.UpstreamAsync != nil &&
		task.PrivateData.UpstreamAsync.MediaType == "video" &&
		task.PrivateData.UpstreamAsync.Poll.Response.ActualSecondsPath != ""
}

func ensureMeasuredTaskSubmitAccounting(ctx context.Context, task *model.Task) error {
	if task == nil || (!task.PrivateData.DurableBilling && !taskUsesMeasuredVideoBilling(task)) || task.PrivateData.SubmitAccountingRecorded {
		return nil
	}
	_, err := model.RecordMeasuredTaskSubmitAccounting(ctx, task)
	return err
}

func failTasksFromPoll(ctx context.Context, adaptor TaskPollingAdaptor, tasks []*model.Task, reason string) error {
	var firstErr error
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if err := failTaskFromPoll(ctx, adaptor, task, task.Status, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
