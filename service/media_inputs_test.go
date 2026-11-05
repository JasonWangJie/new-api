package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mediaInputFixture(t *testing.T) (model.AsyncMediaJob, MediaRuntimeConfig, ImageRuntimeConfig) {
	t.Helper()
	db, imageTask, _, imageCfg, _ := asyncImageWorkerFixture(t)
	entities := []any{&model.Task{}, &model.AsyncMediaJob{}, &model.AsyncMediaEvent{}, &model.MediaInputObject{}, &model.MediaArtifactObject{}, &model.ImageInputAlias{}, &model.ImageInputObject{}, &model.ImageStorageObject{}}
	require.NoError(t, db.AutoMigrate(entities...))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(entities...)) })
	oldSecret, oldAddress, oldTaskAddress := common.CryptoSecret, system_setting.ServerAddress, system_setting.TaskPublicAddress
	common.CryptoSecret, system_setting.ServerAddress, system_setting.TaskPublicAddress = "media-input-test-signing-secret", "https://media.example.com", ""
	t.Cleanup(func() {
		common.CryptoSecret, system_setting.ServerAddress, system_setting.TaskPublicAddress = oldSecret, oldAddress, oldTaskAddress
	})
	now := time.Now().Unix()
	job := model.AsyncMediaJob{TaskId: "video-input-test", IdentityHash: "video-input-test", UserId: imageTask.UserId, TokenId: imageTask.TokenId, Status: "submitted", StorageStatus: "pending", CreatedAt: now, ExpiresAt: now + 86400, AttemptGeneration: 1}
	require.NoError(t, db.Create(&job).Error)
	cfg := DefaultMediaRuntimeConfig()
	cfg.LocalPath, cfg.MaxFileBytes = t.TempDir(), 1<<20
	require.NoError(t, SaveMediaRuntimeConfig(t.Context(), cfg))
	return job, cfg, imageCfg
}

type mediaInputRoundTripper func(*http.Request) (*http.Response, error)

func (transport mediaInputRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func mediaInputSaveFixture(t *testing.T, job model.AsyncMediaJob, cfg MediaRuntimeConfig, input AsyncVideoReferenceInput, data []byte) model.MediaInputObject {
	t.Helper()
	root := filepath.Join(cfg.LocalPath, "inputs")
	require.NoError(t, os.MkdirAll(root, 0700))
	key := "fixture-" + strconv.Itoa(input.Index)
	require.NoError(t, os.WriteFile(filepath.Join(root, key), data, 0600))
	checksum := sha256.Sum256(data)
	mime := map[string]string{"image": "image/png", "video": "video/mp4", "audio": "audio/wav"}[input.Kind]
	object, err := model.SaveMediaInputObject(t.Context(), model.MediaInputObject{ObjectId: key, TaskId: job.TaskId, Slot: strconv.Itoa(input.Index), Kind: input.Kind, SourceHash: ImageIdentityHash(input.Source), UserId: job.UserId, TokenId: job.TokenId, RootPath: root, ObjectKey: key, MimeType: mime, ByteSize: int64(len(data)), Checksum: hex.EncodeToString(checksum[:]), Status: "active", CreatedAt: job.CreatedAt, ExpiresAt: job.ExpiresAt})
	require.NoError(t, err)
	return object
}

func TestMediaInputDownloadLimitsAndRawStorage(t *testing.T) {
	job, cfg, imageCfg := mediaInputFixture(t)
	data := mediaTestMP4()
	input := AsyncVideoReferenceInput{Index: 0, Kind: "video", Source: "http://public.example.com/reference.mp4"}
	client := &http.Client{Transport: mediaInputRoundTripper(func(request *http.Request) (*http.Response, error) {
		assert.Equal(t, "identity", request.Header.Get("Accept-Encoding"))
		assert.Empty(t, request.Header.Get("Authorization"))
		assert.Empty(t, request.Header.Get("Cookie"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/mp4"}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: request}, nil
	})}
	var remaining atomic.Int64
	remaining.Store(cfg.MaxFileBytes)
	object, err := downloadMediaInput(t.Context(), job, input, cfg, imageCfg, job.ExpiresAt, &remaining, client)
	require.NoError(t, err)
	stored, err := os.ReadFile(filepath.Join(object.RootPath, object.ObjectKey))
	require.NoError(t, err)
	assert.Equal(t, data, stored)
	// An interrupted worker reuses its slot and removes its unused new file.
	remaining.Store(cfg.MaxFileBytes)
	again, err := downloadMediaInput(t.Context(), job, input, cfg, imageCfg, job.ExpiresAt, &remaining, client)
	require.NoError(t, err)
	assert.Equal(t, object.ObjectId, again.ObjectId)
	files, err := os.ReadDir(object.RootPath)
	require.NoError(t, err)
	assert.Len(t, files, 1)
	for _, tc := range []struct {
		name              string
		budget, fileLimit int64
		length            int64
		encoding          string
	}{
		{"aggregate", int64(len(data) - 1), cfg.MaxFileBytes, int64(len(data)), ""},
		{"file", cfg.MaxFileBytes, int64(len(data) - 1), int64(len(data)), ""},
		{"truncated", cfg.MaxFileBytes, cfg.MaxFileBytes, int64(len(data) + 1), ""},
		{"compressed", cfg.MaxFileBytes, cfg.MaxFileBytes, int64(len(data)), "gzip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining.Store(tc.budget)
			limited := cfg
			limited.MaxFileBytes = tc.fileLimit
			broken := &http.Client{Transport: mediaInputRoundTripper(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}, "Content-Encoding": {tc.encoding}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: tc.length, Request: request}, nil
			})}
			_, err := downloadMediaInput(t.Context(), job, input, limited, imageCfg, job.ExpiresAt, &remaining, broken)
			assert.Error(t, err)
		})
	}
	client = mediaInputClient(cfg, imageCfg, nil, nil)
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, time.Duration(cfg.DownloadTimeout)*time.Second, transport.ResponseHeaderTimeout, "header waiting must leave later image download attempts their longer request deadlines")
	redirect, err := http.NewRequest(http.MethodGet, "http://other.example.com/reference", nil)
	require.NoError(t, err)
	redirect.Header.Set("Authorization", "secret")
	redirect.Header.Set("Cookie", "secret")
	redirect.Header.Set("Referer", "https://source.example.com/private?key=secret")
	require.NoError(t, client.CheckRedirect(redirect, []*http.Request{{}}))
	assert.Equal(t, http.Header{"Accept-Encoding": {"identity"}}, redirect.Header)
	for _, source := range []string{"http://127.0.0.1/a", "https://169.254.169.254/a", "https://user:password@example.com/a", "file:///tmp/a", "http://[::1]/a"} {
		_, err := validateMediaInputURL(source)
		assert.Error(t, err, source)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	reader := mediaInputBudgetReader{ctx: cancelled, reader: bytes.NewReader(data), remaining: &remaining}
	_, err = io.ReadAll(&reader)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestMediaInputDownloadRetries(t *testing.T) {
	for _, tc := range []struct {
		name, reason, source string
		kind                 string
		failure              error
		status, failures     int
		calls                int
		partial, limited     bool
		cancel, retryAfter   bool
		invalid              bool
		success              bool
		attemptSeconds       []int
		mediaTimeout         int
		parentTimeout        int
	}{
		{name: "timeout then success", failure: context.DeadlineExceeded, failures: 1, calls: 2, success: true},
		{name: "image timeout budgets increase", failure: context.DeadlineExceeded, failures: 2, calls: 3, success: true, attemptSeconds: []int{30, 60, 120}},
		{name: "image recovers on fourth attempt", failure: fmt.Errorf("a newly worded provider timeout: %w", context.DeadlineExceeded), failures: 3, calls: 4, success: true, attemptSeconds: []int{30, 60, 120, 240}},
		{name: "image recovers on fifth attempt", failure: &net.DNSError{Err: "different transient source failure", Name: "private.example", IsTimeout: true}, failures: 4, calls: 5, success: true, attemptSeconds: []int{30, 60, 120, 240, 480}},
		{name: "image timeout respects media budget", failure: context.DeadlineExceeded, failures: 2, calls: 3, success: true, attemptSeconds: []int{30, 60, 120}, mediaTimeout: 45},
		{name: "image timeout respects parent deadline", failure: context.DeadlineExceeded, failures: 2, calls: 3, success: true, attemptSeconds: []int{30, 60, 120}, parentTimeout: 45},
		{name: "wrapped timeout with different diagnostic", failure: fmt.Errorf("the remote service did not answer: %w", context.DeadlineExceeded), failures: 1, calls: 2, success: true},
		{name: "typed timeout with different diagnostic", failure: &net.DNSError{Err: "a different upstream diagnostic", Name: "private.example", IsTimeout: true}, failures: 1, calls: 2, success: true},
		{name: "connection then success", failure: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset")}, failures: 1, calls: 2, success: true},
		{name: "bad gateway then success", status: 502, failures: 1, calls: 2, success: true},
		{name: "rate limited then success", status: 429, failures: 1, calls: 2, success: true},
		{name: "interrupted body then success", partial: true, failures: 1, calls: 2, success: true},
		{name: "exhausted timeout", failure: context.DeadlineExceeded, failures: 5, calls: 5, reason: "after 5 attempts"},
		{name: "missing source", status: 404, failures: 3, calls: 1, reason: "HTTP 404"},
		{name: "bad request is terminal", status: 400, failures: 3, calls: 1, reason: "HTTP 400"},
		{name: "authentication is terminal", status: 401, failures: 3, calls: 1, reason: "HTTP 401"},
		{name: "forbidden source", status: 403, failures: 3, calls: 1, reason: "HTTP 403"},
		{name: "expired source is terminal", status: 410, failures: 3, calls: 1, reason: "HTTP 410"},
		{name: "invalid image is terminal", invalid: true, calls: 1, reason: "reference image"},
		{name: "video timeout budget is unchanged", kind: "video", failure: context.DeadlineExceeded, failures: 5, calls: 5, reason: "after 5 attempts", attemptSeconds: []int{900, 900, 900, 900, 900}},
		{name: "audio timeout budget is unchanged", kind: "audio", failure: context.DeadlineExceeded, failures: 5, calls: 5, reason: "after 5 attempts", attemptSeconds: []int{900, 900, 900, 900, 900}},
		{name: "unsupported source server", status: 501, failures: 3, calls: 1, reason: "HTTP 501"},
		{name: "security rejection", failure: errors.New("reference resolved to a non-public address"), failures: 3, calls: 1, reason: "request or read failed"},
		{name: "retry after exceeds budget", status: 429, failures: 3, calls: 1, retryAfter: true, reason: "remaining deadline"},
		{name: "interrupted bytes remain charged", partial: true, limited: true, failures: 1, calls: 2, reason: "batch byte limit"},
		{name: "cancel during retry", status: 503, failures: 3, calls: 1, cancel: true, reason: "context canceled"},
		{name: "private URL", source: "http://127.0.0.1/private?token=private", failures: 3, calls: 0, reason: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, cfg, imageCfg := mediaInputFixture(t)
			if tc.mediaTimeout > 0 {
				cfg.DownloadTimeout = tc.mediaTimeout
			}
			data := asyncImageBytesFixture(t).Data
			if tc.invalid {
				data = []byte("invalid image bytes")
			}
			input := AsyncVideoReferenceInput{Index: 0, Kind: "image", Source: "https://public.example/reference.png?token=private"}
			if tc.kind != "" {
				input.Kind = tc.kind
			}
			if tc.source != "" {
				input.Source = tc.source
			}
			var remaining atomic.Int64
			budget := cfg.MaxFileBytes
			if tc.limited {
				budget = int64(len(data))
			}
			remaining.Store(budget)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.parentTimeout > 0 {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, time.Duration(tc.parentTimeout)*time.Second)
				defer deadlineCancel()
			}
			downloadStartedAt := time.Now()
			downloadDeadline := downloadStartedAt.Add(time.Duration(cfg.DownloadTimeout) * time.Second)
			if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(downloadDeadline) {
				downloadDeadline = parentDeadline
			}
			calls := 0
			client := &http.Client{Transport: mediaInputRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				assert.Equal(t, http.MethodGet, request.Method)
				assert.Empty(t, request.Header.Get("Authorization"))
				if len(tc.attemptSeconds) > 0 {
					requestedAt := time.Now()
					expectedDeadline := requestedAt.Add(time.Duration(tc.attemptSeconds[calls-1]) * time.Second)
					if downloadDeadline.Before(expectedDeadline) {
						expectedDeadline = downloadDeadline
					}
					deadline, ok := request.Context().Deadline()
					require.True(t, ok)
					assert.WithinDuration(t, expectedDeadline, deadline, 500*time.Millisecond, "GET %d must use its attempt budget bounded by the original download and parent deadlines", calls)
				}
				if tc.cancel {
					cancel()
				}
				status := http.StatusOK
				var body io.Reader = bytes.NewReader(data)
				if calls <= tc.failures {
					if tc.failure != nil {
						return nil, tc.failure
					}
					if tc.status != 0 {
						status = tc.status
					}
					if tc.partial {
						body = io.MultiReader(bytes.NewReader(data[:len(data)/2]), iotest.ErrReader(io.ErrUnexpectedEOF))
					}
				}
				header := http.Header{"Content-Type": {"image/png"}}
				if tc.retryAfter {
					header.Set("Retry-After", "3600")
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(body), ContentLength: int64(len(data)), Request: request}, nil
			})}
			object, err := downloadMediaInput(ctx, job, input, cfg, imageCfg, job.ExpiresAt, &remaining, client)
			assert.Equal(t, tc.calls, calls)
			if tc.success {
				require.NoError(t, err)
				stored, readErr := os.ReadFile(filepath.Join(object.RootPath, object.ObjectKey))
				require.NoError(t, readErr)
				assert.Equal(t, data, stored)
				consumed := int64(len(data))
				if tc.partial {
					consumed += int64(len(data) / 2)
				}
				assert.Equal(t, budget-consumed, remaining.Load())
			} else {
				require.ErrorContains(t, err, tc.reason)
				assert.NotContains(t, err.Error(), "token=private")
				assert.NotContains(t, err.Error(), "public.example")
				for _, index := range []int{1, 2, 3, 64} {
					notice := VideoTaskFailureNotice(fmt.Sprintf("reference %d (%s): %s", index, input.Kind, err))
					assert.Contains(t, notice.Summary, "本站下载参考素材失败", "reference index %d", index)
					assert.Contains(t, notice.Suggestion, "原渠道", "reference index %d", index)
				}
				if tc.cancel {
					assert.ErrorIs(t, err, context.Canceled)
				}
			}
			objects, listErr := model.ListMediaInputObjects(t.Context(), job.TaskId, job.UserId, job.TokenId)
			require.NoError(t, listErr)
			objectCount := 0
			if tc.success {
				objectCount = 1
			}
			assert.Len(t, objects, objectCount)
			entries, readErr := os.ReadDir(filepath.Join(cfg.LocalPath, "inputs"))
			if !os.IsNotExist(readErr) {
				require.NoError(t, readErr)
				assert.Len(t, entries, len(objects), "failed attempts must leave no partial files")
			}
		})
	}
}

func TestMediaInputPreparationPreservesCanonicalSlots(t *testing.T) {
	job, cfg, _ := mediaInputFixture(t)
	inlineImage := asyncImageBytesFixture(t).Data
	inlinePayload := base64.RawStdEncoding.EncodeToString(inlineImage)
	require.NotEqual(t, base64.StdEncoding.EncodeToString(inlineImage), inlinePayload, "the fixture must exercise admitted unpadded base64")
	decoded, err := base64.RawStdEncoding.DecodeString(inlinePayload)
	require.NoError(t, err)
	assert.Equal(t, inlineImage, decoded)
	references := []any{
		map[string]any{"type": "image", "role": "first_frame", "source": "https://source.example.com/first.png"},
		map[string]any{"type": "video", "role": "reference_video", "source": "https://source.example.com/video.mp4"},
		map[string]any{"type": "audio", "role": "reference_audio", "source": "https://source.example.com/audio.wav"},
		map[string]any{"type": "image", "role": "last_frame", "source": "asset://kept-last"},
		map[string]any{"type": "image", "role": "reference", "source": "data:image/png;base64," + inlinePayload},
		map[string]any{"type": "video", "role": "reference_video", "source": "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(mediaTestMP4())},
		map[string]any{"type": "audio", "role": "reference_audio", "source": "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(mediaInputPCMFixture(false))},
	}
	body := map[string]any{"prompt": "keep https://source.example.com/first.png literally", "references": references, "seed": 0}
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	request := AsyncVideoRequest{Body: encoded, ReferenceRecoveryVersion: 1, ReferenceRecoveryDeadline: time.Now().Unix() + 600, ReferenceURLExpiresAt: job.ExpiresAt}
	for index, value := range references {
		r := value.(map[string]any)
		input := AsyncVideoReferenceInput{Index: index, Kind: r["type"].(string), Role: r["role"].(string), Source: r["source"].(string), Path: "/references/" + strconv.Itoa(index) + "/source"}
		request.ReferenceInputs = append(request.ReferenceInputs, input)
		if index < 3 {
			data := mediaTestMP4()
			if input.Kind == "image" {
				data = asyncImageBytesFixture(t).Data
			} else if input.Kind == "audio" {
				data = mediaInputPCMFixture(false)
			}
			mediaInputSaveFixture(t, job, cfg, input, data)
		}
	}
	prepared, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, common.Unmarshal(prepared.Body, &result))
	assert.Equal(t, body["prompt"], result["prompt"])
	got := result["references"].([]any)
	for index, value := range got {
		r := value.(map[string]any)
		assert.Equal(t, request.ReferenceInputs[index].Kind, r["type"])
		assert.Equal(t, request.ReferenceInputs[index].Role, r["role"])
		if index < 3 {
			assert.Contains(t, r["source"], "/api/media-inputs/fixture-"+strconv.Itoa(index)+"/content?")
		} else {
			assert.Equal(t, request.ReferenceInputs[index].Source, r["source"], "provider assets and inline data retain their exact canonical bytes")
			assert.Empty(t, prepared.ReferenceInputs[index].ObjectId)
		}
	}
	assert.Empty(t, request.ReferenceInputs[0].ObjectId)
	for _, testCase := range []struct {
		name, source string
		bodyOnly     bool
	}{
		{name: "wrong inline media kind", source: "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(mediaTestMP4())},
		{name: "invalid inline encoding", source: "data:image/png;base64,invalid!"},
		{name: "invalid interior padding", source: "data:image/png;base64,a=b="},
		{name: "unsupported inline source", source: "file:///private/reference.png"},
		{name: "changed canonical body", source: "data:image/png;base64,aW1hZ2U=", bodyOnly: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := request
			changed.ReferenceInputs = append([]AsyncVideoReferenceInput(nil), request.ReferenceInputs...)
			var body map[string]any
			require.NoError(t, common.Unmarshal(request.Body, &body))
			body["references"].([]any)[4].(map[string]any)["source"] = testCase.source
			if !testCase.bodyOnly {
				changed.ReferenceInputs[4].Source = testCase.source
			}
			var err error
			changed.Body, err = common.Marshal(body)
			require.NoError(t, err)
			_, err = PrepareAsyncVideoLocalReferences(t.Context(), job, changed)
			assert.Error(t, err)
		})
	}
	request.ReferenceInputs[1].Index = 0
	_, err = PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	assert.Error(t, err)
}

func TestMediaInputCapabilitiesRangeAndTerminalGrace(t *testing.T) {
	job, cfg, _ := mediaInputFixture(t)
	data := mediaTestMP4()
	object := mediaInputSaveFixture(t, job, cfg, AsyncVideoReferenceInput{Index: 0, Kind: "video", Source: "https://source.example.com/video"}, data)
	link, err := MediaInputObjectURL(object)
	require.NoError(t, err)
	parsed, err := url.Parse(link)
	require.NoError(t, err)
	query := parsed.Query()
	local := AsyncVideoReferenceInput{Kind: "video", Source: link}
	kept, err := verifiedLocalMediaReference(t.Context(), job, local, object.ExpiresAt)
	require.NoError(t, err)
	assert.True(t, kept)
	_, err = verifiedLocalMediaReference(t.Context(), job, local, object.ExpiresAt+1)
	assert.Error(t, err, "both signed and persisted expiry must cover the complete upstream lifetime")
	_, err = ReadMediaInputObject(t.Context(), object.ObjectId, query.Get("expires"), query.Get("access"))
	require.NoError(t, err)
	_, err = ReadMediaInputObject(t.Context(), object.ObjectId, query.Get("expires"), query.Get("access")+"x")
	assert.ErrorIs(t, err, ErrMediaInputUnavailable)
	orphan := filepath.Join(object.RootPath, "minput_crashed.part")
	require.NoError(t, os.WriteFile(orphan, []byte("interrupted"), 0600))
	old := time.Now().Add(-time.Duration(cfg.DownloadTimeout+421) * time.Second)
	require.NoError(t, os.Chtimes(orphan, old, old))
	require.NoError(t, CleanupMediaInputs(t.Context(), time.Now().Unix()))
	_, err = os.Stat(orphan)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(object.RootPath, object.ObjectKey))
	require.NoError(t, err, "cleanup must preserve persisted unexpired inputs")
	changed := object
	changed.Slot = "1"
	assert.NotEqual(t, mediaInputSignature(object, query.Get("expires")), mediaInputSignature(changed, query.Get("expires")))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(method, link, nil)
		c.Request.Header.Set("Range", "bytes=2-7")
		require.NoError(t, ServeMediaInputObject(c, object))
		assert.Equal(t, 206, recorder.Code)
		assert.Equal(t, "bytes 2-7/"+strconv.Itoa(len(data)), recorder.Header().Get("Content-Range"))
		assert.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
		if method == http.MethodGet {
			assert.Equal(t, data[2:8], recorder.Body.Bytes())
		} else {
			assert.Empty(t, recorder.Body.Bytes())
		}
	}
	task := &model.Task{TaskID: job.TaskId, UserId: job.UserId, Status: model.TaskStatusUnknown, AttemptGeneration: job.AttemptGeneration}
	require.NoError(t, model.DB.Create(task).Error)
	require.NoError(t, RetireMediaInputObjects(t.Context(), task))
	unchanged, err := model.GetMediaInputObject(t.Context(), object.ObjectId)
	require.NoError(t, err)
	assert.Equal(t, object.ExpiresAt, unchanged.ExpiresAt)
	task.Status = model.TaskStatusFailure
	task.FailReason = "local task deadline expired"
	task.PrivateData.UpstreamTaskID = "accepted-replacement-operation"
	require.NoError(t, model.DB.Save(task).Error)
	require.NoError(t, RetireMediaInputObjects(t.Context(), task))
	unchanged, err = model.GetMediaInputObject(t.Context(), object.ObjectId)
	require.NoError(t, err)
	assert.Equal(t, object.ExpiresAt, unchanged.ExpiresAt, "local timeout after native acceptance must retain hard expiry")
	task.PrivateData.VideoTerminalConfirmed = true
	require.NoError(t, model.DB.Save(task).Error)
	stale := *task
	stale.AttemptGeneration--
	require.NoError(t, RetireMediaInputObjects(t.Context(), &stale))
	require.NoError(t, RetireMediaInputObjects(t.Context(), task))
	shortened, err := model.GetMediaInputObject(t.Context(), object.ObjectId)
	require.NoError(t, err)
	assert.InDelta(t, time.Now().Unix()+3600, shortened.ExpiresAt, 2)
	// The original capability remains valid until the shorter terminal grace.
	_, err = ReadMediaInputObject(t.Context(), object.ObjectId, query.Get("expires"), query.Get("access"))
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", job.TokenId).Update("status", common.TokenStatusDisabled).Error)
	_, err = ReadMediaInputObject(t.Context(), object.ObjectId, query.Get("expires"), query.Get("access"))
	assert.ErrorIs(t, err, ErrMediaInputUnavailable)
	require.NoError(t, CleanupMediaInputs(t.Context(), shortened.ExpiresAt))
	_, err = os.Stat(filepath.Join(object.RootPath, object.ObjectKey))
	assert.True(t, os.IsNotExist(err))
	_, err = model.GetMediaInputObject(t.Context(), object.ObjectId)
	assert.Error(t, err)
}

func TestMediaInputFormatValidation(t *testing.T) {
	webpData, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	require.NoError(t, err)
	var jpegData bytes.Buffer
	require.NoError(t, jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil))
	var flacData bytes.Buffer
	encoder, err := flac.NewEncoder(&flacData, &meta.StreamInfo{BlockSizeMin: 16, BlockSizeMax: 16, SampleRate: 8000, NChannels: 1, BitsPerSample: 16, NSamples: 16})
	require.NoError(t, err)
	require.NoError(t, encoder.WriteFrame(&frame.Frame{Header: frame.Header{BlockSize: 16, SampleRate: 8000, Channels: frame.ChannelsMono, BitsPerSample: 16}, Subframes: []*frame.Subframe{{SubHeader: frame.SubHeader{Pred: frame.PredConstant}, Samples: make([]int32, 16), NSamples: 16}}}))
	require.NoError(t, encoder.Close())
	mp4Data := mediaTestMP4()
	mov := bytes.Replace(mp4Data, []byte("isom"), []byte("qt  "), 1)
	m4a := bytes.Replace(mp4Data, []byte("vide"), []byte("soun"), 1)
	mp3Data := make([]byte, 626)
	copy(mp3Data, []byte{0xff, 0xfb, 0xb0, 0x64})
	for _, tc := range []struct {
		name, kind, mime string
		data             []byte
	}{
		{"png", "image", "image/png", asyncImageBytesFixture(t).Data}, {"jpeg", "image", "image/jpeg", jpegData.Bytes()},
		{"webp", "image", "image/webp", webpData},
		{"mp4", "video", "video/mp4", mp4Data}, {"mov", "video", "video/quicktime", mov}, {"m4a", "audio", "audio/mp4", m4a},
		{"wav", "audio", "audio/wav", mediaInputPCMFixture(false)}, {"aiff", "audio", "audio/aiff", mediaInputPCMFixture(true)},
		{"mp3", "audio", "audio/mpeg", mp3Data}, {"aac", "audio", "audio/aac", []byte{0xff, 0xf1, 0x50, 0x80, 1, 0x3f, 0xfc, 0, 0}},
		{"flac", "audio", "audio/flac", flacData.Bytes()}, {"opus", "audio", "audio/ogg", mediaInputOggFixture()},
		{"webm-video", "video", "video/webm", mediaInputWebMFixture(false)}, {"webm-audio", "audio", "audio/webm", mediaInputWebMFixture(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "reference")
			require.NoError(t, err)
			defer file.Close()
			_, err = file.Write(tc.data)
			require.NoError(t, err)
			mime, _, err := ValidateMediaInputFile(t.Context(), file, int64(len(tc.data)), tc.kind, tc.mime, DefaultImageRuntimeConfig())
			require.NoError(t, err)
			assert.Equal(t, tc.mime, mime)
			_, _, err = ValidateMediaInputFile(t.Context(), file, int64(len(tc.data)-1), tc.kind, tc.mime, DefaultImageRuntimeConfig())
			assert.Error(t, err, "truncated container/frame must fail")
			_, _, err = ValidateMediaInputFile(t.Context(), file, int64(len(tc.data)), tc.kind, "text/html", DefaultImageRuntimeConfig())
			assert.Error(t, err)
		})
	}
}

func mediaInputPCMFixture(aiff bool) []byte {
	var order binary.ByteOrder = binary.LittleEndian
	magic, format, formatName, dataName := "RIFF", "WAVE", "fmt ", "data"
	metadata, samples := make([]byte, 16), []byte{0, 0, 0, 0}
	if aiff {
		order, magic, format, formatName, dataName = binary.BigEndian, "FORM", "AIFF", "COMM", "SSND"
		metadata = make([]byte, 18)
		order.PutUint16(metadata[:2], 1)
		order.PutUint32(metadata[2:6], 2)
		order.PutUint16(metadata[6:8], 16)
		copy(metadata[8:], []byte{0x40, 0x0b, 0xfa, 0, 0, 0, 0, 0, 0, 0})
		samples = append(make([]byte, 8), samples...)
	} else {
		order.PutUint16(metadata[:2], 1)
		order.PutUint16(metadata[2:4], 1)
		order.PutUint32(metadata[4:8], 8000)
		order.PutUint32(metadata[8:12], 16000)
		order.PutUint16(metadata[12:14], 2)
		order.PutUint16(metadata[14:16], 16)
	}
	result := make([]byte, 12)
	copy(result, magic)
	copy(result[8:], format)
	for _, chunk := range []struct {
		name string
		data []byte
	}{{formatName, metadata}, {dataName, samples}} {
		header := make([]byte, 8)
		copy(header, chunk.name)
		order.PutUint32(header[4:], uint32(len(chunk.data)))
		result = append(result, header...)
		result = append(result, chunk.data...)
	}
	order.PutUint32(result[4:8], uint32(len(result)-8))
	return result
}

func mediaInputOggFixture() []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	var result []byte
	for index, packet := range [][]byte{head, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"), {0xf8, 0xff, 0xfe}} {
		page := make([]byte, 28)
		copy(page, "OggS")
		page[26], page[27] = 1, byte(len(packet))
		if index == 0 {
			page[5] = 2
		}
		if index == 2 {
			page[5] = 4
		}
		binary.LittleEndian.PutUint32(page[14:18], 1)
		binary.LittleEndian.PutUint32(page[18:22], uint32(index))
		page = append(page, packet...)
		var crc uint32
		for _, value := range page {
			crc ^= uint32(value) << 24
			for range 8 {
				if crc&0x80000000 != 0 {
					crc = crc<<1 ^ 0x04c11db7
				} else {
					crc <<= 1
				}
			}
		}
		binary.LittleEndian.PutUint32(page[22:26], crc)
		result = append(result, page...)
	}
	return result
}

func mediaInputWebMFixture(audio bool) []byte {
	element := func(id []byte, data []byte) []byte {
		result := append([]byte(nil), id...)
		result = append(result, 0x80|byte(len(data)))
		return append(result, data...)
	}
	kind, codec := byte(1), "V_VP9"
	if audio {
		kind, codec = 2, "A_OPUS"
	}
	header := element([]byte{0x1a, 0x45, 0xdf, 0xa3}, element([]byte{0x42, 0x82}, []byte("webm")))
	track := append(element([]byte{0xd7}, []byte{1}), element([]byte{0x83}, []byte{kind})...)
	track = append(track, element([]byte{0x86}, []byte(codec))...)
	tracks := element([]byte{0x16, 0x54, 0xae, 0x6b}, element([]byte{0xae}, track))
	cluster := element([]byte{0x1f, 0x43, 0xb6, 0x75}, element([]byte{0xa3}, []byte{0x81, 0, 0, 0x80, 0xf8, 0xff, 0xfe}))
	return append(header, element([]byte{0x18, 0x53, 0x80, 0x67}, append(tracks, cluster...))...)
}

func TestMediaInputDeclaredSlotsPreserveBodyAndReuseDownloadedFiles(t *testing.T) {
	job, cfg, imageCfg := mediaInputFixture(t)
	oldRegistry, oldTimeout := jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	constant.TaskTimeoutMinutes = 60
	t.Cleanup(func() { jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes = oldRegistry, oldTimeout })
	source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "doubao", "plugin.js"))
	require.NoError(t, err)
	plugin, err := jsplugin.DefaultRegistry.Register(string(source), jsplugin.Options{})
	require.NoError(t, err)
	body := []byte(`{"model":"doubao-seedance-2-0","prompt":"keep https://source.example.com/first.png literally","image":{"url":"https://source.example.com/first.png"},"reference_videos":[{"url":"https://source.example.com/source.mp4"}],"reference_audios":["https://source.example.com/source.wav"],"metadata":{"unrelated":"https://source.example.com/private"},"seed":0}`)
	request := AsyncVideoRequest{Body: body, ContentType: "application/json", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key, Version: plugin.Meta.Version}}}
	require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, job.ExpiresAt))
	require.Equal(t, 2, request.ReferenceRecoveryVersion)
	require.Len(t, request.ReferenceInputs, 3)
	var remaining atomic.Int64
	remaining.Store(cfg.MaxFileBytes)
	downloads := 0
	for _, input := range request.ReferenceInputs {
		payload := asyncImageBytesFixture(t).Data
		if input.Kind == "video" {
			payload = mediaTestMP4()
		} else if input.Kind == "audio" {
			payload = mediaInputPCMFixture(false)
		}
		client := &http.Client{Transport: mediaInputRoundTripper(func(req *http.Request) (*http.Response, error) {
			downloads++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload)), Request: req}, nil
		})}
		_, err := downloadMediaInput(t.Context(), job, input, cfg, imageCfg, request.ReferenceURLExpiresAt, &remaining, client)
		require.NoError(t, err)
	}
	prepared, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	require.NoError(t, err)
	assert.True(t, prepared.LocalReferencesReady)
	var result map[string]any
	require.NoError(t, common.Unmarshal(prepared.Body, &result))
	assert.Equal(t, "keep https://source.example.com/first.png literally", result["prompt"])
	assert.EqualValues(t, 0, result["seed"])
	assert.Equal(t, "https://source.example.com/private", result["metadata"].(map[string]any)["unrelated"])
	for _, input := range request.ReferenceInputs {
		value, err := videoRetryReferenceValue(result, input.Path, nil)
		require.NoError(t, err)
		assert.Contains(t, value, "/api/media-inputs/")
		assert.NotEqual(t, input.Source, value)
	}
	reused, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	require.NoError(t, err)
	assert.JSONEq(t, string(prepared.Body), string(reused.Body))
	assert.Equal(t, 3, downloads)
	tampered := request
	tampered.Body = bytes.Replace(body, []byte("first.png"), []byte("changed.png"), -1)
	_, err = PrepareAsyncVideoLocalReferences(t.Context(), job, tampered)
	assert.ErrorIs(t, err, ErrMediaInputUnavailable)
}

func legacyDramaRetryPluginFixture(t *testing.T, key, declaration string) *jsplugin.LoadedPlugin {
	t.Helper()
	previousRegistry, previousTimeout := jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes
	jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes = jsplugin.NewRegistry(), 60
	t.Cleanup(func() { jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes = previousRegistry, previousTimeout })
	sourcePath := os.Getenv("NEWAPI_DRAMA_PLUGIN_SOURCE")
	if sourcePath == "" {
		sourcePath = filepath.Join("..", "plugins", "tasks", "drama", "plugin.js")
	}
	source, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	legacy := regexp.MustCompile(`(?s)\s*videoRetryInputs:\s*\[.*?\],`).ReplaceAllString(string(source), "")
	if key != "" {
		legacy = strings.Replace(legacy, `key: "drama"`, `key: "`+key+`"`, 1)
	}
	legacy = strings.Replace(legacy, "export const meta = {", "export const meta = {"+declaration, 1)
	plugin, err := jsplugin.DefaultRegistry.Register(legacy, jsplugin.Options{})
	require.NoError(t, err)
	return plugin
}

func TestLegacyDramaMixedVideoReferenceSlots(t *testing.T) {
	for _, tc := range []struct {
		name, key, declaration, body string
		paths, kinds, roles          []string
		unsupported                  bool
	}{
		{name: "ordered references", body: `{"model":"seedance2.5-A","prompt":"@图片1 @视频1 @图片2","seconds":5,"resolution":"720p","references":[{"type":"image","role":"reference","source":"https://source.example/first.png"},{"type":"video","role":"reference","source":"https://source.example/clip.mp4"},{"type":"image","role":"reference","source":"https://source.example/last.png"}]}`, paths: []string{"/references/0/source", "/references/1/source", "/references/2/source"}, kinds: []string{"image", "video", "image"}, roles: []string{"reference", "reference", "reference"}},
		{name: "pinned frame roles", body: `{"model":"seedance2.0-A","prompt":"first and last frame","references":[{"type":"image","role":"first_frame","source":"https://source.example/first.png"},{"type":"image","role":"last_frame","source":"https://source.example/last.png"}]}`, paths: []string{"/references/0/source", "/references/1/source"}, kinds: []string{"image", "image"}, roles: []string{"first_frame", "last_frame"}},
		{name: "legacy arrays", body: `{"reference_images":["https://source.example/image.png"],"reference_videos":["https://source.example/clip.mp4"],"reference_audios":["https://source.example/audio.wav"]}`, paths: []string{"/reference_images/0", "/reference_videos/0", "/reference_audios/0"}, kinds: []string{"image", "video", "audio"}},
		{name: "frame aliases", body: `{"first_image":"https://source.example/first.png","last_image":"https://source.example/last.png"}`, paths: []string{"/first_image", "/last_image"}, kinds: []string{"image", "image"}},
		{name: "video edit", body: `{"video":"https://source.example/clip.mp4","image":"https://source.example/image.png"}`, paths: []string{"/image", "/video"}, kinds: []string{"image", "video"}},
		{name: "inline and private assets", body: `{"references":[{"type":"image","source":"data:image/png;base64,aW1hZ2U="},{"type":"video","source":"asset://private"}],"prompt":"https://source.example/prompt.png"}`},
		{name: "unsupported media", body: `{"references":[{"type":"file","source":"https://source.example/private"}]}`, unsupported: true},
		{name: "explicit declaration remains authoritative", declaration: `videoRetryInputs:[{path:"/image",kind:"image"}],`, body: `{"image":"https://source.example/image.png","references":[{"type":"video","source":"https://source.example/clip.mp4"}]}`, paths: []string{"/image"}, kinds: []string{"image"}},
		{name: "other plugins do not inherit Drama compatibility", key: "another-video", body: `{"references":[{"type":"image","source":"https://source.example/image.png"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := legacyDramaRetryPluginFixture(t, tc.key, tc.declaration)
			if tc.declaration == "" {
				require.Empty(t, plugin.Meta.VideoRetryInputs)
			}
			now := time.Now().Unix()
			request := AsyncVideoRequest{Body: []byte(tc.body), ContentType: "application/json", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key, Version: plugin.Meta.Version}}, MixedRouting: &MixedVideoRouting{Model: "public-video"}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, now+5*86400))
			require.Equal(t, 2, request.ReferenceRecoveryVersion)
			assert.Equal(t, []byte(tc.body), request.Body)
			assert.Equal(t, []byte(tc.body), request.OriginalBody)
			for _, rebound := range []bool{false, true} {
				if rebound {
					// A channel change rebuilds slots from the original public body.
					request.Body = []byte(`{"references":[]}`)
					upstream := "seedance2.5-A"
					var original map[string]any
					require.NoError(t, common.Unmarshal([]byte(tc.body), &original))
					if name, ok := original["model"].(string); ok {
						upstream = name
					}
					channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Models: upstream}
					channel.SetSetting(relaydto.ChannelSettings{TaskPluginKey: plugin.Meta.Key})
					require.NoError(t, RebindMixedVideoRequest(&request, MixedVideoCandidate{Channel: channel, UpstreamModel: upstream, Binding: jsplugin.ProtocolBinding{Plugin: plugin}}, now+5*86400))
				}
				require.Len(t, request.ReferenceInputs, len(tc.paths))
				assert.Equal(t, tc.unsupported, request.ReferenceLocalizationUnsupported)
				for index, input := range request.ReferenceInputs {
					assert.Equal(t, tc.paths[index], input.Path)
					assert.Equal(t, tc.kinds[index], input.Kind)
					if tc.roles != nil {
						assert.Equal(t, tc.roles[index], input.Role)
					}
				}
			}
		})
	}
}

func TestLegacyDramaMixedVideoReferenceAliases(t *testing.T) {
	plugin := legacyDramaRetryPluginFixture(t, "", "")
	for _, tc := range []struct {
		name, model, media, videoInput, decodeError string
		paths                                       []string
	}{
		{"mixed image arrays", "minimax-h3", `"images":["https://source.example/one.png"],"image_urls":[{"url":"HTTPS://source.example/two.png","source":"https://ignored.example/fallback.png"}],"referenceImages":[{"source":"https://source.example/three.png"}]`, "none", "", []string{"/images/0", "/image_urls/0/url", "/referenceImages/0/source"}},
		{"ordered wrapped sources", "minimax-h3", `"references":[{"type":"image","role":"reference","source":{"url":"https://source.example/one.png"}},{"type":"image","role":"reference","source":{"url":"","source":"HTTPS://source.example/two.png"}}]`, "none", "", []string{"/references/0/source/url", "/references/1/source/source"}},
		{"frames", "seedance2.0-A", `"aspect_ratio":"Auto","first_image":{"url":"https://source.example/first.png"},"last_image":"https://source.example/last.png"`, "none", "", []string{"/first_image/url", "/last_image"}},
		{"video and audio aliases", "seedance2.5-G", `"referenceVideos":["https://source.example/video.mp4"],"audio_urls":["https://source.example/voice.wav"]`, "video", "", []string{"/referenceVideos/0", "/audio_urls/0"}},
		{"duplicate alias URL", "minimax-h3", `"reference_images":["https://source.example/duplicate.png"],"referenceImages":["https://source.example/duplicate.png"]`, "", "Duplicate reference media source", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Id: 1, Type: constant.ChannelTypeTaskPlugin, Models: tc.model}
			channel.SetSetting(relaydto.ChannelSettings{TaskPluginKey: plugin.Meta.Key})
			binding, _, err := MixedVideoChannelBinding(channel, tc.model, "create")
			require.NoError(t, err)
			profile, err := EffectiveMixedVideoProfile(binding, tc.model, VideoModelOverrides{})
			require.NoError(t, err)
			raw := []byte(fmt.Sprintf(`{"model":%q,"prompt":"animate","seconds":5,"resolution":"720p",%s}`, tc.model, tc.media))
			normalized, contentType, body, err := NormalizeMixedVideoDefaults(raw, "application/json", "create", []jsplugin.VideoProfile{profile})
			require.NoError(t, err)
			_, err = DecodeMixedVideoCandidate(t.Context(), channel, tc.model, "create", normalized, VideoModelOverrides{}, 0)
			if tc.decodeError != "" {
				require.ErrorContains(t, err, tc.decodeError)
				return
			}
			require.NoError(t, err)
			facts, err := MixedVideoPublicFacts(body, "create", nil, profile)
			require.NoError(t, err)
			assert.Equal(t, tc.videoInput, facts["video_input"])

			request := AsyncVideoRequest{Body: normalized, ContentType: contentType, MixedRouting: &MixedVideoRouting{Model: tc.model}, Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key}}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, time.Now().Unix()+86400))
			assert.Equal(t, normalized, request.Body)
			require.Len(t, request.ReferenceInputs, len(tc.paths))
			for index, input := range request.ReferenceInputs {
				assert.Equal(t, tc.paths[index], input.Path)
				assert.Contains(t, input.Source, "://source.example/")
			}
		})
	}
}

func TestSudashuiVideoRetryReferenceSlots(t *testing.T) {
	previousRegistry, previousTimeout := jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes
	jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes = jsplugin.NewRegistry(), 60
	t.Cleanup(func() { jsplugin.DefaultRegistry, constant.TaskTimeoutMinutes = previousRegistry, previousTimeout })
	source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "sudashui", "plugin.js"))
	require.NoError(t, err)
	plugin, err := jsplugin.DefaultRegistry.Register(string(source), jsplugin.Options{})
	require.NoError(t, err)
	for _, tc := range []struct{ name, value, suffix string }{
		{"scalars", `"HTTPS://source.example/input"`, ""},
		{"arrays", `["https://source.example/input"]`, "/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"model":"sd-api-2-fast-720p","prompt":"animate","seconds":5,"resolution":"720p","reference_images":%s,"reference_videos":%s,"reference_audios":%s}`, tc.value, tc.value, tc.value))
			view, _, err := MixedVideoRequestContext(raw, "application/json", "create")
			require.NoError(t, err)
			_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, (jsplugin.ProtocolRequestContext{RouteRequestContext: view, Protocol: "openai_video", Operation: "create", Model: "sd-api-2-fast-720p"}).JSValue())
			require.NoError(t, err)
			request := AsyncVideoRequest{Body: raw, ContentType: "application/json", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: plugin.Meta.Key}}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, time.Now().Unix()+86400))
			require.Len(t, request.ReferenceInputs, 3)
			for index, kind := range []string{"image", "video", "audio"} {
				assert.Equal(t, "/reference_"+kind+"s"+tc.suffix, request.ReferenceInputs[index].Path)
				assert.Equal(t, kind, request.ReferenceInputs[index].Kind)
			}
		})
	}
}

func TestLegacyDramaMixedVideoWrappedLocalRecovery(t *testing.T) {
	job, cfg, imageCfg := mediaInputFixture(t)
	plugin := legacyDramaRetryPluginFixture(t, "", "")
	require.Empty(t, plugin.Meta.VideoRetryInputs)
	job.Group, job.Provider, job.Model = "default", "drama", "seedance2.5-A"
	channel := model.Channel{Group: job.Group, Models: job.Model, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "legacy-drama-test-key"}
	channel.SetSetting(relaydto.ChannelSettings{TaskPluginKey: job.Provider})
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{Group: job.Group, Model: job.Model, ChannelId: channel.Id, Enabled: true}).Error)
	pool := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash(job.Group, "mixed"), Group: job.Group, Platform: "mixed", PoolMode: "model", Enabled: true, AsyncEnabled: true, Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
	require.NoError(t, model.DB.Create(&pool).Error)
	resolution := VideoPoolResolution("720p")
	binding := MediaPoolBindingKey(pool, job.Model, resolution, "video")
	require.NoError(t, model.DB.Create(&model.ImageChannelPool{BindingKey: binding, ChannelId: channel.Id, Priority: 1}).Error)

	body := []byte(`{"model":"seedance2.5-A","prompt":"@图片1 preserve https://source.example/image.png literally","seconds":5,"resolution":"720p","aspect_ratio":"16:9","references":[{"type":"image","role":"reference","source":{"url":"HTTPS://source.example/image.png","source":"https://ignored.example/fallback.png"}}]}`)
	request := AsyncVideoRequest{Body: body, ContentType: "application/json", Operation: "create", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: job.Provider, Version: plugin.Meta.Version}}, MixedRouting: &MixedVideoRouting{Model: job.Model, Group: job.Group, PoolMode: pool.PoolMode, Resolution: resolution, BindingKey: binding}}
	require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, job.ExpiresAt))
	request.Details = request.RequestDetails()
	encoded, err := common.Marshal(request)
	require.NoError(t, err)
	job.RequestCipher, err = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
	require.NoError(t, err)
	job.ChannelId, job.Quota, job.BillingStatus, job.AttemptGeneration = channel.Id, 100, "reserved", 0
	require.NoError(t, model.DB.Save(&job).Error)
	task := model.Task{TaskID: job.TaskId, UserId: job.UserId, ChannelId: job.ChannelId, Platform: "drama", Status: model.TaskStatusSubmitted, Quota: job.Quota, Properties: model.Properties{OriginModelName: job.Model, UpstreamModelName: job.Model}}
	task.PrivateData = model.TaskPrivateData{AsyncMedia: true, DurableBilling: true, SubmitAccountingRecorded: true, TokenId: job.TokenId, UpstreamTaskID: "native-original", CurrentAttempt: &model.VideoTaskAttempt{MixedVideo: true, PluginKey: "drama"}}
	require.NoError(t, model.DB.Create(&task).Error)
	policy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "download", Name: "Local reference recovery", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"素材下载超时"}}, Steps: []mediaretry.Step{{ID: "local", Action: mediaretry.LocalSameChannel, MaxAttempts: 1}}}}}
	_, err = model.SaveMediaRetryPolicy(t.Context(), policy, 0, job.UserId, nil)
	require.NoError(t, err)
	const reason = "（图片1）素材下载超时，请联系管理员处理"
	scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), task, &relaycommon.TaskInfo{TaskID: "native-original", Status: string(model.TaskStatusFailure), Reason: reason}, 200, []byte(`{"id":"native-original","status":"failed","metadata":{"url":"`+reason+`"}}`))
	require.NoError(t, err)
	require.True(t, scheduled)
	require.NoError(t, model.DB.First(&task, task.ID).Error)
	require.NoError(t, model.DB.First(&job, job.ID).Error)
	require.NotNil(t, task.PrivateData.MediaRetryDecision)
	assert.Equal(t, mediaretry.LocalSameChannel, task.PrivateData.MediaRetryDecision.Action)
	assert.Empty(t, task.PrivateData.MediaRetryDecision.BlockedReason)
	assert.Equal(t, "prepare", job.ReferenceRecoveryStage)
	assert.Equal(t, "reserved", job.BillingStatus)
	assert.Equal(t, 100, job.Quota)
	assert.Equal(t, int64(1), task.AttemptGeneration)
	require.Len(t, task.PrivateData.VideoAttempts, 1)
	assert.Equal(t, "native-original", task.PrivateData.VideoAttempts[0].UpstreamTaskID)
	// Recovery survives persistence without falling back to display-only details.
	plain, err := DecryptImagePayload(retainAsyncVideoRequestDetails(job), "media-request:"+job.TaskId)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(plain, &request))
	require.Len(t, request.ReferenceInputs, 1)
	payload := asyncImageBytesFixture(t).Data
	var remaining atomic.Int64
	remaining.Store(cfg.MaxFileBytes)
	downloads := 0
	client := &http.Client{Transport: mediaInputRoundTripper(func(req *http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload)), Request: req}, nil
	})}
	_, err = downloadMediaInput(t.Context(), job, request.ReferenceInputs[0], cfg, imageCfg, request.ReferenceURLExpiresAt, &remaining, client)
	require.NoError(t, err)
	prepared, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	require.NoError(t, err)
	assert.True(t, prepared.LocalReferencesReady)
	t.Run("ordinary pool can select the legacy channel", func(t *testing.T) {
		channels, err := VideoRetryCandidateChannels(t.Context(), job, prepared, job.Model)
		require.NoError(t, err)
		require.Len(t, channels, 1)
		assert.Equal(t, channel.Id, channels[0].Id)
	})
	t.Run("conditional routing can select the legacy target", func(t *testing.T) {
		rule := model.VideoRoutingRule{ID: "legacy-local", Name: "Legacy local", OnExhausted: "fail", Targets: []model.VideoRoutingTarget{{ChannelID: channel.Id, Model: job.Model}}}
		snapshot, _, _, _, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1}, rule, job.Group, job.Model, request.Operation, body, request.ContentType, map[string]any{"seconds": float64(5), "resolution": "720p"}, job.UserId, 0, nil)
		require.NoError(t, err)
		conditional := prepared
		conditional.ConditionalRouting = snapshot
		channels, err := VideoRetryCandidateChannels(t.Context(), job, conditional, job.Model)
		require.NoError(t, err)
		require.Len(t, channels, 1)
		assert.Equal(t, channel.Id, channels[0].Id)
	})
	var preparedBody map[string]any
	require.NoError(t, common.Unmarshal(prepared.Body, &preparedBody))
	var expectedBody map[string]any
	require.NoError(t, common.Unmarshal(body, &expectedBody))
	localSource := preparedBody["references"].([]any)[0].(map[string]any)["source"].(map[string]any)["url"]
	assert.Contains(t, localSource, "/api/media-inputs/")
	expectedBody["references"].([]any)[0].(map[string]any)["source"].(map[string]any)["url"] = localSource
	assert.Equal(t, expectedBody, preparedBody, "only the frozen source changes; prompt, order, roles and generation parameters survive")
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{"model": job.Model, "body": map[string]any{"kind": "json", "value": preparedBody}})
	require.NoError(t, err)
	intent, ok := value.(map[string]any)
	require.True(t, ok)
	value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"model": job.Model, "baseUrl": "https://upstream.example", "apiKey": "test", "requestBody": intent["requestBody"]})
	require.NoError(t, err)
	encoded, err = common.Marshal(value)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "/api/media-inputs/")
	assert.Contains(t, string(encoded), "source.example/image.png literally")
	reused, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
	require.NoError(t, err)
	assert.JSONEq(t, string(prepared.Body), string(reused.Body))
	assert.Equal(t, 1, downloads)
}

func TestLegacyDramaMixedVideoAliasAdmission(t *testing.T) {
	plugin := legacyDramaRetryPluginFixture(t, "", "")
	setting := `{"task_plugin_key":"drama"}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Models: "seedance2.5-A", Key: "test-key", Setting: &setting}
	for _, tc := range []struct {
		field, kind string
	}{
		{"image_urls", "image"}, {"referenceImages", "image"},
		{"videos", "video"}, {"video_urls", "video"}, {"referenceVideos", "video"},
		{"audios", "audio"}, {"audio_urls", "audio"}, {"referenceAudios", "audio"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			mention := map[string]string{"image": "@图片2", "video": "@视频1", "audio": "@音频1"}[tc.kind]
			body := map[string]any{"model": "seedance2.5-A", "prompt": "@图片1 " + mention, "seconds": 5, "resolution": "720p", "image": "https://source.example/first.png", tc.field: []string{"https://source.example/" + tc.kind}}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			candidate, err := DecodeMixedVideoCandidate(t.Context(), channel, "seedance2.5-A", "create", encoded, VideoModelOverrides{}, 1)
			require.NoError(t, err, "aliases accepted by the real plugin must preserve media identity in mixed admission")
			assert.Equal(t, "reference_to_video", candidate.Intent["action"])
			decoded, ok := candidate.Intent["requestBody"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, []any{
				map[string]any{"type": tc.kind, "role": "reference", "source": "https://source.example/" + tc.kind},
				map[string]any{"type": "image", "role": "reference", "source": "https://source.example/first.png"},
			}, decoded["references"])
			facts, err := MixedVideoPublicFacts(body, "create", nil)
			require.NoError(t, err)
			wantVideo := "none"
			if tc.kind == "video" {
				wantVideo = "video"
			}
			assert.Equal(t, wantVideo, facts["video_input"], "video aliases must retain their billing classification")

			inputName := "reference_" + tc.kind + "s"
			profile := jsplugin.VideoProfile{Modes: []jsplugin.VideoModeProfile{{Name: "reference_to_video", Inputs: []jsplugin.VideoInputProfile{{Name: inputName, Kind: tc.kind, Sources: []string{"url"}, MaxItems: 1, Required: true}}}}}
			aliasBody := map[string]any{tc.field: []any{"https://source.example/" + tc.kind}}
			require.NoError(t, ValidateVideoProfileRequest(profile, "reference_to_video", aliasBody, nil))
			aliasBody[inputName] = []any{"https://source.example/extra"}
			assert.ErrorContains(t, ValidateVideoProfileRequest(profile, "reference_to_video", aliasBody, nil), "exceeds the enabled limit", "aliases and canonical fields share one input limit")
			delete(aliasBody, inputName)
			aliasBody[tc.field] = []any{"data:" + tc.kind + "/fixture;base64,eA=="}
			assert.ErrorContains(t, ValidateVideoProfileRequest(profile, "reference_to_video", aliasBody, nil), "does not allow source")
			assert.ErrorContains(t, ValidateVideoProfileRequest(jsplugin.VideoProfile{Modes: []jsplugin.VideoModeProfile{{Name: "reference_to_video"}}}, "reference_to_video", aliasBody, nil), "not enabled")

			request := AsyncVideoRequest{Body: encoded, ContentType: "application/json", Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "drama", Version: plugin.Meta.Version}}, MixedRouting: &MixedVideoRouting{Model: "seedance2.5-A"}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, time.Now().Unix()+86400))
			require.Len(t, request.ReferenceInputs, 2)
			assert.Contains(t, request.ReferenceInputs, AsyncVideoReferenceInput{Index: 0, Path: "/" + tc.field + "/0", Kind: tc.kind, KindIndex: 1, Source: "https://source.example/" + tc.kind})
		})
	}
	for _, primary := range []bool{false, true} {
		t.Run(fmt.Sprintf("images array with primary %t", primary), func(t *testing.T) {
			body := map[string]any{"model": "seedance2.5-A", "prompt": "@图片1 @图片2", "seconds": 5, "resolution": "720p", "images": []string{"https://source.example/first.png", "https://source.example/last.png"}}
			if primary {
				body["image"], body["prompt"] = "https://source.example/primary.png", "@图片1 @图片2 @图片3"
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			candidate, err := DecodeMixedVideoCandidate(t.Context(), channel, "seedance2.5-A", "create", encoded, VideoModelOverrides{}, 1)
			require.NoError(t, err)
			decoded, ok := candidate.Intent["requestBody"].(map[string]any)
			require.True(t, ok)
			want := []any{
				map[string]any{"type": "image", "role": "reference", "source": "https://source.example/first.png"},
				map[string]any{"type": "image", "role": "reference", "source": "https://source.example/last.png"},
			}
			if primary {
				want = append(want, map[string]any{"type": "image", "role": "reference", "source": "https://source.example/primary.png"})
			}
			assert.Equal(t, want, decoded["references"])
		})
	}
	t.Run("paired frame aliases", func(t *testing.T) {
		pairedChannel := channel
		pairedChannel.Models = "seedance2.0-A"
		body := []byte(`{"model":"seedance2.0-A","prompt":"transition","seconds":5,"resolution":"720p","first_image":"https://source.example/first.png","last_image":"https://source.example/last.png"}`)
		candidate, err := DecodeMixedVideoCandidate(t.Context(), pairedChannel, "seedance2.0-A", "create", body, VideoModelOverrides{}, 1)
		require.NoError(t, err)
		assert.Equal(t, "first_last_frame", candidate.Intent["action"])
		decoded, ok := candidate.Intent["requestBody"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, []any{
			map[string]any{"type": "image", "role": "first_frame", "source": "https://source.example/first.png"},
			map[string]any{"type": "image", "role": "last_frame", "source": "https://source.example/last.png"},
		}, decoded["references"])
	})
}

func TestLegacyDramaMixedVideoLocalRecovery(t *testing.T) {
	for _, action := range []string{mediaretry.LocalSameChannel, mediaretry.LocalOtherChannel} {
		t.Run(action, func(t *testing.T) {
			job, cfg, imageCfg := mediaInputFixture(t)
			plugin := legacyDramaRetryPluginFixture(t, "", "")
			require.Empty(t, plugin.Meta.VideoRetryInputs)
			policyConfig := model.ImageGroupPolicy{Group: "default", Platform: "mixed", PolicyKey: ImageIdentityHash("default", "mixed"), Enabled: true, AsyncEnabled: true, PoolMode: "model_resolution", Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
			require.NoError(t, model.DB.Create(&policyConfig).Error)
			bindingKey := MediaPoolBindingKey(policyConfig, "seedance2.5-A", "1K", "video")
			setting := `{"task_plugin_key":"drama"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "legacy-drama", Status: common.ChannelStatusEnabled, Models: "seedance2.5-A", Group: "default", Key: "legacy-key", Setting: &setting}
			require.NoError(t, model.DB.Create(&channel).Error)
			other := channel
			other.Id, other.Key = 0, "other-legacy-key"
			require.NoError(t, model.DB.Create(&other).Error)
			for _, member := range []model.Channel{channel, other} {
				require.NoError(t, model.DB.Create(&model.Ability{ChannelId: member.Id, Group: "default", Model: "seedance2.5-A", Enabled: true}).Error)
				require.NoError(t, model.DB.Create(&model.ImageChannelPool{BindingKey: bindingKey, Group: "default", Platform: "mixed", Mode: policyConfig.PoolMode, Model: "seedance2.5-A", Resolution: "1K", ChannelId: member.Id}).Error)
			}
			selected := channel
			if action == mediaretry.LocalOtherChannel {
				selected = other
			}
			body := []byte(`{"model":"seedance2.5-A","prompt":"@图片1 preserve https://source.example/image.png literally","seconds":5,"resolution":"720p","references":[{"type":"image","role":"reference","source":"https://source.example/image.png"}]}`)
			request := AsyncVideoRequest{Body: body, ContentType: "application/json", Operation: "create", PluginHash: plugin.Engine.SourceFingerprint(), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "drama", Version: plugin.Meta.Version}}, MixedRouting: &MixedVideoRouting{Model: "seedance2.5-A", Group: "default", PoolMode: policyConfig.PoolMode, BindingKey: bindingKey, Resolution: "1K"}}
			require.NoError(t, FreezeAsyncVideoReferenceRecovery(&request, nil, job.ExpiresAt))
			request.Details = request.RequestDetails()
			encoded, err := common.Marshal(request)
			require.NoError(t, err)
			job.RequestCipher, err = EncryptImagePayload(encoded, "media-request:"+job.TaskId)
			require.NoError(t, err)
			job.Group = "default"
			job.ChannelId, job.Provider, job.Model, job.Quota, job.BillingStatus, job.AttemptGeneration = channel.Id, "drama", "seedance2.5-A", 100, "reserved", 0
			require.NoError(t, model.DB.Save(&job).Error)
			task := model.Task{TaskID: job.TaskId, UserId: job.UserId, ChannelId: job.ChannelId, Platform: "drama", Status: model.TaskStatusSubmitted, Quota: job.Quota, Properties: model.Properties{OriginModelName: job.Model, UpstreamModelName: job.Model}}
			task.PrivateData = model.TaskPrivateData{AsyncMedia: true, DurableBilling: true, SubmitAccountingRecorded: true, TokenId: job.TokenId, UpstreamTaskID: "native-original", CurrentAttempt: &model.VideoTaskAttempt{MixedVideo: true, PluginKey: "drama"}}
			require.NoError(t, model.DB.Create(&task).Error)
			policy := mediaretry.Policy{Rules: []mediaretry.Rule{{ID: "download", Name: "Local reference recovery", Enabled: true, Scope: mediaretry.Scope{MediaTypes: []string{"video"}}, Match: mediaretry.Match{ContainsAny: []string{"素材下载超时"}}, Steps: []mediaretry.Step{{ID: "local", Action: action, MaxAttempts: 1}}}}}
			_, err = model.SaveMediaRetryPolicy(t.Context(), policy, 0, job.UserId, nil)
			require.NoError(t, err)
			const reason = "（图片1）素材下载超时，请联系管理员处理"
			scheduled, err := TryAsyncVideoReferenceRecovery(t.Context(), task, &relaycommon.TaskInfo{TaskID: "native-original", Status: string(model.TaskStatusFailure), Reason: reason}, 200, []byte(`{"id":"native-original","status":"failed","metadata":{"url":"`+reason+`"}}`))
			require.NoError(t, err)
			require.True(t, scheduled)
			require.NoError(t, model.DB.First(&task, task.ID).Error)
			require.NoError(t, model.DB.First(&job, job.ID).Error)
			require.NotNil(t, task.PrivateData.MediaRetryDecision)
			assert.Equal(t, action, task.PrivateData.MediaRetryDecision.Action)
			assert.Empty(t, task.PrivateData.MediaRetryDecision.BlockedReason)
			assert.Equal(t, "prepare", job.ReferenceRecoveryStage)
			assert.Equal(t, "reserved", job.BillingStatus)
			assert.Equal(t, 100, job.Quota)
			assert.Equal(t, int64(1), task.AttemptGeneration)
			require.Len(t, task.PrivateData.VideoAttempts, 1)
			assert.Equal(t, "native-original", task.PrivateData.VideoAttempts[0].UpstreamTaskID)
			// Recovery survives persistence without falling back to display-only details.
			plain, err := DecryptImagePayload(retainAsyncVideoRequestDetails(job), "media-request:"+job.TaskId)
			require.NoError(t, err)
			require.NoError(t, common.Unmarshal(plain, &request))
			require.Len(t, request.ReferenceInputs, 1)
			candidates, err := VideoRetryCandidateChannels(t.Context(), job, request, job.Model)
			require.NoError(t, err)
			require.Len(t, candidates, 1, "the worker must accept a legacy Drama channel for local recovery")
			assert.Equal(t, selected.Id, candidates[0].Id)
			preview, err := PreviewVideoRetryEligibility(t.Context(), task.TaskID, *request.RetryFact, *request.PendingDecision)
			require.NoError(t, err)
			assert.True(t, preview.Eligible, "%+v", preview)
			assert.Equal(t, []int{selected.Id}, preview.CandidateChannelIDs)
			payload := asyncImageBytesFixture(t).Data
			var remaining atomic.Int64
			remaining.Store(cfg.MaxFileBytes)
			downloads := 0
			client := &http.Client{Transport: mediaInputRoundTripper(func(req *http.Request) (*http.Response, error) {
				downloads++
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: int64(len(payload)), Request: req}, nil
			})}
			_, err = downloadMediaInput(t.Context(), job, request.ReferenceInputs[0], cfg, imageCfg, request.ReferenceURLExpiresAt, &remaining, client)
			require.NoError(t, err)
			prepared, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
			require.NoError(t, err)
			assert.True(t, prepared.LocalReferencesReady)
			var preparedBody map[string]any
			require.NoError(t, common.Unmarshal(prepared.Body, &preparedBody))
			var expectedBody map[string]any
			require.NoError(t, common.Unmarshal(body, &expectedBody))
			localSource := preparedBody["references"].([]any)[0].(map[string]any)["source"]
			assert.Contains(t, localSource, "/api/media-inputs/")
			expectedBody["references"].([]any)[0].(map[string]any)["source"] = localSource
			assert.Equal(t, expectedBody, preparedBody, "only the frozen source changes; prompt, order, roles and generation parameters survive")
			reused, err := PrepareAsyncVideoLocalReferences(t.Context(), job, request)
			require.NoError(t, err)
			assert.JSONEq(t, string(prepared.Body), string(reused.Body))
			assert.Equal(t, 1, downloads)
			// Exercise both worker phases, including channel rebinding after preparation
			// and the durable dispatch fence. The upstream is isolated to this test.
			var submissions atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				submissions.Add(1)
				assert.Equal(t, "/v1/videos", req.URL.Path)
				assert.Equal(t, "Bearer "+selected.Key, req.Header.Get("Authorization"))
				var posted map[string]any
				if !assert.NoError(t, common.DecodeJson(req.Body, &posted)) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				assert.Equal(t, preparedBody["references"], posted["references"])
				assert.Equal(t, preparedBody["prompt"], posted["prompt"])
				_, _ = io.WriteString(w, `{"id":"replacement-native","status":"queued"}`)
			}))
			defer upstream.Close()
			previousSubmit := SubmitAsyncVideoReferenceRetryFunc
			t.Cleanup(func() { SubmitAsyncVideoReferenceRetryFunc = previousSubmit })
			SubmitAsyncVideoReferenceRetryFunc = func(ctx context.Context, activeJob model.AsyncMediaJob, activeTask model.Task, activeRequest AsyncVideoRequest) error {
				assert.Equal(t, selected.Id, activeJob.ChannelId)
				assert.Equal(t, selected.Key, activeRequest.Channel.Key)
				assert.Equal(t, channel.Id, activeTask.BillingChannelID(), "changing the execution channel retains the original hold")
				assert.True(t, activeRequest.LocalReferencesReady)
				assert.JSONEq(t, string(prepared.Body), string(activeRequest.Body))
				intent, err := DecodeMixedVideoCandidate(ctx, activeRequest.Channel, job.Model, "create", activeRequest.Body, VideoModelOverrides{}, job.UserId, activeRequest.ContentType)
				require.NoError(t, err)
				pluginContext := map[string]any{"model": job.Model, "upstreamModel": intent.UpstreamModel, "baseUrl": upstream.URL, "apiKey": activeRequest.Channel.Key, "requestBody": intent.Intent["requestBody"]}
				value, err := plugin.Engine.Call(ctx, "buildSubmitRequest", pluginContext)
				require.NoError(t, err)
				descriptor := value.(map[string]any)
				payload, err := common.Marshal(descriptor["body"])
				require.NoError(t, err)
				req, err := http.NewRequestWithContext(ctx, descriptor["method"].(string), descriptor["url"].(string), bytes.NewReader(payload))
				require.NoError(t, err)
				for name, value := range descriptor["headers"].(map[string]any) {
					req.Header.Set(name, value.(string))
				}
				require.NoError(t, model.DispatchAsyncVideoRecovery(ctx, activeJob, activeTask, activeRequest.PendingDecision, 0, "local_download"))
				assert.ErrorIs(t, model.DispatchAsyncVideoRecovery(ctx, activeJob, activeTask, activeRequest.PendingDecision, 0, "local_download"), model.ErrTaskAttemptConflict)
				response, err := upstream.Client().Do(req)
				require.NoError(t, err)
				defer response.Body.Close()
				var receipt map[string]any
				require.NoError(t, common.DecodeJson(response.Body, &receipt))
				value, err = plugin.Engine.Call(ctx, "parseSubmitResponse", pluginContext, map[string]any{"statusCode": response.StatusCode, "body": receipt})
				require.NoError(t, err)
				parsed := value.(map[string]any)
				data, err := common.Marshal(parsed["taskData"])
				require.NoError(t, err)
				state, err := common.Marshal(parsed["state"])
				require.NoError(t, err)
				won, err := model.CommitAsyncVideoReferenceSubmission(ctx, activeJob, activeTask, parsed["taskId"].(string), data, state)
				require.NoError(t, err)
				require.True(t, won)
				won, err = model.CommitAsyncVideoReferenceSubmission(ctx, activeJob, activeTask, parsed["taskId"].(string), data, state)
				require.NoError(t, err)
				assert.False(t, won, "a duplicate receipt cannot create another attempt")
				return nil
			}
			for range 2 {
				require.NoError(t, model.DB.First(&job, job.ID).Error)
				claimed, err := model.ClaimAsyncMediaJob(t.Context(), &job, common.GetUUID(), 120)
				require.NoError(t, err)
				require.True(t, claimed)
				require.NoError(t, ProcessAsyncMediaJob(t.Context(), job, cfg))
			}
			require.NoError(t, model.DB.First(&job, job.ID).Error)
			require.NoError(t, model.DB.First(&task, task.ID).Error)
			assert.EqualValues(t, 1, submissions.Load())
			assert.Equal(t, "submitted", job.Status)
			assert.Equal(t, "waiting", job.ReferenceRecoveryStage)
			assert.Equal(t, "reserved", job.BillingStatus)
			assert.Equal(t, 100, job.Quota)
			assert.Equal(t, "replacement-native", task.PrivateData.UpstreamTaskID)
			require.NotNil(t, task.PrivateData.MediaRetryState)
			assert.Equal(t, 1, task.PrivateData.MediaRetryState.TotalUsed)
			assert.EqualValues(t, 1, task.AttemptGeneration)
			assert.Equal(t, selected.Id, task.ChannelId)
		})
	}
}
