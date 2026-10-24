package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestMixedImageBillingUsesPublicAlias(t *testing.T) {
	previousDB, previousCount, previousQuotaUnit := model.DB, constant.CountToken, common.QuotaPerUnit
	previousPrices := ratio_setting.ModelPrice2JSONString()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "mixed-image-pricing.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.QuotaPerUnit = db, false, 500000
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.QuotaPerUnit = previousDB, previousCount, previousQuotaUnit
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices))
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "mixed-price-user", Group: "default", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	prices := ratio_setting.GetModelPriceCopy()
	prices["mixed-image-alias"], prices["mixed-vendor-model"] = 0.04, 99
	encoded, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
	request := service.AsyncImageRequest{Platform: "openai", Dialect: "async", Model: "mixed-image-alias", Count: 2, Prompt: "draw", Size: "1024x1024", Native: map[string]common.RawMessage{}, MixedRouting: &service.ImageMixedRouting{Group: "default"}}
	for _, channelType := range []int{constant.ChannelTypeVolcEngine, constant.ChannelTypeNewAPI, constant.ChannelTypeAli, constant.ChannelTypeOpenAI} {
		t.Run(strconv.Itoa(channelType), func(t *testing.T) {
			mapping := `{"mixed-image-alias":"mixed-vendor-model"}`
			selected := &model.Channel{Type: channelType, Key: "local-test-only", ModelMapping: &mapping}
			billing, err := FreezeAsyncImageBilling(t.Context(), model.Token{UserId: user.Id}, request, "default", selected)
			require.NoError(t, err)
			assert.Equal(t, 0.04, billing.Price.ModelPrice)
			assert.Equal(t, 40000, billing.Price.QuotaToPreConsume)
			assert.Equal(t, float64(2), billing.Price.OtherRatios()["n"])
		})
	}
	request.Model = "unpriced-mixed-alias"
	mapping := `{"unpriced-mixed-alias":"mixed-vendor-model"}`
	_, err = FreezeAsyncImageBilling(t.Context(), model.Token{UserId: user.Id}, request, "default", &model.Channel{Type: constant.ChannelTypeNewAPI, ModelMapping: &mapping})
	require.ErrorContains(t, err, "requires an explicit billing configuration")
}

func TestMixedImageBillingExecutionAndRecovery(t *testing.T) {
	previousDB, previousCount, previousRedis, previousQuotaUnit := model.DB, constant.CountToken, common.RedisEnabled, common.QuotaPerUnit
	previousType := common.MainDatabaseType()
	previousPrices := ratio_setting.ModelPrice2JSONString()
	billingSettings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previousModes, previousExpressions := maps.Clone(billingSettings.BillingMode), maps.Clone(billingSettings.BillingExpr)
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "mixed-execution-billing.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled, common.QuotaPerUnit = db, false, false, 500000
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}, &model.ImageInputAlias{}, &model.UserSubscription{}, &model.Token{}, &model.User{}))
		model.DB, constant.CountToken, common.RedisEnabled, common.QuotaPerUnit = previousDB, previousCount, previousRedis, previousQuotaUnit
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices))
		billingSettings.BillingMode, billingSettings.BillingExpr = previousModes, previousExpressions
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "mixed-billing:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "mixed-billing")
	prices := ratio_setting.GetModelPriceCopy()
	prices["mixed-billing-image"], prices["z-image-mixed-billing"] = 0.04, 0.04
	encoded, err := common.Marshal(prices)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(encoded)))
	billingSettings.BillingMode["mixed-expression-image"] = billing_setting.BillingModeTieredExpr
	billingSettings.BillingExpr["mixed-expression-image"] = `tier("image", fixed(0.03)) * image_count`
	service.InitHttpClient()
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	for _, tc := range []struct {
		name, alias string
		mixed       bool
		reserved    int
		actual      int
	}{
		{name: "mixed_unit_price", alias: "mixed-billing-image", mixed: true, reserved: 40000, actual: 20000},
		{name: "mixed_expression", alias: "mixed-expression-image", mixed: true, reserved: 30000, actual: 15000},
		{name: "legacy_ali_surcharge", alias: "mixed-billing-image", reserved: 80000, actual: 40000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: tc.name, AffCode: tc.name[:min(len(tc.name), 32)], Group: "default", Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&token).Error)
			received := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				received <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"output":{"results":[{"b64_image":%q}]},"usage":{"image_count":1}}`, base64.StdEncoding.EncodeToString(bitmap.Bytes()))
			}))
			defer upstream.Close()
			mapping, err := common.Marshal(map[string]string{tc.alias: "z-image-mixed-billing"})
			require.NoError(t, err)
			channel := &model.Channel{Id: 45, Type: constant.ChannelTypeAli, Key: "local-mixed-billing", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, ModelMapping: common.GetPointer(string(mapping)), ParamOverride: common.GetPointer(`{"operations":[{"path":"parameters.prompt_extend","mode":"set","value":true}]}`)}
			request, err := service.ParseAsyncImageRequest([]byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","n":2,"size":"1024x1024","parameters":{"prompt_extend":false}}`, tc.alias)), "application/json", "/v1/images/generations_async", service.DefaultImageRuntimeConfig())
			require.NoError(t, err)
			if tc.mixed {
				request.MixedRouting = &service.ImageMixedRouting{Group: "default"}
			}
			billing, err := FreezeAsyncImageBilling(t.Context(), token, request, "default", channel)
			require.NoError(t, err)
			request.Billing = &billing
			task := model.AsyncImageTask{TaskId: tc.name, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			channelPayload, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelPayload, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				reservation, err := model.GetAsyncImageReservation(t.Context(), task)
				require.NoError(t, err)
				require.NotNil(t, reservation)
				assert.Equal(t, tc.reserved, reservation.ReservedQuota, "reserve the complete public bill before sending")
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.Images, 1)
			assert.Equal(t, tc.actual, bill.Quota)
			body := <-received
			var sent struct {
				Parameters struct {
					N            int  `json:"n"`
					PromptExtend bool `json:"prompt_extend"`
				} `json:"parameters"`
			}
			require.NoError(t, common.Unmarshal(body, &sent))
			assert.Equal(t, 2, sent.Parameters.N)
			assert.True(t, sent.Parameters.PromptExtend, "the channel override is sent without changing the mixed price")
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			payload, err := service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			var recovered service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(payload, &recovered))
			require.NotNil(t, recovered.Completion)
			if tc.mixed {
				require.NotNil(t, recovered.MixedRouting)
			}
			_, recoveredBill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return nil })
			require.NoError(t, err)
			assert.Equal(t, bill.Quota, recoveredBill.Quota)
			assert.Equal(t, 1, dispatches)
			assert.Empty(t, received, "completion recovery must not submit again")
			var log model.Log
			require.NoError(t, common.UnmarshalJsonStr(recoveredBill.LogPayload, &log))
			assert.Equal(t, tc.alias, log.ModelName)
		})
	}
	for _, tc := range []struct {
		name, upstream, wantError string
		channelType               int
		mask, tokenPrice          bool
		passThrough               bool
	}{
		{name: "ali_token_not_dispatched", upstream: "z-image", channelType: constant.ChannelTypeAli, tokenPrice: true, wantError: "token usage"},
		{name: "ali_mask_not_dispatched", upstream: "qwen-image-edit-plus", channelType: constant.ChannelTypeAli, mask: true, wantError: "mask"},
		{name: "ali_edit_preserves_size", upstream: "qwen-image-edit-plus", channelType: constant.ChannelTypeAli},
		{name: "openai_mask_preserved", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, mask: true},
		{name: "openai_multipart_passthrough", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, mask: true, passThrough: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: tc.name, AffCode: tc.name[:min(len(tc.name), 32)], Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&token).Error)
			received := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body []byte
				var readErr error
				if tc.passThrough {
					if !assert.NoError(t, r.ParseMultipartForm(1<<20)) {
						return
					}
					defer r.MultipartForm.RemoveAll()
					assert.Len(t, r.MultipartForm.File["mask"], 1)
					assert.Len(t, r.MultipartForm.File["image[]"], 1)
					body, readErr = common.Marshal(map[string]any{"model": r.FormValue("model"), "size": r.FormValue("size"), "mask": len(r.MultipartForm.File["mask"])})
				} else {
					body, readErr = io.ReadAll(r.Body)
				}
				assert.NoError(t, readErr)
				received <- body
				w.Header().Set("Content-Type", "application/json")
				if tc.channelType == constant.ChannelTypeAli {
					_, _ = fmt.Fprintf(w, `{"output":{"results":[{"b64_image":%q}]},"usage":{"image_count":1}}`, base64.StdEncoding.EncodeToString(bitmap.Bytes()))
				} else {
					_, _ = fmt.Fprintf(w, `{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(bitmap.Bytes()))
				}
			}))
			defer upstream.Close()
			channel := &model.Channel{Id: 51, Type: tc.channelType, Key: "local", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, ModelMapping: common.GetPointer(fmt.Sprintf(`{"mixed-billing-image":%q}`, tc.upstream))}
			channel.SetSetting(dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough})
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bitmap.Bytes())
			body := map[string]any{"model": "mixed-billing-image", "prompt": "draw", "n": 1, "size": "1024x1024", "images": []map[string]string{{"image_url": dataURL}}}
			if tc.mask {
				body["mask"] = dataURL
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			request, err := service.ParseAsyncImageRequest(encoded, "application/json", "/v1/images/edits_async", service.DefaultImageRuntimeConfig())
			require.NoError(t, err)
			if tc.passThrough {
				request.InputFormat = "multipart"
			}
			request.MixedRouting = &service.ImageMixedRouting{Group: "default"}
			billing, err := FreezeAsyncImageBilling(t.Context(), token, request, "default", channel)
			require.NoError(t, err)
			if tc.tokenPrice {
				billing.Price.UsePrice = false
				billing.Tiered = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: `tier("tokens", p * 2 + c * 8)`, GroupRatio: 1}
			}
			request.Billing = &billing
			task := model.AsyncImageTask{TaskId: tc.name, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			channelBytes, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelBytes, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id) })
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				reservation, reserveErr := model.GetAsyncImageReservation(t.Context(), task)
				require.NoError(t, reserveErr)
				assert.Nil(t, reservation, "incompatible candidates must not reserve funds")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, output)
			assert.Equal(t, 20000, bill.Quota)
			assert.Equal(t, 1, dispatches)
			var sent map[string]any
			require.NoError(t, common.Unmarshal(<-received, &sent))
			assert.Equal(t, tc.upstream, sent["model"], "the recorded upstream model must match JSON and multipart dispatch")
			if tc.channelType == constant.ChannelTypeAli {
				assert.Equal(t, "1024*1024", sent["parameters"].(map[string]any)["size"])
				assert.NotNil(t, sent["input"])
			} else {
				assert.NotNil(t, sent["mask"])
				assert.Equal(t, "1024x1024", sent["size"])
			}
		})
	}
	for _, tc := range []struct {
		name, body, expression string
		mixed, historical      bool
		quota                  int
		missing                bool
		canonicalInput         bool
	}{
		{name: "required_usage_missing", body: `{"input":10}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, missing: true},
		{name: "unused_usage_missing", body: `{"output":20}`, expression: `tier("tokens", c * 8)`, mixed: true, quota: 80},
		{name: "real_mapped_usage", body: `{"input":10,"output":20}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, quota: 90},
		{name: "missing_canonical_uses_alias", body: `{"input":10,"output":20}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, canonicalInput: true, quota: 90},
		{name: "explicit_canonical_zero_preserved", body: `{"input":10,"prompt":0,"output":20}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, canonicalInput: true, quota: 80},
		{name: "missing_alias_uses_canonical", body: `{"prompt":10,"output":20}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, canonicalInput: true, quota: 90},
		{name: "conflicting_alias_keeps_canonical", body: `{"input":20,"prompt":10,"output":20}`, expression: `tier("tokens", p * 2 + c * 8)`, mixed: true, canonicalInput: true, quota: 90},
		{name: "fixed_provider_partial_usage", body: `{"input":10}`, expression: `tier("tokens", p * 2 + c * 8)`, quota: 10},
		{name: "historical_completion_without_presence", body: `{"output":20}`, expression: `tier("tokens", c * 8)`, mixed: true, historical: true, quota: 80},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: tc.name, AffCode: tc.name[:min(len(tc.name), 32)], Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&token).Error)
			downloads := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				downloads <- r.URL.Path
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(bitmap.Bytes())
			}))
			defer upstream.Close()
			profile := service.TianyueAsyncImageProfile("frozen-upstream")
			profile.Poll.Response.UsagePaths = map[string]string{"input_tokens": "usage.input", "output_tokens": "usage.output", "input_tokens_details.cached_tokens": "usage.cache"}
			if tc.canonicalInput {
				profile.Poll.Response.UsagePaths["prompt_tokens"] = "usage.prompt"
			}
			parsed, err := service.ParseUpstreamAsyncPollResponse(profile, []byte(fmt.Sprintf(`{"status":"SUCCESS","url":%q,"usage":%s}`, upstream.URL+"/image.png", tc.body)))
			require.NoError(t, err)
			billing := service.MediaBillingSnapshot{Price: types.PriceData{QuotaToPreConsume: 1000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}, Tiered: &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, ExprVersion: 1}, ImageQuotaBeforeGroup: 1000}
			completion := service.AsyncImageCompletion{EvaluatedAt: time.Now(), Body: []byte(`{"data":[]}`), Usage: parsed.Usage, UsageFields: parsed.UsageFields, Profile: profile, URLs: parsed.URLs, Billing: billing, BillingBody: []byte(`{"n":1}`), UpstreamModel: "frozen-upstream", BillingModel: "frozen-alias"}
			if tc.historical {
				completion.UsageFields = nil
			}
			request := service.AsyncImageRequest{Platform: "openai", Model: "frozen-alias", Count: 1, Kind: "text_to_image", Billing: &billing}
			if tc.mixed {
				request.MixedRouting = &service.ImageMixedRouting{Group: "default"}
			}
			task := model.AsyncImageTask{TaskId: tc.name, UserId: user.Id, TokenId: token.Id, ChannelId: 51, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			task.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":51,"key":"local"}`), "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), task, 1000, model.ImageFundingSelection{Source: "wallet"}))
			require.NoError(t, service.PersistAsyncImageCompletion(t.Context(), &task, request, completion))
			payload, err := service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			var recovered service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(payload, &recovered))
			require.NotNil(t, recovered.Completion)
			assert.Equal(t, completion.UsageFields, recovered.Completion.UsageFields)
			channel := &model.Channel{Id: 51, Type: constant.ChannelTypeAli, Key: "local", BaseURL: &upstream.URL, Status: common.ChannelStatusManuallyDisabled}
			dispatches := 0
			_, bill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return nil })
			assert.Zero(t, dispatches, "accepted completion recovery never submits again")
			if tc.missing {
				require.ErrorContains(t, err, "token usage")
				assert.Empty(t, downloads)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.quota, bill.Quota)
				assert.Equal(t, "/image.png", <-downloads)
			}
			reservation, reserveErr := model.GetAsyncImageReservation(t.Context(), task)
			require.NoError(t, reserveErr)
			require.NotNil(t, reservation)
			assert.Equal(t, "reserved", reservation.Status)
			assert.Equal(t, 1000, reservation.ReservedQuota)
		})
	}

	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("token_ratio_mixed_%t", mixed), func(t *testing.T) {
			info := &relaycommon.RelayInfo{ImageMixedBilling: mixed, ImageQuotaBeforeGroup: 1000, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAli, UpstreamModelName: "z-image"}, PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, service.EstimateImageBillingForRequest(info, 3, true))
			want, n, extension := 6000, 3.0, 2.0
			if mixed {
				want, n, extension = 1000, 1, 1
			}
			assert.Equal(t, want, info.PriceData.QuotaToPreConsume)
			assert.Equal(t, n, info.PriceData.OtherRatios()["n"])
			assert.Equal(t, extension, info.PriceData.OtherRatios()["prompt_extend"])
		})
	}
	// Quantity limits apply before either legacy or frozen expression arithmetic.
	for _, expression := range []bool{false, true} {
		info := &relaycommon.RelayInfo{ImageMixedBilling: true}
		if expression {
			info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: billing_setting.BillingModeTieredExpr}
		}
		require.NotNil(t, service.EstimateImageBillingForRequest(info, dto.MaxImageN+1, true))
	}
}

func TestMixedImageChannelValidatesNativeLimitsBeforeSelection(t *testing.T) {
	var referencePNG bytes.Buffer
	require.NoError(t, png.Encode(&referencePNG, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	dataReference := "data:image/png;base64," + base64.StdEncoding.EncodeToString(referencePNG.Bytes())
	previousAddress := system_setting.ServerAddress
	system_setting.ServerAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.ServerAddress = previousAddress })
	for _, tc := range []struct {
		name, upstream    string
		referenceURL      string
		count, references int
		channelType       int
		override          string
		native            map[string]common.RawMessage
		wantError         string
		undeclaredModel   bool
		tokenPrice        bool
		mask              bool
		passThrough       bool
		tianyue           bool
		usagePaths        map[string]string
		customProfile     bool
	}{
		{name: "pro_single", upstream: "doubao-seedream-5-0-pro-260628", count: 1},
		{name: "ark_explicit_nonstreaming", upstream: "doubao-seedream-5-0-pro-260628", count: 1, native: map[string]common.RawMessage{"stream": common.RawMessage(`false`)}},
		{name: "newapi_explicit_nonstreaming", upstream: "image-model", channelType: constant.ChannelTypeNewAPI, count: 1, native: map[string]common.RawMessage{"stream": common.RawMessage(`false`)}},
		{name: "pro_group_rejected", upstream: "doubao-seedream-5-0-pro-260628", count: 2, wantError: "between 1 and 1"},
		{name: "lite_total_limit", upstream: "doubao-seedream-5-0-260128", count: 2, references: 14, wantError: "15"},
		{name: "unsupported_quality", upstream: "doubao-seedream-5-0-pro-260628", count: 1, native: map[string]common.RawMessage{"quality": common.RawMessage(`"hd"`)}, wantError: "parameter quality"},
		{name: "openai_public_parameters", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 2, native: map[string]common.RawMessage{"quality": common.RawMessage(`"high"`)}},
		{name: "openai_rejects_ali_container", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 2, native: map[string]common.RawMessage{"parameters": common.RawMessage(`{"n":2}`)}, wantError: "parameter parameters"},
		{name: "ali_native_parameters", upstream: "z-image", channelType: constant.ChannelTypeAli, count: 2, native: map[string]common.RawMessage{"parameters": common.RawMessage(`{"n":2,"prompt_extend":true}`)}},
		{name: "ali_native_input", upstream: "z-image", channelType: constant.ChannelTypeAli, count: 2, native: map[string]common.RawMessage{"input": common.RawMessage(`{"prompt":"draw"}`)}},
		{name: "ali_token_price_rejected", upstream: "z-image", channelType: constant.ChannelTypeAli, count: 1, tokenPrice: true, wantError: "token usage"},
		{name: "openai_token_price_supported", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, tokenPrice: true},
		{name: "ali_mask_rejected", upstream: "qwen-image-edit-plus", channelType: constant.ChannelTypeAli, count: 1, references: 1, mask: true, wantError: "mask"},
		{name: "openai_mask_supported", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, references: 1, mask: true},
		{name: "openai_empty_mask_override_rejected", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, references: 1, mask: true, override: `{"operations":[{"path":"mask","mode":"set","value":""}]}`, wantError: "mask"},
		{name: "openai_passthrough_skips_override", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, passThrough: true, override: `{"operations":[{"path":"n","mode":"set","value":2}]}`},
		{name: "tianyue_mask_rejected", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, references: 1, mask: true, tianyue: true, wantError: "mask"},
		{name: "tianyue_uploaded_local_reference_supported", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, references: 1, referenceURL: dataReference, tianyue: true},
		{name: "tianyue_gateway_reference_supported", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, references: 1, referenceURL: "https://gateway.example/api/image-objects/imo_owned/content", tianyue: true},
		{name: "tianyue_local_reference_batch_rejected", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 2, references: 1, referenceURL: dataReference, tianyue: true, wantError: "require n=1"},
		{name: "tianyue_local_reference_mask_rejected", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, references: 1, referenceURL: dataReference, mask: true, tianyue: true, wantError: "mask"},
		{name: "tianyue_private_external_reference_rejected", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, references: 1, referenceURL: "https://127.0.0.1/reference.png", tianyue: true, wantError: "address is not public"},
		{name: "tianyue_builtin_token_usage_rejected", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, tokenPrice: true, tianyue: true, wantError: "token usage"},
		{name: "custom_profile_without_usage_rejected", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, tokenPrice: true, customProfile: true, wantError: "token usage"},
		{name: "custom_profile_total_only_rejected", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, tokenPrice: true, customProfile: true, usagePaths: map[string]string{"total_tokens": "usage.total"}, wantError: "token usage"},
		{name: "custom_profile_input_only_rejected", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, tokenPrice: true, customProfile: true, usagePaths: map[string]string{"input_tokens": "usage.input"}, wantError: "token usage"},
		{name: "custom_profile_real_usage_supported", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, tokenPrice: true, customProfile: true, usagePaths: map[string]string{"input_tokens": "usage.input", "output_tokens": "usage.output"}},
		{name: "custom_profile_fixed_price_without_usage_supported", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, customProfile: true},
		{name: "tianyue_explicit_profile_usage_precedes_builtin", upstream: "seedream-5-pro-1K", channelType: constant.ChannelTypeNewAPI, count: 1, tokenPrice: true, customProfile: true, tianyue: true, usagePaths: map[string]string{"prompt_tokens": "usage.input", "completion_tokens": "usage.output"}},
		{name: "siliconflow_passthrough_requires_conversion", upstream: "Qwen/Qwen-Image", channelType: constant.ChannelTypeSiliconFlow, count: 1, passThrough: true, wantError: "pass-through"},
		{name: "ali_passthrough_requires_native_body", upstream: "z-image", channelType: constant.ChannelTypeAli, count: 1, passThrough: true, wantError: "pass-through"},
		{name: "ali_quantity_conflict", upstream: "z-image", channelType: constant.ChannelTypeAli, count: 2, native: map[string]common.RawMessage{"parameters": common.RawMessage(`{"n":3}`)}, wantError: "count conflicts"},
		{name: "xai_output_limit", upstream: "grok-imagine-image", channelType: constant.ChannelTypeXai, count: 11, wantError: "10 output images"},
		{name: "xai_reference_limit", upstream: "grok-imagine-image", channelType: constant.ChannelTypeXai, count: 1, references: 4, wantError: "3 reference images"},
		{name: "imagen_native_count", upstream: "imagen-4.0-generate-001", channelType: constant.ChannelTypeGemini, count: 2},
		{name: "imagen_override_count", upstream: "imagen-4.0-generate-001", channelType: constant.ChannelTypeGemini, count: 2, override: `{"operations":[{"path":"parameters.sampleCount","mode":"set","value":3}]}`, wantError: "sampleCount conflicts"},
		{name: "jimeng_single", upstream: "jimeng_high_aes_general_v21_L", channelType: constant.ChannelTypeJimeng, count: 1},
		{name: "jimeng_cannot_represent_batch", upstream: "jimeng_high_aes_general_v21_L", channelType: constant.ChannelTypeJimeng, count: 2, wantError: "batch count"},
		{name: "undeclared_parameter", upstream: "gpt-image-1", channelType: constant.ChannelTypeOpenAI, count: 1, native: map[string]common.RawMessage{"undeclared_option": common.RawMessage(`true`)}, wantError: "parameter undeclared_option"},
		{name: "undeclared_ali_model", upstream: "undeclared-image-model", channelType: constant.ChannelTypeAli, count: 1, undeclaredModel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapping, err := common.Marshal(map[string]string{"public-alias": tc.upstream})
			require.NoError(t, err)
			channelType := tc.channelType
			if channelType == 0 {
				channelType = constant.ChannelTypeVolcEngine
			}
			ch := model.Channel{Type: channelType, ModelMapping: common.GetPointer(string(mapping))}
			if tc.override != "" {
				ch.ParamOverride = &tc.override
			}
			ch.SetSetting(dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough, TianyueMediaCompatibility: tc.tianyue})
			if tc.customProfile {
				profile := service.TianyueAsyncImageProfile(tc.upstream)
				profile.Poll.Response.UsagePaths = tc.usagePaths
				ch.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &dto.UpstreamAsyncConfig{Profiles: []dto.UpstreamAsyncProfile{*profile}}})
			}
			request := service.AsyncImageRequest{Model: "public-alias", Count: tc.count, Prompt: "draw", Size: "1024x1024", Native: tc.native}
			if tc.tokenPrice {
				request.Billing = &service.MediaBillingSnapshot{Price: types.PriceData{QuotaToPreConsume: 1000}, Tiered: &billingexpr.BillingSnapshot{BillingMode: billing_setting.BillingModeTieredExpr, ExprString: `tier("tokens", p * 2 + c * 8)`}}
			}
			for i := range tc.references {
				reference := fmt.Sprintf("https://example.com/%d.png", i)
				if tc.referenceURL != "" {
					reference = tc.referenceURL
					request.Kind, request.Dialect = "image_to_image", "async"
				}
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "image_url", URL: reference})
			}
			if tc.mask {
				request.Kind, request.Dialect = "image_to_image", "async"
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "mask", URL: "https://example.com/mask.png"})
			}
			if tc.undeclaredModel {
				assert.False(t, service.MixedImageChannelRequestSupported(ch, request), "an adaptor must declare the mapped model before mixed admission")
				return
			}
			originalParts := append([]service.AsyncImageInputPart(nil), request.Parts...)
			err = validateMixedImageChannelRequest(ch, request)
			assert.Equal(t, originalParts, request.Parts, "admission must not replace the persisted reference input")
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMixedImageFinalNativeCountGuard(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		count      int
		wantError  string
	}{
		{name: "imagen_count", body: `{"parameters":{"sampleCount":2}}`, count: 2},
		{name: "imagen_override", body: `{"parameters":{"sampleCount":3}}`, count: 2, wantError: "sampleCount conflicts"},
		{name: "unsigned_overflow", body: `{"parameters":{"sampleCount":18446744073709551615}}`, count: 2, wantError: "sampleCount conflicts"},
		{name: "missing_batch", body: `{"prompt":"draw"}`, count: 2, wantError: "batch count"},
		{name: "single_without_quantity", body: `{"prompt":"draw"}`, count: 1},
		{name: "unbounded_expected", body: `{"n":129}`, count: 129, wantError: "outside the allowed range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			// Match the replayable JSON bodies prepared by the relay.
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.body)), nil }
			err := service.ValidateMixedImageSubmitCount(request, tc.count)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
	t.Run("imagen_override_stops_before_dispatch", func(t *testing.T) {
		previousPassThrough := model_setting.GetGlobalSettings().PassThroughRequestEnabled
		model_setting.GetGlobalSettings().PassThroughRequestEnabled = false
		t.Cleanup(func() { model_setting.GetGlobalSettings().PassThroughRequestEnabled = previousPassThrough })
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"imagen-4.0-generate-001","prompt":"draw","n":2}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("async_image_execution", true)
		common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeGemini)
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "imagen-4.0-generate-001")
		var overrides map[string]any
		require.NoError(t, common.UnmarshalJsonStr(`{"operations":[{"path":"parameters.sampleCount","mode":"set","value":3}]}`, &overrides))
		common.SetContextKey(c, constant.ContextKeyChannelParamOverride, overrides)
		request := &dto.ImageRequest{Model: "imagen-4.0-generate-001", Prompt: "draw", N: common.GetPointer(uint(2)), Size: "1024x1024"}
		info := &relaycommon.RelayInfo{OriginModelName: request.Model, Request: request, RelayMode: relayconstant.RelayModeImagesGenerations, ImageMixedBilling: true, PriceData: types.PriceData{UsePrice: true, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
		dispatches := 0
		_, apiErr := ExecuteImageRequest(c, info, false, func() error { dispatches++; return errors.New("unexpected dispatch") })
		require.NotNil(t, apiErr)
		assert.Contains(t, apiErr.Error(), "sampleCount conflicts")
		assert.Zero(t, dispatches, "reject native quantity changes before reserving or sending")
	})
	// Corrupt configuration must fail locally without invoking repair-on-read getters.
	for _, field := range []string{"setting", "other_settings", "param_override"} {
		t.Run(field, func(t *testing.T) {
			previousDB := model.DB
			model.DB = nil
			t.Cleanup(func() { model.DB = previousDB })
			invalid := "{broken"
			channel := model.Channel{Type: constant.ChannelTypeNewAPI}
			switch field {
			case "setting":
				channel.Setting = &invalid
			case "other_settings":
				channel.OtherSettings = invalid
			case "param_override":
				channel.ParamOverride = &invalid
			}
			err := validateMixedImageChannelRequest(channel, service.AsyncImageRequest{Model: "gpt-image-1", Count: 1})
			require.ErrorContains(t, err, "invalid image channel")
			assert.Equal(t, "{broken", invalid)
		})
	}
}

func TestAsyncImageUnreadableReceiptDoesNotRepeatSubmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limit     int64
		readError bool
	}{
		{name: "truncated_json", body: `{"id":"accepted-job"`, limit: 1024},
		{name: "non_json", body: `<html>gateway error</html>`, limit: 1024},
		{name: "oversized_receipt", body: `{"id":"accepted-job"}`, limit: 4},
		{name: "read_failure", limit: 1024, readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, upstreamID := range []string{"", "accepted-job"} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("async_image_resume_task", upstreamID)
				c.Set("async_image_response_limit", tc.limit)
				var reader io.Reader = strings.NewReader(tc.body)
				if tc.readError {
					reader = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				response := &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(reader)}
				_, apiErr := handleUpstreamAsyncImageResponse(c, &relaycommon.RelayInfo{}, response, &dto.UpstreamAsyncProfile{})
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, apiErr, &failure)
				if upstreamID == "" {
					assert.Equal(t, 608, failure.Code)
					assert.Equal(t, "execution_unknown", failure.InternalCode)
					assert.True(t, failure.ExecutionUnknown, "an unreadable accepted submission must never be retried")
				} else {
					assert.Equal(t, 606, failure.Code)
					assert.False(t, failure.ExecutionUnknown, "known upstream jobs can safely retry polling")
				}
			}
		})
	}
}

func TestAsyncImageTerminalFailureClassification(t *testing.T) {
	var profile dto.UpstreamAsyncProfile
	require.NoError(t, common.UnmarshalJsonStr(`{"poll":{"response":{"status_path":"status","status_values":{"failed":["failed"]},"failure_reason_path":"reason","result_path":"urls"}}}`, &profile))
	for _, tc := range []struct {
		name, reason string
		code         int
	}{
		{name: "service_timeout", reason: "Service timeout, please try again later", code: 606},
		{name: "policy_precedes_timeout", reason: "图片审核失败，生成超时，请重试", code: 601},
		{name: "prompt_length_limit", reason: "POST /: Your prompt cannot exceed 10000 characters.", code: 604},
		{name: "busy_ascii_comma", reason: "服务繁忙,请稍后重试", code: 606},
		{name: "busy_fullwidth_comma", reason: "服务繁忙，请稍后重试", code: 606},
		{name: "asset_busy", reason: "参考image上传失败：POST / 上游服务繁忙：asset name service busy, please try again later", code: 606},
		{name: "internal_error", reason: "Internal error encountered.", code: 606},
		{name: "oversize_before_busy", reason: "参考image上传失败：参考图过大（25.4MB，上限 20MB）；服务繁忙,请稍后重试", code: 604},
		{name: "guardrail_before_busy", reason: "guardrails detection failed: 服务繁忙,请稍后重试", code: 601},
		{name: "invalid_argument_remains_conservative", reason: "AI Studio ProxyUnaryCall 返回 HTTP 400、协议错误码 3: Request contains an invalid argument.", code: 610},
		{name: "unclassified_failure", reason: "provider-specific rejection", code: 610},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("async_image_resume_task", "accepted-job")
			c.Set("async_image_response_limit", int64(1024))
			body, err := common.Marshal(map[string]string{"status": "failed", "reason": tc.reason})
			require.NoError(t, err)
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
			_, apiErr := handleUpstreamAsyncImageResponse(c, &relaycommon.RelayInfo{}, response, &profile)
			var failure *service.AsyncImageFailure
			require.ErrorAs(t, apiErr, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.Equal(t, tc.reason, failure.Message)
			assert.Equal(t, "upstream_job_failed", failure.InternalCode)
			assert.True(t, failure.UpstreamTerminal)
			assert.False(t, failure.ExecutionUnknown)
		})
	}
}

func TestAsyncImageFailureDiagnosticsAndTransportBarrier(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
		status, code        int
	}{
		{name: "nested_detail", body: `{"detail":[{"loc":["body","references",0,"source"],"msg":"reference image size exceeds 20MB","input":"private-prompt"}],"prompt":"private-prompt","request_id":"diagnostic-id"}`, message: "/body/references/0/source: reference image size exceeds 20MB", status: 400, code: 604},
		{name: "encoded_json_error", body: `{"error":"{\"message\":\"服务繁忙,请稍后重试\",\"input\":\"private-prompt\"}"}`, message: "服务繁忙,请稍后重试", status: 400, code: 606},
		{name: "malformed_json_error", body: `{"error":"{\"message\":\"broken\",\"input\":\"private-prompt\""}`, message: "Upstream image request failed", status: 400, code: 610},
		{name: "no_diagnostic_fields", body: `{"input":{"request_id":"private-prompt"},"prompt":"private-prompt"}`, message: "Upstream image request failed", status: 400, code: 610},
		{name: "explicit_busy_rejection", body: `{"error":{"message":"服务繁忙，请稍后重试"}}`, message: "服务繁忙，请稍后重试", status: 400, code: 606},
		{name: "authorization_not_busy_contract", body: `{"error":{"message":"服务繁忙，请稍后重试"}}`, message: "服务繁忙，请稍后重试", status: 403, code: 610},
		{name: "limit_not_busy", body: `{"error":{"message":"参考image上传失败：参考图过大（25.4MB，上限 20MB）；服务繁忙,请稍后重试"}}`, message: "25.4MB，上限 20MB", status: 400, code: 604},
		{name: "refusal_not_busy", body: `{"error":{"message":"upstream refusal: 服务繁忙,请稍后重试"}}`, message: "upstream refusal", status: 400, code: 601},
		{name: "generic_invalid_argument", body: `{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`, message: "Request contains an invalid argument.", status: 400, code: 610},
		{name: "third_party_unavailable_is_not_review", body: `{"error":{"message":"third-party service unavailable"}}`, message: "third-party service unavailable", status: 503, code: 606},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"X-Request-Id": []string{"header-diagnostic-id"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			failure := service.ClassifyGeminiAsyncImageHTTPFailure(t.Context(), response)
			assert.Equal(t, tc.code, failure.Code)
			assert.Contains(t, failure.Message, tc.message)
			assert.NotContains(t, failure.Message, "private-prompt")
			assert.NotContains(t, failure.UpstreamRequestID, "private-prompt")
			assert.NotEmpty(t, failure.UpstreamRequestID)
			assert.False(t, failure.ExecutionUnknown)
			assert.False(t, failure.UpstreamTerminal, "an HTTP query error cannot terminate an accepted upstream job")
		})
	}
	assert.Empty(t, service.SanitizeAsyncImageDiagnostic(`{"input":"private-prompt"`, 1024))
	for _, tc := range []struct {
		name, internal string
		err            error
		response       bool
	}{
		{name: "deadline", internal: "upstream_transport_timeout", err: &url.Error{Op: "Post", URL: "https://signed.example/image?token=private-token", Err: context.DeadlineExceeded}},
		{name: "disconnect", internal: "upstream_transport_disconnected", err: io.ErrUnexpectedEOF},
		{name: "canceled", internal: "upstream_transport_canceled", err: context.Canceled},
		{name: "parse", internal: "upstream_response_invalid", err: errors.New(`bad response contains private-prompt`), response: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, jobID := range []string{"", "accepted-job"} {
				failure := service.AsyncImageTransportFailure(tc.err, model.AsyncImageTask{UpstreamTaskId: jobID}, tc.response, http.Header{"X-Request-Id": []string{"upstream-request-id"}})
				assert.Equal(t, tc.internal, failure.InternalCode)
				assert.Equal(t, "upstream-request-id", failure.UpstreamRequestID)
				assert.NotContains(t, failure.Message, "signed.example")
				assert.NotContains(t, failure.Message, "private-token")
				assert.NotContains(t, failure.Message, "private-prompt")
				assert.Equal(t, jobID == "", failure.ExecutionUnknown)
				if jobID == "" {
					assert.Equal(t, 608, failure.Code)
				} else {
					assert.Equal(t, 606, failure.Code)
					assert.Contains(t, failure.Message, "only the existing upstream job may be polled again")
				}
			}
		})
	}
}

func TestAsyncImageCompletionRecoveryPreservesAcceptedOutputAndPrice(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "completion-recovery.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "completion-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "completion-test")
	user := model.User{Username: "completion-recovery-user", Status: common.UserStatusDisabled}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "completion-recovery-token", ModelLimitsEnabled: true, ModelLimits: "removed-model", UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	require.NoError(t, db.Delete(&token).Error)
	var pngImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	imageData := base64.StdEncoding.EncodeToString(pngImage.Bytes())
	for _, tc := range []struct {
		name, platform    string
		resolutionMapping bool
	}{
		{name: "openai", platform: "openai"},
		{name: "gemini", platform: "gemini"},
		{name: "openai_resolution_mapping", platform: "openai", resolutionMapping: true},
		{name: "gemini_resolution_mapping", platform: "gemini", resolutionMapping: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response any = dto.ImageResponse{Data: []dto.ImageData{{B64Json: imageData}}}
			channelType := constant.ChannelTypeOpenAI
			if tc.platform == "gemini" {
				channelType = constant.ChannelTypeGemini
				response = dto.GeminiChatResponse{Candidates: []dto.GeminiChatCandidate{{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{InlineData: &dto.GeminiInlineData{MimeType: "image/png", Data: imageData}}}}}}}
			}
			body, err := common.Marshal(response)
			require.NoError(t, err)
			completion := &service.AsyncImageCompletion{
				Body: body, Usage: &dto.Usage{}, BillingBody: []byte(`{"n":1}`), UpstreamModel: "frozen-upstream", BillingModel: "frozen-billing",
				Billing: service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
			}
			request := service.AsyncImageRequest{
				Platform: tc.platform, Model: "accepted-image", Count: 1, Kind: "image_to_image", Completion: completion,
				Parts:       []service.AsyncImageInputPart{{Type: "image_url", URL: "https://expired.invalid/reference.png"}},
				Native:      map[string]common.RawMessage{"ignored_after_completion": common.RawMessage(`invalid JSON`)},
				Billing:     &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 999}},
				Reservation: &model.AsyncImageBill{FundingSource: "wallet"},
			}
			invalidMapping := "invalid mapping"
			unreachableURL := "http://127.0.0.1:1"
			channel := &model.Channel{Id: 14, Type: channelType, Key: "frozen-key", BaseURL: &unreachableURL, Status: common.ChannelStatusManuallyDisabled, ModelMapping: &invalidMapping}
			if tc.resolutionMapping {
				channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"accepted-image": {"1K": "changed-1k-upstream", "2K": "changed-2k-upstream"}}})
				completion.BillingModel = request.Model
			}
			task := model.AsyncImageTask{TaskId: "completion-" + tc.name, UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", UpstreamSucceededAt: time.Now().Unix()}
			attemptContext, cancel := context.WithCancel(t.Context())
			cancel()
			dispatches := 0
			output, bill, err := ExecuteAsyncImage(attemptContext, task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				return nil
			})
			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.Images, 1)
			assert.Equal(t, 2, output.Images[0].Width)
			assert.Equal(t, 3, output.Images[0].Height)
			assert.Equal(t, 5000, bill.Quota, "recovery must use the accepted price, even if current pricing changed")
			var log model.Log
			require.NoError(t, common.UnmarshalJsonStr(bill.LogPayload, &log))
			assert.Equal(t, completion.BillingModel, log.ModelName)
			if tc.resolutionMapping {
				assert.Equal(t, request.Model, log.ModelName, "resolution aliases must not replace the public billing model")
			}
			assert.Zero(t, dispatches, "accepted output must never generate or poll again")
		})
	}

	service.InitHttpClient()
	for _, tc := range []struct {
		name, imageData string
		valid           bool
	}{
		{name: "oversized_valid_output", imageData: imageData, valid: true},
		{name: "oversized_invalid_output", imageData: base64.StdEncoding.EncodeToString([]byte("not an image"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := map[string]any{
				"candidates":    []dto.GeminiChatCandidate{{Content: dto.GeminiChatContent{Parts: []dto.GeminiPart{{InlineData: &dto.GeminiInlineData{MimeType: "image/png", Data: tc.imageData}}}}}},
				"usageMetadata": map[string]int{"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2},
				"metadata":      "",
			}
			encoded, err := common.Marshal(response)
			require.NoError(t, err)
			response["metadata"] = strings.Repeat("x", (32<<20)+1-len(encoded))
			encoded, err = common.Marshal(response)
			require.NoError(t, err)
			upstreamRequests := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			defer upstream.Close()
			freshUser := model.User{Username: tc.name, AffCode: tc.name, Status: common.UserStatusEnabled, Quota: 1000000}
			require.NoError(t, db.Create(&freshUser).Error)
			freshToken := model.Token{UserId: freshUser.Id, Key: tc.name, Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
			require.NoError(t, db.Create(&freshToken).Error)
			request := service.AsyncImageRequest{
				Platform: "gemini", Dialect: "async", Model: "gemini-image-test", Resolution: "2K", Prompt: "draw", Count: 1,
				Parts:   []service.AsyncImageInputPart{{Type: "text", Text: "draw"}},
				Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
			}
			channel := &model.Channel{Id: 14, Type: constant.ChannelTypeGemini, Key: "synthetic-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
			channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"gemini-image-test": {"1K": "gemini-image-test-1k", "2K": "gemini-image-test-2k"}}})
			channel.ModelMapping = common.GetPointer(`{"gemini-image-test":"gemini-image-test-2k","gemini-image-test-2k":"gemini-image-test-2k"}`)
			task := model.AsyncImageTask{TaskId: tc.name, UserId: freshUser.Id, TokenId: freshToken.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "oversized-worker", LeaseExpiresAt: time.Now().Unix() + 120}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			task.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+task.TaskId)
			require.NoError(t, err)
			channelBytes, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelBytes, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			if tc.valid {
				require.NoError(t, err)
				require.NotNil(t, output)
				require.Len(t, output.Images, 1)
				assert.Equal(t, 2, output.Images[0].Width)
				assert.Equal(t, 5000, bill.Quota, "large metadata must not prevent a valid current invocation from finishing")
				var log model.Log
				require.NoError(t, common.UnmarshalJsonStr(bill.LogPayload, &log))
				assert.Equal(t, request.Model, log.ModelName, "the resolution-specific upstream name must not change the public billing model")
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				assert.Equal(t, "gemini-image-test-2k", other["upstream_model_name"])
			} else {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.True(t, failure.ExecutionUnknown, "unsaved output cannot authorize a second generation")
				assert.Equal(t, 608, failure.Code)
				assert.Nil(t, output)
			}
			assert.Contains(t, <-upstreamRequests, "/models/gemini-image-test-2k:")
			assert.Empty(t, upstreamRequests)
			var stored model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
			assert.Zero(t, stored.UpstreamSucceededAt)
			assert.Equal(t, task.RequestCipher, stored.RequestCipher, "oversized checkpoints must not overwrite the original request")
		})
	}
}

func TestOpenAIAsyncImageOutboundParameters(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "openai-outbound.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "openai-outbound-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "openai-outbound-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()

	const referenceURL = "https://example.com/reference%20image.png?sig=synthetic%2Fsignature%2Bvalue&part=1"
	const maskURL = "https://example.com/mask.png?sig=synthetic%3Dmask"
	var pngImage, jpegImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	require.NoError(t, jpeg.Encode(&jpegImage, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil))
	for _, tc := range []struct {
		name, path, referenceField string
		providerStatus, requestID  string
		secrets                    []string
		channelReferenceLimit      int64
		localReferenceRejected     bool
		unreadableResponse         bool
		maxReferences              int
		referenceRetryCount        int
		status, failureCode        int
		responseMessage            string
		providerCode               string
		statusMapping              string
		fileFormat                 string
		passThrough                bool
		volcengine                 bool
		xai                        bool
		newapi                     bool
		synchronousRetry           bool
		resolutionMapping          bool
		resolution                 string
		parameterOverride          string
		modelConflict              bool
		templateBody               map[string]any
		expectedTemplate           string
	}{
		{name: "volcengine_edits_json", path: "/v1/images/edits_async", referenceField: "image", volcengine: true},
		{name: "volcengine_edits_json_passthrough", path: "/v1/images/edits_async", referenceField: "image", volcengine: true, passThrough: true},
		{name: "volcengine_edits_uploaded_reference", path: "/v1/images/edits_async", fileFormat: "png", volcengine: true},
		{name: "volcengine_generations_reference", path: "/v1/images/generations_async", referenceField: "image_urls", volcengine: true},
		{name: "resolution_mapping_json", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true},
		{name: "resolution_mapping_json_passthrough", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, passThrough: true},
		{name: "resolution_mapping_2k_json_passthrough", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, resolution: "2K", passThrough: true},
		{name: "resolution_mapping_multipart_passthrough", path: "/v1/images/edits_async", fileFormat: "jpeg", resolutionMapping: true, passThrough: true},
		{name: "resolution_mapping_rejects_model_override", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, parameterOverride: `{"model":"other-model"}`, modelConflict: true},
		{name: "resolution_mapping_rejects_model_deletion", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, parameterOverride: `{"operations":[{"path":"model","mode":"delete"}]}`, modelConflict: true},
		{name: "resolution_mapping_rejects_template_alias", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"model": "{request.model}", "n": "{request.n}"}, modelConflict: true},
		{name: "resolution_mapping_template_model", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"model": "{upstream_model}", "n": "{request.n}"}, expectedTemplate: `{"model":"gpt-image-2","n":1}`},
		{name: "resolution_mapping_template_custom_field", path: "/v1/images/edits_async", referenceField: "images", resolutionMapping: true, templateBody: map[string]any{"provider_model": "{upstream_model}", "n": "{request.n}"}, expectedTemplate: `{"provider_model":"gpt-image-2","n":1}`},
		{name: "xai_normalized_geometry", path: "/v1/images/generations_async", referenceField: "images", xai: true},
		{name: "xai_provider_diagnostic", path: "/v1/images/generations_async", referenceField: "images", xai: true, status: http.StatusBadRequest, failureCode: 610, responseMessage: "unknown provider for model grok-imagine-image-2.0"},
		{name: "edits_oa", path: "/v1/images/edits_oa", referenceField: "image_urls"},
		{name: "edits_async", path: "/v1/images/edits_async", referenceField: "image"},
		{name: "generations_oa_with_reference", path: "/v1/images/generations_oa", referenceField: "images"},
		{name: "generations_async_with_reference", path: "/v1/images/generations_async", referenceField: "images"},
		{name: "channel_switch_keeps_original_reference_after_retry", path: "/v1/images/generations_async", referenceField: "images", referenceRetryCount: 1},
		{name: "edits_oa_body_passthrough", path: "/v1/images/edits_oa", referenceField: "images", passThrough: true},
		{name: "edits_async_body_passthrough", path: "/v1/images/edits_async", referenceField: "image", passThrough: true},
		{name: "edits_async_multipart_png", path: "/v1/images/edits_async", fileFormat: "png"},
		{name: "edits_async_multipart_jpeg", path: "/v1/images/edits_async", fileFormat: "jpeg"},
		{name: "edits_async_multipart_jpeg_body_passthrough", path: "/v1/images/edits_async", fileFormat: "jpeg", passThrough: true},
		{name: "edits_oa_multipart_png", path: "/v1/images/edits_oa", fileFormat: "png"},
		{name: "multipart_mask_does_not_count_as_reference", path: "/v1/images/edits_async", fileFormat: "png", maxReferences: 1},
		{name: "channel_reference_limit_before_dispatch", path: "/v1/images/edits_async", fileFormat: "png", channelReferenceLimit: 8, localReferenceRejected: true},
		{name: "channel_reference_limit_keeps_remote_provider_reason", path: "/v1/images/edits_async", referenceField: "images", channelReferenceLimit: 8, status: http.StatusBadRequest, failureCode: 604, responseMessage: "参考image上传失败：参考图过大（25.4MB，上限 20MB）"},
		{name: "accepted_response_parse_failure_keeps_request_id", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusOK, unreadableResponse: true},
		{name: "reference_fetch_diagnostics", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadRequest, failureCode: 602, providerCode: "image_fetch_failed", responseMessage: "image_url fetch failed for https://example.com/private.png?token=secret-url; authorization=Bearer secret-bearer api_key=secret-key"},
		{name: "upstream_service_diagnostics", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 606, providerCode: "overloaded", responseMessage: "upstream image worker unavailable"},
		{name: "synchronous_gateway_failure_retries", path: "/v1/images/edits_async", referenceField: "images", newapi: true, synchronousRetry: true, status: 502, failureCode: 606, providerCode: "upstream_request_failed", responseMessage: "The upstream image service could not complete the image request. Please retry shortly."},
		{name: "asynchronous_gateway_failure_keeps_acceptance_boundary", path: "/v1/images/edits_async", referenceField: "images", newapi: true, status: 502, failureCode: 606, providerCode: "upstream_request_failed", responseMessage: "The upstream image service could not complete the image request. Please retry shortly.", templateBody: map[string]any{"model": "{upstream_model}", "n": "{request.n}"}, expectedTemplate: `{"model":"gpt-image-2","n":1}`},
		{name: "mapped_status_keeps_original_diagnostic", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 605, providerCode: "overloaded", responseMessage: "upstream image worker unavailable", statusMapping: `{"502":"429"}`},
		{name: "mapped_status_not_overridden_by_generic_error_type", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadRequest, failureCode: 605, responseMessage: "please retry later", statusMapping: `{"400":"429"}`},
		{name: "server_status_not_overridden_by_generic_error_type", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusBadGateway, failureCode: 606, responseMessage: "please retry later"},
		{
			name: "credential_diagnostics_redact_all_provider_fields", path: "/v1/images/edits_async", referenceField: "images", status: http.StatusUnauthorized, failureCode: 610,
			responseMessage: "Incorrect API key provided: sk-proj-SYNTHETIC123456789NOTAREALKEY",
			providerCode:    "https://example.com/private-code?signature=SYNTHETIC-CODE-SECRET",
			providerStatus:  "sk-proj-SYNTHETICSTATUS123456789NOTAREALKEY",
			requestID:       "https://example.com/private-request?signature=SYNTHETIC-REQUEST-SECRET sk-proj-SYNTHETICREQUEST123456789NOTAREALKEY",
			secrets: []string{
				"sk-proj-SYNTHETIC123456789NOTAREALKEY", "SYNTHETIC-CODE-SECRET", "sk-proj-SYNTHETICSTATUS123456789NOTAREALKEY",
				"SYNTHETIC-REQUEST-SECRET", "sk-proj-SYNTHETICREQUEST123456789NOTAREALKEY", "https://example.com/private-code", "https://example.com/private-request",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recordedRequest struct {
				method, path, contentType, authorization, marker string
				body                                             []byte
			}
			received := make(chan recordedRequest, 1)
			responseStatus := tc.status
			if responseStatus == 0 {
				responseStatus = http.StatusBadRequest
			}
			responseMessage := tc.responseMessage
			if responseMessage == "" {
				responseMessage = "synthetic request capture complete"
			}
			providerError := map[string]string{"message": responseMessage, "type": "invalid_request_error"}
			if tc.providerCode != "" {
				providerError["code"] = tc.providerCode
			}
			if tc.providerStatus != "" {
				providerError["status"] = tc.providerStatus
			}
			requestID := tc.requestID
			if requestID == "" {
				requestID = "synthetic-upstream-request"
			}
			responseBody, err := common.Marshal(map[string]any{"error": providerError})
			require.NoError(t, err)
			if tc.unreadableResponse {
				responseBody = []byte(`{"data":[`)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if !assert.NoError(t, readErr) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- recordedRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Header.Get("X-Image-Probe"), body}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Request-Id", requestID)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(responseStatus)
				_, _ = w.Write(responseBody)
			}))
			defer upstream.Close()
			mapping := `{"image-alias":"gpt-image-2"}`
			parameters := `{"quality":"low","vendor_override":true}`
			if tc.parameterOverride != "" {
				parameters = tc.parameterOverride
			}
			headers := `{"X-Image-Probe":"synthetic"}`
			channel := &model.Channel{Id: 1, Key: "synthetic-upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled, Models: "image-alias", Group: "default", ModelMapping: &mapping, ParamOverride: &parameters, HeaderOverride: &headers}
			if tc.newapi {
				channel.Type = constant.ChannelTypeNewAPI
			}
			if tc.xai {
				channel.Type = constant.ChannelTypeXai
				mapping = `{"image-alias":"grok-imagine-image-quality"}`
				channel.ParamOverride = nil
			}
			if tc.volcengine {
				channel.Type = constant.ChannelTypeVolcEngine
				mapping = `{"image-alias":"doubao-seedream-5-0-pro-260628"}`
			}
			channel.StatusCodeMapping = &tc.statusMapping
			channelSettings := dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough}
			if tc.referenceRetryCount > 0 {
				channelSettings.ImageReferenceTransportMode = "passthrough"
			}
			if tc.resolutionMapping {
				channelSettings.ImageResolutionModelMapping = dto.ImageResolutionModelMapping{"image-alias": {"1K": "gpt-image-2", "2K": "gpt-image-2-2k"}}
				mapping = `{"image-alias":"gpt-image-2","gpt-image-2":"gpt-image-2"}`
				if tc.resolution == "2K" {
					mapping = `{"image-alias":"gpt-image-2-2k","gpt-image-2-2k":"gpt-image-2-2k"}`
				}
			}
			if tc.channelReferenceLimit > 0 {
				channelSettings.ImageMaxReferenceBytes = &tc.channelReferenceLimit
			}
			channel.SetSetting(channelSettings)
			if tc.templateBody != nil {
				var config dto.UpstreamAsyncConfig
				require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"resolution-template","media_type":"image","models":["gpt-image-2"],"operations":["edit"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/v1/images/edits","headers":{"Authorization":"Bearer {api_key}"}}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
				config.Profiles[0].Submit.Request.Body = tc.templateBody
				require.NoError(t, config.Validate())
				channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &config})
			}
			body := map[string]any{
				"model": "image-alias", "prompt": "draw a river", "n": 1,
				"size": "1024x1024", "quality": "high", "response_format": "b64_json",
				"mask":      map[string]string{"image_url": maskURL},
				"watermark": false, "seed": 0, "custom_toggle": false,
				"vendor_options": map[string]any{"strength": 0, "enabled": false},
			}
			if tc.resolution == "2K" {
				body["size"] = "2048x2048"
			}
			if tc.xai {
				delete(body, "mask")
				body["resolution"], body["aspect_ratio"], body["size"] = "2K", "16:9", "2048x1152"
			}
			if tc.volcengine {
				delete(body, "mask")
				body["size"] = "1.5K"
			}
			if tc.referenceField == "images" {
				body[tc.referenceField] = []map[string]string{{"image_url": referenceURL}}
			} else if tc.referenceField != "" {
				body[tc.referenceField] = []string{referenceURL}
			}
			raw, err := common.Marshal(body)
			require.NoError(t, err)
			contentType := "application/json"
			imageBytes, imageFilename := pngImage.Bytes(), "input.png"
			if tc.fileFormat != "" {
				var formBody bytes.Buffer
				form := multipart.NewWriter(&formBody)
				for key, value := range body {
					if key == "mask" {
						continue
					}
					text, ok := value.(string)
					if !ok {
						encoded, err := common.Marshal(value)
						require.NoError(t, err)
						text = string(encoded)
					}
					require.NoError(t, form.WriteField(key, text))
				}
				if tc.fileFormat == "jpeg" {
					imageBytes, imageFilename = jpegImage.Bytes(), "input.jpg"
				}
				part, err := form.CreateFormFile("image", imageFilename)
				require.NoError(t, err)
				_, err = part.Write(imageBytes)
				require.NoError(t, err)
				if !tc.volcengine {
					part, err = form.CreateFormFile("mask", "mask.png")
					require.NoError(t, err)
					_, err = part.Write(pngImage.Bytes())
					require.NoError(t, err)
				}
				require.NoError(t, form.Close())
				raw, contentType = formBody.Bytes(), form.FormDataContentType()
			}
			cfg := service.DefaultImageRuntimeConfig()
			cfg.OpenAIReferenceMode = "passthrough"
			if tc.referenceRetryCount > 0 {
				cfg.OpenAIReferenceMode = "passthrough_fallback_local"
			}
			if tc.maxReferences > 0 {
				cfg.MaxReferences = tc.maxReferences
			}
			request, err := service.ParseAsyncImageRequest(raw, contentType, tc.path, cfg)
			require.NoError(t, err)
			request.Billing = &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			persisted, err := common.Marshal(request)
			require.NoError(t, err)
			request = service.AsyncImageRequest{}
			require.NoError(t, common.Unmarshal(persisted, &request))
			task := model.AsyncImageTask{TaskId: "openai-outbound-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120, ReferenceRetryCount: tc.referenceRetryCount}
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, cfg, func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			require.Error(t, err)
			if tc.modelConflict {
				assert.ErrorContains(t, err, "outbound image model conflicts")
				assert.Zero(t, dispatches, "model conflicts must fail before quota reservation or dispatch")
				assert.Empty(t, received)
				var reservations int64
				require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&reservations).Error)
				assert.Zero(t, reservations)
				return
			}
			if tc.localReferenceRejected {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 604, failure.Code)
				assert.Contains(t, failure.Message, "Reference image 1:")
				assert.Contains(t, failure.Message, "reference image limit: 8 bytes")
				assert.Zero(t, dispatches, "a known oversized reference must be rejected before quota reservation or upstream dispatch")
				assert.Empty(t, received)
				return
			}
			if tc.unreadableResponse {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 608, failure.Code)
				assert.Equal(t, "upstream_response_invalid", failure.InternalCode)
				assert.Equal(t, http.StatusOK, failure.HTTPStatus, "a response parser error must retain the actual upstream status")
				assert.True(t, failure.ExecutionUnknown)
				assert.Equal(t, requestID, failure.UpstreamRequestID)
				assert.Equal(t, 1, dispatches)
				assert.Len(t, received, 1)
				return
			}
			if tc.failureCode != 0 {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, tc.failureCode, failure.Code)
				assert.Equal(t, tc.synchronousRetry, failure.SynchronousRetryableResponse)
				assert.False(t, failure.UpstreamTerminal)
				assert.Equal(t, tc.status, failure.HTTPStatus, "attempt diagnostics must keep the real upstream status")
				assert.Equal(t, 7*time.Second, failure.RetryAfter)
				if len(tc.secrets) > 0 {
					assert.Contains(t, failure.Message, "Incorrect API key provided:")
					for field, value := range map[string]string{"message": failure.Message, "provider_code": failure.ProviderCode, "provider_status": failure.ProviderStatus, "request_id": failure.UpstreamRequestID} {
						assert.NotEmpty(t, value, "field %s should retain a redacted diagnostic", field)
						for _, secret := range tc.secrets {
							assert.NotContains(t, value, secret, "field %s must not persist provider-echoed credentials or signed URLs", field)
						}
					}
				} else {
					expectedCode := tc.providerCode
					if expectedCode == "" {
						expectedCode = "invalid_request_error"
					}
					assert.Equal(t, expectedCode, failure.ProviderCode)
					assert.Equal(t, requestID, failure.UpstreamRequestID)
				}
				if tc.failureCode == 602 {
					assert.Contains(t, failure.Message, "image_url fetch failed")
					for _, secret := range []string{"secret-url", "secret-bearer", "secret-key", "https://example.com/private.png"} {
						assert.NotContains(t, failure.Message, secret)
					}
				} else if len(tc.secrets) == 0 {
					assert.Equal(t, tc.responseMessage, failure.Message)
				}
			}
			require.Equal(t, 1, dispatches)
			require.Len(t, received, 1)
			record := <-received
			assert.Equal(t, http.MethodPost, record.method)
			if tc.volcengine {
				assert.Equal(t, "/api/v3/images/generations", record.path)
			} else {
				assert.Equal(t, "/v1/images/edits", record.path)
			}
			assert.Equal(t, "Bearer synthetic-upstream-key", record.authorization)
			if tc.expectedTemplate != "" {
				assert.JSONEq(t, tc.expectedTemplate, string(record.body))
				return
			}
			assert.Equal(t, "synthetic", record.marker)
			if tc.volcengine {
				assert.Equal(t, "application/json", record.contentType)
				var outbound struct {
					Model     string   `json:"model"`
					Image     []string `json:"image"`
					Size      string   `json:"size"`
					Seed      *int     `json:"seed"`
					Watermark *bool    `json:"watermark"`
				}
				require.NoError(t, common.Unmarshal(record.body, &outbound))
				assert.Equal(t, "doubao-seedream-5-0-pro-260628", outbound.Model)
				assert.Equal(t, "1.5K", outbound.Size)
				require.Len(t, outbound.Image, 1)
				if tc.fileFormat == "" {
					assert.Equal(t, referenceURL, outbound.Image[0])
				} else {
					assert.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(imageBytes), outbound.Image[0])
				}
				require.NotNil(t, outbound.Seed)
				require.NotNil(t, outbound.Watermark)
				assert.Zero(t, *outbound.Seed)
				assert.False(t, *outbound.Watermark)
				assert.NotContains(t, string(record.body), "sequential_image_generation")
				return
			}
			if tc.xai {
				assert.JSONEq(t, `{"model":"grok-imagine-image-quality","prompt":"draw a river","n":1,"resolution":"2k","aspect_ratio":"16:9","response_format":"b64_json","image":{"url":"`+referenceURL+`","type":"image_url"}}`, string(record.body))
				return
			}
			if tc.fileFormat != "" {
				require.Contains(t, record.contentType, "multipart/form-data", "file uploads must retain their upstream transport format")
				replayed := httptest.NewRequest(http.MethodPost, record.path, bytes.NewReader(record.body))
				replayed.Header.Set("Content-Type", record.contentType)
				require.NoError(t, replayed.ParseMultipartForm(1<<20))
				defer replayed.MultipartForm.RemoveAll()
				imageField, expectedModel := "image", "gpt-image-2"
				if tc.passThrough {
					imageField, expectedModel = "image[]", "image-alias"
				}
				if tc.resolutionMapping {
					expectedModel = "gpt-image-2"
				}
				assert.Equal(t, expectedModel, replayed.PostForm.Get("model"))
				assert.Equal(t, "draw a river", replayed.PostForm.Get("prompt"))
				for field, value := range map[string]string{"n": "1", "size": "1024x1024", "response_format": "b64_json", "watermark": "false", "seed": "0", "custom_toggle": "false"} {
					assert.Equal(t, value, replayed.PostForm.Get(field), "multipart field %s must survive persistence and relay conversion", field)
				}
				assert.JSONEq(t, `{"strength":0,"enabled":false}`, replayed.PostForm.Get("vendor_options"))
				for _, file := range []struct {
					field, contentType string
					data               []byte
				}{
					{field: imageField, contentType: "image/" + tc.fileFormat, data: imageBytes},
					{field: "mask", contentType: "image/png", data: pngImage.Bytes()},
				} {
					require.Len(t, replayed.MultipartForm.File[file.field], 1)
					header := replayed.MultipartForm.File[file.field][0]
					assert.Equal(t, file.contentType, header.Header.Get("Content-Type"), "field %s must declare its real image encoding", file.field)
					reader, err := header.Open()
					require.NoError(t, err)
					actual, err := io.ReadAll(reader)
					require.NoError(t, err)
					require.NoError(t, reader.Close())
					assert.Equal(t, file.data, actual)
				}
				return
			}
			assert.Equal(t, "application/json", record.contentType)
			var outbound map[string]common.RawMessage
			require.NoError(t, common.Unmarshal(record.body, &outbound))
			expectedModel, expectedQuality := `"gpt-image-2"`, `"low"`
			if tc.passThrough {
				expectedModel, expectedQuality = `"image-alias"`, `"high"`
				assert.NotContains(t, outbound, "vendor_override", "body passthrough bypasses channel parameter overrides")
			} else {
				assert.JSONEq(t, "true", string(outbound["vendor_override"]))
			}
			if tc.resolutionMapping {
				expectedModel = `"gpt-image-2"`
				if tc.resolution == "2K" {
					expectedModel = `"gpt-image-2-2k"`
				}
			}
			assert.JSONEq(t, expectedModel, string(outbound["model"]))
			assert.JSONEq(t, expectedQuality, string(outbound["quality"]))
			for _, key := range []string{"prompt", "n", "size", "response_format", "watermark", "seed", "custom_toggle", "vendor_options", "mask"} {
				encoded, err := common.Marshal(body[key])
				require.NoError(t, err)
				assert.JSONEq(t, string(encoded), string(outbound[key]), "field %s must survive persistence and relay conversion", key)
			}
			var images []struct {
				URL string `json:"image_url"`
			}
			require.NoError(t, common.Unmarshal(outbound["images"], &images))
			require.Len(t, images, 1)
			assert.Equal(t, referenceURL, images[0].URL, "external signed URL bytes must be preserved")
			assert.NotContains(t, outbound, "image")
			assert.NotContains(t, outbound, "image_urls")
		})
	}
}

func TestSeedreamFinalImageSubmission(t *testing.T) {
	service.InitHttpClient()
	const pro = "doubao-seedream-5-0-pro-260628"
	const flash = "doubao-seedream-5-0-flash-260915"
	const lite = "doubao-seedream-5-0-260128"
	for _, tc := range []struct {
		name, model, size, failure string
		count, references          int
		asynchronous, passThrough  bool
		fields, override           map[string]any
		template                   map[string]any
	}{
		{name: "sync_pro_preserves_native_options", model: pro, references: 3, size: "1.5K"},
		{name: "async_flash_ten_references", model: flash, references: 10, asynchronous: true},
		{name: "lite_group", model: lite, count: 3, size: "3K", asynchronous: true},
		{name: "lite_native_group", model: lite, count: 3, asynchronous: true, fields: map[string]any{"sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 3}}},
		{name: "lite_maximum_group", model: lite, count: 15, asynchronous: true},
		{name: "lite_reference_group_boundary", model: lite, count: 14, references: 1, asynchronous: true},
		{name: "lite_reference_output_total_limit", model: lite, count: 15, references: 1, asynchronous: true, failure: "must total at most 15"},
		{name: "legacy_45_group", model: "doubao-seedream-4-5-251128", count: 2, asynchronous: true},
		{name: "single_image_passthrough_mapping", model: pro, references: 3, asynchronous: true, passThrough: true},
		{name: "pro_rejects_group", model: pro, count: 2, failure: "supports between 1 and 1"},
		{name: "flash_rejects_disabled_sequential", model: flash, fields: map[string]any{"sequential_image_generation": "disabled"}, failure: "does not support sequential"},
		{name: "pro_rejects_options", model: pro, fields: map[string]any{"sequential_image_generation_options": map[string]any{"max_images": 1}}, failure: "does not support sequential"},
		{name: "pro_rejects_web_search", model: pro, fields: map[string]any{"web_search": true}, failure: "web_search is not supported"},
		{name: "pro_rejects_stream", model: pro, fields: map[string]any{"stream": true}, failure: "stream is not supported"},
		{name: "async_stream_override", model: lite, asynchronous: true, override: map[string]any{"stream": true}, failure: "stream is not supported"},
		{name: "pro_reference_limit", model: pro, references: 11, failure: "at most 10 reference"},
		{name: "lite_reference_limit", model: lite, references: 15, failure: "at most 14 reference"},
		{name: "lite_rejects_sixteen", model: lite, count: 16, failure: "supports between 1 and 15"},
		{name: "negative_count_override", model: lite, override: map[string]any{"n": -1}, failure: "invalid image billing parameters"},
		{name: "huge_count_override", model: lite, override: map[string]any{"n": uint64(18446744073686646784)}, failure: "n must be an integer"},
		{name: "negative_native_quantity", model: lite, fields: map[string]any{"sequential_image_generation_options": map[string]any{"max_images": -1}}, failure: "max_images conflicts"},
		{name: "huge_native_quantity", model: lite, fields: map[string]any{"sequential_image_generation_options": map[string]any{"max_images": uint64(18446744073686646784)}}, failure: "max_images conflicts"},
		{name: "native_quantity_conflict", model: lite, count: 3, fields: map[string]any{"sequential_image_generation_options": map[string]any{"max_images": 4}}, failure: "max_images conflicts"},
		{name: "disabled_group_conflict", model: lite, count: 3, fields: map[string]any{"sequential_image_generation": "disabled"}, failure: "disabled conflicts"},
		{name: "model_override_cannot_bypass_pro", model: pro, override: map[string]any{"model": lite}, failure: "model conflicts"},
		{name: "group_override_cannot_bypass_pro", model: pro, override: map[string]any{"n": 2}, failure: "supports between 1 and 1"},
		{name: "native_options_override_cannot_bypass_pro", model: pro, override: map[string]any{"sequential_image_generation_options": map[string]any{"max_images": 1}}, failure: "does not support sequential"},
		{name: "provider_quantity_conflict", model: pro, fields: map[string]any{"parameters": map[string]any{"n": 2}}, failure: "parameters.n conflicts"},
		{name: "async_layers_are_unsupported", model: pro, asynchronous: true, fields: map[string]any{"layer_decomposition": true}, failure: "layer_decomposition is not supported"},
		{name: "mask_is_unsupported", model: pro, fields: map[string]any{"mask": "https://example.com/mask.png"}, failure: "does not support mask"},
		{name: "passthrough_sequential_is_checked", model: pro, asynchronous: true, passThrough: true, fields: map[string]any{"sequential_image_generation": "disabled"}, failure: "does not support sequential"},
		{name: "submit_template_cannot_add_pro_group_options", model: pro, asynchronous: true, template: map[string]any{"model": pro, "n": 1, "sequential_image_generation_options": map[string]any{"max_images": 1}}, failure: "does not support sequential"},
		{name: "submit_template_cannot_change_model", model: pro, asynchronous: true, template: map[string]any{"model": lite, "n": 1}, failure: "model conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := max(1, tc.count)
			size := tc.size
			if size == "" {
				size = "2K"
			}
			body := map[string]any{"model": "image-alias", "prompt": "draw a river", "n": count, "size": size, "seed": 0, "watermark": false, "vendor_options": map[string]any{"strength": 0, "enabled": false}}
			references := make([]string, tc.references)
			for index := range references {
				references[index] = "https://example.com/reference-" + string(rune('a'+index)) + ".png"
			}
			if len(references) > 0 {
				body["image"] = references
			}
			for name, value := range tc.fields {
				body[name] = value
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			var request dto.ImageRequest
			require.NoError(t, common.Unmarshal(encoded, &request))
			var outbound map[string]common.RawMessage
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v3/images/generations", r.URL.Path)
				assert.Equal(t, "Bearer synthetic-seedream-key", r.Header.Get("Authorization"))
				assert.NoError(t, common.DecodeJson(r.Body, &outbound))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic request capture complete","type":"invalid_request_error"}}`)
			}))
			t.Cleanup(upstream.Close)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(encoded))
			c.Request.Header.Set("Content-Type", "application/json")
			storage, err := common.CreateBodyStorage(encoded)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, storage.Close()) })
			c.Set(common.KeyBodyStorage, storage)
			mapping, err := common.Marshal(map[string]string{"image-alias": "mapped-image", "mapped-image": tc.model})
			require.NoError(t, err)
			c.Set("model_mapping", string(mapping))
			c.Set("async_image_execution", tc.asynchronous)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeVolcEngine)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "synthetic-seedream-key")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "image-alias")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passThrough})
			if tc.template != nil {
				var config dto.UpstreamAsyncConfig
				require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"seedream-template","media_type":"image","operations":["generate"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/api/v3/images/generations"}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
				config.Profiles[0].Models = []string{tc.model}
				template, err := common.Marshal(tc.template)
				require.NoError(t, err)
				require.NoError(t, common.Unmarshal(template, &config.Profiles[0].Submit.Request.Body))
				require.NoError(t, config.Validate())
				common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{UpstreamAsync: &config})
			}
			if tc.override != nil {
				common.SetContextKey(c, constant.ContextKeyChannelParamOverride, tc.override)
			}
			info := &relaycommon.RelayInfo{Request: &request, OriginModelName: "image-alias", RelayMode: relayconstant.RelayModeImagesGenerations, PriceData: types.PriceData{UsePrice: true, ModelPrice: 0.01, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			dispatches := 0
			_, apiErr := ExecuteImageRequest(c, info, false, func() error { dispatches++; return nil })
			require.NotNil(t, apiErr)
			if tc.failure != "" {
				assert.ErrorContains(t, apiErr, tc.failure)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Zero(t, calls)
				assert.Zero(t, dispatches, "invalid provider parameters must fail before quota reservation or dispatch")
				return
			}
			assert.Equal(t, 1, calls)
			assert.Equal(t, 1, dispatches)
			assert.Equal(t, count, info.ImageRequestCount)
			assert.Equal(t, common.QuotaRound(0.01*common.QuotaPerUnit*float64(count)), info.PriceData.QuotaToPreConsume)
			assert.JSONEq(t, `"`+tc.model+`"`, string(outbound["model"]))
			assert.JSONEq(t, `"`+size+`"`, string(outbound["size"]))
			assert.JSONEq(t, `0`, string(outbound["seed"]))
			assert.JSONEq(t, `false`, string(outbound["watermark"]))
			assert.JSONEq(t, `{"strength":0,"enabled":false}`, string(outbound["vendor_options"]))
			if count > 1 {
				assert.JSONEq(t, `"auto"`, string(outbound["sequential_image_generation"]))
				options, err := common.Marshal(map[string]int{"max_images": count})
				require.NoError(t, err)
				assert.JSONEq(t, string(options), string(outbound["sequential_image_generation_options"]))
			} else {
				assert.NotContains(t, outbound, "sequential_image_generation")
				assert.NotContains(t, outbound, "sequential_image_generation_options")
			}
			if len(references) > 0 {
				var actual []string
				require.NoError(t, common.Unmarshal(outbound["image"], &actual))
				assert.Equal(t, references, actual)
			}
		})
	}
}

func TestTianyueAsyncImageOutbound(t *testing.T) {
	previousDB, previousType, previousCount, previousRedis, previousAddress := model.DB, common.MainDatabaseType(), constant.CountToken, common.RedisEnabled, system_setting.ServerAddress
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "tianyue-outbound.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled, system_setting.ServerAddress = db, false, false, "https://gateway.example"
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled, system_setting.ServerAddress = previousDB, previousCount, previousRedis, previousAddress
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}, &model.ImageInputAlias{}, &model.UserSubscription{}, &model.Token{}, &model.User{}))
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "tianyue:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "tianyue")
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "tianyue-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "tianyue-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	for _, tc := range []struct {
		name                       string
		model, resolution          string
		reference, multipart, mask bool
		disabled, badOverride      bool
		badTemplate, customProfile bool
		providerErrorCode          string
		disconnect, failed         bool
	}{
		{name: "asynchronous_text_1k"},
		{name: "asynchronous_text_2k", model: "seedream-5-pro-2K", resolution: "2K"},
		{name: "provider_request_failure", providerErrorCode: "do_request_failed"},
		{name: "provider_response_read_failure", providerErrorCode: "read_response_body_failed"},
		{name: "provider_bad_response", providerErrorCode: "bad_response"},
		{name: "provider_bad_response_body", providerErrorCode: "bad_response_body"},
		{name: "provider_empty_response", providerErrorCode: "empty_response"},
		{name: "transport_eof", disconnect: true},
		{name: "uploaded_reference", reference: true},
		{name: "multipart_reference_2k", reference: true, multipart: true, model: "seedream-5-pro-2K", resolution: "2K"},
		{name: "asynchronous_yb_receipt_and_poll", reference: true, model: "gpt-image-2-yb"},
		{name: "asynchronous_failure", failed: true},
		{name: "mask_is_rejected", reference: true, mask: true},
		{name: "unsafe_override_is_rejected", badOverride: true},
		{name: "unsafe_async_template_is_rejected", badTemplate: true},
		{name: "custom_profile_with_builtin_id_is_preserved", customProfile: true},
		{name: "ordinary_newapi_edits_are_preserved", reference: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type receivedRequest struct {
				method, path string
				body         map[string]common.RawMessage
			}
			received := make(chan receivedRequest, 3)
			polls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]common.RawMessage
				if r.Method == http.MethodPost {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
					assert.NoError(t, common.DecodeJson(r.Body, &body))
				}
				assert.Equal(t, "Bearer tianyue-upstream-key", r.Header.Get("Authorization"))
				received <- receivedRequest{r.Method, r.URL.Path, body}
				if r.URL.Path == "/image.png" {
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(bitmap.Bytes())
					return
				}
				if tc.disconnect {
					connection, _, err := w.(http.Hijacker).Hijack()
					if assert.NoError(t, err) {
						assert.NoError(t, connection.Close())
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.providerErrorCode != "" {
					w.Header().Set(common.RequestIdKey, "tianyue-provider-request-id")
					w.WriteHeader(http.StatusInternalServerError)
					encoded, err := common.Marshal(map[string]any{"error": map[string]string{"message": "TianYue could not confirm image generation; Authorization: Bearer private-provider-key", "type": "new_api_error", "code": tc.providerErrorCode}})
					if assert.NoError(t, err) {
						_, _ = w.Write(encoded)
					}
					return
				}
				if !tc.disabled {
					if r.Method == http.MethodGet {
						polls++
					}
					if tc.customProfile {
						if r.Method == http.MethodPost {
							w.WriteHeader(http.StatusAccepted)
							_, _ = w.Write([]byte(`{"job":{"identifier":"custom-job","state":"queued"}}`))
						} else if polls == 1 {
							_, _ = w.Write([]byte(`{"job":{"identifier":"custom-job","state":"running","progress":30}}`))
						} else {
							_, _ = w.Write([]byte(`{"job":{"identifier":"custom-job","state":"done","output":"http://` + r.Host + `/image.png"}}`))
						}
						return
					}
					if r.Method == http.MethodPost {
						w.WriteHeader(http.StatusAccepted)
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"IN_PROGRESS"}}`))
					} else if polls == 1 {
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"IN_PROGRESS","progress":"30%"}}`))
					} else if tc.failed {
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"FAILURE","fail_reason":"Image generation rejected"}}`))
					} else {
						_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"tianyue-job","status":"SUCCESS","data":{"content_url":"/image.png"}}}`))
					}
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"synthetic capture completed"}}`))
			}))
			t.Cleanup(upstream.Close)
			mapped := "seedream-5-pro-1K"
			if tc.model != "" {
				mapped = tc.model
			}
			mapping, err := common.Marshal(map[string]string{"image-alias": mapped})
			require.NoError(t, err)
			mappingText := string(mapping)
			channel := &model.Channel{Id: 30, Type: constant.ChannelTypeNewAPI, Key: "tianyue-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: "image-alias", Group: "default", ModelMapping: &mappingText}
			channel.SetSetting(dto.ChannelSettings{TianyueMediaCompatibility: !tc.disabled})
			if tc.badOverride {
				override := `{"duration":2}`
				channel.ParamOverride = &override
			}
			if tc.badTemplate || tc.customProfile {
				var custom dto.UpstreamAsyncConfig
				require.NoError(t, common.Unmarshal([]byte(`{"profiles":[{"id":"tianyue-image","media_type":"image","models":["seedream-5-pro-1K"],"operations":["generate"],"submit":{"task_id_path":"job.identifier","request":{"method":"POST","path":"/custom/images/jobs","headers":{"Authorization":"Bearer {api_key}"}}},"poll":{"request":{"method":"GET","path":"/custom/images/jobs/{task_id}","headers":{"Authorization":"Bearer {api_key}"}},"response":{"status_path":"job.state","progress_path":"job.progress","status_values":{"queued":["queued"],"in_progress":["running"],"succeeded":["done"],"failed":["failed"]},"result_path":"job.output","download_headers":{"Authorization":"Bearer {api_key}"}}}}]}`), &custom))
				if tc.badTemplate {
					custom.Profiles[0].Submit.Request.Body = map[string]any{"model": "{upstream_model}", "prompt": "{request.prompt}", "duration": float64(2)}
				}
				require.NoError(t, custom.Validate())
				channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &custom})
			}
			request := service.AsyncImageRequest{Platform: "openai", Provider: "newapi", Dialect: "async", Model: "image-alias", Prompt: "test picture", Count: 1, Kind: "text_to_image", Size: "1024x1024", Resolution: "1K", SourcePath: "/v1/images/generations_async", Native: map[string]common.RawMessage{"model": common.RawMessage(`"image-alias"`), "prompt": common.RawMessage(`"test picture"`), "n": common.RawMessage(`1`), "size": common.RawMessage(`"1024x1024"`)}}
			if tc.resolution != "" {
				request.Resolution = tc.resolution
			}
			if tc.reference {
				request.Kind, request.SourcePath = "image_to_image", "/v1/images/edits_async"
				request.Parts = []service.AsyncImageInputPart{{Type: "image_url", URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(bitmap.Bytes())}}
			}
			if tc.multipart {
				request.InputFormat = "multipart"
			}
			if tc.mask {
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "mask", URL: request.Parts[0].URL})
			}
			request.Billing = &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			task := model.AsyncImageTask{TaskId: "tianyue-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120, StartedAt: time.Now().Unix()}
			payload, err := common.Marshal(request)
			require.NoError(t, err)
			task.RequestCipher, err = service.EncryptImagePayload(payload, "task:"+task.TaskId)
			require.NoError(t, err)
			channelPayload, err := common.Marshal(channel)
			require.NoError(t, err)
			task.SelectedChannelCipher, err = service.EncryptImagePayload(channelPayload, "image-channel:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			beforeInvoke := func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			}
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
			require.Error(t, err)
			if tc.mask || tc.badOverride || tc.badTemplate {
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				if tc.badTemplate {
					assert.ErrorContains(t, err, "duration=1")
				}
				return
			}
			require.Len(t, received, 1)
			sent := <-received
			assert.Equal(t, 1, dispatches)
			if tc.disabled {
				assert.Equal(t, "/v1/images/edits", sent.path)
				var references []map[string]string
				require.NoError(t, common.Unmarshal(sent.body["images"], &references))
				require.Len(t, references, 1)
				assert.True(t, strings.HasPrefix(references[0]["image_url"], "data:"))
				return
			}
			expectedPath := "/v1/video/generations"
			if tc.customProfile {
				expectedPath = "/custom/images/jobs"
			}
			assert.Equal(t, expectedPath, sent.path)
			assert.Equal(t, http.MethodPost, sent.method)
			assert.JSONEq(t, `"`+mapped+`"`, string(sent.body["model"]))
			assert.JSONEq(t, `1`, string(sent.body["duration"]))
			assert.JSONEq(t, `"1:1"`, string(sent.body["aspect_ratio"]))
			assert.JSONEq(t, `"`+request.Resolution+`"`, string(sent.body["output_resolution"]))
			assert.NotContains(t, sent.body, "size")
			if tc.providerErrorCode != "" || tc.disconnect {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, 608, failure.Code)
				assert.True(t, failure.ExecutionUnknown, "an unconfirmed dispatched generation must retain its reservation for reconciliation")
				assert.False(t, failure.UpstreamTerminal)
				if tc.disconnect {
					assert.Equal(t, "upstream_transport_disconnected", failure.InternalCode)
					assert.NotEmpty(t, failure.Message, "the OS-specific disconnect text must not change the safe failure contract")
					assert.Zero(t, failure.HTTPStatus)
					assert.Empty(t, failure.UpstreamRequestID)
					assert.Empty(t, failure.ProviderCode)
				} else {
					assert.Equal(t, "execution_unknown", failure.InternalCode)
					assert.Contains(t, failure.Message, "TianYue could not confirm image generation")
					assert.NotContains(t, failure.Message, "private-provider-key")
					assert.Equal(t, http.StatusInternalServerError, failure.HTTPStatus)
					assert.Equal(t, "tianyue-provider-request-id", failure.UpstreamRequestID)
					assert.Equal(t, tc.providerErrorCode, failure.ProviderCode)
				}
				return
			}
			if tc.reference {
				var references []string
				require.NoError(t, common.Unmarshal(sent.body["images"], &references))
				require.Len(t, references, 1)
				assert.True(t, strings.HasPrefix(references[0], "https://gateway.example/api/image-task-references/"))
				assert.Contains(t, references[0], "signature=")
				assert.NotContains(t, references[0], "data:")
			}
			var pending *service.AsyncImagePending
			require.ErrorAs(t, err, &pending)
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			expectedTaskID, expectedPollPath := "tianyue-job", "/v1/video/generations/tianyue-job"
			if tc.customProfile {
				expectedTaskID, expectedPollPath = "custom-job", "/custom/images/jobs/custom-job"
			}
			assert.Equal(t, expectedTaskID, task.UpstreamTaskId)
			payload, err = service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			var recovered service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(payload, &recovered))
			_, _, err = ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
			require.ErrorAs(t, err, &pending)
			require.Len(t, received, 1)
			polled := <-received
			assert.Equal(t, expectedPollPath, polled.path)
			assert.Equal(t, http.MethodGet, polled.method)
			assert.Equal(t, 1, dispatches, "polling a known task must never submit again")
			output, bill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
			if tc.failed {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.True(t, failure.UpstreamTerminal)
				assert.Equal(t, "upstream_job_failed", failure.InternalCode)
				assert.Equal(t, "Image generation rejected", failure.Message)
				require.Len(t, received, 1)
				assert.Equal(t, expectedPollPath, (<-received).path)
				assert.Equal(t, 1, dispatches, "a failed upstream job must not be submitted again")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, output)
			require.Len(t, output.Images, 1)
			assert.Equal(t, bitmap.Bytes(), output.Images[0].Data)
			assert.Equal(t, 5000, bill.Quota)
			require.Len(t, received, 2)
			assert.Equal(t, expectedPollPath, (<-received).path)
			assert.Equal(t, "/image.png", (<-received).path)
			assert.Equal(t, 1, dispatches, "completed polling and downloading must not repeat submission")
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			payload, err = service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
			require.NoError(t, err)
			require.NoError(t, common.Unmarshal(payload, &recovered))
			require.NotNil(t, recovered.Completion)
			_, recoveredBill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), beforeInvoke)
			require.NoError(t, err)
			assert.Equal(t, bill.Quota, recoveredBill.Quota)
			require.Len(t, received, 1)
			assert.Equal(t, "/image.png", (<-received).path)
			assert.Equal(t, 1, dispatches, "completion recovery must only download the saved result")
		})
	}
}

func TestGeminiAsyncImageCustomSubmitAndResume(t *testing.T) {
	previousDB, previousCount, previousRedis := model.DB, constant.CountToken, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-submit.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}, &model.ImageInputAlias{}))
	user := model.User{Username: "gemini-submit-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{Id: 17, UserId: user.Id, Key: "gemini-submit-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var referenceBytes bytes.Buffer
	require.NoError(t, png.Encode(&referenceBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	referenceBase64 := base64.StdEncoding.EncodeToString(referenceBytes.Bytes())
	canonicalReference := "data:image/png;base64," + referenceBase64
	sourceReference := "data:image/png;base64," + referenceBase64[:32] + "\n" + referenceBase64[32:]
	for _, tc := range []struct {
		name              string
		body              map[string]any
		reference         bool
		invalid           bool
		resolutionMapping bool
		parameterOverride string
		expectedBody      string
	}{
		{name: "template", body: map[string]any{"prompt": "{request.prompt}", "n": "{request.n}"}},
		{name: "native_body"},
		{name: "converted_reference_template", body: map[string]any{"image_urls": "{request.image_urls}", "n": "{request.n}"}, reference: true},
		{name: "conflicting_count", body: map[string]any{"n": float64(2)}, invalid: true},
		{name: "resolution_mapping_native_body", resolutionMapping: true},
		{name: "resolution_mapping_custom_field", resolutionMapping: true, body: map[string]any{"provider_model": "{upstream_model}", "n": "{request.n}"}, expectedBody: `{"provider_model":"gemini-image-test","n":1}`},
		{name: "resolution_mapping_conflicting_template", resolutionMapping: true, body: map[string]any{"model": "{request.model}"}, invalid: true},
		{name: "resolution_mapping_conflicting_override", resolutionMapping: true, parameterOverride: `{"model":"other-model"}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type recordedRequest struct{ method, path, auth, query, body string }
			received := make(chan recordedRequest, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				received <- recordedRequest{r.Method, r.URL.Path, r.Header.Get("X-Custom-Key"), r.URL.Query().Get("model"), string(body)}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"status":"running"}`))
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"vendor-job"}`))
			}))
			defer upstream.Close()
			var config dto.UpstreamAsyncConfig
			require.NoError(t, common.UnmarshalJsonStr(`{"profiles":[{"id":"custom","media_type":"image","models":["gemini-image-test"],"operations":["generate"],"submit":{"task_id_path":"id","request":{"method":"POST","path":"/custom/jobs","headers":{"X-Custom-Key":"{api_key}"},"query":{"model":"{upstream_model}"}}},"poll":{"request":{"method":"GET","path":"/tasks/{task_id}"},"response":{"status_path":"status","status_values":{"in_progress":["running"],"succeeded":["done"],"failed":["failed"]},"result_path":"urls"}}}]}`, &config))
			config.Profiles[0].Submit.Request.Body = tc.body
			require.NoError(t, config.Validate())
			channel := &model.Channel{Id: 1, Key: "upstream-key", BaseURL: &upstream.URL, Type: constant.ChannelTypeGemini, Status: common.ChannelStatusEnabled, Models: "gemini-image-test", Group: "default"}
			channel.SetOtherSettings(dto.ChannelOtherSettings{UpstreamAsync: &config})
			request := service.AsyncImageRequest{Platform: "gemini", Model: "gemini-image-test", Prompt: "draw", Count: 1, Parts: []service.AsyncImageInputPart{{Type: "text", Text: "draw"}}, Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}}
			if tc.resolutionMapping {
				request.Model, request.Resolution = "gemini-alias", "1K"
				channel.ModelMapping = common.GetPointer(`{"gemini-alias":"gemini-image-test","gemini-image-test":"gemini-image-test"}`)
				channel.SetSetting(dto.ChannelSettings{ImageResolutionModelMapping: dto.ImageResolutionModelMapping{"gemini-alias": {"1K": "gemini-image-test"}}})
			}
			if tc.parameterOverride != "" {
				channel.ParamOverride = &tc.parameterOverride
			}
			if tc.reference {
				channel.SetSetting(dto.ChannelSettings{ImageReferenceTransportMode: "local"})
				request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "image_url", URL: sourceReference})
			}
			task := model.AsyncImageTask{TaskId: "gemini-submit-" + tc.name, UserId: user.Id, TokenId: token.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
			require.NoError(t, db.Create(&task).Error)
			dispatches := 0
			_, _, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
				dispatches++
				return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
			})
			if tc.invalid {
				var failure *service.AsyncImageFailure
				require.ErrorAs(t, err, &failure)
				assert.Equal(t, http.StatusBadRequest, failure.HTTPStatus)
				assert.Zero(t, dispatches)
				assert.Empty(t, received)
				return
			}
			var pending *service.AsyncImagePending
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches)
			record := <-received
			assert.Equal(t, http.MethodPost, record.method)
			assert.Equal(t, "/custom/jobs", record.path)
			assert.Equal(t, "upstream-key", record.auth)
			assert.Equal(t, "gemini-image-test", record.query)
			if tc.expectedBody != "" {
				assert.JSONEq(t, tc.expectedBody, record.body)
			} else if tc.reference {
				expected, err := common.Marshal(map[string]any{"image_urls": []string{canonicalReference}, "n": 1})
				require.NoError(t, err)
				assert.JSONEq(t, string(expected), record.body, "custom templates must receive the validated and converted reference")
			} else if tc.body != nil {
				assert.JSONEq(t, `{"prompt":"draw","n":1}`, record.body)
			} else {
				assert.Contains(t, record.body, `"contents"`)
				assert.NotContains(t, record.body, `"model"`, "Gemini uses the URL model without an injected JSON field")
			}
			require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
			assert.Equal(t, "vendor-job", task.UpstreamTaskId)
			request.Reservation, err = model.GetAsyncImageReservation(t.Context(), task)
			require.NoError(t, err)
			require.NotNil(t, request.Reservation)
			request.ClientIP = "192.0.2.10"
			request.Parts = append(request.Parts, service.AsyncImageInputPart{Type: "image_url", URL: "https://expired.invalid/reference.png"})
			require.NoError(t, db.Model(&token).Update("allow_ips", "198.51.100.0/24").Error)
			require.NoError(t, db.Model(&user).Update("status", common.UserStatusDisabled).Error)
			require.NoError(t, db.Delete(&token).Error)
			defer func() {
				require.NoError(t, db.Unscoped().Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"deleted_at": nil, "allow_ips": nil}).Error)
				require.NoError(t, db.Model(&user).Update("status", common.UserStatusEnabled).Error)
			}()
			_, _, err = ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error { dispatches++; return nil })
			require.ErrorAs(t, err, &pending)
			assert.Equal(t, 1, dispatches, "polling must not cross the submission barrier again")
			record = <-received
			assert.Equal(t, http.MethodGet, record.method)
			assert.Equal(t, "/tasks/vendor-job", record.path)
		})
	}
}

func TestAsyncImageSettlementPreservesPromptExtension(t *testing.T) {
	previousDB, previousQuotaUnit := model.DB, common.QuotaPerUnit
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ali-image-bill.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, common.QuotaPerUnit = db, 500000
	t.Cleanup(func() {
		model.DB, common.QuotaPerUnit = previousDB, previousQuotaUnit
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "ali-image-bill-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		name             string
		extend, fallback bool
		want             int
	}{
		{name: "local_extended", extend: true, want: 10000},
		{name: "fallback_extended", extend: true, fallback: true, want: 10000},
		{name: "local_disabled", want: 5000},
		{name: "fallback_disabled", fallback: true, want: 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations_async", nil)
			images := []service.ImageBytes{{Width: 1024, Height: 1024}}
			if tc.fallback {
				c.Set("async_image_result_source", "upstream")
				images[0] = service.ImageBytes{}
			}
			info := &relaycommon.RelayInfo{UserId: user.Id, OriginModelName: "z-image", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeAli, UpstreamModelName: "z-image"}, StartTime: time.Now(), Request: &dto.ImageRequest{}, PriceData: types.PriceData{UsePrice: true, ModelPrice: 0.01, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
			require.Nil(t, service.EstimateImageBillingForRequest(info, 2, tc.extend))
			assert.Equal(t, 2*tc.want, info.PriceData.QuotaToPreConsume)
			bill, err := service.FixAsyncImageBill(c, model.AsyncImageTask{TaskId: "ali-bill-" + tc.name, UserId: user.Id, TokenId: 1, ChannelId: 1}, info, &dto.Usage{}, images, service.AsyncImageRequest{Platform: "openai", Model: "z-image", Count: 2}, model.ImageFundingSelection{Source: "wallet"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, bill.Quota, "settle actual image count using the captured prompt-extension multiplier")
		})
	}
}

func TestUpstreamImageResultReadRetriesExistingTask(t *testing.T) {
	service.InitHttpClient()
	var pngBytes bytes.Buffer
	require.NoError(t, png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	redirectAuthorization := make(chan string, 1)
	privateRedirects := make(chan struct{}, 1)
	privateDestination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		privateRedirects <- struct{}{}
	}))
	defer privateDestination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/partial":
			w.Header().Set("Content-Length", "12")
			_, _ = w.Write([]byte("partial"))
		case "/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/redirect":
			http.Redirect(w, request, "/image", http.StatusFound)
		case "/redirect-private":
			http.Redirect(w, request, privateDestination.URL, http.StatusFound)
		case "/image":
			redirectAuthorization <- request.Header.Get("Authorization")
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes.Bytes())
		}
	}))
	defer upstream.Close()
	profile := &dto.UpstreamAsyncProfile{}
	profile.Poll.Response.DownloadHeaders = map[string]string{"Authorization": "Bearer {api_key}"}
	cfg := service.DefaultImageRuntimeConfig()
	for _, path := range []string{"/partial", "/busy", "/unavailable"} {
		_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + path}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
		require.ErrorIs(t, err, service.ErrUpstreamAsyncImageTransport, path)
		failure := classifyAsyncImageOutputFailure(model.AsyncImageTask{UpstreamTaskId: "vendor-job-1"}, err)
		assert.Equal(t, 606, failure.Code, path)
		assert.Equal(t, "result_download_failed", failure.InternalCode, path)
		assert.False(t, failure.ExecutionUnknown, path)
		assert.Equal(t, 607, classifyAsyncImageOutputFailure(model.AsyncImageTask{}, err).Code, path)
	}
	_, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/missing"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.Error(t, err)
	assert.NotErrorIs(t, err, service.ErrUpstreamAsyncImageTransport)
	images, err := service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "Bearer test-key", <-redirectAuthorization)
	_, err = service.DownloadUpstreamAsyncImages(t.Context(), []string{upstream.URL + "/redirect-private"}, profile, upstream.URL, "", service.UpstreamAsyncTemplateContext{APIKey: "test-key"}, cfg)
	require.ErrorContains(t, err, "not public")
	assert.Empty(t, privateRedirects)
}

func TestRecordAsyncImageUpstreamJobAfterDispatch(t *testing.T) {
	previousDB, previousType := model.DB, common.MainDatabaseType()
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "upstream-job.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}))
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	require.NoError(t, db.AutoMigrate(&model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "checkpoint-test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "checkpoint-test")

	now := time.Now().Unix()
	task := model.AsyncImageTask{
		TaskId:         "asyncimg_stale_after_dispatch",
		Status:         model.ImageTaskInvoking,
		Version:        1,
		LeaseToken:     "worker-lease",
		LeaseExpiresAt: now + 120,
		CreatedAt:      now,
	}
	require.NoError(t, db.Create(&task).Error)
	require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), task, 20))

	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.Equal(t, "vendor-job-1", task.UpstreamTaskId)
	assert.EqualValues(t, 3, task.Version)
	require.NoError(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-1"))
	assert.EqualValues(t, 3, task.Version)
	require.ErrorIs(t, recordAsyncImageUpstreamJob(t.Context(), &task, "vendor-job-2"), model.ErrImageConflict)

	var stored model.AsyncImageTask
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&stored).Error)
	assert.Equal(t, "vendor-job-1", stored.UpstreamTaskId)
	var accepted int64
	require.NoError(t, db.Model(&model.AsyncImageEvent{}).Where("task_id = ? AND event_type = ?", task.TaskId, "upstream_accepted").Count(&accepted).Error)
	assert.EqualValues(t, 1, accepted)

	request := service.AsyncImageRequest{Platform: "openai", Model: "checkpoint-image", Count: 1}
	completion := service.AsyncImageCompletion{EvaluatedAt: time.Date(2026, time.October, 13, 8, 15, 0, 0, time.UTC), Body: []byte(`{"data":[]}`), Usage: &dto.Usage{}}
	for _, name := range []string{"current_lease_after_dispatch", "expired_lease", "superseded_lease", "admin_terminated"} {
		t.Run(name, func(t *testing.T) {
			checkpointTask := model.AsyncImageTask{TaskId: "checkpoint-" + name, Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "checkpoint-worker", LeaseExpiresAt: now + 120, ChannelId: 20}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
			require.NoError(t, err)
			checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
			require.NoError(t, err)
			if name == "expired_lease" {
				checkpointTask.LeaseExpiresAt = now - 1
			}
			require.NoError(t, db.Create(&checkpointTask).Error)
			switch name {
			case "current_lease_after_dispatch":
				require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), checkpointTask, checkpointTask.ChannelId))
			case "superseded_lease":
				require.NoError(t, db.Model(&model.AsyncImageTask{}).Where("task_id = ?", checkpointTask.TaskId).Updates(map[string]any{"lease_token": "replacement-worker", "version": 2}).Error)
			case "admin_terminated":
				require.NoError(t, model.TerminateAsyncImageTask(t.Context(), checkpointTask))
			}
			var before model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&before).Error)
			err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, completion)
			if name == "current_lease_after_dispatch" {
				require.NoError(t, err, "a live owner must reload the version incremented by dispatch")
				assert.Equal(t, before.Version+1, checkpointTask.Version)
				assert.Equal(t, completion.EvaluatedAt.Unix(), checkpointTask.UpstreamSucceededAt)
				data, err := service.DecryptImagePayload(checkpointTask.RequestCipher, "task:"+checkpointTask.TaskId)
				require.NoError(t, err)
				var recovered service.AsyncImageRequest
				require.NoError(t, common.Unmarshal(data, &recovered))
				require.NotNil(t, recovered.Completion)
				assert.JSONEq(t, string(completion.Body), string(recovered.Completion.Body))
				laterCompletion := completion
				laterCompletion.EvaluatedAt = completion.EvaluatedAt.Add(time.Hour)
				require.NoError(t, service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, laterCompletion))
				assert.Equal(t, completion.EvaluatedAt.Unix(), checkpointTask.UpstreamSucceededAt, "repeat checkpoints must retain the first provider completion time")
				data, err = service.DecryptImagePayload(checkpointTask.RequestCipher, "task:"+checkpointTask.TaskId)
				require.NoError(t, err)
				var repeated service.AsyncImageRequest
				require.NoError(t, common.Unmarshal(data, &repeated))
				require.NotNil(t, repeated.Completion)
				assert.True(t, completion.EvaluatedAt.Equal(repeated.Completion.EvaluatedAt), "the saved pricing time must match the first completion marker")
				return
			}
			require.Error(t, err)
			var after model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&after).Error)
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.Version, after.Version)
			assert.Equal(t, before.RequestCipher, after.RequestCipher)
			assert.Equal(t, before.UpstreamSucceededAt, after.UpstreamSucceededAt, "a stale worker cannot revive or overwrite a task")
		})
	}

	var pngImage bytes.Buffer
	require.NoError(t, png.Encode(&pngImage, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	for _, tc := range []struct {
		name  string
		bytes int
	}{
		{name: "checkpoint_at_32_mib", bytes: 32 << 20},
		{name: "checkpoint_above_32_mib", bytes: (32 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("IMAGE_UPSTREAM_JOB_TEST_SMALL_PACKET") != "" {
				require.Equal(t, "mysql", db.Dialector.Name(), "small-packet mode requires the isolated MySQL fixture")
				t.Skip("the isolated small-packet fixture exercises its lower checkpoint limit")
			}
			response := map[string]any{"data": []dto.ImageData{{B64Json: base64.StdEncoding.EncodeToString(pngImage.Bytes())}}, "metadata": ""}
			body, err := common.Marshal(response)
			require.NoError(t, err)
			boundedCompletion := service.AsyncImageCompletion{EvaluatedAt: completion.EvaluatedAt, Body: body, Usage: &dto.Usage{}}
			encodedRequest := request
			encodedRequest.Completion = &boundedCompletion
			baseEnvelope, err := common.Marshal(encodedRequest)
			require.NoError(t, err)
			response["metadata"] = strings.Repeat("x", tc.bytes-len(baseEnvelope))
			boundedCompletion.Body, err = common.Marshal(response)
			require.NoError(t, err)
			checkpointTask := model.AsyncImageTask{TaskId: tc.name, Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "boundary-worker", LeaseExpiresAt: time.Now().Unix() + 120}
			requestBytes, err := common.Marshal(request)
			require.NoError(t, err)
			checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
			require.NoError(t, err)
			checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
			require.NoError(t, err)
			originalCipher := append([]byte(nil), checkpointTask.RequestCipher...)
			require.NoError(t, db.Create(&checkpointTask).Error)
			err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, boundedCompletion)
			if tc.name == "checkpoint_above_32_mib" {
				require.ErrorIs(t, err, service.ErrAsyncImageCompletionTooLarge)
			} else {
				require.NoError(t, err)
			}
			var stored model.AsyncImageTask
			require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&stored).Error)
			if err != nil {
				assert.Equal(t, originalCipher, stored.RequestCipher)
				assert.Zero(t, stored.UpstreamSucceededAt)
				assert.EqualValues(t, 1, stored.Version)
				return
			}
			assert.Positive(t, stored.UpstreamSucceededAt)
			data, err := service.DecryptImagePayload(stored.RequestCipher, "task:"+stored.TaskId)
			require.NoError(t, err)
			var recovered service.AsyncImageRequest
			require.NoError(t, common.Unmarshal(data, &recovered))
			require.NotNil(t, recovered.Completion)
			var imageResponse dto.ImageResponse
			require.NoError(t, common.Unmarshal(recovered.Completion.Body, &imageResponse))
			require.Len(t, imageResponse.Data, 1)
			assert.Equal(t, base64.StdEncoding.EncodeToString(pngImage.Bytes()), imageResponse.Data[0].B64Json)
		})
	}
	t.Run("checkpoint_respects_mysql_small_packet", func(t *testing.T) {
		if os.Getenv("IMAGE_UPSTREAM_JOB_TEST_SMALL_PACKET") == "" {
			t.Skip("requires an isolated MySQL instance configured with a 4 MiB packet")
		}
		require.Equal(t, "mysql", db.Dialector.Name())
		var packetBytes int64
		require.NoError(t, db.Raw("SELECT @@max_allowed_packet").Scan(&packetBytes).Error)
		require.EqualValues(t, 4<<20, packetBytes, "configure the disposable instance before starting the test process")
		response := map[string]any{"data": []dto.ImageData{{B64Json: base64.StdEncoding.EncodeToString(pngImage.Bytes())}}, "metadata": ""}
		body, err := common.Marshal(response)
		require.NoError(t, err)
		response["metadata"] = strings.Repeat("x", (3<<20)-len(body))
		body, err = common.Marshal(response)
		require.NoError(t, err)
		checkpointTask := model.AsyncImageTask{TaskId: "checkpoint-small-mysql-packet", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "small-packet-worker", LeaseExpiresAt: time.Now().Unix() + 120}
		requestBytes, err := common.Marshal(request)
		require.NoError(t, err)
		checkpointTask.RequestCipher, err = service.EncryptImagePayload(requestBytes, "task:"+checkpointTask.TaskId)
		require.NoError(t, err)
		checkpointTask.SelectedChannelCipher, err = service.EncryptImagePayload([]byte(`{"id":20,"key":"synthetic-key"}`), "image-channel:"+checkpointTask.TaskId)
		require.NoError(t, err)
		originalCipher := append([]byte(nil), checkpointTask.RequestCipher...)
		require.NoError(t, db.Create(&checkpointTask).Error)
		err = service.PersistAsyncImageCompletion(t.Context(), &checkpointTask, request, service.AsyncImageCompletion{Body: body, Usage: &dto.Usage{}})
		require.ErrorIs(t, err, service.ErrAsyncImageCompletionTooLarge)
		var stored model.AsyncImageTask
		require.NoError(t, db.Where("task_id = ?", checkpointTask.TaskId).Take(&stored).Error)
		assert.Equal(t, originalCipher, stored.RequestCipher, "an output exceeding the server packet budget must not overwrite encrypted evidence")
		assert.Zero(t, stored.UpstreamSucceededAt)
		assert.EqualValues(t, 1, stored.Version)
	})
}

func TestTianyueSynchronousImageCompletionRecovery(t *testing.T) {
	previousDB, previousType, previousCount, previousRedis := model.DB, common.MainDatabaseType(), constant.CountToken, common.RedisEnabled
	var dialector gorm.Dialector = sqlite.Open(filepath.Join(t.TempDir(), "tianyue-sync-completion.db"))
	if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_MYSQL_DSN"); dsn != "" {
		require.Contains(t, dsn, "/new_api_image_job_test", "use a dedicated disposable database")
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("IMAGE_UPSTREAM_JOB_TEST_PG_DSN"); dsn != "" {
		require.True(t, strings.Contains(dsn, "dbname=new_api_image_job_test") || strings.Contains(dsn, "/new_api_image_job_test"), "use a dedicated disposable database")
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB, constant.CountToken, common.RedisEnabled = db, false, false
	common.SetMainDatabaseType(common.DatabaseType(db.Dialector.Name()))
	t.Cleanup(func() {
		model.DB, constant.CountToken, common.RedisEnabled = previousDB, previousCount, previousRedis
		common.SetMainDatabaseType(previousType)
		assert.NoError(t, db.Migrator().DropTable(&model.AsyncImageBill{}, &model.AsyncImageEvent{}, &model.AsyncImageTask{}, &model.UserSubscription{}, &model.Token{}, &model.User{}))
		connection, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.AsyncImageTask{}, &model.AsyncImageEvent{}, &model.AsyncImageBill{}))
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "tianyue-sync:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "tianyue-sync")
	user := model.User{Username: "tianyue-sync-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "tianyue-sync-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, db.Create(&token).Error)
	service.InitHttpClient()
	var bitmap bytes.Buffer
	require.NoError(t, png.Encode(&bitmap, image.NewRGBA(image.Rect(0, 0, 2, 3))))
	received := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Method + " " + r.URL.Path
		assert.Equal(t, "Bearer tianyue-upstream-key", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(bitmap.Bytes())
		default:
			t.Errorf("historical completion recovery sent an unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	channel := &model.Channel{Id: 34, Type: constant.ChannelTypeNewAPI, Key: "tianyue-upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
	channel.SetSetting(dto.ChannelSettings{TianyueMediaCompatibility: true})
	request := service.AsyncImageRequest{
		Platform: "openai", Provider: "newapi", Dialect: "async", Model: "seedream-5-pro", Prompt: "test picture", Count: 1,
		Kind: "text_to_image", Size: "1024x1024", Resolution: "1K", SourcePath: "/v1/images/generations_async",
		Native:  map[string]common.RawMessage{"model": common.RawMessage(`"seedream-5-pro"`), "prompt": common.RawMessage(`"test picture"`), "n": common.RawMessage(`1`)},
		Billing: &service.MediaBillingSnapshot{Price: types.PriceData{UsePrice: true, ModelPrice: 0.01, QuotaToPreConsume: 5000, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}},
	}
	task := model.AsyncImageTask{TaskId: "tianyue-sync-completion", UserId: user.Id, TokenId: token.Id, ChannelId: channel.Id, Group: "default", Status: model.ImageTaskInvoking, Version: 1, LeaseToken: "worker", LeaseExpiresAt: time.Now().Unix() + 120}
	payload, err := common.Marshal(request)
	require.NoError(t, err)
	task.RequestCipher, err = service.EncryptImagePayload(payload, "task:"+task.TaskId)
	require.NoError(t, err)
	channelPayload, err := common.Marshal(channel)
	require.NoError(t, err)
	task.SelectedChannelCipher, err = service.EncryptImagePayload(channelPayload, "image-channel:"+task.TaskId)
	require.NoError(t, err)
	require.NoError(t, db.Create(&task).Error)
	// Simulate a pre-upgrade synchronous response that was durably accepted
	// before the worker stopped. New submissions use the asynchronous endpoint.
	require.NoError(t, model.ReserveAsyncImageQuota(t.Context(), task, 5000, model.ImageFundingSelection{Source: "wallet"}))
	require.NoError(t, model.MarkAsyncImageDispatched(t.Context(), task, channel.Id))
	completion := service.AsyncImageCompletion{
		EvaluatedAt: time.Now(), Body: []byte(`{"created":1,"data":[{"url":"/image.png"}]}`), Usage: &dto.Usage{},
		Billing: *request.Billing, UpstreamModel: request.Model, BillingModel: request.Model,
		URLs:    []string{upstream.URL + "/image.png"},
		Profile: &dto.UpstreamAsyncProfile{ID: "tianyue-sync-image", MediaType: dto.UpstreamAsyncMediaImage, Poll: dto.UpstreamAsyncPoll{Response: dto.UpstreamAsyncPollResponse{DownloadHeaders: map[string]string{"Authorization": "Bearer {api_key}"}}}},
	}
	require.NoError(t, service.PersistAsyncImageCompletion(t.Context(), &task, request, completion))
	payload, err = service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(payload, &request))
	dispatches := 0
	output, bill, err := ExecuteAsyncImage(t.Context(), task, request, channel, service.DefaultImageRuntimeConfig(), func() error {
		dispatches++
		return model.MarkAsyncImageDispatched(t.Context(), task, channel.Id)
	})
	require.NoError(t, err, "a historical synchronous completion must finish without a provider job ID")
	require.NotNil(t, output)
	require.Len(t, output.Images, 1)
	assert.Equal(t, 2, output.Images[0].Width)
	assert.Equal(t, 3, output.Images[0].Height)
	assert.Equal(t, 5000, bill.Quota)
	require.Len(t, received, 1)
	assert.Equal(t, "GET /image.png", <-received)
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Take(&task).Error)
	assert.Empty(t, task.UpstreamTaskId, "synchronous completion must not invent a provider job")
	assert.Positive(t, task.UpstreamSucceededAt)
	payload, err = service.DecryptImagePayload(task.RequestCipher, "task:"+task.TaskId)
	require.NoError(t, err)
	var recovered service.AsyncImageRequest
	require.NoError(t, common.Unmarshal(payload, &recovered))
	require.NotNil(t, recovered.Completion)
	require.NotNil(t, recovered.Completion.Profile)
	assert.Equal(t, []string{upstream.URL + "/image.png"}, recovered.Completion.URLs)
	recoveredOutput, recoveredBill, err := ExecuteAsyncImage(t.Context(), task, recovered, channel, service.DefaultImageRuntimeConfig(), func() error {
		dispatches++
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, recoveredOutput)
	require.Len(t, recoveredOutput.Images, 1)
	assert.Equal(t, output.Images[0].Data, recoveredOutput.Images[0].Data)
	assert.Equal(t, bill.Quota, recoveredBill.Quota)
	require.Len(t, received, 1)
	assert.Equal(t, "GET /image.png", <-received)
	assert.Empty(t, received, "checkpoint recovery must only download the accepted image")
	assert.Zero(t, dispatches, "historical completion recovery must not submit a new generation")
	require.NoError(t, db.Take(&user, user.Id).Error)
	assert.Equal(t, 995000, user.Quota, "checkpoint recovery must reuse the existing reservation")
	var reservations int64
	require.NoError(t, db.Model(&model.AsyncImageBill{}).Where("task_id = ?", task.TaskId).Count(&reservations).Error)
	assert.EqualValues(t, 1, reservations)
}

func TestBuildGeminiAsyncImagePartsReferenceTransport(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "gemini-reference-transport.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		connection, openErr := db.DB()
		if openErr == nil {
			_ = connection.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.ImageInputAlias{}))

	t.Run("passes external URL to upstream before fallback", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "passthrough_fallback_local"
		referenceURL := "https://signed.example/reference.jpg?token=private"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: referenceURL},
		}}

		parts, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.NoError(t, err)
		require.Len(t, parts, 2)
		require.NotNil(t, parts[1].FileData)
		assert.Equal(t, referenceURL, parts[1].FileData.FileUri)
		assert.Empty(t, parts[1].FileData.MimeType)
		assert.Nil(t, parts[1].InlineData)
	})

	t.Run("indexes invalid locally transported reference without URL", func(t *testing.T) {
		cfg := service.DefaultImageRuntimeConfig()
		cfg.GeminiReferenceMode = "local"
		request := service.AsyncImageRequest{Parts: []service.AsyncImageInputPart{
			{Type: "text", Text: "edit"},
			{Type: "image_url", URL: "data:image/jpeg;base64,bm90LWltYWdl"},
		}}

		_, err := buildGeminiAsyncImageParts(t.Context(), model.AsyncImageTask{TokenId: 7}, request, cfg)
		require.Error(t, err)
		var failure *service.AsyncImageFailure
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, 604, failure.Code)
		assert.Equal(t, "invalid_reference_image", failure.InternalCode)
		assert.Equal(t, "Reference image 1: invalid image header", failure.Message)
		assert.NotContains(t, failure.Message, "bm90LWltYWdl")
	})
}
