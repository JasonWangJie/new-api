package service

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type imageHostingTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (transport imageHostingTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copyURL := *request.URL
	copy.URL = &copyURL
	copy.URL.Scheme, copy.URL.Host = transport.target.Scheme, transport.target.Host
	return transport.base.RoundTrip(copy)
}

func TestImageHostingConfigurationEncryptsPreservesAndClearsToken(t *testing.T) {
	db, _, _, _, _ := asyncImageWorkerFixture(t)
	cfg, err := GetImageHostingConfig(t.Context())
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	assert.True(t, cfg.ImageEnabled)
	cfg.Enabled, cfg.BaseURL = true, "https://hosting.example.com"
	cfg.ExtraFields["policy_id"], cfg.ExtraFields["expires_in"], cfg.ExtraFields["watermark"] = 3, 3600, false
	saved, err := SaveImageHostingConfig(t.Context(), cfg, "hosting-secret-value", false, 0)
	require.NoError(t, err)
	assert.True(t, saved.HasToken)
	assert.EqualValues(t, 1, saved.Revision)
	loadedScalars, err := GetImageHostingConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, float64(3), loadedScalars.ExtraFields["policy_id"])
	assert.Equal(t, float64(3600), loadedScalars.ExtraFields["expires_in"])
	assert.Equal(t, false, loadedScalars.ExtraFields["watermark"])
	var option model.Option
	require.NoError(t, db.Where(map[string]any{"key": imageHostingOption}).Take(&option).Error)
	assert.NotContains(t, option.Value, "hosting-secret-value")
	public, err := common.Marshal(saved)
	require.NoError(t, err)
	assert.NotContains(t, string(public), "hosting-secret-value")
	assert.NotContains(t, string(public), "token_cipher")
	envelope, err := getImageHostingEnvelope(t.Context())
	require.NoError(t, err)
	plain, err := DecryptImagePayload(envelope.TokenCipher, "image-hosting-token:"+cfg.BaseURL)
	require.NoError(t, err)
	assert.Equal(t, "hosting-secret-value", string(plain))
	saved.TimeoutSeconds = 15
	saved.ExtraFields = map[string]any{}
	saved, err = SaveImageHostingConfig(t.Context(), saved, "", false, saved.Revision)
	require.NoError(t, err)
	assert.True(t, saved.HasToken)
	loaded, err := GetImageHostingConfig(t.Context())
	require.NoError(t, err)
	assert.Empty(t, loaded.ExtraFields, "an explicit empty map must not restore public visibility")
	_, err = SaveImageHostingConfig(t.Context(), saved, "", false, 0)
	assert.ErrorIs(t, err, model.ErrOptionRevisionConflict)
	changed := saved
	changed.BaseURL = "https://different.example.com"
	_, err = SaveImageHostingConfig(t.Context(), changed, "", false, saved.Revision)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replace")
	saved.Enabled = false
	saved, err = SaveImageHostingConfig(t.Context(), saved, "", true, saved.Revision)
	require.NoError(t, err)
	assert.False(t, saved.HasToken)
}

func TestImageHostingRejectsUnsafeConfigurationAndDeliveryURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ImageHostingConfig)
	}{
		{"plain HTTP credential transport", func(cfg *ImageHostingConfig) { cfg.BaseURL = "http://hosting.example.com" }},
		{"private destination without opt-in", func(cfg *ImageHostingConfig) { cfg.BaseURL = "https://127.0.0.1" }},
		{"endpoint host replacement", func(cfg *ImageHostingConfig) { cfg.FileUploadPath = "//evil.example.com/upload" }},
		{"endpoint with credential query", func(cfg *ImageHostingConfig) { cfg.URLUploadPath = "/upload?key=secret" }},
		{"unsafe method", func(cfg *ImageHostingConfig) { cfg.FileUploadMethod = "GET" }},
		{"cookie credential injection", func(cfg *ImageHostingConfig) { cfg.TokenHeader = "Cookie" }},
		{"header value injection", func(cfg *ImageHostingConfig) { cfg.TokenPrefix = "Bearer\r\nInjected: value" }},
		{"source URL override", func(cfg *ImageHostingConfig) { cfg.ExtraFields[cfg.URLField] = "https://evil.example.com" }},
		{"unbounded retries", func(cfg *ImageHostingConfig) { cfg.Retries = 99 }},
		{"nonfinite extra field", func(cfg *ImageHostingConfig) { cfg.ExtraFields["quality"] = math.NaN() }},
		{"nested extra field", func(cfg *ImageHostingConfig) { cfg.ExtraFields["policy"] = map[string]any{"id": 3} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultImageHostingConfig()
			cfg.BaseURL = "https://hosting.example.com"
			tc.edit(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}
	for _, raw := range []string{"http://example.com/image.png", "https://user:password@example.com/image.png", "https://127.0.0.1/image.png", "https://169.254.169.254/image.png", "https://example.com/image.png\r\nInjected: value"} {
		_, err := ValidateHostedImageURL(raw)
		assert.Error(t, err)
	}
	assert.False(t, imageHostingAddressAllowed(netip.MustParseAddr("169.254.169.254"), true), "private opt-in must not permit metadata/link-local endpoints")
}

func TestImageHostingUploadsURLAndFileAndProbesWithoutCredentials(t *testing.T) {
	for _, sourceURL := range []string{"https://upstream.example.com/image.png?signature=temporary", ""} {
		t.Run(map[bool]string{true: "URL", false: "file"}[sourceURL != ""], func(t *testing.T) {
			image := asyncImageBytesFixture(t)
			var uploads, probes atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/image.png" {
					probes.Add(1)
					assert.Empty(t, r.Header.Get("Authorization"))
					assert.Equal(t, http.MethodHead, r.Method)
					w.Header().Set("Content-Type", "image/png")
					w.Header().Set("Content-Length", "78")
					w.WriteHeader(http.StatusOK)
					return
				}
				uploads.Add(1)
				assert.Equal(t, "Bearer upload-secret", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("X-Upstream-Key"))
				idempotency := r.Header.Get("Idempotency-Key")
				assert.NotEmpty(t, idempotency)
				if sourceURL != "" {
					assert.Equal(t, "/api/v1/upload/url", r.URL.Path)
					var body map[string]any
					require.NoError(t, common.DecodeJson(r.Body, &body))
					assert.Equal(t, sourceURL, body["url"])
					assert.Equal(t, "public", body["visibility"])
					assert.Equal(t, float64(3), body["policy_id"])
					assert.Equal(t, float64(3600), body["expires_in"])
					assert.Equal(t, false, body["watermark"])
				} else {
					assert.Equal(t, "/api/v1/upload", r.URL.Path)
					file, header, err := r.FormFile("file")
					require.NoError(t, err)
					defer file.Close()
					defer r.MultipartForm.RemoveAll()
					assert.Equal(t, "image.png", header.Filename)
					data, err := io.ReadAll(file)
					require.NoError(t, err)
					assert.Equal(t, image.Data, data)
					assert.Equal(t, "3", r.FormValue("policy_id"))
					assert.Equal(t, "3600", r.FormValue("expires_in"))
					assert.Equal(t, "false", r.FormValue("watermark"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"code":0,"error":false,"data":{"links":{"url":"https://cdn.example.com/image.png"}}}`)
			}))
			defer server.Close()
			cfg := DefaultImageHostingConfig()
			cfg.BaseURL, cfg.AllowPrivateNetwork = server.URL, true
			cfg.ExtraFields["policy_id"], cfg.ExtraFields["expires_in"], cfg.ExtraFields["watermark"] = 3, 3600, false
			cfg.Revision = int64(len(sourceURL) + 101)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			client.Transport = imageHostingTestTransport{target: target, base: client.Transport}
			result, err := uploadHostedImage(t.Context(), cfg, "upload-secret", "task:0", sourceURL, image, client, client)
			require.NoError(t, err)
			assert.Equal(t, "https://cdn.example.com/image.png", result)
			assert.EqualValues(t, 1, uploads.Load())
			assert.EqualValues(t, 1, probes.Load())
		})
	}
}

func TestImageHostingRejectsUnusableSuccessAndLimitsRetries(t *testing.T) {
	for index, tc := range []struct {
		name      string
		status    int
		body      string
		probeType string
		wantCalls int
	}{
		{"business failure", 200, `{"success":false,"data":{"links":{"url":"https://cdn.example.com/image.png"}}}`, "", 1},
		{"private returned URL", 200, `{"data":{"links":{"url":"https://127.0.0.1/image.png"}}}`, "", 1},
		{"HTML share page", 200, `{"data":{"links":{"url":"https://cdn.example.com/image.png"}}}`, "text/html", 1},
		{"oversized response", 200, strings.Repeat("x", (1<<20)+1), "", 1},
		{"denied upload", 401, `{"error":"denied"}`, "", 1},
		{"transient failure has bounded retry", 503, `{"error":"busy"}`, "", 2},
		{"redirect is not followed", 302, ``, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/image.png" {
					assert.Empty(t, r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", tc.probeType)
					w.Header().Set("Content-Length", "12")
					return
				}
				calls.Add(1)
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			cfg := DefaultImageHostingConfig()
			cfg.BaseURL, cfg.Revision = server.URL, int64(index+201)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			client.Transport = imageHostingTestTransport{target: target, base: client.Transport}
			result, err := uploadHostedImage(t.Context(), cfg, "secret", "task:1", "", asyncImageBytesFixture(t), client, client)
			require.ErrorIs(t, err, ErrImageHostingUpload)
			assert.Empty(t, result)
			assert.EqualValues(t, tc.wantCalls, calls.Load())
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestImageHostingCircuitOwnsHalfOpenProbeAndIgnoresLateCompletions(t *testing.T) {
	cfg := DefaultImageHostingConfig()
	cfg.BaseURL, cfg.Revision, cfg.FailureThreshold, cfg.Concurrency = "https://circuit.example.com", 999, 1, 3
	oldSuccess, err := acquireImageHostingUpload(cfg)
	require.NoError(t, err)
	oldFailure, err := acquireImageHostingUpload(cfg)
	require.NoError(t, err)
	firstFailure, err := acquireImageHostingUpload(cfg)
	require.NoError(t, err)
	_, err = acquireImageHostingUpload(cfg)
	assert.ErrorIs(t, err, ErrImageHostingBusy)
	completeImageHostingUpload(firstFailure, cfg, false, false)
	assert.Equal(t, "open", GetImageHostingRuntimeStatus().State)
	_, err = acquireImageHostingUpload(cfg)
	assert.ErrorIs(t, err, ErrImageHostingCircuitOpen)
	completeImageHostingUpload(oldSuccess, cfg, true, false)
	assert.Equal(t, "open", GetImageHostingRuntimeStatus().State, "a pre-open success cannot reset the circuit")
	imageHostingRuntime.Lock()
	imageHostingRuntime.openUntil = time.Now().Add(-time.Second)
	imageHostingRuntime.Unlock()
	probe, err := acquireImageHostingUpload(cfg)
	require.NoError(t, err)
	assert.True(t, probe.probe)
	completeImageHostingUpload(oldFailure, cfg, false, false)
	_, err = acquireImageHostingUpload(cfg)
	assert.ErrorIs(t, err, ErrImageHostingCircuitOpen, "a late completion cannot release another request's probe")
	completeImageHostingUpload(probe, cfg, true, false)
	assert.Equal(t, "closed", GetImageHostingRuntimeStatus().State)
	assert.Equal(t, 0, GetImageHostingRuntimeStatus().InFlight)
	claim, err := acquireImageHostingUpload(cfg)
	require.NoError(t, err)
	completeImageHostingUpload(claim, cfg, false, true)
	assert.Equal(t, "closed", GetImageHostingRuntimeStatus().State, "a canceled caller does not count as a host failure")
}

func TestImageHostingNetworkPolicyRejectsPrivateDNSAndCanceledRequests(t *testing.T) {
	cfg := DefaultImageHostingConfig()
	client := imageHostingHTTPClient(cfg)
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodHead, "https://localhost/image.png", nil)
	require.NoError(t, err)
	_, err = client.Do(request)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	request, err = http.NewRequestWithContext(canceled, http.MethodHead, "https://cdn.example.com/image.png", nil)
	require.NoError(t, err)
	_, err = client.Do(request)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestImageHostingAnonymousProbeFollowsPublicRedirectsOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		redirectTo string
		wantError  bool
	}{
		{"public CDN redirect", "https://cdn.example.com/final.png", false},
		{"private redirect", "https://127.0.0.1/final.png", true},
		{"plaintext downgrade", "http://cdn.example.com/final.png", true},
		{"redirect loop", "https://hosting.example.com/start.png", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Empty(t, r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("X-API-Key"))
				if r.URL.Path == "/start.png" {
					w.Header().Set("Location", tc.redirectTo)
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Content-Length", "100")
			}))
			defer server.Close()
			client := imageHostingProbeHTTPClient(DefaultImageHostingConfig())
			defer client.CloseIdleConnections()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			client.Transport = imageHostingTestTransport{target: target, base: server.Client().Transport}
			err = probeHostedImage(t.Context(), client, "https://hosting.example.com/start.png")
			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.EqualValues(t, 2, calls.Load())
			}
			assert.LessOrEqual(t, calls.Load(), int32(4))
		})
	}
}

func TestImageHostingHTTPTimeoutOpensCircuitAndWorkerKeepsFallbackResults(t *testing.T) {
	db, task, _, workerConfig, _ := asyncImageWorkerFixture(t)
	extra := []any{&model.ImageStorageObject{}, &model.ImageUploadIntent{}, &model.AsyncImageStagingObject{}, &model.AsyncImageResult{}, &model.CanvasGrant{}}
	require.NoError(t, db.AutoMigrate(extra...))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(extra...)) })
	var uploads, generations atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	hosting := DefaultImageHostingConfig()
	hosting.Enabled, hosting.AllowPrivateNetwork = true, true
	hosting.BaseURL, hosting.TimeoutSeconds, hosting.Retries = server.URL, 1, 0
	hosting.FailureThreshold, hosting.URLFallback = 1, "upstream"
	hosting, err := SaveImageHostingConfig(t.Context(), hosting, "timeout-test-token", false, 0)
	require.NoError(t, err)
	image := asyncImageBytesFixture(t)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	link, err := uploadHostedImage(t.Context(), hosting, "timeout-test-token", "timeout-smoke:0", "", image, client, client)
	require.ErrorIs(t, err, ErrImageHostingUpload)
	assert.Empty(t, link)
	assert.Equal(t, "open", GetImageHostingRuntimeStatus(hosting).State)
	assert.EqualValues(t, 1, uploads.Load())
	ExecuteAsyncImageFunc = func(context.Context, model.AsyncImageTask, AsyncImageRequest, *model.Channel, ImageRuntimeConfig, func() error) (*AsyncImageOutput, model.AsyncImageBill, error) {
		generations.Add(1)
		return nil, model.AsyncImageBill{}, ErrImageHostingUpload
	}
	sourceURL := "https://provider.example.com/generated.png?expires=2000000000"
	staging := []model.AsyncImageStagingObject{
		{TaskId: task.TaskId, ImageIndex: 0, Data: image.Data, ContentType: image.ContentType, Width: image.Width, Height: image.Height, Checksum: image.Checksum, SourceURL: sourceURL},
		{TaskId: task.TaskId, ImageIndex: 1, Data: image.Data, ContentType: image.ContentType, Width: image.Width, Height: image.Height, Checksum: image.Checksum},
	}
	require.NoError(t, db.Create(&staging).Error)
	require.NoError(t, db.Model(&task).Updates(map[string]any{"status": model.ImageTaskUploading, "image_count": 2}).Error)
	require.NoError(t, db.First(&task, task.Id).Error)
	require.NoError(t, UploadAsyncImageTask(t.Context(), &task, workerConfig))
	var results []model.AsyncImageResult
	require.NoError(t, db.Where("task_id = ?", task.TaskId).Order("image_index").Find(&results).Error)
	require.Len(t, results, 2)
	for index, result := range results {
		var object model.ImageStorageObject
		require.NoError(t, db.Where("object_id = ?", result.ObjectId).Take(&object).Error)
		if index == 0 {
			assert.Equal(t, "upstream", object.DeliverySource)
			assert.Equal(t, sourceURL, ImageResultDeliveryURL(object))
		} else {
			assert.Equal(t, "local", object.DeliverySource)
			assert.Empty(t, ImageResultDeliveryURL(object))
		}
		store, err := GetImageObjectStorage(t.Context(), object)
		require.NoError(t, err)
		stored, err := store.Read(t.Context(), object.ObjectKey, 1<<20)
		require.NoError(t, err)
		assert.Equal(t, image.Data, stored)
	}
	var current model.AsyncImageTask
	require.NoError(t, db.First(&current, task.Id).Error)
	assert.Equal(t, model.ImageTaskBillingPending, current.Status)
	assert.Equal(t, 2, current.ResultCount)
	assert.Zero(t, current.RetryCount)
	assert.Zero(t, generations.Load(), "hosting failures must never invoke image generation again")
	assert.EqualValues(t, 1, uploads.Load(), "the open circuit must skip both worker uploads")
	assert.Equal(t, "open", GetImageHostingRuntimeStatus(hosting).State)
}
