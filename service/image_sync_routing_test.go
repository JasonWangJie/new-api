package service

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSynchronousImagePoolRouting(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		code int
	}{
		{"pool_excludes_ordinary_channel", 1, 0},
		{"pool_zero_weight_is_not_selected", 1, 0},
		{"affinity_inside_pool_is_preserved", 2, 0},
		{"affinity_outside_pool_is_reselected", 1, 0},
		{"resolution_selects_matching_binding", 2, 0},
		{"multipart_edit_preserves_body", 2, 0},
		{"resolution_mapping_applies_without_async", 1, 0},
		{"token_platform_group_is_used_for_billing", 3, 0},
		{"unavailable_platform_group_rejects", 0, http.StatusForbidden},
		{"disabled_policy_rejects", 0, http.StatusForbidden},
		{"explicit_empty_pool_rejects", 0, http.StatusServiceUnavailable},
		{"disabled_pool_channel_rejects", 0, http.StatusServiceUnavailable},
		{"pin_outside_pool_rejects", 0, http.StatusServiceUnavailable},
		{"builtin_async_channel_rejects", 0, http.StatusServiceUnavailable},
		{"unsupported_resolution_rejects", 0, http.StatusBadRequest},
		{"no_policy_preserves_ordinary_selection", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			require.NoError(t, db.AutoMigrate(&model.ImageGroupPolicy{}, &model.ImageChannelPool{}, &model.TokenImagePlatformMapping{}))
			originalCapability := ImageChannelCapabilityFunc
			ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
				return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Generate: true, Edit: true}
			}
			t.Cleanup(func() { ImageChannelCapabilityFunc = originalCapability })
			const modelName = "gpt-image-2"
			createChannelSelectAutoGroupsChannel(t, db, 1, "default", modelName)
			createChannelSelectAutoGroupsChannel(t, db, 2, "default", modelName)
			policy := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "openai"), Group: "default", Platform: "openai", Enabled: true, PoolMode: "resolution", Models: `[{"id":"gpt-image-2","resolutions":["1K","2K"]}]`}
			if tc.name != "no_policy_preserves_ordinary_selection" {
				require.NoError(t, db.Create(&policy).Error)
			}
			pool := model.ImageChannelPool{BindingKey: ImagePoolBindingKey(policy, modelName, "1K"), Group: "default", Platform: "openai", Mode: "resolution", Resolution: "1K", ChannelId: 1, Priority: 10}
			require.NoError(t, db.Create(&pool).Error)
			body := `{"model":"gpt-image-2","prompt":"draw","size":"1024x1024","n":1}`
			path, contentType := "/v1/images/generations", "application/json"
			switch tc.name {
			case "pool_zero_weight_is_not_selected", "affinity_inside_pool_is_preserved":
				pool.Id, pool.ChannelId, pool.Priority = 0, 2, 0
				require.NoError(t, db.Create(&pool).Error)
			case "resolution_selects_matching_binding":
				pool.Id, pool.ChannelId, pool.Resolution = 0, 2, "2K"
				pool.BindingKey = ImagePoolBindingKey(policy, modelName, "2K")
				require.NoError(t, db.Create(&pool).Error)
				body = `{"model":"gpt-image-2","prompt":"draw","size":"2048x2048","n":1}`
			case "multipart_edit_preserves_body":
				pool.Id, pool.ChannelId, pool.Resolution = 0, 2, "2K"
				pool.BindingKey = ImagePoolBindingKey(policy, modelName, "2K")
				require.NoError(t, db.Create(&pool).Error)
				var encoded strings.Builder
				writer := multipart.NewWriter(&encoded)
				require.NoError(t, writer.WriteField("model", modelName))
				require.NoError(t, writer.WriteField("size", "2048x2048"))
				require.NoError(t, writer.WriteField("prompt", "edit"))
				part, err := writer.CreateFormFile("image", "reference.png")
				require.NoError(t, err)
				_, err = part.Write([]byte("reference-bytes"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				body, path, contentType = encoded.String(), "/v1/images/edits", writer.FormDataContentType()
			case "resolution_mapping_applies_without_async":
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 1).Update("setting", `{"image_resolution_model_mapping":{"gpt-image-2":{"1K":"image-upstream-1k"}}}`).Error)
			case "token_platform_group_is_used_for_billing", "unavailable_platform_group_rejects":
				group := "vip"
				if tc.name == "unavailable_platform_group_rejects" {
					group = "forbidden"
				}
				createChannelSelectAutoGroupsChannel(t, db, 3, group, modelName)
				policy.Id, policy.Group, policy.PolicyKey = 0, group, ImageIdentityHash(group, "openai")
				require.NoError(t, db.Create(&policy).Error)
				pool.Id, pool.Group, pool.ChannelId, pool.BindingKey = 0, group, 3, ImagePoolBindingKey(policy, modelName, "1K")
				require.NoError(t, db.Create(&pool).Error)
				require.NoError(t, db.Create(&model.TokenImagePlatformMapping{TokenId: 11, Platform: "openai", Group: group}).Error)
			case "disabled_policy_rejects":
				require.NoError(t, db.Model(&policy).Update("enabled", false).Error)
			case "explicit_empty_pool_rejects":
				require.NoError(t, db.Model(&pool).Update("channel_id", 0).Error)
			case "disabled_pool_channel_rejects":
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 1).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "builtin_async_channel_rejects":
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"type": constant.ChannelTypeNewAPI, "setting": `{"tianyue_media_compatibility":true}`}).Error)
			case "unsupported_resolution_rejects":
				body = `{"model":"gpt-image-2","prompt":"draw","size":"4096x4096","n":1}`
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", contentType)
			c.Set("id", 1)
			c.Set("token_id", 11)
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			GetChannelConstraints(c).AddFilter(hostdto.ChannelFilter{Kind: hostdto.FilterRequestPath, RequestPath: path})
			if strings.HasPrefix(tc.name, "affinity_") {
				c.Set(ginKeyChannelAffinityLogInfo, map[string]any{"channel_id": 2})
			}
			if tc.name == "pin_outside_pool_rejects" {
				GetChannelConstraints(c).AddPin(hostdto.ChannelPin{ChannelId: 2, Source: hostdto.PinSourceToken, Rank: hostdto.PinRankToken})
			}
			selected, apiErr := SelectSyncImageChannel(c, modelName, &model.Channel{Id: 2})
			if tc.code != 0 {
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.code, apiErr.StatusCode)
				assert.Nil(t, selected)
				return
			}
			require.Nil(t, apiErr)
			require.NotNil(t, selected)
			assert.Equal(t, tc.want, selected.Id)
			restored, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			assert.Equal(t, body, string(restored), "pool routing must leave the original synchronous request intact")
			assert.Equal(t, contentType, c.Request.Header.Get("Content-Type"))
			if tc.name == "token_platform_group_is_used_for_billing" {
				assert.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
				assert.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
			}
			if tc.name == "resolution_mapping_applies_without_async" {
				upstream, err := ResolveImageChannelUpstreamModel(*selected, modelName)
				require.NoError(t, err)
				assert.Equal(t, "image-upstream-1k", upstream)
			}
			if tc.name != "no_policy_preserves_ordinary_selection" {
				again, apiErr := SelectSyncImageChannel(c, modelName, selected)
				require.Nil(t, apiErr)
				assert.Same(t, selected, again, "the distributor's final check must preserve its first pool selection")
			}
		})
	}
}

func TestImageExecutionCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sync, async bool
		endpoint    string
	}{
		{"both_modes", true, true, "image-generation"},
		{"policy_background_off", true, false, "image-generation"},
		{"runtime_background_off", true, false, "image-generation"},
		{"invalid_background_runtime", true, false, "image-generation"},
		{"no_policy_uses_ordinary_sync", true, false, "image-generation"},
		{"disabled_policy", false, false, ""},
		{"explicit_empty_pool", false, false, ""},
		{"disabled_channels", false, false, ""},
		{"builtin_async_only", false, true, ""},
		{"native_gemini", true, false, "gemini"},
		{"unauthorized_mapping", false, false, ""},
		{"mixed_empty_pool_prevents_provider_fallback", true, false, "image-generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, task, request, cfg, _ := asyncImageWorkerFixture(t)
			ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
				return dto.ImageCapability{Provider: "openai", Protocol: "openai_images", Models: []string{request.Model}, Generate: true, Edit: true}
			}
			policy := model.ImageGroupPolicy{}
			require.NoError(t, db.Where("policy_key = ?", ImageIdentityHash("default", "openai")).Take(&policy).Error)
			switch tc.name {
			case "policy_background_off":
				require.NoError(t, db.Model(&policy).Update("async_enabled", false).Error)
			case "runtime_background_off":
				cfg.AsyncEnabled = false
				require.NoError(t, SaveImageRuntimeConfig(t.Context(), cfg))
			case "invalid_background_runtime":
				require.NoError(t, db.Model(&model.Option{}).Where(map[string]any{"key": "image_runtime_config"}).Update("value", "invalid").Error)
			case "no_policy_uses_ordinary_sync":
				require.NoError(t, db.Delete(&policy).Error)
			case "disabled_policy":
				require.NoError(t, db.Model(&policy).Update("enabled", false).Error)
			case "explicit_empty_pool":
				require.NoError(t, db.Model(&policy).Update("pool_mode", "model").Error)
				policy.PoolMode = "model"
				require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: ImagePoolBindingKey(policy, request.Model, ""), Group: policy.Group, Platform: policy.Platform, Mode: "model", Model: request.Model, ChannelId: 0}).Error)
			case "disabled_channels":
				require.NoError(t, db.Model(&model.Channel{}).Where("id > ?", 0).Update("status", common.ChannelStatusManuallyDisabled).Error)
			case "builtin_async_only":
				require.NoError(t, db.Model(&model.Channel{}).Where("id > ?", 0).Updates(map[string]any{"type": constant.ChannelTypeNewAPI, "setting": `{"tianyue_media_compatibility":true}`}).Error)
			case "native_gemini":
				ImageChannelCapabilityFunc = func(model.Channel, string) dto.ImageCapability {
					return dto.ImageCapability{Provider: "gemini", Protocol: "gemini_native", Models: []string{request.Model}, Generate: true}
				}
			case "unauthorized_mapping":
				policy.Id, policy.Group, policy.PolicyKey = 0, "not-authorized", ImageIdentityHash("not-authorized", "openai")
				require.NoError(t, db.Create(&policy).Error)
				require.NoError(t, db.Create(&model.TokenImagePlatformMapping{TokenId: task.TokenId, Platform: "openai", Group: policy.Group}).Error)
			case "mixed_empty_pool_prevents_provider_fallback":
				policy.Id, policy.Platform, policy.PolicyKey, policy.PoolMode = 0, "mixed", ImageIdentityHash("default", "mixed"), "model"
				require.NoError(t, db.Create(&policy).Error)
				require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: ImagePoolBindingKey(policy, request.Model, ""), Group: policy.Group, Platform: policy.Platform, Mode: "model", Model: request.Model, ChannelId: 0}).Error)
			}
			root := filepath.Join(t.TempDir(), "must-not-create")
			require.NoError(t, db.Model(&model.ImageStorageProfile{}).Where("class = ?", "temporary").Update("root", root).Error)
			const callback = "test:image-execution-readonly"
			failWrite := func(tx *gorm.DB) { tx.AddError(errors.New("model capability discovery must be read only")) }
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, failWrite))
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, failWrite))
			require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callback, failWrite))
			t.Cleanup(func() {
				assert.NoError(t, db.Callback().Create().Remove(callback))
				assert.NoError(t, db.Callback().Update().Remove(callback))
				assert.NoError(t, db.Callback().Delete().Remove(callback))
			})
			keys, err := common.RDB.Keys(t.Context(), "*").Result()
			require.NoError(t, err)
			capabilities, err := ImageExecutionCapabilities(t.Context(), task.TokenId, "default", []string{"default"}, []string{request.Model, "gpt-ordinary-text"})
			require.NoError(t, err)
			assert.Equal(t, ImageExecutionCapability{Sync: tc.sync, Async: tc.async, SyncEndpoint: tc.endpoint}, capabilities[request.Model])
			assert.NotContains(t, capabilities, "gpt-ordinary-text")
			_, err = os.Stat(root)
			assert.True(t, os.IsNotExist(err), "model discovery must not create storage directories")
			after, err := common.RDB.Keys(t.Context(), "*").Result()
			require.NoError(t, err)
			assert.ElementsMatch(t, keys, after)
		})
	}
}

func TestSynchronousImagePreviewMatchesPoolAndBackgroundSwitch(t *testing.T) {
	db, task, request, _, channel := asyncImageWorkerFixture(t)
	var policy model.ImageGroupPolicy
	require.NoError(t, db.Where("policy_key = ?", ImageIdentityHash("default", "openai")).Take(&policy).Error)
	require.NoError(t, db.Model(&policy).Updates(map[string]any{"async_enabled": false, "pool_mode": "model"}).Error)
	policy.PoolMode = "model"
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: ImagePoolBindingKey(policy, request.Model, "1K"), Group: policy.Group, Platform: policy.Platform, Mode: "model", Model: request.Model, ChannelId: channel.Id, Priority: 7}).Error)
	input := MediaRoutingPreviewRequest{ExecutionMode: "sync", Group: "default", Platform: "openai", MediaType: "image", Model: request.Model, Resolution: "1K", Operation: "text_to_image", TokenID: task.TokenId}
	preview, err := PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, preview.Eligible, 1)
	assert.Equal(t, channel.Id, preview.Eligible[0].ChannelID)
	assert.Equal(t, 7, preview.Eligible[0].Weight)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Updates(map[string]any{"group": "auto", "auto_groups": `["default"]`}).Error)
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, preview.Eligible, 1)
	assert.Equal(t, []string{"default"}, preview.RoutingGroups)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", task.TokenId).Update("group", "default").Error)
	input.ExecutionMode = "async"
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	assert.Empty(t, preview.Eligible)
	assert.Contains(t, preview.Reasons, "async_disabled")
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{"type": constant.ChannelTypeNewAPI, "setting": `{"tianyue_media_compatibility":true}`}).Error)
	input.ExecutionMode = "sync"
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	assert.Empty(t, preview.Eligible)
	for _, candidate := range preview.Excluded {
		if candidate.ChannelID == channel.Id {
			assert.Contains(t, candidate.Reasons, "synchronous_protocol_not_supported")
		}
	}
}
