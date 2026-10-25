package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
	entities := []any{&model.Task{}, &model.AsyncMediaJob{}, &model.MediaInputObject{}, &model.MediaArtifactObject{}, &model.ImageInputAlias{}, &model.ImageInputObject{}, &model.ImageStorageObject{}}
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
