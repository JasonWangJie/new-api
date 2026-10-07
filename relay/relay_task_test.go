package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRelayChannelDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousCache := common.MemoryCacheEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.Channel{}))
	model.DB = database
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled = previousCache
		require.NoError(t, sqlDB.Close())
	})
	return database
}

func TestApplyChannelPinPreservesOriginTasksAndRetryMode(t *testing.T) {
	database := setupRelayChannelDB(t)
	channel := &model.Channel{Name: "origin-channel", Key: "sk-test", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeDoubaoVideo}
	require.NoError(t, database.Create(channel).Error)
	originTask := &model.Task{
		TaskID: "task-lock", ChannelId: channel.Id, Action: "text_to_video", Status: model.TaskStatusSuccess,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task-lock"},
		Data:        []byte(`{"id":"upstream-task-lock"}`),
	}

	for _, tc := range []struct {
		name     string
		tokenPin bool
		apply    func(*gin.Context, *relaycommon.RelayInfo) *dto.TaskError
	}{
		{name: "origin affinity", apply: ApplyOriginTaskAffinity},
		{name: "same channel retry", apply: ApplyChannelPin},
		{name: "token pin suppresses channel lock", tokenPin: true, apply: ApplyChannelPin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyOriginTasks, []*model.Task{originTask})
			constraints := service.GetChannelConstraints(c)
			constraints.AddPin(dto.ChannelPin{ChannelId: channel.Id, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySameChannel})
			if tc.tokenPin {
				constraints.AddPin(dto.ChannelPin{ChannelId: channel.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt})
			}
			info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			require.Nil(t, tc.apply(c, info))
			if tc.tokenPin {
				assert.Nil(t, info.LockedChannel)
			} else {
				locked, ok := info.LockedChannel.(*model.Channel)
				require.True(t, ok)
				require.NotNil(t, locked)
				assert.Equal(t, channel.Id, locked.Id)
			}
			require.Len(t, info.OriginTasks, 1)
			assert.Equal(t, "task-lock", info.OriginTasks[0].TaskID)
			assert.Equal(t, "upstream-task-lock", info.OriginTasks[0].UpstreamTaskID)
			assert.Equal(t, "text_to_video", info.OriginTasks[0].Action)
			assert.Equal(t, string(model.TaskStatusSuccess), info.OriginTasks[0].Status)
			assert.Equal(t, []byte(originTask.Data), info.OriginTasks[0].Data)
		})
	}
}

func TestSoraRemixUsesOriginVideoAccountIDAndBillingUsage(t *testing.T) {
	database := setupRelayChannelDB(t)
	require.NoError(t, database.AutoMigrate(&model.Task{}))
	saveBillingConfig(t)
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		billing_setting.PluginBillingExprOption: `{"sora::sora-2-pro":"tier(\"video\", u(\"seconds\") * 0.5)"}`,
	}))
	channel := &model.Channel{
		Name: "origin-accounts", Key: "account-a\naccount-b", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeSora,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2},
	}
	require.NoError(t, database.Create(channel).Error)
	origin := &model.Task{
		TaskID: "task_public_origin", UserId: 1, ChannelId: channel.Id, Status: model.TaskStatusSuccess,
		Properties: model.Properties{OriginModelName: "sora-2-pro", UpstreamModelName: "sora-2-pro"},
		PrivateData: model.TaskPrivateData{
			Key: "account-b", UpstreamTaskID: "video_upstream_origin",
			BillingContext: &model.TaskBillingContext{TieredSnapshot: &billingexpr.BillingSnapshot{
				UsageFacts: map[string]any{"seconds": float64(8), "size": "1792x1024"},
			}},
		},
		Data: []byte(`{"id":"video_upstream_origin","seconds":"4","size":"720x1280"}`),
	}
	require.NoError(t, database.Create(origin).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/task_public_origin/remix", strings.NewReader(`{"prompt":"remix this video"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "video_id", Value: origin.TaskID}}
	c.Set("group", "default")
	c.Set("async_media_prepare", true)
	info := &relaycommon.RelayInfo{UserId: 1, UserGroup: "default", UsingGroup: "default", TaskRelayInfo: &relaycommon.TaskRelayInfo{}, ChannelMeta: &relaycommon.ChannelMeta{}}
	require.Nil(t, ResolveOriginTask(c, info))
	_, adaptor := getTaskAdaptorForRequest(c, constant.TaskPlatform("sora"))
	require.NotNil(t, adaptor)
	adaptor.Init(info)
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	upstreamURL, err := adaptor.BuildRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://api.openai.com/v1/videos/video_upstream_origin/remix", upstreamURL)
	assert.Equal(t, constant.TaskActionRemix, info.Action)
	assert.Equal(t, "account-b", info.ApiKey)
	assert.True(t, service.GetChannelConstraints(c).SuppressesRetry())

	result, taskErr := RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	require.NotNil(t, result)
	assert.Equal(t, common.QuotaRound(4*common.QuotaPerUnit), result.Quota)
	assert.Equal(t, map[string]any{"seconds": float64(8), "size": "1792x1024"}, info.TieredBillingSnapshot.UsageFacts)

	channel.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	require.NoError(t, channel.SaveChannelInfo())
	taskErr = ResolveOriginTask(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "origin_task_key_unavailable", taskErr.Code)
}

func TestTaskModel2DtoNormalizesLegacyAction(t *testing.T) {
	task := &model.Task{Action: "firstTailGenerate"}

	dtoTask := TaskModel2Dto(task)

	assert.Equal(t, constant.TaskActionFirstTailToVideo, dtoTask.Action)
	assert.Equal(t, "firstTailGenerate", task.Action)
}

const mappingOrderSubmitPlugin = `
export const meta = {apiVersion:1,key:"maporder",name:"Map Order",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

const mappingOrderRewritePlugin = `
export const meta = {apiVersion:1,key:"maporder-rw",name:"Map Order RW",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel}, rewriteModel:"rewritten"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func pinMappingOrderPlugin(t *testing.T, c *gin.Context, source string) {
	t.Helper()
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
}

func newTaskSubmitContext(t *testing.T, originalModel, mapping string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, originalModel)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
	if mapping != "" {
		c.Set("model_mapping", mapping)
	}
	c.Set("task_request", map[string]any{"prompt": "p"})
	return c, &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
}

func TestRelayTaskSubmitMapsBeforeValidateWhenOriginSet(t *testing.T) {
	const mapping = `{"alias-model":"mid-model","mid-model":"declared-model"}`

	c, info := newTaskSubmitContext(t, "alias-model", mapping)
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "alias-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDeclaredNameWithoutMappingIsUnchanged(t *testing.T) {
	c, info := newTaskSubmitContext(t, "declared-model", "")
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "declared-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "declared-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.False(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDoesNotApplyMappingTwice(t *testing.T) {
	c, info := newTaskSubmitContext(t, "alias-model", `{"alias-model":"declared-model"}`)
	pinMappingOrderPlugin(t, c, mappingOrderRewritePlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "rewritten", info.UpstreamModelName, "late mapping would overwrite rewriteModel with the chain tail")
	assert.Equal(t, "alias-model", info.OriginModelName)
}

func TestRelayTaskSubmitEmptyOriginKeepsLateMapping(t *testing.T) {
	plugin, err := pluginruntime.NewRegistry().Register(mappingOrderSubmitPlugin, pluginruntime.Options{})
	require.NoError(t, err)
	synthesized := service.CoverTaskActionToModelName(constant.TaskPlatform(plugin.Meta.Key), "text_to_video")
	c, info := newTaskSubmitContext(t, "pre-validate-upstream",
		`{"pre-validate-upstream":"should-not-apply-early","`+synthesized+`":"legacy-tail"}`)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
	info.OriginModelName = ""

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, synthesized, info.OriginModelName)
	assert.Equal(t, "legacy-tail", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

const billingFallbackPlugin = `
export const meta = {apiVersion:1,key:"bill-fallback",name:"Bill Fallback",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func saveBillingConfig(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
}

func TestRelayTaskSubmitAliasBillingIdentityAndExprFallback(t *testing.T) {
	const mapping = `{"alias-model":"declared-model"}`
	const aliasExpr = `tier("alias", 2)`
	const tailExpr = `tier("tail", 3)`
	const legacyExpr = `tier("legacy", u("old_units") * 2)`

	tests := []struct {
		name       string
		modes      map[string]string
		exprs      map[string]string
		wantTiered bool
		wantExpr   string
		source     string
	}{
		{
			name:       "alias own tiered wins",
			modes:      map[string]string{"alias-model": "tiered_expr", "declared-model": "tiered_expr"},
			exprs:      map[string]string{"alias-model": aliasExpr, "declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   aliasExpr,
		},
		{
			name:       "fallback uses tail expr",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   tailExpr,
		},
		{
			name:       "stored expression keeps running after schema narrows",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": legacyExpr},
			wantTiered: true,
			wantExpr:   legacyExpr,
			source: strings.Replace(billingFallbackPlugin, `fetchMode:"per_task"`, `fetchMode:"per_task", usageSchema:{old_units:{type:"number",unit:"count"}}, usageProfiles:[{models:["declared-model"],schema:{seconds:{type:"number",unit:"second"}}}]`, 1) + `
export function extractUsage(){return {old_units:2};}
`,
		},
		{
			name:       "neither tiered uses ordinary pricing",
			wantTiered: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			saveBillingConfig(t)
			if len(testCase.modes) > 0 {
				modeJSON, marshalErr := common.Marshal(testCase.modes)
				require.NoError(t, marshalErr)
				exprJSON, marshalErr := common.Marshal(testCase.exprs)
				require.NoError(t, marshalErr)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": string(modeJSON),
					"billing_setting.billing_expr": string(exprJSON),
				}))
				if testCase.wantExpr == aliasExpr {
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("alias-model"))
				} else {
					require.Equal(t, billing_setting.BillingModeRatio, billing_setting.GetBillingMode("alias-model"))
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("declared-model"))
				}
			}

			c, info := newTaskSubmitContext(t, "alias-model", mapping)
			c.Set("group", "default")
			info.UserGroup = "default"
			info.UsingGroup = "default"
			source := testCase.source
			if source == "" {
				source = billingFallbackPlugin
			}
			pinMappingOrderPlugin(t, c, source)
			info.OriginModelName = "alias-model"

			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "alias-model", info.OriginModelName)
			assert.Equal(t, "declared-model", info.UpstreamModelName)
			assert.True(t, info.IsModelMapped)

			task := model.InitTask(constant.TaskPlatform("bill-fallback"), info)
			assert.Equal(t, "alias-model", task.Properties.OriginModelName)
			assert.Equal(t, "declared-model", task.Properties.UpstreamModelName)

			if testCase.wantTiered {
				require.NotNil(t, info.TieredBillingSnapshot, "submission error: %+v", taskErr)
				assert.Equal(t, "alias-model", info.TieredBillingSnapshot.ModelName)
				assert.Equal(t, testCase.wantExpr, info.TieredBillingSnapshot.ExprString)
				assert.Equal(t, billingexpr.ExprHashString(testCase.wantExpr), info.TieredBillingSnapshot.ExprHash)
				assert.NotEqual(t, "model_price_error", taskErr.Code)
				if testCase.wantExpr == legacyExpr {
					assert.Equal(t, 4*common.QuotaPerUnit, info.TieredBillingSnapshot.EstimatedQuotaBeforeGroup)
				}
			} else {
				assert.Nil(t, info.TieredBillingSnapshot)
				assert.Equal(t, "model_price_error", taskErr.Code)
			}
		})
	}
}

func TestSharedTaskBillingExpressionSelectionAndFrozenSettlement(t *testing.T) {
	const baseExpr = `tier("base", u("seconds") * 2)`
	const alphaExpr = `tier("alpha", u("seconds") * 3)`
	const betaExpr = `tier("beta", u("credits") * 5)`
	const aliasExpr = `tier("alias", u("credits") * 7)`
	for _, tc := range []struct {
		name, plugin, model, mapping, modelExpr, mode, wantExpr string
		variants                                                map[string]string
		wantPriceError                                          bool
	}{
		{name: "executing plugin override", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-alpha::declared-model": alphaExpr, "billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "override ignores model mode", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "ratio", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "model expression fallback", plugin: "billing-alpha", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantExpr: baseExpr},
		{name: "alias override precedes mapped override", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr, "billing-beta::alias-model": aliasExpr}, wantExpr: aliasExpr},
		{name: "mapped override precedes model fallback", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "unconfigured plugin cannot use another schema", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantPriceError: true},
		{name: "missing usage in skipped branch remains incompatible", plugin: "billing-beta", model: "declared-model", modelExpr: `true ? tier("free", 0) : tier("missing", u("seconds"))`, mode: "tiered_expr", wantPriceError: true},
		{name: "fixed pricing is still rejected", plugin: "billing-beta", model: "declared-model", variants: map[string]string{"billing-beta::declared-model": `tier("fixed", fixed(1))`}, wantPriceError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveBillingConfig(t)
			registry := pluginruntime.NewRegistry()
			for _, spec := range []struct{ key, field, unit string }{{"billing-alpha", "seconds", "second"}, {"billing-beta", "credits", "credit"}} {
				source := strings.ReplaceAll(billingFallbackPlugin, "bill-fallback", spec.key)
				source = strings.Replace(source, `fetchMode:"per_task"`, `fetchMode:"per_task",usageSchema:{`+spec.field+`:{type:"number",unit:"`+spec.unit+`"}}`, 1)
				source += `export function extractUsage(){return {` + spec.field + `:2};}`
				_, err := registry.Register(source, pluginruntime.Options{})
				require.NoError(t, err)
			}
			variants := tc.variants
			if variants == nil {
				variants = map[string]string{}
			}
			rawVariants, err := common.Marshal(variants)
			require.NoError(t, err)
			modes, err := common.Marshal(map[string]string{"declared-model": tc.mode})
			require.NoError(t, err)
			expressions, err := common.Marshal(map[string]string{"declared-model": tc.modelExpr})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				billing_setting.PluginBillingExprOption: string(rawVariants), "billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions),
			}))
			c, info := newTaskSubmitContext(t, tc.model, tc.mapping)
			c.Set("group", "default")
			c.Set("task_plugin_key", tc.plugin)
			info.UserGroup = "default"
			info.UsingGroup = "default"
			info.OriginModelName = tc.model
			plugin, ok := registry.Generation().Get(tc.plugin)
			require.True(t, ok)
			c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: registry.Generation(), Plugin: plugin})
			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr) // This fixture stops at reservation, before upstream submission.
			if tc.wantPriceError {
				assert.Equal(t, "model_price_error", taskErr.Code)
				assert.Nil(t, info.TieredBillingSnapshot)
				return
			}
			require.NotNil(t, info.TieredBillingSnapshot, "submission error: %+v", taskErr)
			assert.Equal(t, tc.wantExpr, info.TieredBillingSnapshot.ExprString)
			assert.NotEqual(t, "model_price_error", taskErr.Code)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: `{}`, "billing_setting.billing_expr": `{}`}))
			field := "seconds"
			if tc.plugin == "billing-beta" {
				field = "credits"
			}
			result, usage, err := service.EvaluateTaskCompletionUsage(info.TieredBillingSnapshot, map[string]any{field: float64(4)})
			require.NoError(t, err)
			assert.Equal(t, float64(4), usage[field])
			assert.Equal(t, float64(2), info.TieredBillingSnapshot.UsageFacts[field])
			assert.Equal(t, 2*info.TieredBillingSnapshot.EstimatedQuotaAfterGroup, result.ActualQuotaAfterGroup)
			assert.Equal(t, tc.wantExpr, info.TieredBillingSnapshot.ExprString)
		})
	}
}

func TestTaskConditionalPricingFreezesRequestThroughCompletion(t *testing.T) {
	const expression = `tier("seconds", u("seconds") * 0.1) * (param("metadata.priority") == true ? 2 : 1) * (header("X-Video-Tier") == "fast" ? 3 : 1) * (hour("UTC") == 3 && minute("UTC") == 4 && weekday("UTC") == 0 && month("UTC") == 1 && day("UTC") == 2 ? 5 : 1) * (u("seconds") > 2 ? (param("metadata.discount") == true ? 0.5 : 1) : 1)`
	for _, tc := range []struct {
		name, contentType, tier string
		priority                bool
		wantCost                float64
	}{
		{name: "JSON matched request conditions", contentType: "application/json", tier: "fast", priority: true, wantCost: 6},
		{name: "JSON unmatched request conditions", contentType: "application/json", tier: "standard", wantCost: 1},
		{name: "multipart canonical parameters", contentType: "multipart/form-data; boundary=test", tier: "fast", priority: true, wantCost: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveBillingConfig(t)
			rawExprs, err := common.Marshal(map[string]string{"bill-fallback::declared-model": expression})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(rawExprs)}))
			c, info := newTaskSubmitContext(t, "declared-model", "")
			c.Set("async_media_prepare", true)
			info.UserGroup, info.UsingGroup, info.OriginModelName = "default", "default", "declared-model"
			info.StartTime = time.Date(2000, time.January, 2, 3, 4, 0, 0, time.UTC)
			info.RequestHeaders = map[string]string{"X-Video-Tier": tc.tier, "Authorization": "Bearer usable-client-secret", "Cookie": "session=usable-session-secret"}
			request := map[string]any{
				"metadata": map[string]any{"priority": tc.priority, "discount": true},
				"prompt":   "private-video-prompt", "input_reference": "private-reference-url", "api_key": "private-body-secret",
			}
			c.Set("task_request", request)
			body, err := common.Marshal(request)
			require.NoError(t, err)
			if tc.contentType == "application/json" {
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(string(body)))
			}
			c.Request.Header.Set("Content-Type", tc.contentType)
			pinMappingOrderPlugin(t, c, billingFallbackPlugin+`export function extractUsage(){return {seconds:2};}`)
			submission, taskErr := RelayTaskSubmit(c, info)
			require.Nil(t, taskErr)
			require.NotNil(t, info.TieredBillingSnapshot)
			assert.Equal(t, common.QuotaRound(tc.wantCost*common.QuotaPerUnit), submission.Quota)

			// The scanner round trip is the persisted billing contract. Neither
			// the request payload nor unrelated credential headers belong in it.
			private := model.TaskPrivateData{BillingContext: &model.TaskBillingContext{TieredSnapshot: info.TieredBillingSnapshot}}
			stored, err := private.Value()
			require.NoError(t, err)
			storedJSON, ok := stored.(string)
			require.True(t, ok)
			for _, secret := range []string{"usable-client-secret", "usable-session-secret", "private-video-prompt", "private-reference-url", "private-body-secret"} {
				assert.NotContains(t, storedJSON, secret)
			}
			var persisted model.TaskPrivateData
			require.NoError(t, persisted.Scan(stored))
			publicJSON, err := common.Marshal(&model.Task{PrivateData: persisted})
			require.NoError(t, err)
			assert.NotContains(t, string(publicJSON), "task_request")
			assert.NotContains(t, string(publicJSON), "X-Video-Tier")

			// Completion has no HTTP request. Configuration and the original
			// request may have changed while the task was pending.
			info.RequestHeaders["X-Video-Tier"] = "changed"
			request["metadata"].(map[string]any)["priority"] = !tc.priority
			request["metadata"].(map[string]any)["discount"] = false
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: `{}`}))
			snap := persisted.BillingContext.TieredSnapshot
			before, err := common.Marshal(snap)
			require.NoError(t, err)
			result, usage, err := service.EvaluateTaskCompletionUsage(snap, map[string]any{"seconds": float64(4)})
			require.NoError(t, err)
			// Twice the seconds enters a previously skipped discount branch,
			// while every body, header, and time condition remains frozen.
			assert.Equal(t, submission.Quota, result.ActualQuotaAfterGroup)
			assert.Equal(t, float64(4), usage["seconds"])
			after, err := common.Marshal(snap)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after))

			// Deferred dispatch uses the same acceptance snapshot even when its
			// reconstructed request has different headers and a later start time.
			c.Set("async_media_billing", service.MediaBillingSnapshot{Price: info.PriceData, Tiered: snap})
			info.StartTime = time.Date(2000, time.February, 7, 12, 0, 0, 0, time.UTC)
			replayed, taskErr := RelayTaskSubmit(c, info)
			require.Nil(t, taskErr)
			assert.Equal(t, submission.Quota, replayed.Quota)
		})
	}
}

func TestTaskConditionalPricingRejectsUnsafeRequestSnapshots(t *testing.T) {
	for _, expression := range []string{
		`tier("seconds", u("seconds") * 0.1) * (header("Authorization") == "Bearer usable-client-secret" ? 2 : 1)`,
		`tier("seconds", u("seconds") * 0.1) * (param(header("X-Price-Field")) == true ? 2 : 1)`,
	} {
		t.Run(expression, func(t *testing.T) {
			saveBillingConfig(t)
			rawExprs, err := common.Marshal(map[string]string{"bill-fallback::declared-model": expression})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: string(rawExprs)}))
			c, info := newTaskSubmitContext(t, "declared-model", "")
			c.Set("async_media_prepare", true)
			info.UserGroup, info.UsingGroup, info.OriginModelName = "default", "default", "declared-model"
			info.RequestHeaders = map[string]string{"Authorization": "Bearer usable-client-secret", "X-Price-Field": "priority"}
			pinMappingOrderPlugin(t, c, billingFallbackPlugin+`export function extractUsage(){return {seconds:2};}`)
			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "model_price_error", taskErr.Code)
			assert.NotContains(t, taskErr.Error.Error(), "usable-client-secret")
			assert.Nil(t, info.TieredBillingSnapshot)
		})
	}
}

func TestTaskRetryDoesNotCarryPreviousPluginExpression(t *testing.T) {
	saveBillingConfig(t)
	previousPrices := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices))
	})
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"declared-model":0.1}`))
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":          `{}`,
		"billing_setting.billing_expr":          `{}`,
		billing_setting.PluginBillingExprOption: `{"bill-fallback::declared-model":"tier(\"seconds\", u(\"seconds\") * 0.5)"}`,
	}))
	c, info := newTaskSubmitContext(t, "declared-model", "")
	c.Set("group", "default")
	c.Set("async_media_prepare", true)
	info.UserGroup, info.UsingGroup, info.OriginModelName = "default", "default", "declared-model"
	source := billingFallbackPlugin + `export function extractUsage(){return {seconds:2};}`
	pinMappingOrderPlugin(t, c, source)
	first, taskErr := RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	require.NotNil(t, info.TieredBillingSnapshot)
	assert.Equal(t, common.QuotaRound(common.QuotaPerUnit), first.Quota)
	frozen := service.MediaBillingSnapshot{Price: info.PriceData, Tiered: info.TieredBillingSnapshot}

	// The next selected plugin has no expression override, so its legacy
	// per-unit price and duration multiplier must both apply.
	pinMappingOrderPlugin(t, c, strings.ReplaceAll(source, "bill-fallback", "legacy-video"))
	retried, taskErr := RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	assert.Nil(t, info.TieredBillingSnapshot)
	assert.Equal(t, common.QuotaRound(0.2*common.QuotaPerUnit), retried.Quota)
	assert.Equal(t, float64(2), info.PriceData.OtherRatios()["seconds"])

	// Persisted media jobs keep their accepted price even if live routing or
	// pricing has changed before execution.
	c.Set("async_media_billing", frozen)
	prepared, taskErr := RelayTaskSubmit(c, info)
	require.Nil(t, taskErr)
	require.NotNil(t, info.TieredBillingSnapshot)
	assert.Equal(t, first.Quota, prepared.Quota)
	assert.Equal(t, frozen.Tiered.ExprString, info.TieredBillingSnapshot.ExprString)
}

func TestMeasuredVideoPrechargeRequiresSecondsContract(t *testing.T) {
	schema := map[string]pluginruntime.UsageFieldSchema{"seconds": {Type: "number", Unit: "second"}}
	snapshot := &billingexpr.BillingSnapshot{
		TaskUsageBilling:         true,
		ExprString:               `tier("base", u("seconds") * 0.05)`,
		UsageFacts:               map[string]any{"seconds": float64(12)},
		EstimatedQuotaAfterGroup: 100,
	}
	require.NoError(t, validateMeasuredVideoPrecharge(schema, snapshot, 100))

	for _, tc := range []struct {
		name   string
		schema map[string]pluginruntime.UsageFieldSchema
		snap   *billingexpr.BillingSnapshot
		quota  int
	}{
		{name: "undeclared seconds", schema: nil, snap: snapshot, quota: 100},
		{name: "wrong unit", schema: map[string]pluginruntime.UsageFieldSchema{"seconds": {Type: "number", Unit: "count"}}, snap: snapshot, quota: 100},
		{name: "missing expression", schema: schema, snap: nil, quota: 100},
		{name: "expression does not meter seconds", schema: schema, snap: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprString: `tier("base", 1)`, UsageFacts: snapshot.UsageFacts}, quota: 100},
		{name: "missing bound", schema: schema, snap: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprString: snapshot.ExprString, UsageFacts: nil}, quota: 100},
		{name: "duration exceeds limit", schema: schema, snap: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprString: snapshot.ExprString, UsageFacts: map[string]any{"seconds": float64(relaycommon.MaxTaskDurationSeconds + 1)}}, quota: 100},
		{name: "reservation below estimate", schema: schema, snap: snapshot, quota: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, validateMeasuredVideoPrecharge(tc.schema, tc.snap, tc.quota))
		})
	}
}

func TestRelayTaskSubmitRejectsMeasuredVideoWithoutSecondsPrice(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unit       string
		expression string
		usage      string
		wantError  bool
	}{
		{name: "valid seconds contract", unit: "second", expression: `tier("base", u("seconds") * 0.05)`, usage: `{seconds:5}`},
		{name: "price ignores seconds", unit: "second", expression: `tier("base", 0.25)`, usage: `{seconds:5}`, wantError: true},
		{name: "wrong declared unit", unit: "count", expression: `tier("base", u("seconds") * 0.05)`, usage: `{seconds:5}`, wantError: true},
		{name: "missing precharge bound", unit: "second", expression: `tier("base", u("seconds") * 0.05)`, usage: `{}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveBillingConfig(t)
			expressions, err := common.Marshal(map[string]string{"declared-model": tc.expression})
			require.NoError(t, err)
			modes, err := common.Marshal(map[string]string{"declared-model": billing_setting.BillingModeTieredExpr})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions),
			}))
			var rules kitdto.UpstreamAsyncConfig
			require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"measured","media_type":"video","models":["declared-model"],"operations":["generate"],"submit":{"task_id_path":"job.id"},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"job.status","status_values":{"succeeded":["done"],"failed":["failed"]},"result_path":"job.output","actual_seconds_path":"job.seconds"}}}]}`, &rules))
			require.NoError(t, rules.Validate())
			c, info := newTaskSubmitContext(t, "declared-model", "")
			c.Set("group", "default")
			c.Set("task_plugin_key", "bill-fallback")
			c.Set("async_media_prepare", true)
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, kitdto.ChannelOtherSettings{UpstreamAsync: &rules})
			info.UserGroup, info.UsingGroup, info.OriginModelName = "default", "default", "declared-model"
			source := strings.Replace(billingFallbackPlugin, `fetchMode:"per_task"`, `fetchMode:"per_task",usageSchema:{seconds:{type:"number",unit:"`+tc.unit+`"}}`, 1)
			source += `export function extractUsage(){return ` + tc.usage + `;}`
			pinMappingOrderPlugin(t, c, source)
			result, taskErr := RelayTaskSubmit(c, info)
			if tc.wantError {
				require.NotNil(t, taskErr)
				assert.Equal(t, "model_price_error", taskErr.Code)
				assert.Nil(t, result)
				assert.Nil(t, info.Billing)
				return
			}
			require.Nil(t, taskErr)
			require.NotNil(t, result)
			assert.Positive(t, result.Quota)
			assert.Nil(t, info.Billing, "prepare must not precharge")
		})
	}
}
