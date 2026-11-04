package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	gatewaydto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sudashuiFixture(t *testing.T) (*jsplugin.LoadedPlugin, dto.TaskVideoModelConfig) {
	t.Helper()
	source, err := plugins.Source("sudashui")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "sudashui"})
	require.NoError(t, err)
	previous := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = registry
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	images, videos, audios := 2, 1, 0
	config := dto.TaskVideoModelConfig{Duration: dto.TaskVideoDuration{Min: 4, Max: 10, Step: 2}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame"}, References: dto.TaskVideoReferences{Images: &images, Videos: &videos, Audios: &audios}, BillingMode: "per_second"}
	require.NoError(t, config.Validate(relaycommon.MaxTaskDurationSeconds))
	return plugin, config
}

func TestHuahuoDynamicVideoModelBindings(t *testing.T) {
	setupTaskPluginControllerTest(t)
	_, config := sudashuiFixture(t)
	source, err := plugins.Source("huahuo-video")
	require.NoError(t, err)
	_, err = jsplugin.CompilePlugin(source, jsplugin.Options{Key: "huahuo-video"})
	require.ErrorContains(t, err, "models must contain at least one model")
	plugin, err := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "huahuo-video"})
	require.NoError(t, err)
	require.Empty(t, plugin.Meta.Models)

	const publicModel, upstream = "my-huahuo-video", "vendor/future-video"
	mapping := `{"my-huahuo-video":"vendor/future-video"}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "fixture", Models: publicModel, Status: common.ChannelStatusEnabled, ModelMapping: &mapping}
	config.Resolutions = []string{"540p", "720p", "1080p"}
	config.DurationsByResolution = map[string]dto.TaskVideoDuration{
		"1080p": {Values: []int{8, 10}, Default: 10},
		"540p":  {Values: []int{4}},
	}
	config.References.ImageUpload = true
	config.References.ImageDataURI = true
	settings := dto.ChannelSettings{TaskPluginKey: "huahuo-video", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{upstream: config}}
	channel.SetSetting(settings)
	require.NoError(t, channel.ValidateSettings())
	require.NoError(t, channel.ValidateTaskVideoModelBindings())
	require.NoError(t, model.DB.Create(&channel).Error)
	snapshot, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
	binding, ok := jsplugin.DefaultRegistry.Generation().LookupEndpoint("POST", "/v1/videos", publicModel)
	require.True(t, ok)
	assert.Equal(t, "huahuo-video", binding.Plugin.Meta.Key)
	assert.NotContains(t, snapshot.VideoModels["sudashui"], publicModel)
	selected, err := service.ChannelTaskVideoPlugin(binding.Plugin, &channel, publicModel)
	require.NoError(t, err)
	profile, found := selected.Meta.VideoProfileForModel(upstream)
	require.True(t, found)
	request := map[string]any{"model": publicModel, "prompt": "Draw the sky", "seconds": 6, "resolution": "720p", "aspect_ratio": "16:9"}
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil))
	request["resolution"] = "1080p"
	require.ErrorContains(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil), "duration is outside")
	request["seconds"] = 8
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil))
	request["image"] = "https://cdn.example/start.png"
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "first_last_frame", request, nil))
	request["last_frame"] = "https://cdn.example/end.png"
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "first_last_frame", request, nil))
	request["last_frame"] = "data:image/png;base64,YQ=="
	require.ErrorContains(t, service.ValidateVideoProfileRequest(profile, "first_last_frame", request, nil), "does not allow source")
	mixedBody := []byte(`{"model":"my-huahuo-video","prompt":"Animate this frame","size":"16:9","resolution":"1080p","start_frame":"https://cdn.example/start.png"}`)
	normalized, contentType, defaults, err := service.NormalizeMixedVideoDefaults(mixedBody, "application/json", "create", []jsplugin.VideoProfile{profile})
	require.NoError(t, err)
	assert.Equal(t, float64(10), defaults["seconds"])
	mixed, err := service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", normalized, service.VideoModelOverrides{}, 1, contentType)
	require.NoError(t, err)
	assert.Equal(t, "first_last_frame", mixed.Intent["action"])
	assert.Equal(t, upstream, mixed.UpstreamModel)
	direct := map[string]any{"seconds": 4, "resolution": "540p", "aspect_ratio": "16:9"}
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", direct, nil))
	channelBinding, _, err := service.MixedVideoChannelBinding(channel, publicModel, "create")
	require.NoError(t, err)
	mixedProfile, err := service.EffectiveMixedVideoProfile(channelBinding, publicModel, service.VideoModelOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "720p", mixedProfile.Modes[0].DefaultResolution)
	assert.NotContains(t, mixedProfile.Modes[0].Resolutions, "540p")
	nonRetailResolution := []byte(`{"model":"my-huahuo-video","prompt":"Generate a short video","size":"16:9","resolution":"540p","seconds":4}`)
	_, err = service.DecodeMixedVideoCandidate(t.Context(), channel, publicModel, "create", nonRetailResolution, service.VideoModelOverrides{}, 1, "application/json")
	require.ErrorContains(t, err, "output resolution is invalid")

	frozen := channel
	require.NoError(t, model.DB.Model(&channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
	disabled, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, disabled.VideoModels))
	assert.Empty(t, jsplugin.DefaultRegistry.Generation().LookupEndpointCandidates("POST", "/v1/videos", publicModel))
	_, err = service.FrozenTaskVideoBinding(&frozen, publicModel)
	require.NoError(t, err)

	previousPrices := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"my-huahuo-video":0.02}`))
	withTieredBillingConfig(t, nil, nil)
	assert.Equal(t, "needs_configuration", videoPricingStatus("huahuo-video", publicModel, publicModel, selected, &frozen))
	withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: "0.02"})
	assert.Equal(t, "invalid_configuration", videoPricingStatus("huahuo-video", publicModel, publicModel, selected, &frozen))
	withTieredBillingConfig(t, map[string]string{publicModel: "tiered_expr"}, map[string]string{publicModel: `u("seconds") * 0.02`})
	assert.Equal(t, "configured", videoPricingStatus("huahuo-video", publicModel, publicModel, selected, &frozen))
	withTieredBillingConfig(t, map[string]string{upstream: "tiered_expr"}, map[string]string{upstream: `u("seconds") * 0.02`})
	assert.Equal(t, "configured", videoPricingStatus("huahuo-video", publicModel, publicModel, selected, &frozen))
}

func TestSudashuiDynamicChannelModelsAndFrozenTasks(t *testing.T) {
	setupTaskPluginControllerTest(t)
	plugin, config := sudashuiFixture(t)
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "fixture", Models: "public-video", Status: common.ChannelStatusEnabled}
	mapping := `{"public-video":"future-model-not-in-plugin"}`
	channel.ModelMapping = &mapping
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model-not-in-plugin": config}})
	require.NoError(t, channel.ValidateTaskVideoModelBindings())
	require.NoError(t, model.DB.Create(&channel).Error)
	first, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, first.VideoModels))
	_, found := jsplugin.DefaultRegistry.Generation().LookupEndpoint("POST", "/v1/videos", "public-video")
	require.True(t, found)
	binding, upstream, err := service.MixedVideoChannelBinding(channel, "public-video", "create")
	require.NoError(t, err)
	assert.Equal(t, "future-model-not-in-plugin", upstream)
	profile, found := binding.Plugin.Meta.VideoProfileForModel(upstream)
	require.True(t, found)
	request := map[string]any{"model": "public-video", "prompt": "Keep @image1 then @image2", "seconds": 6, "resolution": "720p", "aspect_ratio": "16:9"}
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil))
	for _, fields := range []map[string]any{{"resolution": "1080p"}, {"aspect_ratio": "9:16"}, {"seconds": 12}} {
		invalid := maps.Clone(request)
		maps.Copy(invalid, fields)
		require.Error(t, service.ValidateVideoProfileRequest(profile, "text_to_video", invalid, nil))
	}
	for _, count := range []int{2, 3} {
		references := make([]any, count)
		for i := range references {
			references[i] = "https://cdn.example/image.png"
		}
		input := maps.Clone(request)
		input["reference_images"] = references
		encoded, err := common.Marshal(input)
		require.NoError(t, err)
		_, err = service.DecodeMixedVideoCandidate(t.Context(), channel, "public-video", "create", encoded, service.VideoModelOverrides{}, 1, "application/json")
		if count == 2 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
	c.Set(jsplugin.ContextKeyProtocolRequest, jsplugin.ProtocolRequestContext{Model: "public-video", Protocol: "openai_video", Operation: "create", RouteRequestContext: jsplugin.RouteRequestContext{Body: map[string]any{"kind": "json", "value": request}}})
	filter := service.ConfiguredTaskVideoFilter(c)
	c.Set(jsplugin.ContextKeyPinnedEndpoint, jsplugin.PinnedEndpoint{Generation: jsplugin.DefaultRegistry.Generation(), Plugin: binding.Plugin, Protocol: binding.Protocol, Operation: binding.Operation, Model: "public-video", Candidates: []jsplugin.ProtocolBinding{binding}})
	c.Set("expected_task_plugin_key", "sudashui")
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "public-video"))
	snapshot := service.TaskExecutionSnapshotFromContext(c)
	require.NotNil(t, snapshot.VideoModelConfig)
	assert.Equal(t, config, *snapshot.VideoModelConfig)
	ok, _ := model.ChannelSatisfiesFilters(&channel, "public-video", []gatewaydto.ChannelFilter{filter})
	require.True(t, ok)
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model-not-in-plugin": {Duration: dto.TaskVideoDuration{Values: []int{4}}, Resolutions: config.Resolutions, AspectRatios: config.AspectRatios, Modes: config.Modes, References: config.References, BillingMode: config.BillingMode}}})
	ok, kind := model.ChannelSatisfiesFilters(&channel, "public-video", []gatewaydto.ChannelFilter{filter})
	assert.False(t, ok)
	assert.Equal(t, gatewaydto.FilterTaskVideoCapability, kind)
	require.Error(t, service.ValidateVideoProfileRequest(profile, "text_to_video", map[string]any{"seconds": 5, "resolution": "720p"}, nil))
	// The previously selected engine/profile remains executable after publication.
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, map[string]map[string]dto.TaskVideoModelConfig{}))
	assert.Empty(t, jsplugin.DefaultRegistry.Generation().LookupEndpointCandidates("POST", "/v1/videos", "public-video"))
	require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil))
	assert.Same(t, plugin.Engine, binding.Plugin.Engine)
	var frozenChannel model.Channel
	require.NoError(t, model.DB.First(&frozenChannel, channel.Id).Error)
	_, err = service.FrozenTaskVideoBinding(&frozenChannel, "public-video")
	require.NoError(t, err)
	config.BillingMode = "per_request"
	other := model.Channel{Type: 61, Models: "public-video"}
	other.ModelMapping = &mapping
	other.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model-not-in-plugin": config}})
	require.ErrorContains(t, other.ValidateTaskVideoModelBindings(), "different public model aliases")
	other.Models = "public-video-per-call"
	otherMapping := `{"public-video-per-call":"future-model-not-in-plugin"}`
	other.ModelMapping = &otherMapping
	require.NoError(t, other.ValidateTaskVideoModelBindings())
	// Rebuilding from persisted settings after a restart recovers routing.
	second, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Equal(t, first.Revision, second.Revision)
	restarted := jsplugin.NewRegistry()
	source, err := plugins.Source("sudashui")
	require.NoError(t, err)
	_, err = restarted.RegisterFactory(source, jsplugin.Options{Key: "sudashui"})
	require.NoError(t, err)
	require.NoError(t, restarted.ReplaceOverridesWithVideoModels(nil, second.VideoModels))
	_, found = restarted.Generation().LookupEndpoint("POST", "/v1/videos", "public-video")
	assert.True(t, found)
	require.NoError(t, model.DB.Create(&other).Error)
	aliases, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, restarted.ReplaceOverridesWithVideoModels(nil, aliases.VideoModels))
	active, found := restarted.Get("sudashui")
	require.True(t, found)
	secondsSchema, _ := active.Meta.UsageForModel("public-video")
	countSchema, _ := active.Meta.UsageForModel("public-video-per-call")
	assert.Contains(t, secondsSchema, "seconds")
	assert.NotContains(t, secondsSchema, "requests")
	assert.Contains(t, countSchema, "requests")
	assert.NotContains(t, countSchema, "seconds")
	// Omitting type in a partial update must not bypass capability validation.
	recorder := httptest.NewRecorder()
	update, _ := gin.CreateTestContext(recorder)
	update.Request = httptest.NewRequest("PUT", "/api/channel", bytes.NewBufferString(`{"id":`+strconv.Itoa(channel.Id)+`,"models":"missing-configuration"}`))
	update.Request.Header.Set("Content-Type", "application/json")
	UpdateChannel(update)
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	var unchanged model.Channel
	require.NoError(t, model.DB.First(&unchanged, channel.Id).Error)
	assert.Equal(t, "public-video", unchanged.Models)
}

func TestSudashuiCaseDistinctModelProfiles(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.TaskPlugin{}))
	_, config := sudashuiFixture(t)
	taskPluginSyncState.Lock()
	previousHashes, previousErrors, previousRebuild := maps.Clone(taskPluginSyncState.hashes), maps.Clone(taskPluginSyncState.errors), taskPluginSyncState.lastRebuild
	taskPluginSyncState.Unlock()
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousCache
		taskPluginSyncState.Lock()
		taskPluginSyncState.hashes, taskPluginSyncState.errors, taskPluginSyncState.lastRebuild = previousHashes, previousErrors, previousRebuild
		taskPluginSyncState.Unlock()
	})
	otherConfig := config
	otherConfig.Resolutions = []string{"1080p"}
	otherConfig.BillingMode = "per_request"
	mapping := `{"Demo":"vendor-seconds","demo":"vendor-other"}`
	channel := model.Channel{Type: 61, Status: common.ChannelStatusEnabled, Models: "Demo,demo", ModelMapping: &mapping}
	settings := dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"vendor-seconds": config, "vendor-other": otherConfig}}
	channel.SetSetting(settings)
	assert.ErrorContains(t, channel.ValidateTaskVideoModelBindings(), "different public model aliases", "case-only aliases cannot bypass the public billing-unit constraint")
	otherConfig.BillingMode = "per_second"
	settings.TaskPluginModelConfigs["vendor-other"] = otherConfig
	channel.SetSetting(settings)
	require.NoError(t, channel.ValidateTaskVideoModelBindings())
	require.NoError(t, db.Create(&channel).Error)
	snapshot, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, syncTaskPluginsOnceContext(t.Context()))
	generation := jsplugin.DefaultRegistry.Generation()
	require.NoError(t, syncTaskPluginsOnceContext(t.Context()))
	assert.Same(t, generation, jsplugin.DefaultRegistry.Generation(), "unchanged settings must not republish a generation")
	for _, tc := range []struct{ model, resolution string }{{"Demo", "720p"}, {"demo", "1080p"}} {
		t.Run(tc.model, func(t *testing.T) {
			canonical, found := generation.CanonicalModel(tc.model)
			require.True(t, found)
			assert.Equal(t, tc.model, canonical)
			binding, found := generation.LookupEndpoint("POST", "/v1/videos", tc.model)
			require.True(t, found)
			profile, found := binding.Plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			assert.Equal(t, []string{tc.resolution}, profile.Modes[0].Resolutions)
			schema, _ := binding.Plugin.Meta.UsageForModel(tc.model)
			assert.Equal(t, []string{tc.resolution}, schema["resolution"].Enum)
			assert.Contains(t, schema, "seconds")
			assert.NotContains(t, schema, "requests")
		})
	}
	plugin, found := generation.Get("sudashui")
	require.True(t, found)
	_, found = generation.CanonicalModel("DEMO")
	assert.False(t, found)
	_, found = plugin.Meta.VideoProfileForModel("DEMO")
	assert.False(t, found, "ambiguous case variants cannot borrow the first model's capabilities")
	schema, _ := plugin.Meta.UsageForModel("DEMO")
	assert.Equal(t, plugin.Meta.UsageSchema, schema)
	// Invalid persisted settings cannot replace the live routing generation.
	otherConfig.BillingMode = "per_request"
	settings.TaskPluginModelConfigs["vendor-other"] = otherConfig
	channel.SetSetting(settings)
	require.NoError(t, db.Model(&channel).Update("setting", channel.Setting).Error)
	require.ErrorContains(t, syncTaskPluginsOnceContext(t.Context()), "different public model aliases")
	assert.Same(t, generation, jsplugin.DefaultRegistry.Generation())
	// Corrected settings publish atomically while old pins retain their profile.
	otherConfig.BillingMode = "per_second"
	otherConfig.Resolutions = []string{"4K"}
	settings.TaskPluginModelConfigs["vendor-other"] = otherConfig
	channel.SetSetting(settings)
	require.NoError(t, db.Model(&channel).Update("setting", channel.Setting).Error)
	nextSnapshot, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.NotEqual(t, snapshot.Revision, nextSnapshot.Revision)
	require.NoError(t, syncTaskPluginsOnceContext(t.Context()))
	next := jsplugin.DefaultRegistry.Generation()
	assert.NotSame(t, generation, next)
	nextPlugin, found := next.Get("sudashui")
	require.True(t, found)
	newProfile, found := nextPlugin.Meta.VideoProfileForModel("demo")
	require.True(t, found)
	assert.Equal(t, []string{"4k"}, newProfile.Modes[0].Resolutions)
	oldProfile, found := plugin.Meta.VideoProfileForModel("demo")
	require.True(t, found)
	assert.Equal(t, []string{"1080p"}, oldProfile.Modes[0].Resolutions)
	// Distinct public aliases may use case-distinct upstream models and units.
	for _, tc := range []struct{ public, upstream, mode string }{{"seconds-video", "Vendor", "per_second"}, {"count-video", "vendor", "per_request"}} {
		config.BillingMode = tc.mode
		mappingJSON, err := common.Marshal(map[string]string{tc.public: tc.upstream})
		require.NoError(t, err)
		modelMapping := string(mappingJSON)
		other := model.Channel{Type: 61, Status: common.ChannelStatusEnabled, Models: tc.public, ModelMapping: &modelMapping}
		other.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{tc.upstream: config}})
		require.NoError(t, other.ValidateTaskVideoModelBindings())
		require.NoError(t, db.Create(&other).Error)
	}
	require.NoError(t, syncTaskPluginsOnceContext(t.Context()))
	active, found := jsplugin.DefaultRegistry.Get("sudashui")
	require.True(t, found)
	secondsSchema, _ := active.Meta.UsageForModel("Vendor")
	countSchema, _ := active.Meta.UsageForModel("vendor")
	assert.Contains(t, secondsSchema, "seconds")
	assert.NotContains(t, secondsSchema, "requests")
	assert.Contains(t, countSchema, "requests")
	assert.NotContains(t, countSchema, "seconds")
}

func TestSudashuiQueuedMixedTaskRetainsRemovedModel(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}))
	plugin, config := sudashuiFixture(t)
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Group: "default", Key: "fixture", Models: "frozen-video"}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"frozen-video": config}})
	require.NoError(t, db.Create(&channel).Error)
	policy := model.ImageGroupPolicy{Group: "default", Platform: "mixed", PolicyKey: service.ImageIdentityHash("default", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"frozen-video","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	route := &service.MixedVideoRouting{Model: "frozen-video", Group: "default", Resolution: "1K"}
	route.BindingKey = service.MediaPoolBindingKey(policy, route.Model, route.Resolution, "video")
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: route.BindingKey, ChannelId: channel.Id}).Error)
	job := model.AsyncMediaJob{Model: route.Model, Group: "default", ChannelId: channel.Id, UserId: user.Id, TokenId: token.Id, Provider: "sudashui"}
	request := service.AsyncVideoRequest{Channel: channel, Body: []byte(`{"model":"frozen-video","prompt":"test","seconds":6,"resolution":"720p"}`), ContentType: "application/json", Operation: "create", PluginHash: plugin.Engine.SourceFingerprint(), MixedRouting: route}
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{"models": "", "setting": `{"task_plugin_key":"sudashui"}`}).Error)
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, map[string]map[string]dto.TaskVideoModelConfig{}))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
	pinAsyncVideoWorker(c, job, request)
	require.False(t, c.IsAborted(), recorder.Body.String())
	pinned := c.MustGet(jsplugin.ContextKeyPinnedPlugin).(jsplugin.PinnedPlugin)
	profile, found := pinned.Plugin.Meta.VideoProfileForModel(job.Model)
	require.True(t, found)
	assert.Equal(t, 10, profile.Modes[0].Duration.Max)
}

func TestSudashuiChannelPartialUpdates(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.TaskPlugin{}, &model.Option{}, &model.Log{}, &model.AuditLog{}))
	_, config := sudashuiFixture(t)
	settings := dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"vendor-partial": config}}
	settingsJSON, err := common.Marshal(settings)
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		fields       map[string]any
		originalType int
		success      bool
	}{
		{name: "explicit type cannot remove configured model", fields: map[string]any{"type": 61, "setting": `{"task_plugin_key":"sudashui"}`}},
		{name: "empty models cannot bypass validation", fields: map[string]any{"models": "", "setting": `{"task_plugin_key":"sudashui"}`}},
		{name: "explicit type preserves omitted settings and destination", fields: map[string]any{"type": 61, "name": "renamed"}, success: true},
		{name: "explicit type with settings preserves omitted destination", fields: map[string]any{"type": 61, "setting": string(settingsJSON)}, success: true},
		{name: "null settings retain existing bindings", fields: map[string]any{"setting": nil, "name": "renamed"}, success: true},
		{name: "mapping must target configured model", fields: map[string]any{"model_mapping": `{"public-partial":"missing-model"}`}},
		{name: "conversion retains model bindings during validation", originalType: constant.ChannelTypeOpenAI, fields: map[string]any{"type": 61, "setting": `{"task_plugin_key":"sudashui"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapping, baseURL := `{"public-partial":"vendor-partial"}`, "https://custom.example"
			channel := model.Channel{Type: 61, Key: "fixture", Models: "public-partial", Group: "default", BaseURL: &baseURL, ModelMapping: &mapping}
			if tc.originalType == 0 {
				channel.SetSetting(settings)
			} else {
				channel.Type = tc.originalType
			}
			require.NoError(t, db.Create(&channel).Error)
			t.Cleanup(func() { require.NoError(t, db.Delete(&model.Channel{}, channel.Id).Error) })
			body := maps.Clone(tc.fields)
			body["id"] = channel.Id
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Set("role", common.RoleRootUser)
			c.Set("id", user.Id)
			c.Request = httptest.NewRequest("PUT", "/api/channel/", bytes.NewReader(encoded))
			c.Request.Header.Set("Content-Type", "application/json")
			UpdateChannel(c)
			var response struct{ Success bool }
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, tc.success, response.Success, recorder.Body.String())
			var saved model.Channel
			require.NoError(t, db.First(&saved, channel.Id).Error)
			assert.Equal(t, channel.Type, saved.Type)
			assert.Equal(t, channel.Setting, saved.Setting)
			assert.Equal(t, channel.Models, saved.Models)
			assert.Equal(t, channel.ModelMapping, saved.ModelMapping)
			assert.Equal(t, baseURL, saved.GetBaseURL())
		})
	}
}

func TestSudashuiCatalogReadsAndTagWrites(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.TaskPlugin{}))
	_, config := sudashuiFixture(t)
	legacySettings := `{"task_plugin_key":"third-party","task_plugin_model_configs":"previously ignored"}`
	tag, baseURL := "tag-video", "https://saved.example"
	legacy := model.Channel{Type: 61, Name: "legacy", Key: "fixture-key", Group: "default", Models: "legacy-video", Tag: &tag, BaseURL: &baseURL, UsedQuota: 72, Setting: &legacySettings}
	require.NoError(t, legacy.Insert())
	mapping := `{"public-video":"saved-video"}`
	video := model.Channel{Type: 61, Key: "fixture", Group: "default", Models: "public-video", Tag: &tag, ModelMapping: &mapping}
	video.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"saved-video": config}})
	require.NoError(t, video.Insert())
	before, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Contains(t, before.VideoModels["sudashui"], video.Models)

	missingMapping := `{"public-video":"missing-video"}`
	renamed := "new-tag"
	require.ErrorContains(t, model.EditChannelByTag(tag, &renamed, &missingMapping, nil, nil, nil, nil, nil, nil), "missing-video requires a saved capability")
	after, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Equal(t, before.Revision, after.Revision)
	var savedLegacy, savedVideo model.Channel
	require.NoError(t, db.First(&savedLegacy, legacy.Id).Error)
	assert.Equal(t, legacy.Type, savedLegacy.Type)
	assert.Equal(t, legacy.Name, savedLegacy.Name)
	assert.Equal(t, legacy.Key, savedLegacy.Key)
	assert.Equal(t, legacy.BaseURL, savedLegacy.BaseURL)
	assert.Equal(t, legacy.UsedQuota, savedLegacy.UsedQuota)
	assert.Equal(t, legacy.Setting, savedLegacy.Setting)
	assert.Equal(t, legacy.Tag, savedLegacy.Tag)
	require.NoError(t, db.First(&savedVideo, video.Id).Error)
	assert.Equal(t, video.ModelMapping, savedVideo.ModelMapping)
	assert.Equal(t, video.Tag, savedVideo.Tag)

	updatedModels, updatedMapping := "new-video", `{"new-video":"saved-video"}`
	require.NoError(t, model.EditChannelByTag(tag, nil, &updatedMapping, &updatedModels, nil, nil, nil, nil, nil))
	after, err = model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.NotEqual(t, before.Revision, after.Revision)
	assert.Contains(t, after.VideoModels["sudashui"], updatedModels)
	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ?", video.Id).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	assert.Equal(t, updatedModels, abilities[0].Model)

	require.NoError(t, db.Migrator().DropTable(&model.Ability{}))
	uncommittedModel, uncommittedMapping := "uncommitted-video", `{"uncommitted-video":"saved-video"}`
	require.ErrorContains(t, model.EditChannelByTag(tag, nil, &uncommittedMapping, &uncommittedModel, nil, nil, nil, nil, nil), "update abilities")
	require.NoError(t, db.First(&savedVideo, video.Id).Error)
	assert.Equal(t, updatedModels, savedVideo.Models)
	assert.Equal(t, updatedMapping, savedVideo.GetModelMapping())
}

func TestSudashuiAsyncAdmissionFreezesSelectedChannel(t *testing.T) {
	user, token := setupResponsesWSRequestTest(t)
	db := setupVideoRetryTestDatabase(t, user, token)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPlugin{}, &model.Option{}, &model.Log{}, &model.UserSubscription{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.AsyncImageUsageProjection{}, &model.AsyncImageLogReceipt{}, &model.MediaRetryPolicyVersion{}, &model.MediaRetryEvent{}, &model.MediaInputObject{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}))
	_, config := sudashuiFixture(t)
	redisServer := miniredis.RunT(t)
	previousRDB, previousRedis, previousSecret, previousPlugins := common.RDB, common.RedisEnabled, common.CryptoSecret, constant.TaskPluginEnabled
	common.RDB = redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	common.RedisEnabled, common.CryptoSecret, constant.TaskPluginEnabled = true, "fixture-deep-smoke", true
	t.Cleanup(func() {
		assert.NoError(t, common.RDB.Close())
		common.RDB, common.RedisEnabled, common.CryptoSecret = previousRDB, previousRedis, previousSecret
		constant.TaskPluginEnabled = previousPlugins
	})
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "deep:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "deep")
	cfg := service.DefaultMediaRuntimeConfig()
	cfg.VideoAsyncEnabled = true
	require.NoError(t, service.SaveMediaRuntimeConfig(t.Context(), cfg))
	withTieredBillingConfig(t, map[string]string{"public-admission": "tiered_expr"}, map[string]string{"public-admission": `u("seconds") * 0.00001`})
	service.InitHttpClient()
	var submits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		submits.Add(1)
		assert.Equal(t, "POST /v1/video/generations", r.Method+" "+r.URL.Path)
		var body map[string]any
		assert.NoError(t, common.DecodeJson(r.Body, &body))
		assert.Equal(t, "vendor-before", body["model"])
		assert.Equal(t, float64(6), body["duration"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"task_id":"frozen-worker","status":"queued"}`)
	}))
	t.Cleanup(upstream.Close)
	mapping, baseURL := `{"public-admission":"vendor-before"}`, upstream.URL
	channel := model.Channel{Type: 61, Name: "admission", Key: "fixture", Models: "public-admission", Group: "default", BaseURL: &baseURL, ModelMapping: &mapping}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"vendor-before": config}})
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: channel.Models, Enabled: true}).Error)
	snapshot, err := model.GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
	engine := gin.New()
	engine.POST("/v1/videos", func(c *gin.Context) {
		c.Set("id", user.Id)
		c.Set("token_id", token.Id)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		c.Next()
	}, PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, func(c *gin.Context) {
		require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, channel.Models))
		// A save between selection and admission must only affect new requests.
		changed := channel
		changedConfig := config
		changedConfig.BillingMode, changedConfig.Resolutions = "per_request", []string{"1080p"}
		changed.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"vendor-after": changedConfig}})
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{"setting": changed.Setting, "model_mapping": `{"public-admission":"vendor-after"}`, "base_url": "https://after.example"}).Error)
		c.Next()
	}, AcceptAsyncVideo)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/v1/videos", bytes.NewBufferString(`{"model":"public-admission","prompt":"test","seconds":6,"resolution":"720p"}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusAccepted, recorder.Code, recorder.Body.String())
	var job model.AsyncMediaJob
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Take(&job).Error)
	encoded, err := service.DecryptImagePayload(job.RequestCipher, "media-request:"+job.TaskId)
	require.NoError(t, err)
	var frozen service.AsyncVideoRequest
	require.NoError(t, common.Unmarshal(encoded, &frozen))
	assert.Equal(t, mapping, frozen.Channel.GetModelMapping())
	assert.Equal(t, baseURL, frozen.Channel.GetBaseURL())
	upstreamModel, frozenConfig, err := frozen.Channel.TaskVideoModelConfig(job.Model)
	require.NoError(t, err)
	assert.Equal(t, "vendor-before", upstreamModel)
	assert.Equal(t, config, frozenConfig)
	require.NotNil(t, frozen.Execution.VideoModelConfig)
	assert.Equal(t, *frozen.Execution.VideoModelConfig, frozenConfig)
	channelBytes, err := service.DecryptImagePayload(job.ChannelCipher, "media-channel:"+job.TaskId)
	require.NoError(t, err)
	var channelSnapshot model.Channel
	require.NoError(t, common.Unmarshal(channelBytes, &channelSnapshot))
	assert.Equal(t, frozen.Channel, channelSnapshot)
	assert.Zero(t, submits.Load(), "admission must not create a paid upstream task")
	withTieredBillingConfig(t, map[string]string{"public-admission": "tiered_expr"}, map[string]string{"public-admission": `u("seconds") * 100`})
	claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, "deep-worker-lease", 60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, ExecuteAsyncVideo(t.Context(), job, frozen))
	assert.Equal(t, int32(1), submits.Load())
	var submitted model.Task
	require.NoError(t, db.Where("task_id = ?", job.TaskId).Take(&submitted).Error)
	assert.Equal(t, "frozen-worker", submitted.PrivateData.UpstreamTaskID)
	assert.Equal(t, config, *submitted.PrivateData.Execution.VideoModelConfig)
	assert.Equal(t, common.QuotaFromFloat(6*0.00001*common.QuotaPerUnit), submitted.Quota)
}

func TestSudashuiConversionValidationAndBilling(t *testing.T) {
	plugin, config := sudashuiFixture(t)
	for _, unit := range []string{"per_second", "per_request"} {
		t.Run(unit, func(t *testing.T) {
			config.BillingMode = unit
			selected := jsplugin.WithTaskVideoModels(plugin, map[string]dto.TaskVideoModelConfig{"future-model": config})
			request := map[string]any{"prompt": " @image1 then @image2 @video1 ", "seconds": 6, "resolution": "720p", "reference_images": []any{"https://cdn.example/1.png", "https://cdn.example/2.png"}, "reference_videos": "https://cdn.example/clip.mp4"}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": "future-model", "operation": "create", "body": map[string]any{"kind": "json", "value": request}})
			require.NoError(t, err)
			intent := value.(map[string]any)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
			c.Set("task_request", intent["requestBody"])
			info := &relaycommon.RelayInfo{OriginModelName: "future-model", ChannelMeta: &relaycommon.ChannelMeta{}, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: "reference_to_video"}}
			info.UpstreamModelName, info.ChannelBaseUrl, info.ApiKey = "future-model", "https://api.example", "fixture"
			adaptor := taskplugin.New(selected)
			adaptor.Init(info)
			require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			require.NoError(t, err)
			ratios, err := adaptor.EstimateBillingValidated(c, info)
			require.NoError(t, err)
			if unit == "per_second" {
				assert.Equal(t, map[string]float64{"seconds": 6}, ratios)
				assert.NotContains(t, facts, "requests")
			} else {
				assert.Equal(t, map[string]float64{"requests": 1}, ratios)
				assert.NotContains(t, facts, "seconds")
			}
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"requestBody": intent["requestBody"], "upstreamModel": "future-model", "baseUrl": "https://api.example", "apiKey": "fixture"})
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://api.example/v1/video/generations", descriptor["url"])
			body := descriptor["body"].(map[string]any)
			assert.Len(t, body, 4)
			assert.Equal(t, request["prompt"], body["prompt"])
			assert.NotContains(t, body, "resolution")
			payload, ok := body["metadata"].(map[string]any)["payload"].(string)
			require.True(t, ok)
			assert.JSONEq(t, `{"aspectRatio":"16:9","mode":"references","imageUrls":["https://cdn.example/1.png","https://cdn.example/2.png"],"videoUrls":["https://cdn.example/clip.mp4"],"audioUrls":[]}`, payload)
		})
	}
	for _, fields := range []map[string]any{
		{"seconds": 0}, {"seconds": relaycommon.MaxTaskDurationSeconds + 1}, {"seconds": "18446744073686646784"}, {"seconds": 1.5},
		{"metadata": map[string]any{"payload": `{"duration":999999}`}}, {"last_frame": "https://cdn.example/last.png"},
		{"first_frame": "https://cdn.example/first.png"}, {"mode": "frames"},
		{"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png", "mode": "unknown"},
		{"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png", "reference_videos": []any{"https://cdn.example/clip.mp4"}},
		{"image": "data:image/png;base64,a"}, {"image": "https://user:secret@cdn.example/a.png"},
	} {
		request := map[string]any{"prompt": "video", "seconds": 6, "resolution": "720p"}
		maps.Copy(request, fields)
		_, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"requestBody": request})
		require.Error(t, err, "%v", fields)
	}
	value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"requestBody": map[string]any{"prompt": "video", "seconds": 6, "resolution": "720p", "image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png"}})
	require.NoError(t, err)
	payload := value.(map[string]any)["body"].(map[string]any)["metadata"].(map[string]any)["payload"]
	assert.JSONEq(t, `{"aspectRatio":"16:9","mode":"frames","firstFrameUrl":"https://cdn.example/first.png","lastFrameUrl":"https://cdn.example/last.png"}`, payload.(string))
}

func TestSudashuiCapabilityEdges(t *testing.T) {
	plugin, config := sudashuiFixture(t)
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Models: "edge-video"}
	for _, resolution := range []string{"720p", "4K"} {
		t.Run(resolution, func(t *testing.T) {
			config.Resolutions = []string{resolution}
			require.NoError(t, config.Validate(relaycommon.MaxTaskDurationSeconds))
			channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"edge-video": config}})
			for name, fields := range map[string]map[string]any{
				"text":         {},
				"single image": {"image": "https://cdn.example/first.png"},
				"frames":       {"first_frame": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png"},
				"references":   {"reference_images": []any{"https://cdn.example/first.png", "https://cdn.example/last.png"}},
			} {
				t.Run(name, func(t *testing.T) {
					body := map[string]any{"model": "edge-video", "prompt": "test", "seconds": 6, "resolution": resolution}
					maps.Copy(body, fields)
					raw, err := common.Marshal(body)
					require.NoError(t, err)
					candidate, err := service.DecodeMixedVideoCandidate(t.Context(), channel, "edge-video", "create", raw, service.VideoModelOverrides{}, 1, "application/json")
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
					c.Set("task_request", candidate.Intent["requestBody"])
					info := &relaycommon.RelayInfo{OriginModelName: "edge-video", ChannelMeta: &relaycommon.ChannelMeta{}, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: candidate.Intent["action"].(string)}}
					info.UpstreamModelName = "edge-video"
					info.ChannelBaseUrl, info.ApiKey = "https://api.example", "fixture"
					adaptor := taskplugin.New(jsplugin.WithTaskVideoModels(plugin, map[string]dto.TaskVideoModelConfig{"edge-video": config}))
					adaptor.Init(info)
					require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
				})
			}
		})
	}
}

func TestSudashuiLifecycleAndContent(t *testing.T) {
	plugin, _ := sudashuiFixture(t)
	ctx := map[string]any{"taskId": "task_fixture", "baseUrl": "https://api.example", "apiKey": "fixture"}
	_, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": "task_fixture", "task_id": "task_fixture", "status": "queued"}})
	require.NoError(t, err)
	_, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"code": "fail_to_fetch_task", "message": `{"state":"failed","message":"duration rejected"}`}})
	require.ErrorContains(t, err, "duration rejected")
	for _, state := range []struct{ raw, status string }{{"NOT_START", "NOT_START"}, {"SUBMITTED", "QUEUED"}, {"IN_PROGRESS", "IN_PROGRESS"}, {"FUTURE_STATE", "UNKNOWN"}, {"FAILURE", "FAILURE"}} {
		value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"code": "success", "data": map[string]any{"task_id": "task_fixture", "status": state.raw, "fail_reason": "render failed", "result_url": "not a URL on failure"}})
		require.NoError(t, err)
		assert.Equal(t, state.status, value.(map[string]any)["status"])
	}
	body := map[string]any{"code": "success", "data": map[string]any{"task_id": "task_fixture", "status": "SUCCESS", "progress": "100%", "data": map[string]any{"state": "success", "creations": []any{map[string]any{"url": "https://cdn.example/output.mp4"}}}}}
	value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/output.mp4", value.(map[string]any)["url"])
	ctx["data"], ctx["artifactKey"], ctx["clientRequest"] = body, "video", map[string]any{"method": "GET"}
	value, err = plugin.Engine.Call(t.Context(), "buildContentRequest", ctx)
	require.NoError(t, err)
	assert.Equal(t, true, value.(map[string]any)["credentialless"])
	assert.NotContains(t, value.(map[string]any), "headers")
	body["data"].(map[string]any)["task_id"] = "another-task"
	_, err = plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
	require.ErrorContains(t, err, "mismatch")
	delete(body["data"].(map[string]any), "task_id")
	_, err = plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, body)
	require.ErrorContains(t, err, "mismatch")
	value, err = plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, map[string]any{"code": "fail_to_fetch_task", "message": "lookup temporarily unavailable"})
	require.NoError(t, err)
	assert.Equal(t, "UNKNOWN", value.(map[string]any)["status"])
}

func TestSudashuiCatalogIsAnAuthenticatedPreview(t *testing.T) {
	setupTaskPluginControllerTest(t)
	_, config := sudashuiFixture(t)
	fetchSetting := system_setting.GetFetchSetting()
	previous := *fetchSetting
	fetchSetting.EnableSSRFProtection = false
	service.InitHttpClient()
	t.Cleanup(func() { *fetchSetting = previous })
	redirectHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirectHit = true }))
	defer target.Close()
	response := `{"success":true,"data":[{"id":"future-video","model_info":{"modality":"video","resolution":"720p","billing_mode":"per_request","duration":{"values":[5,10]},"references":{"images":null,"videos":0,"audios":0},"aspect_ratios":["16:9"],"final_price":8.16,"currency":"CNY"}},{"id":"image-model","model_info":{"modality":"image"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer catalog-fixture", r.Header.Get("Authorization"))
		assert.Empty(t, r.URL.RawQuery)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target.URL, http.StatusFound)
			return
		}
		if r.URL.Path == "/failure" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	channel := model.Channel{Type: 61, Key: "catalog-fixture", Models: "saved-video"}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelSourceURL: server.URL, TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"saved-video": config}})
	require.NoError(t, model.DB.Create(&channel).Error)
	before := *channel.Setting
	ids, details, err := fetchChannelModelPreview(t.Context(), &channel)
	require.NoError(t, err)
	assert.Equal(t, []string{"future-video"}, ids)
	assert.Nil(t, details["future-video"].References.Images)
	assert.Empty(t, details["future-video"].Modes)
	assert.Equal(t, before, *channel.Setting)
	var saved model.Channel
	require.NoError(t, model.DB.First(&saved, channel.Id).Error)
	assert.Equal(t, before, *saved.Setting)
	for _, suffix := range []string{"/redirect", "/failure"} {
		address := server.URL + suffix
		preview, err := buildSudashuiModelPreviewChannel(fetchModelsRequest{ChannelID: channel.Id, TaskPluginModelSourceURL: &address, Key: "catalog-fixture"})
		require.NoError(t, err)
		_, _, err = fetchChannelModelPreview(t.Context(), preview)
		require.Error(t, err)
	}
	assert.False(t, redirectHit, "catalog credentials must never follow redirects")
	// The legacy data array remains available alongside structured details.
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/api/channel/fetch_models", bytes.NewBufferString(`{"type":61,"task_plugin_key":"sudashui","channel_id":`+strconv.Itoa(channel.Id)+`}`))
	c.Request.Header.Set("Content-Type", "application/json")
	FetchModels(c)
	assert.Equal(t, http.StatusOK, recorder.Code)
	var fetched struct {
		Success      bool
		Data         []string
		ModelDetails map[string]sudashuiModelDetail `json:"model_details"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &fetched))
	assert.True(t, fetched.Success)
	assert.Equal(t, ids, fetched.Data)
	assert.Contains(t, fetched.ModelDetails, "future-video")
	assert.NotContains(t, recorder.Body.String(), "catalog-fixture")
}

func TestSudashuiCatalogPreviewKeepsSavedKeyOnSavedRoute(t *testing.T) {
	setupTaskPluginControllerTest(t)
	fetchSetting := system_setting.GetFetchSetting()
	previous := *fetchSetting
	fetchSetting.EnableSSRFProtection = false
	t.Cleanup(func() { *fetchSetting = previous })
	service.InitHttpClient()

	savedRequests := make(chan string, 4)
	savedCatalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		savedRequests <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	t.Cleanup(savedCatalog.Close)
	previewRequests := make(chan string, 4)
	previewCatalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		previewRequests <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	t.Cleanup(previewCatalog.Close)

	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Key: "saved-fixture"}
	channel.SetSetting(dto.ChannelSettings{
		TaskPluginKey: "sudashui", TaskPluginModelSourceURL: savedCatalog.URL,
	})
	require.NoError(t, model.DB.Create(&channel).Error)

	for _, tc := range []struct {
		name, key, catalog, proxy string
		received                  <-chan string
		wantKey                   string
	}{
		{name: "saved route", catalog: savedCatalog.URL, received: savedRequests, wantKey: "saved-fixture"},
		{name: "different URL", catalog: previewCatalog.URL},
		{name: "different proxy", catalog: savedCatalog.URL, proxy: previewCatalog.URL},
		{name: "caller supplied key", catalog: previewCatalog.URL, key: "preview-fixture", received: previewRequests, wantKey: "preview-fixture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{
				"type": constant.ChannelTypeTaskPlugin, "task_plugin_key": "sudashui", "channel_id": channel.Id,
				"task_plugin_model_source_url": tc.catalog, "proxy": tc.proxy, "key": tc.key,
			})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			FetchModels(c)

			var result struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
			assert.NotContains(t, recorder.Body.String(), "saved-fixture")
			assert.Equal(t, tc.wantKey != "", result.Success)
			if tc.wantKey == "" {
				assert.Contains(t, result.Message, "enter an API key")
				require.Empty(t, savedRequests)
				require.Empty(t, previewRequests)
				return
			}
			select {
			case authorization := <-tc.received:
				assert.Equal(t, "Bearer "+tc.wantKey, authorization)
			default:
				t.Error("catalog did not receive a request")
			}
		})
	}
	var saved model.Channel
	require.NoError(t, model.DB.First(&saved, channel.Id).Error)
	assert.Equal(t, channel.GetSetting(), saved.GetSetting())
	assert.Equal(t, channel.Key, saved.Key)
}

func TestSudashuiRoutedSubmitPollAndFrozenBilling(t *testing.T) {
	for _, tc := range []struct {
		unit string
		fail bool
	}{{"per_second", false}, {"per_request", false}, {"per_second", true}, {"per_request", true}} {
		t.Run(tc.unit+"/failure="+strconv.FormatBool(tc.fail), func(t *testing.T) {
			user, token := setupResponsesWSRequestTest(t)
			db := setupVideoRetryTestDatabase(t, user, token)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.TaskPlugin{}, &model.Log{}, &model.UserSubscription{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaArtifactObject{}, &model.ImageGroupPolicy{}, &model.TokenImagePlatformMapping{}, &model.ImageChannelPool{}))
			user.Quota, token.RemainQuota = 1_000_000, 1_000_000
			require.NoError(t, model.DB.Model(user).Update("quota", user.Quota).Error)
			require.NoError(t, model.DB.Model(token).Update("remain_quota", token.RemainQuota).Error)
			_, config := sudashuiFixture(t)
			config.BillingMode = tc.unit
			quantity, expression := 6, `u("seconds") * 0.01`
			if tc.unit == "per_request" {
				quantity, expression = 1, `u("requests") * 0.01`
			}
			withTieredBillingConfig(t, map[string]string{"smoke-public": "tiered_expr"}, map[string]string{"smoke-public": expression})
			previousBatch, previousConsume := common.BatchUpdateEnabled, common.LogConsumeEnabled
			common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false
			t.Cleanup(func() { common.BatchUpdateEnabled, common.LogConsumeEnabled = previousBatch, previousConsume })
			service.InitHttpClient()
			var submits, queries atomic.Int32
			uncertain := []string{
				`{"code":"fail_to_fetch_task","message":"lookup unavailable","data":{"task_id":"task_smoke","status":"FAILURE"}}`,
				`{"code":"success","data":{"task_id":"task_smoke","status":"FUTURE_STATE","result_url":"http://8.8.8.8/smoke.mp4"}}`,
				`{"code":"success","data":{"task_id":"another-task","status":"SUCCESS","result_url":"http://8.8.8.8/smoke.mp4"}}`,
				`{"code":"success","data":{"task_id":"task_smoke","status":"SUCCESS","data":{"state":"failed"}}}`,
				`{"code":"success","data":{"task_id":"task_smoke","status":"SUCCESS"}}`,
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer smoke-secret", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/video/generations":
					submits.Add(1)
					var body map[string]any
					assert.NoError(t, common.DecodeJson(r.Body, &body))
					assert.Equal(t, "future-vendor-model", body["model"])
					assert.Equal(t, float64(6), body["duration"])
					assert.NotContains(t, body, "resolution")
					metadata, ok := body["metadata"].(map[string]any)
					if assert.True(t, ok) {
						payload, ok := metadata["payload"].(string)
						if assert.True(t, ok) {
							assert.JSONEq(t, `{"aspectRatio":"16:9","mode":"references","imageUrls":[],"videoUrls":[],"audioUrls":[]}`, payload)
						}
					}
					_, _ = io.WriteString(w, `{"id":"task_smoke","task_id":"task_smoke","status":"queued"}`)
				case "GET /v1/video/generations/task_smoke":
					index := int(queries.Add(1)) - 1
					if index < len(uncertain) {
						_, _ = io.WriteString(w, uncertain[index])
					} else if tc.fail {
						_, _ = io.WriteString(w, `{"code":"success","data":{"task_id":"task_smoke","status":"FAILURE","data":{"state":"failed","message":"render rejected"}}}`)
					} else {
						_, _ = io.WriteString(w, `{"code":"success","data":{"task_id":"task_smoke","status":"SUCCESS","result_url":"http://8.8.8.8/smoke.mp4","data":{"state":"success"}}}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(upstream.Close)
			mapping := `{"smoke-public":"future-vendor-model"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "eligible", Key: "smoke-secret", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "smoke-public", Group: "default", ModelMapping: &mapping}
			channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-vendor-model": config}})
			require.NoError(t, model.DB.Create(&channel).Error)
			limited := channel
			limited.Id, limited.Name, limited.Priority = 0, "ineligible high priority", common.GetPointer(int64(100))
			short := config
			short.Duration = dto.TaskVideoDuration{Values: []int{4}}
			limited.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-vendor-model": short}})
			require.NoError(t, model.DB.Create(&limited).Error)
			for _, id := range []int{channel.Id, limited.Id} {
				require.NoError(t, model.DB.Create(&model.Ability{ChannelId: id, Group: "default", Model: "smoke-public", Enabled: true}).Error)
			}
			snapshot, err := model.GetTaskPluginSyncSnapshot()
			require.NoError(t, err)
			require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, snapshot.VideoModels))
			engine := gin.New()
			engine.POST("/v1/videos", func(c *gin.Context) {
				c.Set("id", user.Id)
				c.Set("token_id", token.Id)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
				c.Next()
			}, PrepareAsyncVideo, PinAsyncVideoEndpoint(), FilterAsyncVideoProvider, middleware.PrepareTaskPluginEndpoint(), RouteAsyncVideo, func(c *gin.Context) {
				pin, found, _ := service.GetChannelConstraints(c).ResolvedPin()
				require.True(t, found)
				require.Equal(t, channel.Id, pin.ChannelId, "actual channel capability must override priority")
				require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "smoke-public"))
				info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, UserGroup: "default", UsingGroup: "default", TokenGroup: "default", UserQuota: user.Quota, OriginModelName: "smoke-public", TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: c.GetString("task_action"), PublicTaskID: "task_smoke_public", LockedChannel: &channel}}
				info.UserSetting.BillingPreference = "wallet_only"
				outcome, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
				require.Nil(t, taskErr)
				require.NotNil(t, outcome)
				presentTaskSubmission(c, outcome)
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1/videos", bytes.NewBufferString(`{"model":"smoke-public","prompt":"smoke","seconds":6,"resolution":"720p"}`))
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.Equal(t, int32(1), submits.Load())
			var task model.Task
			require.NoError(t, model.DB.Where("task_id = ?", "task_smoke_public").First(&task).Error)
			assert.Equal(t, config, *task.PrivateData.Execution.VideoModelConfig)
			quota := common.QuotaFromFloat(float64(quantity) * 0.01 * common.QuotaPerUnit)
			assert.Equal(t, quota, task.Quota)
			// A live model removal cannot change an accepted task's execution or price.
			require.NoError(t, model.DB.Model(&channel).Updates(map[string]any{"models": "", "setting": `{"task_plugin_key":"sudashui"}`}).Error)
			require.NoError(t, jsplugin.DefaultRegistry.ReplaceOverridesWithVideoModels(nil, map[string]map[string]dto.TaskVideoModelConfig{}))
			previousAdaptor := service.GetTaskAdaptorFunc
			service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
			t.Cleanup(func() { service.GetTaskAdaptorFunc = previousAdaptor })
			for range len(uncertain) {
				service.DispatchPlatformUpdate(context.Background(), task.Platform, map[int][]string{channel.Id: {"task_smoke"}}, map[string]*model.Task{"task_smoke": &task})
				require.NoError(t, db.First(&task, task.ID).Error)
				assert.NotContains(t, []model.TaskStatus{model.TaskStatusSuccess, model.TaskStatusFailure}, task.Status, "unconfirmed responses must not settle")
				assert.Equal(t, quota, task.Quota)
				var reservedUser model.User
				var reservedToken model.Token
				require.NoError(t, db.First(&reservedUser, user.Id).Error)
				require.NoError(t, db.First(&reservedToken, token.Id).Error)
				assert.Equal(t, user.Quota-quota, reservedUser.Quota)
				assert.Equal(t, token.RemainQuota-quota, reservedToken.RemainQuota)
			}
			stale := task
			// A stale second poller must not settle/refund the same task twice.
			for _, polled := range []*model.Task{&task, &stale} {
				service.DispatchPlatformUpdate(context.Background(), task.Platform, map[int][]string{channel.Id: {"task_smoke"}}, map[string]*model.Task{"task_smoke": polled})
			}
			require.NoError(t, db.First(&task, task.ID).Error)
			if tc.fail {
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
				assert.Equal(t, "render rejected", task.FailReason)
				quota = 0
			} else {
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
			}
			assert.Equal(t, quota, task.Quota)
			assert.Equal(t, int32(len(uncertain)+2), queries.Load())
			var settledUser model.User
			var settledToken model.Token
			require.NoError(t, model.DB.First(&settledUser, user.Id).Error)
			require.NoError(t, model.DB.First(&settledToken, token.Id).Error)
			assert.Equal(t, user.Quota-quota, settledUser.Quota)
			assert.Equal(t, token.RemainQuota-quota, settledToken.RemainQuota)
			if !tc.fail {
				cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "/smoke.mp4", r.URL.Path)
					assert.Empty(t, r.Header.Get("Authorization"))
					assert.Empty(t, r.Header.Get("X-Api-Key"))
					assert.Equal(t, "bytes=0-3", r.Header.Get("Range"))
					w.Header().Set("Content-Type", "video/mp4")
					w.Header().Set("Content-Range", "bytes 0-3/10")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, "data")
				}))
				t.Cleanup(cdn.Close)
				client := service.GetSSRFProtectedHTTPClient()
				previousTransport := client.Transport
				transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					assert.Equal(t, "8.8.8.8:80", address)
					return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(cdn.URL, "http://"))
				}}
				client.Transport = transport
				t.Cleanup(func() {
					client.Transport = previousTransport
					transport.CloseIdleConnections()
				})
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Set("role", common.RoleRootUser)
				c.Params = gin.Params{{Key: "key", Value: task.TaskID}, {Key: "artifact_key", Value: "video"}}
				c.Request = httptest.NewRequest("GET", "/v1/tasks/"+task.TaskID+"/artifacts/video/content", nil)
				c.Request.Header.Set("Authorization", "Bearer client-secret")
				c.Request.Header.Set("Range", "bytes=0-3")
				TaskArtifactContent(c)
				assert.Equal(t, http.StatusPartialContent, recorder.Code, recorder.Body.String())
				assert.Equal(t, "data", recorder.Body.String())
				assert.Equal(t, "bytes 0-3/10", recorder.Header().Get("Content-Range"))
			}
		})
	}
}
