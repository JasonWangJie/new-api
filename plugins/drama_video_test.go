package plugins_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	pluginadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	relaydto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func loadDramaVideoPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("drama")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "drama"})
	require.NoError(t, err)
	return plugin
}

func decodeDramaVideoRequest(t *testing.T, plugin *jsplugin.LoadedPlugin, modelName, operation string, body map[string]any) (map[string]any, error) {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": modelName, "operation": operation, "body": map[string]any{"kind": "json", "value": body},
	})
	if err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, common.Unmarshal(encoded, &intent))
	return intent, nil
}

func TestDramaVideoModelsAndUpstreamRoutes(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		model, resolution, path string
	}{
		{"seedance2.0-A", "4k", "/v1/videos"},
		{"seedance2.5-A", "720p", "/v1/videos"},
		{"minimax-h3", "1080p", "/v1/videos"},
		{"seedance2.0-E-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-E-1080p", "1080p", "/v1/video/generations"},
		{"seedance2.0-F-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-fast-F-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-G-720p", "720p", "/v1/video/generations"},
		{"seedance2.0-E", "480p", "/v1/videos"},
		{"seedance2.0-E", "4k", "/v1/videos"},
		{"seedance2.0-fast-E", "720p", "/v1/videos"},
		{"seedance2.0-fast-E", "4k", "/v1/videos"},
		{"seedance2.0-Mini-E", "480p", "/v1/videos"},
		{"seedance2.5-E", "1080p", "/v1/videos"},
		{"seedance2.0-F", "720px", "/v1/videos"},
		{"seedance2.5-F", "720p", "/v1/videos"},
		{"seedance2.0-G", "4k", "/v1/videos"},
		{"seedance2.0-fast-G", "720p", "/v1/videos"},
		{"seedance2.0-mini-G", "480p", "/v1/videos"},
		{"seedance2.5-G", "1080p", "/v1/videos"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			binding, found := jsplugin.DefaultRegistry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", tc.model)
			require.True(t, found)
			assert.Equal(t, "drama", binding.Plugin.Meta.Key)
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			request := map[string]any{"prompt": "waves at sunset", "seconds": 5, "resolution": tc.resolution, "aspect_ratio": "16:9"}
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", request)
			require.NoError(t, err)
			assert.Equal(t, "text_to_video", intent["action"])
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", intent["requestBody"], nil))
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": tc.model, "requestBody": intent["requestBody"], "action": intent["action"], "baseUrl": "https://upstream.example", "apiKey": "upstream-test-key",
			})
			require.NoError(t, err)
			descriptor := value.(map[string]any)
			assert.Equal(t, "https://upstream.example"+tc.path, descriptor["url"])
			assert.Equal(t, "POST", descriptor["method"])
			assert.Equal(t, "Bearer upstream-test-key", descriptor["headers"].(map[string]any)["Authorization"])
			body := descriptor["body"].(map[string]any)
			assert.Equal(t, tc.model, body["model"])
			assert.Equal(t, tc.resolution, body["resolution"])
			assert.EqualValues(t, 5, body["seconds"])
			value, err = plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
				"model": tc.model, "baseUrl": "https://upstream.example", "apiKey": "upstream-test-key", "taskId": "task/private?one",
			})
			require.NoError(t, err)
			assert.Equal(t, "https://upstream.example"+tc.path+"/task%2Fprivate%3Fone", value.(map[string]any)["url"])
		})
	}
	for _, modelName := range []string{"seedance2.5-G", "seedance2.0-A", "seedance2.5-A", "minimax-h3"} {
		t.Run(modelName+" mapping keeps client identity", func(t *testing.T) {
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "my-video", "upstreamModel": modelName, "operation": "create", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "waves", "seconds": 8}},
			})
			require.NoError(t, err)
			intent := value.(map[string]any)
			assert.Equal(t, "my-video", intent["model"])
			value, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": "my-video", "upstreamModel": modelName, "baseUrl": "https://upstream.example", "apiKey": "test-key", "requestBody": intent["requestBody"],
			})
			require.NoError(t, err)
			assert.Equal(t, modelName, value.(map[string]any)["body"].(map[string]any)["model"])
			assert.NotContains(t, value.(map[string]any), "model")
		})
	}
}

func TestDramaVideoPricingRowsPreserveGenerationLimits(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, modelName := range plugin.Meta.Models {
		t.Run(modelName, func(t *testing.T) {
			profile, found := plugin.Meta.VideoProfileForModel(modelName)
			require.True(t, found)
			require.NotEmpty(t, profile.Modes)
			schema, _ := plugin.Meta.UsageForModel(modelName)
			expected := []string{"480p", "720p", "1080p", "4k"}
			if modelName == "seedance2.0-F" {
				expected = append(expected, "720px")
			}
			assert.Equal(t, expected, schema["resolution"].Enum,
				"all video models retain four editable price rows and any provider-specific resolution")
			for _, resolution := range schema["resolution"].Enum {
				_, err := decodeDramaVideoRequest(t, plugin, modelName, "create", map[string]any{
					"prompt": "waves at sunset", "seconds": 5, "resolution": resolution,
				})
				if slices.Contains(profile.Modes[0].Resolutions, resolution) {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "resolution is not supported")
				}
			}
		})
	}
}

func TestDramaVideoSecondBilledProfiles(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		model       string
		maxSeconds  int
		resolutions []string
	}{
		{"seedance2.0-A", 15, []string{"480p", "720p", "1080p", "4k"}},
		{"seedance2.5-A", 30, []string{"480p", "720p"}},
		{"minimax-h3", 15, []string{"480p", "720p", "1080p"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", map[string]any{"prompt": "sunrise over the sea"})
			require.NoError(t, err)
			request := intent["requestBody"].(map[string]any)
			assert.EqualValues(t, 4, request["seconds"])
			assert.Equal(t, "480p", request["resolution"])
			assert.Equal(t, "16:9", request["aspect_ratio"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, "text_to_video", request, nil))
			assert.Equal(t, tc.maxSeconds, profile.Modes[0].Duration.Max)
			assert.Equal(t, tc.resolutions, profile.Modes[0].Resolutions)
			schema, _ := plugin.Meta.UsageForModel(tc.model)
			assert.Len(t, schema, 2)
			assert.Contains(t, schema, "seconds")
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": tc.model, "requestBody": request, "usagePurpose": "facts"})
			require.NoError(t, err)
			facts := value.(map[string]any)
			assert.Len(t, facts, 2)
			assert.EqualValues(t, 4, facts["seconds"])
			assert.Equal(t, "480p", facts["resolution"])
			for _, operation := range []string{"edit", "extend"} {
				_, err = decodeDramaVideoRequest(t, plugin, tc.model, operation, map[string]any{"prompt": "sunrise"})
				require.ErrorContains(t, err, "editing and extension require")
			}
			for _, invalid := range []any{3, tc.maxSeconds + 1, -1, true, "18446744073686646784"} {
				_, err = decodeDramaVideoRequest(t, plugin, tc.model, "create", map[string]any{"prompt": "sunrise", "seconds": invalid})
				require.Error(t, err)
			}
		})
	}
}

func TestDramaVideoReferenceLimits(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		name, model            string
		images, videos, audios int
		valid                  bool
	}{
		{"A 2.0 total at limit", "seedance2.0-A", 8, 2, 2, true},
		{"A 2.0 total over limit", "seedance2.0-A", 9, 3, 1, false},
		{"A 2.5 total at limit", "seedance2.5-A", 30, 10, 10, true},
		{"A 2.5 images over limit", "seedance2.5-A", 31, 0, 0, false},
		{"H3 total at limit", "minimax-h3", 9, 2, 1, true},
		{"H3 images over limit", "minimax-h3", 10, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{}
			var prompt strings.Builder
			for _, media := range []struct {
				field, label, extension string
				count                   int
			}{
				{"reference_images", "图", "png", tc.images},
				{"reference_videos", "视频", "mp4", tc.videos},
				{"reference_audios", "音频", "mp3", tc.audios},
			} {
				if media.count == 0 {
					continue
				}
				items := make([]any, media.count)
				for i := range items {
					items[i] = fmt.Sprintf("https://cdn.example/%d.%s", i+1, media.extension)
					fmt.Fprintf(&prompt, "@%s%d ", media.label, i+1)
				}
				request[media.field] = items
			}
			request["prompt"] = prompt.String()
			_, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", request)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "media exceeds")
			}
		})
	}
}

func TestDramaVideoMediaAndOperations(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	for _, tc := range []struct {
		name, model, operation, action string
		request                        map[string]any
		wantReferences                 []any
	}{
		{"A ordinary image and video references", "seedance2.0-A", "create", "reference_to_video", map[string]any{"prompt": "@图片1 and @图2 follow @视频1 with @音频1", "reference_images": []any{"https://cdn.example/one.png", "https://cdn.example/two.png"}, "referenceVideos": []any{"https://cdn.example/motion.mp4"}, "referenceAudios": []any{"https://cdn.example/voice.mp3"}}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/one.png"}, map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/two.png"}, map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}, map[string]any{"type": "audio", "role": "reference", "source": "https://cdn.example/voice.mp3"}}},
		{"A 2.5 typed references", "seedance2.5-A", "create", "reference_to_video", map[string]any{"prompt": "@图1 walks in the rain", "references": []any{map[string]any{"type": "image", "role": "reference_image", "source": "https://cdn.example/person.png"}}}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/person.png"}}},
		{"A frame aliases infer Auto", "seedance2.0-A", "create", "first_last_frame", map[string]any{"first_image": "https://cdn.example/first.png", "last_image": "https://cdn.example/last.png"}, []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}, map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/last.png"}}},
		{"A typed frames normalize Auto", "seedance2.0-A", "create", "first_last_frame", map[string]any{"aspect_ratio": "auto", "references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}, map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/last.png"}}}, []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}, map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/last.png"}}},
		{"H3 missing role and false audio", "minimax-h3", "create", "reference_to_video", map[string]any{"generate_audio": false, "references": []any{map[string]any{"type": "image", "source": "https://cdn.example/person.png"}}}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/person.png"}}},
		{"E image is an ordinary reference", "seedance2.0-E", "create", "reference_to_video", map[string]any{"image": "https://cdn.example/image.png"}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/image.png"}}},
		{"E video URL is an ordinary reference", "seedance2.0-E", "create", "reference_to_video", map[string]any{"reference_videos": []any{"https://cdn.example/motion.mp4"}}, []any{map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}}},
		{"E 2.5 mixed typed references need no numbered prompt", "seedance2.5-E", "create", "reference_to_video", map[string]any{"references": []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/character.png"}, map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}, map[string]any{"type": "audio", "role": "reference", "source": "https://cdn.example/voice.mp3"}}}, []any{map[string]any{"type": "image", "role": "reference", "source": "https://cdn.example/character.png"}, map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}, map[string]any{"type": "audio", "role": "reference", "source": "https://cdn.example/voice.mp3"}}},
		{"F typed references and false audio", "seedance2.5-F", "create", "reference_to_video", map[string]any{"generate_audio": false, "references": []any{map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}}}, []any{map[string]any{"type": "video", "role": "reference", "source": "https://cdn.example/motion.mp4"}}},
		{"G image and video references", "seedance2.5-G", "create", "reference_to_video", map[string]any{"reference_images": []any{"asset://asset-one"}, "reference_videos": []any{"https://cdn.example/motion.mp4"}}, []any{map[string]any{"type": "image", "role": "reference_image", "source": "asset://asset-one"}, map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/motion.mp4"}}},
		{"G first and last frame", "seedance2.5-G", "create", "first_last_frame", map[string]any{"image": "https://cdn.example/first.png", "last_frame": "https://cdn.example/last.png"}, []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}, map[string]any{"type": "image", "role": "last_frame", "source": "https://cdn.example/last.png"}}},
		{"G editing auto duration", "seedance2.5-G", "edit", "edit_video", map[string]any{"video": "https://cdn.example/source.mp4"}, []any{map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/source.mp4"}}},
		{"G extension", "seedance2.5-G", "extend", "extend_video", map[string]any{"video": "https://cdn.example/source.mp4", "extension_direction": "backward"}, []any{map[string]any{"type": "video", "role": "reference_video", "source": "https://cdn.example/source.mp4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "use the references"}
			if tc.operation != "edit" {
				request["seconds"] = 8
			}
			for key, value := range tc.request {
				request[key] = value
			}
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, tc.operation, request)
			require.NoError(t, err)
			assert.Equal(t, tc.action, intent["action"])
			profile, found := plugin.Meta.VideoProfileForModel(tc.model)
			require.True(t, found)
			require.NoError(t, service.ValidateVideoProfileRequest(profile, tc.action, intent["requestBody"], nil))
			value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"model": tc.model, "baseUrl": "https://upstream.example", "apiKey": "key", "action": intent["action"], "requestBody": intent["requestBody"],
			})
			require.NoError(t, err)
			body := value.(map[string]any)["body"].(map[string]any)
			encoded, err := common.Marshal(body["references"])
			require.NoError(t, err)
			var references []any
			require.NoError(t, common.Unmarshal(encoded, &references))
			assert.Equal(t, tc.wantReferences, references)
			if _, exists := tc.request["generate_audio"]; exists {
				assert.Equal(t, false, body["generate_audio"])
			}
			if tc.operation == "edit" {
				assert.EqualValues(t, -1, body["seconds"])
				assert.Equal(t, "edit", body["omni_reference_task_type"])
			}
		})
	}
	tooManyImages := make([]any, 10)
	for i := range tooManyImages {
		tooManyImages[i] = fmt.Sprintf("https://cdn.example/%d.png", i)
	}
	for _, tc := range []struct {
		name, model, errorText string
		request                map[string]any
	}{
		{"A index zero", "seedance2.0-A", "reference index", map[string]any{"prompt": "@图0", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"A leading zero", "seedance2.0-A", "reference index", map[string]any{"prompt": "@图01", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"A index outside array", "seedance2.5-A", "reference index", map[string]any{"prompt": "@图片2", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"A negative index", "seedance2.5-A", "reference index", map[string]any{"prompt": "@视频-1", "reference_videos": []any{"https://cdn.example/a.mp4"}}},
		{"A fractional index", "seedance2.5-A", "reference index", map[string]any{"prompt": "@图1.5", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"A unused reference", "seedance2.0-A", "Every A series reference", map[string]any{"reference_images": []any{"https://cdn.example/a.png"}}},
		{"A first frame alone", "seedance2.0-A", "requires last_frame", map[string]any{"image": "https://cdn.example/a.png"}},
		{"A fixed ratio with frames", "seedance2.0-A", "require aspect_ratio Auto", map[string]any{"image": "https://cdn.example/a.png", "last_frame": "https://cdn.example/b.png", "aspect_ratio": "16:9"}},
		{"A mixed pinned and ordinary references", "seedance2.0-A", "cannot be mixed", map[string]any{"image": "https://cdn.example/a.png", "last_frame": "https://cdn.example/b.png", "reference_images": []any{"https://cdn.example/c.png"}}},
		{"A conflicting frame aliases", "seedance2.0-A", "first_image conflicts", map[string]any{"image": "https://cdn.example/a.png", "first_image": "https://cdn.example/b.png"}},
		{"A 2.5 rejects frames", "seedance2.5-A", "ordinary references", map[string]any{"references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/a.png"}}}},
		{"A 2.5 rejects Auto", "seedance2.5-A", "aspect_ratio is not supported", map[string]any{"aspect_ratio": "Auto"}},
		{"A 2.5 rejects 1080p", "seedance2.5-A", "resolution is not supported", map[string]any{"resolution": "1080p"}},
		{"A unsupported audio flag", "seedance2.0-A", "generate_audio is not supported", map[string]any{"generate_audio": false}},
		{"A rejects HTTP", "seedance2.0-A", "not supported", map[string]any{"reference_images": []any{"http://cdn.example/a.png"}}},
		{"A rejects asset", "seedance2.5-A", "not supported", map[string]any{"reference_images": []any{"asset://image-one"}}},
		{"A duplicate reference", "seedance2.0-A", "Duplicate reference", map[string]any{"reference_images": []any{"https://cdn.example/a.png", "https://cdn.example/a.png"}}},
		{"H3 combined audio video limit", "minimax-h3", "media exceeds", map[string]any{"reference_videos": []any{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4"}, "reference_audios": []any{"https://cdn.example/1.mp3", "https://cdn.example/2.mp3"}}},
		{"H3 first frame role", "minimax-h3", "ordinary references", map[string]any{"references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/a.png"}}}},
		{"H3 last frame shortcut", "minimax-h3", "does not support last_frame", map[string]any{"last_frame": "https://cdn.example/a.png"}},
		{"H3 ratio", "minimax-h3", "aspect_ratio is not supported", map[string]any{"aspect_ratio": "1:1"}},
		{"H3 rejects 2k", "minimax-h3", "resolution is not supported", map[string]any{"resolution": "2k"}},
		{"H3 rejects data URI", "minimax-h3", "not supported", map[string]any{"reference_images": []any{"data:image/png;base64,AA=="}}},
		{"H3 duration aliases differ", "minimax-h3", "must match", map[string]any{"seconds": 5, "duration": 8}},
		{"H3 metadata duration bypass", "minimax-h3", "metadata.seconds is not supported", map[string]any{"metadata": map[string]any{"seconds": 99999999999}}},
		{"unsigned duration overflow", "seedance2.5-G", "seconds must be between", map[string]any{"seconds": "18446744073686646784"}},
		{"boolean duration", "seedance2.5-G", "seconds must be an integer", map[string]any{"seconds": true}},
		{"negative generation duration", "seedance2.5-G", "seconds must be between", map[string]any{"seconds": -1}},
		{"duration aliases differ", "seedance2.5-G", "must match", map[string]any{"seconds": 5, "duration": 8}},
		{"F needs 720px", "seedance2.0-F", "resolution is not supported", map[string]any{"resolution": "720p"}},
		{"fast resolution", "seedance2.0-fast-G", "resolution is not supported", map[string]any{"resolution": "1080p"}},
		{"E adaptive ratio", "seedance2.0-E", "aspect_ratio is not supported", map[string]any{"aspect_ratio": "adaptive"}},
		{"E first frame", "seedance2.0-E", "ordinary references", map[string]any{"references": []any{map[string]any{"type": "image", "role": "first_frame", "source": "https://cdn.example/first.png"}}}},
		{"legacy F video", "seedance2.0-F-720p", "media exceeds", map[string]any{"reference_videos": []any{"https://cdn.example/video.mp4"}}},
		{"F combined audio video limit", "seedance2.5-F", "media exceeds", map[string]any{"reference_videos": []any{"https://cdn.example/1.mp4", "https://cdn.example/2.mp4"}, "reference_audios": []any{"https://cdn.example/1.mp3", "https://cdn.example/2.mp3"}}},
		{"too many images", "seedance2.0-G", "media exceeds", map[string]any{"reference_images": tooManyImages}},
		{"duplicate media", "seedance2.0-E", "Duplicate reference", map[string]any{"image": "https://cdn.example/a.png", "reference_images": []any{"https://cdn.example/a.png"}}},
		{"missing G role", "seedance2.0-G", "explicit matching role", map[string]any{"references": []any{map[string]any{"type": "image", "source": "https://cdn.example/a.png"}}}},
		{"E HTTP media", "seedance2.0-E", "not supported", map[string]any{"image": "http://cdn.example/a.png"}},
		{"E asset reference", "seedance2.0-E", "not supported", map[string]any{"image": "asset://asset-one"}},
		{"last frame alone", "seedance2.5-G", "requires first_frame", map[string]any{"last_frame": "https://cdn.example/last.png"}},
		{"pinned and reference images", "seedance2.5-G", "cannot be mixed", map[string]any{"image": "https://cdn.example/first.png", "reference_images": []any{"https://cdn.example/other.png"}}},
		{"G 2.0 audio alone", "seedance2.0-G", "require an image or video", map[string]any{"reference_audios": []any{"https://cdn.example/a.mp3"}}},
		{"metadata duration bypass", "seedance2.5-G", "metadata.seconds is not supported", map[string]any{"metadata": map[string]any{"seconds": 99999999999}}},
		{"unknown billing multiplier", "seedance2.5-G", "n is not supported", map[string]any{"n": 99999999999}},
		{"E unsupported audio flag", "seedance2.0-E", "generate_audio is not supported", map[string]any{"generate_audio": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"prompt": "a cat", "seconds": 5}
			for key, value := range tc.request {
				request[key] = value
			}
			_, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", request)
			require.ErrorContains(t, err, tc.errorText)
		})
	}
}

func TestDramaVideoPollingAndActualUsage(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	const missingFailureReason = "Video generation failed; upstream did not provide an error reason"
	for _, tc := range []struct {
		name, model, action, wantStatus, errorText, wantReason string
		state, body                                            map[string]any
		wantFacts                                              map[string]any
	}{
		{name: "A completes without tokens or measured seconds", model: "seedance2.0-A", body: map[string]any{"status": "completed"}, wantStatus: "SUCCESS", wantFacts: map[string]any{}},
		{name: "A actual seconds ignore unrelated token billing", model: "seedance2.5-A", body: map[string]any{"status": "completed", "seconds": "6", "usage": map[string]any{"total_tokens": -1}, "metadata": map[string]any{"billing": map[string]any{"settle_state": "pending"}}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(6)}},
		{name: "H3 actual duration", model: "minimax-h3", body: map[string]any{"status": "completed", "duration": 6.5}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": 6.5}},
		{name: "H3 failed task", model: "minimax-h3", body: map[string]any{"status": "failed", "error": "generation rejected"}, wantStatus: "FAILURE", wantReason: "generation rejected", wantFacts: map[string]any{}},
		{name: "A duration overflow", model: "seedance2.0-A", body: map[string]any{"status": "completed", "seconds": "18446744073686646784"}, errorText: "Invalid upstream video duration"},
		{name: "H3 zero duration", model: "minimax-h3", body: map[string]any{"status": "completed", "duration": 0}, errorText: "Invalid upstream video duration"},
		{name: "queued receipt", model: "seedance2.5-G", body: map[string]any{"status": "queued", "progress": 0}, wantStatus: "QUEUED", wantFacts: map[string]any{}},
		{name: "unknown status cannot resolve inherited properties", model: "seedance2.5-G", body: map[string]any{"status": "constructor"}, wantStatus: "UNKNOWN", wantFacts: map[string]any{}},
		{name: "failed task", model: "seedance2.5-G", body: map[string]any{"status": "failed", "error": "generation rejected"}, wantStatus: "FAILURE", wantReason: "generation rejected", wantFacts: map[string]any{}},
		{name: "Mini E upstream image dimensions error", model: "seedance2.0-Mini-E", body: map[string]any{"status": "failed", "metadata": map[string]any{"url": "Error while downloading image, error: expected the width to be at most 6000px, but received a 6060x3476px image instead"}}, wantStatus: "FAILURE", wantReason: "Error while downloading image, error: expected the width to be at most 6000px, but received a 6060x3476px image instead", wantFacts: map[string]any{}},
		{name: "A upstream busy error", model: "seedance2.5-A", body: map[string]any{"status": "failed", "metadata": map[string]any{"url": "系统繁忙，请稍后重试"}}, wantStatus: "FAILURE", wantReason: "系统繁忙，请稍后重试", wantFacts: map[string]any{}},
		{name: "error string precedes metadata", model: "seedance2.5-A", body: map[string]any{"status": "failed", "error": "  primary error  ", "metadata": map[string]any{"url": "secondary error"}}, wantStatus: "FAILURE", wantReason: "primary error", wantFacts: map[string]any{}},
		{name: "error message precedes metadata", model: "seedance2.5-A", body: map[string]any{"status": "failed", "error": map[string]any{"message": "primary message"}, "metadata": map[string]any{"url": "secondary error"}}, wantStatus: "FAILURE", wantReason: "primary message", wantFacts: map[string]any{}},
		{name: "empty error string uses metadata", model: "seedance2.5-A", body: map[string]any{"status": "failed", "error": " ", "metadata": map[string]any{"url": "  系统繁忙，请稍后重试  "}}, wantStatus: "FAILURE", wantReason: "系统繁忙，请稍后重试", wantFacts: map[string]any{}},
		{name: "empty error message uses metadata", model: "seedance2.5-A", body: map[string]any{"status": "failed", "error": map[string]any{"message": " "}, "metadata": map[string]any{"url": "系统繁忙，请稍后重试"}}, wantStatus: "FAILURE", wantReason: "系统繁忙，请稍后重试", wantFacts: map[string]any{}},
		{name: "failed signed result URL is not an error", model: "seedance2.5-A", body: map[string]any{"status": "failed", "metadata": map[string]any{"url": "  HTTPS://cdn.example/video.mp4?signature=private-result-token  "}}, wantStatus: "FAILURE", wantReason: missingFailureReason, wantFacts: map[string]any{}},
		{name: "failed data video URI is not an error", model: "seedance2.5-A", body: map[string]any{"status": "failed", "metadata": map[string]any{"url": "data:video/mp4;base64,cHJpdmF0ZS12aWRlbw=="}}, wantStatus: "FAILURE", wantReason: missingFailureReason, wantFacts: map[string]any{}},
		{name: "failed relative result URL is not an error", model: "seedance2.5-A", body: map[string]any{"status": "failed", "metadata": map[string]any{"url": "/v1/videos/upstream/content"}}, wantStatus: "FAILURE", wantReason: missingFailureReason, wantFacts: map[string]any{}},
		{name: "failed without details", model: "seedance2.5-A", body: map[string]any{"status": "failed"}, wantStatus: "FAILURE", wantReason: missingFailureReason, wantFacts: map[string]any{}},
		{name: "failed with malformed details", model: "seedance2.5-A", body: map[string]any{"status": "failed", "error": map[string]any{"message": map[string]any{"input": "not-an-error"}}, "metadata": map[string]any{"url": []any{"not-an-error"}}}, wantStatus: "FAILURE", wantReason: missingFailureReason, wantFacts: map[string]any{}},
		{name: "queued ignores metadata error text", model: "seedance2.5-A", body: map[string]any{"status": "queued", "metadata": map[string]any{"url": "系统繁忙，请稍后重试"}}, wantStatus: "QUEUED", wantFacts: map[string]any{}},
		{name: "completed ignores metadata error text", model: "seedance2.0-Mini-E", body: map[string]any{"status": "completed", "metadata": map[string]any{"url": "系统繁忙，请稍后重试"}}, wantStatus: "SUCCESS", wantFacts: map[string]any{}},
		{name: "E needs no seconds or tokens", model: "seedance2.0-E", body: map[string]any{"status": "completed", "metadata": map[string]any{"url": "/v1/videos/upstream/content"}}, wantStatus: "SUCCESS", wantFacts: map[string]any{}},
		{name: "F seconds from bill", model: "seedance2.5-F", body: map[string]any{"status": "completed", "metadata": map[string]any{"billing": map[string]any{"seconds": 8}}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(8)}},
		{name: "F reference video actual tokens", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed", "seconds": "8", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 600037, "output_tokens": 590000}, "billing": map[string]any{"settle_state": "settled"}}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(8), "tokens": float64(600037)}},
		{name: "F final bill pending", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 42}, "billing": map[string]any{"settle_state": "pending"}}}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "F missing final tokens", model: "seedance2.5-F", state: map[string]any{"video_input": "video"}, body: map[string]any{"status": "completed"}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "G actual total tokens", model: "seedance2.0-G", body: map[string]any{"status": "completed", "seconds": 5, "usage": map[string]any{"total_tokens": 432900, "completion_tokens": 400000}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"seconds": float64(5), "tokens": float64(432900)}},
		{name: "G missing final tokens", model: "seedance2.5-G", body: map[string]any{"status": "completed", "progress": 100}, wantStatus: "IN_PROGRESS", wantFacts: map[string]any{}},
		{name: "G explicit zero usage", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 0}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(0)}},
		{name: "G edit echoes auto duration", model: "seedance2.5-G", action: "edit_video", body: map[string]any{"status": "completed", "seconds": "-1", "usage": map[string]any{"total_tokens": 12345}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(12345)}},
		{name: "G extension retains requested seconds", model: "seedance2.5-G", action: "extend_video", body: map[string]any{"status": "completed", "seconds": 35, "usage": map[string]any{"total_tokens": 12345}}, wantStatus: "SUCCESS", wantFacts: map[string]any{"tokens": float64(12345)}},
		{name: "token overflow", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 2147483648}}, errorText: "Invalid upstream video token usage"},
		{name: "boolean tokens", model: "seedance2.5-G", body: map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": true}}, errorText: "Invalid upstream video token usage"},
		{name: "duration overflow", model: "seedance2.5-F", body: map[string]any{"status": "completed", "seconds": "18446744073686646784"}, errorText: "Invalid upstream video duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := map[string]any{"model": tc.model, "state": tc.state, "action": tc.action}
			value, err := plugin.Engine.Call(t.Context(), "parseTaskResult", ctx, tc.body)
			if tc.errorText != "" {
				require.ErrorContains(t, err, tc.errorText)
				return
			}
			require.NoError(t, err)
			result := value.(map[string]any)
			assert.Equal(t, tc.wantStatus, result["status"])
			if tc.wantStatus == "FAILURE" {
				assert.Equal(t, tc.wantReason, result["reason"])
				adaptor := pluginadaptor.New(plugin)
				adaptor.Init(&relaycommon.RelayInfo{})
				public, renderErr := adaptor.ConvertToOpenAIVideo(&model.Task{TaskID: "task_public_failure", Status: model.TaskStatusFailure, FailReason: result["reason"].(string), Properties: model.Properties{OriginModelName: tc.model}})
				require.NoError(t, renderErr)
				var rendered struct {
					Status string `json:"status"`
					Error  struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				require.NoError(t, common.Unmarshal(public, &rendered))
				assert.Equal(t, "failed", rendered.Status)
				assert.Equal(t, tc.wantReason, rendered.Error.Message)
			} else if tc.wantStatus != "UNKNOWN" {
				assert.NotContains(t, result, "reason", "non-terminal and completed metadata must not become a failure reason")
			}
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", ctx, result, tc.body)
			require.NoError(t, err)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var facts map[string]any
			require.NoError(t, common.Unmarshal(encoded, &facts))
			assert.Equal(t, tc.wantFacts, facts)
		})
	}
}

func TestDramaVideoPublicUpstreamProgress(t *testing.T) {
	source, err := builtinplugins.Source("drama")
	require.NoError(t, err)
	spoofedSource := strings.Replace(source, "const result = {};", "const result = { upstream_progress: 88 };", 1)
	require.NotEqual(t, source, spoofedSource)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(spoofedSource, jsplugin.Options{Key: "drama"})
	require.NoError(t, err)
	adaptor := pluginadaptor.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{})
	for _, tc := range []struct {
		name, snapshot, path, privateSnapshot string
		configured                            bool
		want                                  any
		taskStatus                            model.TaskStatus
		wantStatus                            string
	}{
		{name: "queued provider progress", snapshot: `{"status":"queued","progress":0}`, taskStatus: model.TaskStatusQueued, wantStatus: "queued", want: float64(0)},
		{name: "explicit zero", snapshot: `{"progress":0}`, want: float64(0)},
		{name: "fractional progress", snapshot: `{"progress":42.5}`, want: 42.5},
		{name: "upstream done before host settlement", snapshot: `{"status":"completed","progress":100}`, want: float64(100)},
		{name: "missing progress cannot use host progress", snapshot: `{}`},
		{name: "provider numeric string is not a percentage", snapshot: `{"progress":"42.5"}`},
		{name: "negative progress", snapshot: `{"progress":-1}`},
		{name: "progress over one hundred", snapshot: `{"progress":101}`},
		{name: "null progress", snapshot: `{"progress":null}`},
		{name: "boolean progress", snapshot: `{"progress":false}`},
		{name: "configured numeric progress", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":31.5},"private_url":"https://private.example/result"}`, want: 31.5},
		{name: "configured percentage string", configured: true, path: " job.progress ", privateSnapshot: `{"job":{"progress":" 42.5% "}}`, want: 42.5},
		{name: "configured numeric string zero", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":" 0 "}}`, want: float64(0)},
		{name: "configured missing path ignores public data", snapshot: `{"progress":80}`, configured: true, privateSnapshot: `{"progress":60}`},
		{name: "configured missing value ignores public data", snapshot: `{"progress":80}`, configured: true, path: "job.progress", privateSnapshot: `{"progress":60}`},
		{name: "configured invalid value ignores public data", snapshot: `{"progress":80}`, configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":"unavailable"}}`},
		{name: "configured malformed snapshot", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":40}`},
		{name: "configured nonfinite progress", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":"NaN"}}`},
		{name: "configured infinite progress", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":"+Inf"}}`},
		{name: "configured percentage over one hundred", configured: true, path: "job.progress", privateSnapshot: `{"job":{"progress":"101%"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, wantStatus := tc.taskStatus, tc.wantStatus
			if status == "" {
				status, wantStatus = model.TaskStatusInProgress, "in_progress"
			}
			task := &model.Task{
				TaskID: "public-task", Platform: "drama", Status: status, Progress: "30%",
				Properties: model.Properties{OriginModelName: "seedance2.5-G"},
				Data:       []byte(tc.snapshot),
			}
			if tc.configured {
				task.PrivateData.UpstreamAsync = &relaydto.UpstreamAsyncProfile{Poll: relaydto.UpstreamAsyncPoll{Response: relaydto.UpstreamAsyncPollResponse{ProgressPath: tc.path}}}
				task.PrivateData.UpstreamAsyncResponse = []byte(tc.privateSnapshot)
			}
			public, err := adaptor.ConvertToOpenAIVideo(task)
			require.NoError(t, err)
			var rendered map[string]any
			require.NoError(t, common.Unmarshal(public, &rendered))
			assert.Equal(t, wantStatus, rendered["status"])
			assert.Equal(t, float64(30), rendered["progress"])
			if tc.want == nil {
				assert.NotContains(t, rendered, "upstream_progress")
			} else {
				assert.Equal(t, tc.want, rendered["upstream_progress"])
			}
			assert.NotContains(t, string(public), "private.example")
			assert.Equal(t, "30%", task.Progress)
		})
	}
	_, ok := service.PublicTaskUpstreamProgress(&model.Task{Platform: "sora", Data: []byte(`{"progress":40}`)})
	assert.False(t, ok)
	_, ok = service.PublicTaskUpstreamProgress(nil)
	assert.False(t, ok)
}

func TestDramaVideoHostDownloadAndFrozenBilling(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	adaptor := pluginadaptor.New(plugin)
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://upstream.example", ApiKey: "upstream-test-key"}})
	task := &model.Task{
		TaskID: "task_public", Status: model.TaskStatusSuccess, Progress: "100%", SubmitTime: 10, FinishTime: 20,
		Properties:  model.Properties{OriginModelName: "my-video", UpstreamModelName: "seedance2.5-G"},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "task/private?one"},
		Data:        []byte(`{"id":"task/private?one","metadata":{"url":"https://untrusted.example/private.mp4","billing":{"quota":12345}}}`),
	}
	artifacts, err := adaptor.ListArtifacts(task)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		descriptor, err := adaptor.BuildContentRequest(task, "video", relaychannel.TaskArtifactClientRequest{Method: method})
		require.NoError(t, err)
		assert.Equal(t, "https://upstream.example/v1/videos/task%2Fprivate%3Fone/content", descriptor.URL)
		assert.Equal(t, method, descriptor.Method)
		assert.Equal(t, "Bearer upstream-test-key", descriptor.Headers["Authorization"])
		assert.False(t, descriptor.Credentialless)
	}
	public, err := adaptor.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	var rendered map[string]any
	require.NoError(t, common.Unmarshal(public, &rendered))
	assert.Equal(t, "task_public", rendered["id"])
	assert.Equal(t, "my-video", rendered["model"])
	assert.Equal(t, "completed", rendered["status"])
	assert.NotContains(t, string(public), "untrusted.example")
	assert.NotContains(t, string(public), "upstream-test-key")
	assert.NotContains(t, rendered, "metadata")
	task.Data = []byte(`{"status":"completed","last_frame_url":"https://cdn.example/last-frame.png"}`)
	artifacts, err = adaptor.ListArtifacts(task)
	require.NoError(t, err)
	require.Len(t, artifacts, 2)
	lastFrame, err := adaptor.BuildContentRequest(task, "last_frame", relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
	require.NoError(t, err)
	assert.Equal(t, "https://cdn.example/last-frame.png", lastFrame.URL)
	assert.True(t, lastFrame.Credentialless)
	assert.Empty(t, lastFrame.Headers)

	for _, tc := range []struct {
		model, expression string
		request           map[string]any
		response          map[string]any
		wantQuota         int
	}{
		{"seedance2.0-A", `tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat"}, map[string]any{"status": "completed"}, 200000},
		{"seedance2.5-A", `tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8}, map[string]any{"status": "completed", "seconds": "6"}, 300000},
		{"minimax-h3", `tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8}, map[string]any{"status": "completed", "duration": 6.5}, 325000},
		{"seedance2.0-E", `tier("video", u("requests") * 0.5)`, map[string]any{"prompt": "a cat", "seconds": 5}, map[string]any{"status": "completed"}, 250000},
		{"seedance2.0-E", `tier("video", u("requests") * 0.5)`, map[string]any{"prompt": "a cat", "seconds": 15}, map[string]any{"status": "completed"}, 250000},
		{"seedance2.5-F", `u("video_input") == "video" ? tier("tokens", u("tokens") * 10 / 1000000) : tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8}, map[string]any{"status": "completed", "seconds": "8"}, 400000},
		{"seedance2.5-F", `u("video_input") == "video" ? tier("tokens", u("tokens") * 10 / 1000000) : tier("seconds", u("seconds") * 0.1)`, map[string]any{"prompt": "a cat", "seconds": 8, "reference_videos": []any{"https://cdn.example/motion.mp4"}}, map[string]any{"status": "completed", "metadata": map[string]any{"usage": map[string]any{"total_tokens": 600037}}}, 3000185},
		{"seedance2.5-G", `tier("tokens", u("tokens") * 10 / 1000000)`, map[string]any{"prompt": "a cat", "seconds": 5}, map[string]any{"status": "completed", "usage": map[string]any{"total_tokens": 432900}}, 2164500},
	} {
		t.Run(tc.model, func(t *testing.T) {
			intent, err := decodeDramaVideoRequest(t, plugin, tc.model, "create", tc.request)
			require.NoError(t, err)
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", map[string]any{"model": tc.model, "requestBody": intent["requestBody"], "action": intent["action"], "usagePurpose": "facts"})
			require.NoError(t, err)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var estimate map[string]any
			require.NoError(t, common.Unmarshal(encoded, &estimate))
			snapshot := &billingexpr.BillingSnapshot{ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), ExprVersion: 1, TaskUsageBilling: true, GroupRatio: 1, QuotaPerUnit: 500000, UsageFacts: estimate}
			value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"model": tc.model, "state": estimate}, map[string]any{}, tc.response)
			require.NoError(t, err)
			encoded, err = common.Marshal(value)
			require.NoError(t, err)
			var actual map[string]any
			require.NoError(t, common.Unmarshal(encoded, &actual))
			settled, _, err := service.EvaluateTaskCompletionUsage(snapshot, actual)
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, settled.ActualQuotaAfterGroup)
			assert.Nil(t, settled.Clamp)
		})
	}
}

func TestDramaVideoHTTPTaskLifecycle(t *testing.T) {
	plugin := loadDramaVideoPlugin(t)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "drama-polling.db")), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	previousDB, previousLogDB, previousCache := model.DB, model.LOG_DB, common.MemoryCacheEnabled
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousFactory := service.GetTaskAdaptorFunc
	model.DB, model.LOG_DB, common.MemoryCacheEnabled = db, db, false
	common.SetDatabaseTypes(common.DatabaseType("sqlite"), common.DatabaseType("sqlite"))
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		if platform == "drama" {
			return pluginadaptor.New(plugin)
		}
		return nil
	}
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.MemoryCacheEnabled = previousDB, previousLogDB, previousCache
		common.SetDatabaseTypes(previousType, previousLogType)
		service.GetTaskAdaptorFunc = previousFactory
		assert.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.Channel{}, &model.User{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "drama-polling", Quota: 1000}).Error)
	for _, modelName := range []string{"seedance2.0-A", "seedance2.5-A", "minimax-h3", "seedance2.0-E-720p", "seedance2.5-F", "seedance2.5-G"} {
		t.Run(modelName, func(t *testing.T) {
			var submits, polls, downloads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "Bearer upstream-key", request.Header.Get("Authorization"))
				if request.Method == http.MethodPost {
					submits.Add(1)
					var body map[string]any
					if !assert.NoError(t, common.DecodeJson(request.Body, &body)) {
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					assert.Equal(t, modelName, body["model"])
					assert.Equal(t, float64(5), body["seconds"])
					writer.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"queued","progress":0}`)
					return
				}
				if request.URL.Path == "/v1/videos/remote-task/content" {
					downloads.Add(1)
					writer.Header().Set("Content-Type", "video/mp4")
					_, _ = io.WriteString(writer, "upstream-video-bytes")
					return
				}
				poll := polls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				if poll == 1 {
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"queued","progress":0}`)
					return
				}
				if poll == 2 {
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"in_progress","progress":40}`)
					return
				}
				if modelName == "seedance2.5-G" && poll == 3 {
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"completed","progress":100,"metadata":{"billing":{"settle_state":"pending"}}}`)
					return
				}
				if modelName == "seedance2.0-A" || modelName == "seedance2.5-A" || modelName == "minimax-h3" {
					_, _ = io.WriteString(writer, `{"id":"remote-task","status":"completed","progress":100,"seconds":5}`)
					return
				}
				_, _ = io.WriteString(writer, `{"id":"remote-task","status":"completed","progress":100,"seconds":5,"usage":{"total_tokens":12345},"metadata":{"url":"/v1/videos/remote-task/content"}}`)
			}))
			t.Cleanup(upstream.Close)
			intent, err := decodeDramaVideoRequest(t, plugin, modelName, "create", map[string]any{"prompt": "a cat", "seconds": 5})
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{OriginModelName: modelName, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: modelName, ChannelBaseUrl: upstream.URL, ApiKey: "upstream-key"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: intent["action"].(string)}}
			adaptor := pluginadaptor.New(plugin)
			adaptor.Init(info)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Authorization", "Bearer client-key-must-not-be-forwarded")
			ctx.Set("task_request", intent["requestBody"])
			require.Nil(t, adaptor.ValidateRequestAndSetAction(ctx, info))
			requestBody, err := adaptor.BuildRequestBody(ctx, info)
			require.NoError(t, err)
			response, err := adaptor.DoRequest(ctx, info, requestBody)
			require.NoError(t, err)
			receipt, taskErr := adaptor.ParseResponse(ctx, response, info)
			require.NoError(t, response.Body.Close())
			require.Nil(t, taskErr)
			require.NotNil(t, receipt)
			assert.Equal(t, "remote-task", receipt.UpstreamTaskID)
			require.NotEmpty(t, receipt.PluginState)
			channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: modelName, Key: "upstream-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(channel).Error)
			task := &model.Task{TaskID: "public-" + modelName, UserId: 1, ChannelId: channel.Id, Platform: "drama", Action: info.Action, Status: model.TaskStatusSubmitted, Progress: "0%", CreatedAt: common.GetTimestamp(), Properties: model.Properties{OriginModelName: modelName, UpstreamModelName: modelName}, Data: receipt.TaskData, PrivateData: model.TaskPrivateData{UpstreamTaskID: receipt.UpstreamTaskID, PluginState: receipt.PluginState, BillingContext: &model.TaskBillingContext{PerCallBilling: true}}}
			require.NoError(t, db.Create(task).Error)
			type pollingExpectation struct {
				status           model.TaskStatus
				publicStatus     string
				progress         int
				upstreamProgress float64
			}
			states := []pollingExpectation{{model.TaskStatusQueued, "queued", 0, 0}, {model.TaskStatusInProgress, "in_progress", 40, 40}}
			if modelName == "seedance2.5-G" {
				states = append(states, pollingExpectation{model.TaskStatusInProgress, "in_progress", 99, 100})
			}
			states = append(states, pollingExpectation{model.TaskStatusSuccess, "completed", 100, 100})
			for _, expected := range states {
				require.NoError(t, service.UpdateVideoTasks(t.Context(), "drama", map[int][]string{channel.Id: {receipt.UpstreamTaskID}}, map[string]*model.Task{receipt.UpstreamTaskID: task}))
				var persisted model.Task
				require.NoError(t, db.First(&persisted, task.ID).Error)
				assert.Equal(t, expected.status, persisted.Status)
				assert.Equal(t, fmt.Sprintf("%d%%", expected.progress), persisted.Progress)
				percent, exists := service.PublicTaskUpstreamProgress(&persisted)
				assert.True(t, exists)
				assert.Equal(t, expected.upstreamProgress, percent)
				recorder := httptest.NewRecorder()
				query, _ := gin.CreateTestContext(recorder)
				query.Set("id", 1)
				query.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
				query.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+task.TaskID, nil)
				require.Nil(t, relay.RelayTaskFetch(query, relayconstant.RelayModeVideoFetchByID))
				require.Equal(t, http.StatusOK, recorder.Code)
				var rendered map[string]any
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &rendered))
				assert.Equal(t, task.TaskID, rendered["id"])
				assert.Equal(t, expected.publicStatus, rendered["status"])
				assert.Equal(t, float64(expected.progress), rendered["progress"])
				assert.Equal(t, expected.upstreamProgress, rendered["upstream_progress"])
				assert.NotContains(t, recorder.Body.String(), "remote-task")
				assert.NotContains(t, recorder.Body.String(), "upstream-key")
				if expected.status != model.TaskStatusSuccess {
					contentRecorder := httptest.NewRecorder()
					content, _ := gin.CreateTestContext(contentRecorder)
					content.Set("id", 1)
					content.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
					content.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+task.TaskID+"/content", nil)
					controller.VideoProxy(content)
					assert.Equal(t, http.StatusBadRequest, contentRecorder.Code)
					assert.Contains(t, contentRecorder.Body.String(), "Task is not completed yet")
				}
				assert.Zero(t, downloads.Load(), "status polling must not fetch unfinished content")
				*task = persisted
			}
			descriptor, err := adaptor.BuildContentRequest(task, "video", relaychannel.TaskArtifactClientRequest{Method: http.MethodGet})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), descriptor.Method, descriptor.URL, nil)
			require.NoError(t, err)
			for key, value := range descriptor.Headers {
				request.Header.Set(key, value)
			}
			response, err = upstream.Client().Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, "upstream-video-bytes", string(body))
			assert.EqualValues(t, 1, submits.Load())
			assert.EqualValues(t, len(states), polls.Load())
			assert.EqualValues(t, 1, downloads.Load())
		})
	}
}
