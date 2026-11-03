package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInlineVideoReferencesPreserveRequestAndReuseOwnedObjects(t *testing.T) {
	for _, operation := range []string{"generate", "edit", "extend"} {
		t.Run(operation, func(t *testing.T) {
			job, cfg, _ := mediaInputFixture(t)
			cfg.MaxFileBytes = 8 << 20
			require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
			channel := model.Channel{Type: constant.ChannelTypeXai, Key: "unused", Status: common.ChannelStatusEnabled}
			channel.SetSetting(kitdto.ChannelSettings{VideoReferenceTransportMode: "inline"})
			require.NoError(t, model.DB.Create(&channel).Error)
			const source = "https://cdn.example.com/input"
			body := map[string]any{"model": "grok-imagine-video", "prompt": "keep " + source}
			if operation == "generate" {
				body["image"] = map[string]any{"url": source}
				body["last_frame"] = map[string]any{"url": source + "/last"}
				body["reference_images"] = []any{map[string]any{"url": source + "/ref"}, map[string]any{"file_id": "existing-file"}, map[string]any{"url": "data:image/png;base64,aGVsbG8="}}
				body["keyframes"] = []any{map[string]any{"time": 1, "image": map[string]any{"url": source + "/keyframe"}}}
			} else {
				body["video"] = map[string]any{"url": source + "/video.mp4"}
			}
			original, err := common.Marshal(body)
			require.NoError(t, err)
			request := AsyncVideoRequest{Body: append([]byte(nil), original...), OriginalBody: append([]byte(nil), original...), ContentType: "application/json", Channel: channel, Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "xai"}}, ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: job.ExpiresAt}
			inputs := xaiInlineReferenceInputs(body)
			data := append(mediaTestMP4(), bytes.Repeat([]byte{0}, (1<<20)+16)...)
			for _, input := range inputs {
				object := mediaInputSaveFixture(t, job, cfg, input, data)
				require.NoError(t, model.DB.Model(&object).Update("slot", input.StorageSlot()).Error)
			}
			var total int64
			prepared, err := InlineAsyncVideoReferences(t.Context(), job, &request, body, 8<<20, &total)
			require.NoError(t, err)
			assert.Equal(t, int64(len(data)*len(inputs)), total)
			assert.Equal(t, "keep "+source, prepared.(map[string]any)["prompt"])
			assert.Equal(t, original, request.Body)
			assert.Equal(t, original, request.OriginalBody)
			assert.True(t, request.LocalReferencesReady)
			for index, input := range inputs {
				actual, err := videoRetryReferenceValue(prepared.(map[string]any), input.Path, nil)
				require.NoError(t, err)
				assert.Equal(t, "data:"+map[string]string{"image": "image/png", "video": "video/mp4"}[input.Kind]+";base64,"+base64.StdEncoding.EncodeToString(data), actual)
				assert.NotEmpty(t, request.InlineReferenceInputs[index].ObjectId)
			}
			if operation == "generate" {
				references := prepared.(map[string]any)["reference_images"].([]any)
				assert.Equal(t, map[string]any{"file_id": "existing-file"}, references[1])
				assert.Equal(t, map[string]any{"url": "data:image/png;base64,aGVsbG8="}, references[2])
			}
			first := append([]AsyncVideoReferenceInput(nil), request.InlineReferenceInputs...)
			// Reconstruct the small descriptor as a worker would after a restart.
			require.NoError(t, common.Unmarshal(original, &body))
			total = 0
			_, err = InlineAsyncVideoReferences(t.Context(), job, &request, body, 8<<20, &total)
			require.NoError(t, err)
			assert.Equal(t, first, request.InlineReferenceInputs)
			var count int64
			require.NoError(t, model.DB.Model(&model.MediaInputObject{}).Count(&count).Error)
			assert.Equal(t, int64(len(inputs)), count)
			// The next attempt obeys the destination's current setting.
			channel.SetSetting(kitdto.ChannelSettings{})
			require.NoError(t, model.DB.Save(&channel).Error)
			require.NoError(t, common.Unmarshal(original, &body))
			total = 0
			prepared, err = InlineAsyncVideoReferences(t.Context(), job, &request, body, 8<<20, &total)
			require.NoError(t, err)
			assert.Zero(t, total)
			encoded, err := common.Marshal(prepared)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), string(encoded))
			// A configured local retry still mirrors on a passthrough destination;
			// enabling inline on that destination changes only the wire transport.
			request.PendingDecision = &mediaretry.Decision{Action: mediaretry.LocalSameChannel}
			require.NoError(t, common.Unmarshal(original, &body))
			total = 0
			prepared, err = InlineAsyncVideoReferences(t.Context(), job, &request, body, 8<<20, &total)
			require.NoError(t, err)
			assert.Zero(t, total)
			for _, input := range inputs {
				actual, err := videoRetryReferenceValue(prepared.(map[string]any), input.Path, nil)
				require.NoError(t, err)
				assert.Contains(t, actual, "/api/media-inputs/")
			}
		})
	}
}

func TestInlineVideoReferencesRejectUnavailableAndOversizedInputs(t *testing.T) {
	for _, failure := range []string{"size", "format", "checksum", "ownership", "private URL", "changed source", "aggregate"} {
		t.Run(failure, func(t *testing.T) {
			job, cfg, _ := mediaInputFixture(t)
			require.NoError(t, model.DB.AutoMigrate(&model.Channel{}))
			channel := model.Channel{Type: constant.ChannelTypeXai, Key: "unused", Status: common.ChannelStatusEnabled}
			channel.SetSetting(kitdto.ChannelSettings{VideoReferenceTransportMode: "inline"})
			require.NoError(t, model.DB.Create(&channel).Error)
			body := map[string]any{"video": map[string]any{"url": "https://cdn.example.com/clip.mp4"}}
			input := xaiInlineReferenceInputs(body)[0]
			object := mediaInputSaveFixture(t, job, cfg, input, mediaTestMP4())
			require.NoError(t, model.DB.Model(&object).Update("slot", input.StorageSlot()).Error)
			limit, total := int64(1<<20), int64(0)
			switch failure {
			case "size":
				limit = object.ByteSize - 1
			case "aggregate":
				total = limit - object.ByteSize + 1
			case "format":
				require.NoError(t, model.DB.Model(&object).Update("mime_type", "video/webm").Error)
			case "checksum":
				require.NoError(t, os.WriteFile(filepath.Join(object.RootPath, object.ObjectKey), []byte("corrupted"), 0600))
			case "ownership":
				require.NoError(t, model.DB.Model(&object).Update("token_id", job.TokenId+1).Error)
				previousRedis := common.RDB
				common.RDB = nil
				t.Cleanup(func() { common.RDB = previousRedis })
			case "private URL":
				body["video"].(map[string]any)["url"] = "http://169.254.169.254/latest/meta-data"
			case "changed source":
				body["video"].(map[string]any)["url"] = "https://cdn.example.com/other.mp4"
			}
			request := AsyncVideoRequest{Channel: channel, Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "xai"}}}
			_, err := InlineAsyncVideoReferences(t.Context(), job, &request, body, limit, &total)
			var preparation *MediaReferencePreparationError
			require.ErrorAs(t, err, &preparation)
			if failure == "size" || failure == "aggregate" {
				assert.Equal(t, "media_reference_too_large", preparation.Code)
			} else if failure == "ownership" {
				assert.Equal(t, "media_reference_download_failed", preparation.Code, "another Token's stored object cannot satisfy this request")
			} else {
				assert.Equal(t, "media_reference_invalid", preparation.Code)
			}
			assert.False(t, request.LocalReferencesReady)
		})
	}
}

func TestRunningXaiVideoRetainsRecoverySnapshotUntilTerminal(t *testing.T) {
	job, _, _ := mediaInputFixture(t)
	request := AsyncVideoRequest{Body: []byte(`{"image":"https://cdn.example.com/ref.png"}`), ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: job.ExpiresAt}
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	job.RequestCipher, err = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	require.NoError(t, err)
	assert.Equal(t, job.RequestCipher, retainAsyncVideoRequestDetails(job))
	job.Status = "succeeded"
	retained := retainAsyncVideoRequestDetails(job)
	assert.NotEqual(t, job.RequestCipher, retained)
	if len(retained) > 0 {
		plain, err := DecryptImagePayload(retained, "media-request:"+job.TaskId)
		require.NoError(t, err)
		var restored AsyncVideoRequest
		require.NoError(t, common.Unmarshal(plain, &restored))
		assert.Empty(t, restored.Body)
	}
}

func TestInlineVideoReferencesClearTransportOnProviderSwitch(t *testing.T) {
	request := AsyncVideoRequest{VideoReferenceTransportMode: "inline", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "other"}}}
	body := map[string]any{"image": map[string]any{"url": "https://cdn.example.com/ref.png"}}
	var total int64
	prepared, err := InlineAsyncVideoReferences(t.Context(), model.AsyncMediaJob{}, &request, body, 1<<20, &total)
	require.NoError(t, err)
	assert.Equal(t, body, prepared)
	assert.Empty(t, request.VideoReferenceTransportMode, "another provider must not inherit xAI's inline error classification")
	assert.Zero(t, total)
}

func TestInlineVideoReferenceDownloadPreservesSecurityFailures(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "private DNS", true: "private redirect"}[redirect], func(t *testing.T) {
			job, cfg, imageCfg := mediaInputFixture(t)
			dials := 0
			client := mediaInputClient(cfg, imageCfg, imageResolverFixture{addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}, func(context.Context, string, string) (net.Conn, error) {
				dials++
				return nil, nil
			})
			defer client.CloseIdleConnections()
			if redirect {
				client.Transport = mediaInputRoundTripper(func(request *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://127.0.0.1/private"}}, Body: http.NoBody, Request: request}, nil
				})
			}
			var remaining atomic.Int64
			remaining.Store(cfg.MaxFileBytes)
			_, err := downloadMediaInputAttempt(t.Context(), job, AsyncVideoReferenceInput{Kind: "image", Source: "https://cdn.example.com/ref.png"}, cfg, imageCfg, job.ExpiresAt, &remaining, client)
			require.ErrorIs(t, err, ErrMediaInputUnavailable, "SSRF checks must retain the invalid-reference classification")
			assert.Zero(t, dials)
		})
	}
}

func TestInlineVideoLocalRecoveryReachesSubmission(t *testing.T) {
	job, cfg, _ := mediaInputFixture(t)
	const modelName = "grok-imagine-video"
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel).Error)
	channel.Type, channel.Models = constant.ChannelTypeXai, modelName
	require.NoError(t, model.DB.Save(&channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: modelName, Enabled: true}).Error)
	now := time.Now().Unix()
	fact := mediaretry.Fact{MediaType: "video", Provider: "xai", Model: modelName, ChannelID: channel.Id, Stage: "terminal", Category: "reference_download", Message: "reference download failed", Accepted: true, Terminal: true, HasReferences: true, LocalSupported: true, Deadline: now + 600}
	policy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "local", Name: "Local recovery", Enabled: true, Match: mediaretry.Match{ContainsAny: []string{"download failed"}}, Steps: []mediaretry.Step{{ID: "retry", Action: mediaretry.LocalSameChannel, MaxAttempts: 1}}}}}
	_, err := model.SaveMediaRetryPolicy(t.Context(), policy, 0, job.UserId, nil)
	require.NoError(t, err)
	task := model.Task{TaskID: job.TaskId, Platform: "xai", UserId: job.UserId, ChannelId: channel.Id, Status: model.TaskStatusQueued, Quota: 100, AttemptGeneration: job.AttemptGeneration, Properties: model.Properties{OriginModelName: modelName}, PrivateData: model.TaskPrivateData{DurableBilling: true, SubmitAccountingRecorded: true, MediaRetryFact: &fact}}
	task.PrivateData.TokenId = job.TokenId
	require.NoError(t, model.DB.Create(&task).Error)
	request := AsyncVideoRequest{Body: []byte(`{"model":"grok-imagine-video","image":{"url":"https://cdn.example.com/ref.png"}}`), ContentType: "application/json", Channel: channel, Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "xai"}}, ReferenceRecoveryVersion: 2, ReferenceRecoveryDeadline: now + 600, RetryFact: &fact}
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	job.RequestCipher, err = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	require.NoError(t, err)
	job.ChannelId, job.Provider, job.Model, job.Group = channel.Id, "xai", modelName, "default"
	job.Status, job.BillingStatus, job.Quota = "queued", "reserved", 100
	job.ReferenceRecoveryStage, job.ReferenceRetryCount = "submit", 1
	job.LeaseToken, job.LeaseExpiresAt = "recovery-test", now+120
	require.NoError(t, model.DB.Save(&job).Error)
	previousSubmit := SubmitAsyncVideoReferenceRetryFunc
	t.Cleanup(func() { SubmitAsyncVideoReferenceRetryFunc = previousSubmit })
	submissions := 0
	SubmitAsyncVideoReferenceRetryFunc = func(ctx context.Context, current model.AsyncMediaJob, _ model.Task, restored AsyncVideoRequest) error {
		submissions++
		assert.False(t, restored.LocalReferencesReady, "the Go sender prepares xAI references after channel selection")
		return model.UpdateAsyncMediaJob(ctx, current, map[string]any{"reference_recovery_stage": "waiting"})
	}
	require.NoError(t, ProcessAsyncVideoReferenceRecovery(t.Context(), job, cfg))
	assert.Equal(t, 1, submissions, "a deferred xAI preparation must not cycle back to the prepare worker")
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	assert.Equal(t, "waiting", job.ReferenceRecoveryStage)
}
