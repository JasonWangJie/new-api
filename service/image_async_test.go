package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func asyncImageBytesFixture(t *testing.T) ImageBytes {
	t.Helper()
	bitmap := image.NewRGBA(image.Rect(0, 0, 3, 2))
	bitmap.Set(0, 0, color.RGBA{R: 255, A: 255})
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, bitmap))
	valid, err := ValidateImageBytes(output.Bytes(), "image/png", 1<<20, 100)
	require.NoError(t, err)
	return valid
}

func TestAsyncImageDurableReferenceRoutingAndRedisIsolation(t *testing.T) {
	asyncImageKeyFixture(t)
	ctx := context.Background()
	var history []ImageChannelAttempt
	for _, expected := range []struct {
		channel, pin int
		stop         bool
	}{{10, 10, false}, {10, 10, false}, {10, 0, false}, {20, 0, true}} {
		history = append(history, ImageChannelAttempt{ChannelId: expected.channel, Dispatched: true, ReferenceFailure: true, Code: 602})
		encoded, err := common.Marshal(history)
		require.NoError(t, err)
		history = nil
		require.NoError(t, common.Unmarshal(encoded, &history))
		pin, excluded, stop := ImageAttemptRouting(history, 2)
		assert.Equal(t, expected.pin, pin)
		assert.Equal(t, expected.stop, stop)
		if expected.pin == 0 {
			assert.Equal(t, []int{10}, excluded)
		}
	}
	pin, excluded, stop := ImageAttemptRouting([]ImageChannelAttempt{{ChannelId: 10, Code: 602, ReferenceFailure: false}}, 2)
	assert.Zero(t, pin)
	assert.Empty(t, excluded)
	assert.False(t, stop)
	accountA := ImageChannelAttempt{ChannelId: 10, KeyFingerprint: "account-a", Dispatched: true, ReferenceFailure: true}
	accountB := accountA
	accountB.KeyFingerprint = "account-b"
	for _, tc := range []struct {
		history  []ImageChannelAttempt
		pin      string
		excluded []string
		stop     bool
	}{
		{[]ImageChannelAttempt{accountA}, "account-a", nil, false},
		{[]ImageChannelAttempt{accountA, accountA}, "account-a", nil, false},
		{[]ImageChannelAttempt{accountA, accountA, accountA}, "", []string{"account-a"}, false},
		{[]ImageChannelAttempt{accountA, accountA, accountA, accountB}, "", nil, true},
	} {
		routing := ImageAccountAttemptRouting(tc.history, 2, "openai", 3)
		assert.Equal(t, tc.pin, routing.Pin)
		assert.Equal(t, tc.excluded, routing.Excluded)
		assert.Equal(t, tc.stop, routing.Stop)
	}
	assert.True(t, ImageAccountAttemptRouting([]ImageChannelAttempt{accountA, accountA, accountA}, 2, "gemini", 0).Stop)
	accountA.ReferenceFailure, accountB.ReferenceFailure = false, false
	assert.Equal(t, "account-b", ImageAccountAttemptRouting([]ImageChannelAttempt{accountA, accountB}, 2, "gemini", 1).Pin)
	accountChannel := model.Channel{Id: 10, Key: "key-a\nkey-b", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	available := ImageChannelAccounts(accountChannel, ImageAccountRouting{})
	require.Len(t, available, 2)
	remaining := ImageChannelAccounts(accountChannel, ImageAccountRouting{Excluded: []string{available[0].Fingerprint}})
	require.Len(t, remaining, 1)
	assert.Equal(t, "key-b", remaining[0].Key)
	accountChannel.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	assert.Empty(t, ImageChannelAccounts(accountChannel, ImageAccountRouting{Pin: available[1].Fingerprint}))
	assert.Equal(t, 60, ImageRetryDelay(15, 60, 5, 0, 0, 900))
	assert.Equal(t, 900, ImageRetryDelay(15, 60, 0, 0, 3600*time.Second, 900))
	classified := ClassifyAsyncImageFailure(errors.New("image_url fetch failed: Bearer private-key https://signed.example/?secret=token"), 400)
	assert.Equal(t, 602, classified.Code)
	assert.NotContains(t, classified.Message, "private-key")
	assert.NotContains(t, classified.Message, "secret")
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	previous := common.RDB
	common.RDB = client
	t.Cleanup(func() { common.RDB = previous })
	t.Setenv("ASYNC_IMAGE_REDIS_PREFIX", "image-unit-test")
	cfg := DefaultImageRuntimeConfig()
	cfg.CircuitBreaker = true
	cfg.FailureThreshold = 2
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, false, cfg))
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, false, cfg))
	allowed, err := ImageCircuitAllows(ctx, "async", 10, cfg)
	require.NoError(t, err)
	assert.False(t, allowed)
	allowed, err = ImageCircuitAllows(ctx, "sync", 10, cfg)
	require.NoError(t, err)
	assert.True(t, allowed)
	require.NoError(t, RecordImageCircuit(ctx, "async", 10, true, cfg))
	allowed, err = ImageCircuitAllows(ctx, "async", 10, cfg)
	require.NoError(t, err)
	assert.True(t, allowed)
	valid := asyncImageBytesFixture(t)
	raw := "https://reference.example/image.png"
	identity := ImageIdentityHash("1", raw)
	plain, err := common.Marshal(valid)
	require.NoError(t, err)
	cipher, err := EncryptImagePayload(plain, "reference:"+identity)
	require.NoError(t, err)
	keys := []string{ImageRedisKey("reference-cache", "data"), ImageRedisKey("reference-cache", "expires"), ImageRedisKey("reference-cache", "sizes"), ImageRedisKey("reference-cache", "bytes")}
	result, err := imageReferenceCacheWrite.Run(ctx, client, keys, time.Now().Unix(), identity, time.Now().Unix()+60, len(cipher), len(cipher), cipher, 60).Int()
	require.NoError(t, err)
	assert.Equal(t, 1, result)
	result, err = imageReferenceCacheWrite.Run(ctx, client, keys, time.Now().Unix(), "another", time.Now().Unix()+60, len(cipher), len(cipher), cipher, 60).Int()
	require.NoError(t, err)
	assert.Zero(t, result, "the shared byte budget must reject another entry")
	cfg.ReferenceConcurrency = 1
	require.NoError(t, client.ZAdd(ctx, ImageRedisKey("reference-download-slots"), &redis.Z{Score: float64(time.Now().Unix() + 60), Member: "busy"}).Err())
	hit, err := DownloadImageReferenceCached(ctx, 1, raw, cfg)
	require.NoError(t, err)
	assert.Equal(t, valid.Data, hit.Data)
	_, err = DownloadImageReferenceCached(ctx, 2, raw, cfg)
	var failure *AsyncImageFailure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, 603, failure.Code, "another Token cannot reuse private cached bytes")
}

func asyncImageKeyFixture(t *testing.T) {
	t.Helper()
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "test")
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

func TestAsyncImageCipherBindingRotationAndMissingKeys(t *testing.T) {
	asyncImageKeyFixture(t)
	ciphertext, err := EncryptImagePayload([]byte("private prompt"), "task:1")
	require.NoError(t, err)
	assert.NotContains(t, string(ciphertext), "private prompt")
	plain, err := DecryptImagePayload(ciphertext, "task:1")
	require.NoError(t, err)
	assert.Equal(t, "private prompt", string(plain))
	_, err = DecryptImagePayload(ciphertext, "task:2")
	assert.Error(t, err)
	ciphertext[len(ciphertext)-1] ^= 1
	_, err = DecryptImagePayload(ciphertext, "task:1")
	assert.Error(t, err)
	ciphertext[len(ciphertext)-1] ^= 1
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "test:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))+",next:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	t.Setenv("ASYNC_IMAGE_ACTIVE_KEY_ID", "next")
	_, err = DecryptImagePayload(ciphertext, "task:1")
	assert.NoError(t, err)
	t.Setenv("ASYNC_IMAGE_PAYLOAD_KEYS", "")
	_, err = EncryptImagePayload([]byte("secret"), "task:1")
	assert.Error(t, err)
}

func TestAsyncImageCleanupPreviewActorScopeExpiryAndTamper(t *testing.T) {
	asyncImageKeyFixture(t)
	filters := ImageCleanupFilters{UserId: 10}
	now := int64(1000)
	preview, err := SignImageCleanupPreview("expired", filters, 7, now)
	require.NoError(t, err)
	assert.True(t, ValidateImageCleanupPreview(preview, "expired", filters, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", filters, 8, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "all", filters, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", ImageCleanupFilters{UserId: 11}, 7, now+1))
	assert.False(t, ValidateImageCleanupPreview(preview, "expired", filters, 7, now+600))
	assert.False(t, ValidateImageCleanupPreview(preview+"tampered", "expired", filters, 7, now+1))
}

type imageResolverFixture struct{ addresses []netip.Addr }

func (fixture imageResolverFixture) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return fixture.addresses, nil
}

type imagePeerFixture struct {
	net.Conn
	peer   net.Addr
	closed bool
}

func (fixture *imagePeerFixture) RemoteAddr() net.Addr { return fixture.peer }
func (fixture *imagePeerFixture) Close() error         { fixture.closed = true; return nil }

func TestAsyncImageReferenceDNSRedirectAndConnectedPeer(t *testing.T) {
	cfg := DefaultImageRuntimeConfig()
	for _, tc := range []struct {
		name      string
		addresses []netip.Addr
		peer      string
		reject    bool
		dialed    bool
	}{
		{"mixed DNS", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, "8.8.8.8", true, false},
		{"private peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "127.0.0.1", true, true},
		{"changed peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "9.9.9.9", true, true},
		{"pinned public peer", []netip.Addr{netip.MustParseAddr("8.8.8.8")}, "8.8.8.8", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &imagePeerFixture{peer: &net.TCPAddr{IP: net.ParseIP(tc.peer), Port: 443}}
			var target string
			client := imageReferenceClient(cfg, imageResolverFixture{tc.addresses}, func(_ context.Context, _, address string) (net.Conn, error) { target = address; return peer, nil })
			conn, err := client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "reference.example:443")
			assert.Equal(t, tc.reject, err != nil)
			assert.Equal(t, tc.dialed, target != "")
			if tc.dialed {
				assert.Equal(t, "8.8.8.8:443", target)
			}
			if tc.reject && tc.dialed {
				assert.True(t, peer.closed)
			}
			if !tc.reject {
				require.NotNil(t, conn)
				require.NoError(t, conn.Close())
			}
			for _, location := range []string{"https://127.0.0.1/private", "http://reference.example/x", "https://user:secret@reference.example/x"} {
				parsed, parseErr := url.Parse(location)
				require.NoError(t, parseErr)
				assert.Error(t, client.CheckRedirect(&http.Request{URL: parsed}, []*http.Request{{}}))
			}
			parsed, err := url.Parse("https://public.example/x")
			require.NoError(t, err)
			assert.NoError(t, client.CheckRedirect(&http.Request{URL: parsed}, []*http.Request{{}}))
			assert.Error(t, client.CheckRedirect(&http.Request{URL: parsed}, make([]*http.Request, cfg.DownloadRedirects+1)))
		})
	}
}

func TestAsyncImageProtocolDimensionsAndRawIdempotency(t *testing.T) {
	for _, test := range []struct{ resolution, ratio, size string }{{"1K", "21:9", "2384x1024"}, {"2K", "5:4", "2048x1632"}, {"4K", "4:5", "3272x4096"}, {"2K", "16:9", "2048x1152"}, {"1K", "auto", "auto"}} {
		size, err := MapOpenAIImageDimensions(test.resolution, test.ratio)
		require.NoError(t, err)
		assert.Equal(t, test.size, size)
	}
	_, err := MapOpenAIImageDimensions("invalid", "auto")
	assert.Error(t, err)
	assert.Equal(t, "4K", ImageNativeTier(2384, 1024, "openai"))
	assert.Equal(t, "1K", ImageNativeTier(2384, 1024, "gemini"))
	assert.Equal(t, "2K", ImageNativeTier(2048, 1152, "openai"))
	assert.Equal(t, "2K", ImageNativeTier(2048, 1152, "gemini"))
	first := AsyncImageRequestHash("openai", "bb", "/v1/images/generations_oa", []byte(`{"prompt":"x"}`))
	assert.NotEqual(t, first, AsyncImageRequestHash("openai", "bb", "/v1/images/generations_oa", []byte(`{ "prompt":"x" }`)))
	assert.NotEqual(t, first, AsyncImageRequestHash("openai", "bb", "/v1/images/edits_oa", []byte(`{"prompt":"x"}`)))
	request, err := ParseAsyncImageRequest([]byte(`{"model":"gpt-image-2","prompt":"x","resolution":"1K","aspect_ratio":"21:9","output_compression":0,"stream":false}`), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "2384x1024", request.Size)
	assert.True(t, request.ExplicitTier)
	assert.Equal(t, "0", string(request.Native["output_compression"]))
	assert.Equal(t, "false", string(request.Native["stream"]))
	request, err = ParseAsyncImageRequest([]byte(`{"prompt":"x","size":"auto","resolution":"1K"}`), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "AUTO", request.Resolution)
	assert.Equal(t, "4K", ImageBillingSpecifications(request, []ImageBytes{{Width: 4096, Height: 4096}})[0]["resolution"])
	for _, body := range []string{`{"prompt":"x","n":0}`, `{"prompt":"x","n":129}`, `{"prompt":"x","n":18446744073709551615}`, `{"prompt":"x","stream":true}`, `{"prompt":"x","mask":{"image_url":"x"}}`} {
		_, err := ParseAsyncImageRequest([]byte(body), "application/json", "/v1/images/generations_oa", DefaultImageRuntimeConfig())
		assert.Error(t, err, body)
	}
}

func TestAsyncImageFixedBillMixedSpecificationsAndUsageBoundaries(t *testing.T) {
	previous := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "bill.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previous
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}))
	user := model.User{Username: "image-bill-user", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/images/generations_oa", nil)
	expression := `param("resolution") == "4K" ? tier("large", fixed(0.04)) * image_count : tier("small", fixed(0.01)) * image_count`
	count := 2
	info := &relaycommon.RelayInfo{UserId: user.Id, OriginModelName: "image-model", ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "image-model"}, StartTime: time.Now(), Request: &dto.ImageRequest{}, TieredBillingSnapshot: &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000, EstimatedImageCount: &count}, PriceData: types.PriceData{GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
	task := model.AsyncImageTask{TaskId: "mixed-specifications", UserId: user.Id, TokenId: 1, ChannelId: 1, Model: "image-model", Group: "default"}
	request := AsyncImageRequest{Platform: "openai", Model: "image-model", Count: 7, Size: "auto", Resolution: "AUTO", Prompt: "private prompt", Native: map[string]common.RawMessage{"quality": common.RawMessage(`"high"`)}}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(`{"model":"image-model","n":7,"size":"auto"}`)}
	images := []ImageBytes{{Width: 1024, Height: 1024}, {Width: 4096, Height: 4096}}
	usage := &dto.Usage{PromptTokens: 1, TotalTokens: 1}
	bill, err := FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	require.NoError(t, err)
	assert.Equal(t, 25000, bill.Quota, "sum 1K + 4K, never largest tier multiplied by count")
	assert.NotContains(t, bill.Snapshot, "private prompt")
	assert.Contains(t, bill.Snapshot, `"n":7`)
	assert.Contains(t, bill.Snapshot, `"quality":"high"`)
	usage.PromptTokens, usage.TotalTokens = 0, 0
	zeroUsageBill, err := FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	require.NoError(t, err)
	assert.Equal(t, 25000, zeroUsageBill.Quota, "image-count expressions charge actual outputs with zero tokens")
	var persistedUsage dto.Usage
	require.NoError(t, common.UnmarshalJsonStr(zeroUsageBill.Usage, &persistedUsage))
	assert.Zero(t, persistedUsage.PromptTokens)
	assert.Zero(t, persistedUsage.TotalTokens)
	for _, price := range []struct {
		unitPrice float64
		quota     int
	}{{0.01, 10000}, {0.1, 100000}} {
		legacyInfo := *info
		legacyInfo.TieredBillingSnapshot = nil
		legacyInfo.PriceData = types.PriceData{UsePrice: true, ModelPrice: price.unitPrice, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
		legacyBill, err := FixAsyncImageBill(c, task, &legacyInfo, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
		require.NoError(t, err)
		assert.Equal(t, price.quota, legacyBill.Quota, "both actual images are charged at unit price %g", price.unitPrice)
	}
	assert.Zero(t, usage.PromptTokens, "billing must not fabricate upstream usage")
	for _, tc := range []struct {
		name       string
		expression string
		usage      dto.Usage
		quota      int
		wantError  bool
	}{
		{"two images at 0.1 each cost 0.2", `tier("images", fixed(0.1)) * image_count`, dto.Usage{}, 100000, false},
		{"token fallback is charged once", `len > 100 ? tier("tokens", p * 2) : tier("images", fixed(0.01)) * image_count`, dto.Usage{PromptTokens: 200, TotalTokens: 200}, 200, false},
		{"batch rounding happens once", `tier("images", fixed(0.000001)) * image_count`, dto.Usage{}, 1, false},
		{"mixed units cannot duplicate aggregate usage", `param("resolution") == "4K" ? tier("tokens", p * 2) : tier("images", fixed(0.01)) * image_count`, dto.Usage{PromptTokens: 200, TotalTokens: 200}, 0, true},
		{"missing paid token usage cannot be silently free", `tier("tokens", p * 2 + c * 8)`, dto.Usage{}, 0, true},
		{"estimated token usage cannot replace actual usage", `tier("tokens", p * 2 + c * 8)`, dto.Usage{PromptTokens: 200, TotalTokens: 200, BillingUsage: &dto.BillingUsage{Estimated: true}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caseInfo := *info
			caseInfo.PriceData = types.PriceData{QuotaToPreConsume: 100, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}
			caseInfo.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000, EstimatedImageCount: &count}
			if !billingexpr.UsedVarsByHash(tc.expression, caseInfo.TieredBillingSnapshot.ExprHash)["image_count"] {
				caseInfo.TieredBillingSnapshot.EstimatedImageCount = nil
			}
			actual, err := FixAsyncImageBill(c, task, &caseInfo, &tc.usage, images, request, model.ImageFundingSelection{Source: "wallet"})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.quota, actual.Quota)
		})
	}
	request.ExplicitTier, request.Resolution, request.Size = true, "1K", "2384x1024"
	assert.Equal(t, "1K", ImageBillingSpecifications(request, images)[1]["resolution"])
	request.ExplicitTier, request.Resolution, request.Size = false, "", "4096x2304"
	assert.Equal(t, "4K", ImageBillingSpecifications(request, images)[0]["resolution"])
	usage.PromptTokens = -1
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
	usage.PromptTokens = common.MaxQuota + 1
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
	usage.PromptTokens = 1
	info.TieredBillingSnapshot.ExprString = `tier("huge", fixed(1000000)) * image_count`
	info.TieredBillingSnapshot.ExprHash = billingexpr.ExprHashString(info.TieredBillingSnapshot.ExprString)
	_, err = FixAsyncImageBill(c, task, info, usage, images, request, model.ImageFundingSelection{Source: "wallet"})
	assert.Error(t, err)
}

func TestAsyncImageGeminiOrderedParts(t *testing.T) {
	body := `{"model":"gemini-image","messages":[{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":"https://example.com/image.png"}},{"type":"text","text":"last"}]}],"extra_body":{"google":{"image_config":{"image_size":"2K","aspect_ratio":"auto"}}}}`
	request, err := ParseAsyncImageRequest([]byte(body), "application/json", "/v1/chat/completions_gm", DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, "first\nlast", request.Prompt)
	assert.Equal(t, "image_url", request.Parts[1].Type)
	assert.Equal(t, "last", request.Parts[2].Text)
	assert.Equal(t, "2K", request.Resolution)
	assert.Empty(t, request.AspectRatio)
	_, err = ParseAsyncImageRequest([]byte(`{"model":"gemini-image","prompt":"x","size":"2048x1152"}`), "application/json", "/v1/images/generations_sc", DefaultImageRuntimeConfig())
	assert.NoError(t, err)
}

func TestAsyncImageValidationContainerMIMEAndPublicAddress(t *testing.T) {
	valid := asyncImageBytesFixture(t)
	_, err := ValidateImageBytes(valid.Data, "image/jpeg", 1<<20, 100)
	assert.Error(t, err)
	_, err = ValidateImageBytes(valid.Data, "image/png", 1<<20, 5)
	assert.Error(t, err)
	_, err = ValidateImageBytes(append(bytes.Clone(valid.Data), []byte("trailing")...), "image/png", 1<<20, 100)
	assert.Error(t, err)
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	_, err = ValidateImageBytes(append(encoded.Bytes(), 0xff, 0xd9), "image/jpeg", 1<<20, 100)
	assert.Error(t, err)
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1"} {
		assert.False(t, ImagePublicIP(netip.MustParseAddr(address)), address)
	}
	assert.True(t, ImagePublicIP(netip.MustParseAddr("8.8.8.8")))
	for _, value := range []string{"http://example.com/x", "https://user:pass@example.com/x", "https://127.0.0.1/x", "https://[::ffff:127.0.0.1]/x"} {
		_, err := ValidateImageReferenceURL(value)
		assert.Error(t, err, value)
	}
	_, err = ValidateImageReferenceURL("https://example.com:8443/x")
	assert.NoError(t, err)
	decoded, err := DownloadImageReference(context.Background(), valid.DataURL(), DefaultImageRuntimeConfig())
	require.NoError(t, err)
	assert.Equal(t, valid.Checksum, decoded.Checksum)
}

func TestAsyncImageLocalStorageAndDurableIntentRetry(t *testing.T) {
	asyncImageKeyFixture(t)
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "storage.db")), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	require.NoError(t, model.MigrateImageModels(db))
	require.NoError(t, model.MigrateImageModels(db))
	profile, err := SaveImageStorageProfile(context.Background(), model.ImageStorageProfile{Class: "temporary", Backend: "local", Root: t.TempDir(), Active: true}, nil)
	require.NoError(t, err)
	valid := asyncImageBytesFixture(t)
	store, err := OpenImageStorage(context.Background(), profile)
	require.NoError(t, err)
	key := "results/2026/09/16/image.png"
	intent := model.ImageUploadIntent{IntentKey: "deterministic-intent", Class: "temporary", ProfileId: profile.ProfileId, ObjectKey: key, Checksum: valid.Checksum, ByteSize: int64(len(valid.Data)), ContentType: valid.ContentType, Status: "pending", CreatedAt: time.Now().Unix()}
	first, err := StoreImageIntent(context.Background(), intent, valid, time.Now().Unix()+3600)
	require.NoError(t, err)
	second, err := StoreImageIntent(context.Background(), intent, valid, time.Now().Unix()+3600)
	require.NoError(t, err)
	assert.Equal(t, first.ObjectId, second.ObjectId)
	var count int64
	require.NoError(t, db.Model(&model.ImageStorageObject{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	read, err := store.Read(context.Background(), key, int64(len(valid.Data)))
	require.NoError(t, err)
	assert.Equal(t, valid.Data, read)
	for _, key := range []string{"../outside.png", "/outside.png", "C:/outside.png", "a/../../outside.png", "a\\outside.png"} {
		_, err := store.LocalPath(key, true)
		assert.Error(t, err, key)
	}
	expires := strconv.FormatInt(time.Now().Unix()+60, 10)
	signed, err := ImageObjectURL(context.Background(), first, "https://gateway.example", 60)
	require.NoError(t, err)
	assert.Contains(t, signed, "/api/image-objects/")
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, expires, "test", "invalid"))
	link, err := url.Parse(signed)
	require.NoError(t, err)
	query := link.Query()
	assert.True(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires"), query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature("another-object", query.Get("expires"), query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, "0", query.Get("key"), query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires"), "unknown-key", query.Get("signature")))
	assert.False(t, VerifyImageObjectSignature(first.ObjectId, query.Get("expires")+"0", query.Get("key"), query.Get("signature")))
	require.NoError(t, store.Delete(context.Background(), key))
	require.NoError(t, store.Delete(context.Background(), key))
	_, err = store.Read(context.Background(), key, 1<<20)
	assert.ErrorIs(t, err, ErrImageObjectMissing)
	_, err = os.Stat(filepath.Join(profile.Root, "outside.png"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
