package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/mediaretry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestVideoRoutingRewritePreservesPublicRequest(t *testing.T) {
	for _, tc := range []struct {
		name, body, resolution, expected, failure string
	}{
		{name: "keep resolution", body: `{"model":"A","resolution":"480p","seconds":20,"seed":18446744073709551615,"generate_audio":false}`, expected: `{"model":"E","resolution":"480p","seconds":20,"seed":18446744073709551615,"generate_audio":false}`},
		{name: "upgrade and synchronize metadata", body: `{"model":"A","resolution":"480p","seconds":20,"metadata":{"resolution":"480p","size":"854x480","tag":"literal 480p"},"prompt":"keep 480p @图1","references":[{"type":"image","role":"reference","source":"https://source.test/a.png"},{"type":"video","role":"reference","source":"https://source.test/a.mp4"}]}`, resolution: "720p", expected: `{"model":"E","resolution":"720p","seconds":20,"metadata":{"resolution":"720p","size":"1281x720","tag":"literal 480p"},"prompt":"keep 480p @图1","references":[{"type":"image","role":"reference","source":"https://source.test/a.png"},{"type":"video","role":"reference","source":"https://source.test/a.mp4"}]}`},
		{name: "portrait pixel alias", body: `{"model":"A","size":"480*854","aspect_ratio":"9:16","duration":"20"}`, resolution: "720p", expected: `{"model":"E","size":"720x1281","aspect_ratio":"9:16","duration":"20"}`},
		{name: "metadata only stays metadata only", body: `{"model":"A","metadata":{"resolution":"480p","seconds":20}}`, resolution: "720p", expected: `{"model":"E","metadata":{"resolution":"720p","seconds":20}}`},
		{name: "explicit default insertion", body: `{"model":"A","seconds":20}`, resolution: "720p", expected: `{"model":"E","resolution":"720p","seconds":20}`},
		{name: "explicit downgrade", body: `{"model":"A","resolution":"1080p","size":"1920x1080","seconds":20}`, resolution: "720p", expected: `{"model":"E","resolution":"720p","size":"1280x720","seconds":20}`},
		{name: "preserve size aspect ratio", body: `{"model":"A","resolution":"480p","size":"16:9","seconds":20}`, resolution: "720p", expected: `{"model":"E","resolution":"720p","size":"16:9","seconds":20}`},
		{name: "preserve metadata adaptive ratio", body: `{"model":"A","metadata":{"resolution":"480p","size":"adaptive","seconds":20}}`, resolution: "720p", expected: `{"model":"E","metadata":{"resolution":"720p","size":"adaptive","seconds":20}}`},
		{name: "ratio does not replace explicit resolution insertion", body: `{"model":"A","size":"adaptive","seconds":20}`, resolution: "720p", expected: `{"model":"E","resolution":"720p","size":"adaptive","seconds":20}`},
		{name: "downgrade rounding forbidden", body: `{"model":"A","size":"1280x720"}`, resolution: "480p", failure: "exact pixel aspect ratio"},
		{name: "rounding forbidden", body: `{"model":"A","size":"853x480"}`, resolution: "720p", failure: "exact pixel aspect ratio"},
		{name: "pixel category mismatch", body: `{"model":"A","size":"480x480"}`, resolution: "720p", failure: "does not represent"},
		{name: "invalid metadata", body: `{"model":"A","metadata":"unexpected"}`, resolution: "720p", failure: "metadata must be an object"},
		{name: "invalid resolution", body: `{"model":"A","resolution":"unknown"}`, resolution: "720p", failure: "source resolution is invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.body)
			rewritten, contentType, err := RewriteVideoRoutingRequest(original, "application/json", "create", "E", tc.resolution)
			assert.Equal(t, tc.body, string(original))
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "application/json", contentType)
			var actual, expected map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(rewritten, &actual))
			require.NoError(t, common.Unmarshal([]byte(tc.expected), &expected))
			assert.JSONEq(t, tc.expected, string(rewritten))
			// Compare scalar encodings too: JSONEq alone would round large seeds.
			assert.Equal(t, expected["seed"], actual["seed"])
			assert.Equal(t, expected["seconds"], actual["seconds"])
			assert.Equal(t, expected["duration"], actual["duration"])
		})
	}
}

func TestVideoRoutingRewriteMultipartPreservesFilesAndOrder(t *testing.T) {
	var source bytes.Buffer
	writer := multipart.NewWriter(&source)
	require.NoError(t, writer.WriteField("model", "A"))
	require.NoError(t, writer.WriteField("prompt", "unchanged @图1 @图2 480p"))
	first, err := writer.CreateFormFile("input_reference", "first.png")
	require.NoError(t, err)
	firstBytes := []byte{0, 255, 1, 2, '\r', '\n'}
	_, err = first.Write(firstBytes)
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("resolution", "480p"))
	require.NoError(t, writer.WriteField("size", "9:16"))
	require.NoError(t, writer.WriteField("metadata", `{"size":"480x854","duration":20,"references":[{"role":"last_frame","source":"unchanged"}]}`))
	second, err := writer.CreateFormFile("input_reference", "second.png")
	require.NoError(t, err)
	secondBytes := []byte{3, 2, 1, 0, 128}
	_, err = second.Write(secondBytes)
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("seconds", "20"))
	require.NoError(t, writer.Close())
	before := append([]byte(nil), source.Bytes()...)
	raw, contentType, err := RewriteVideoRoutingRequest(source.Bytes(), writer.FormDataContentType(), "create", "E", "720p")
	require.NoError(t, err)
	assert.Equal(t, before, source.Bytes())
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	var names, filenames []string
	var files [][]byte
	fields := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(part)
		require.NoError(t, err)
		names = append(names, part.FormName())
		if part.FileName() != "" {
			filenames = append(filenames, part.FileName())
			files = append(files, data)
		} else {
			fields[part.FormName()] = string(data)
		}
		require.NoError(t, part.Close())
	}
	assert.Equal(t, []string{"model", "prompt", "input_reference", "resolution", "size", "metadata", "input_reference", "seconds"}, names)
	assert.Equal(t, []string{"first.png", "second.png"}, filenames)
	assert.Equal(t, [][]byte{firstBytes, secondBytes}, files)
	assert.Equal(t, "E", fields["model"])
	assert.Equal(t, "720p", fields["resolution"])
	assert.Equal(t, "9:16", fields["size"])
	assert.Equal(t, "20", fields["seconds"])
	assert.Equal(t, "unchanged @图1 @图2 480p", fields["prompt"])
	assert.JSONEq(t, `{"size":"720x1281","duration":20,"references":[{"role":"last_frame","source":"unchanged"}]}`, fields["metadata"])
	for _, field := range []string{"model", "resolution", "metadata"} {
		t.Run("duplicate "+field, func(t *testing.T) {
			var duplicate bytes.Buffer
			form := multipart.NewWriter(&duplicate)
			require.NoError(t, form.WriteField(field, "first"))
			require.NoError(t, form.WriteField(field, "second"))
			require.NoError(t, form.Close())
			_, _, err := RewriteVideoRoutingRequest(duplicate.Bytes(), form.FormDataContentType(), "create", "E", "720p")
			require.ErrorContains(t, err, "duplicate video multipart field")
		})
	}
}

func videoRoutingFixture(t *testing.T) (*gorm.DB, model.AsyncImageTask, model.Channel) {
	t.Helper()
	db, task, _, _, _ := asyncImageWorkerFixture(t)
	previous := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "drama", "plugin.js"))
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.Register(string(source), jsplugin.Options{})
	require.NoError(t, err)
	setting, mapping := `{"task_plugin_key":"drama"}`, `{"target-alias":"seedance2.5-E"}`
	channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "routing-test-key", Models: "target-alias", Group: "default", Setting: &setting, ModelMapping: &mapping}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "target-alias", ChannelId: channel.Id, Enabled: true}).Error)
	return db, task, channel
}

func TestVideoRoutingTargetChecksLivePermissionsAndCapability(t *testing.T) {
	db, task, channel := videoRoutingFixture(t)
	target := model.VideoRoutingTarget{ChannelID: channel.Id, Model: "target-alias", Resolution: "720p"}
	body := []byte(`{"model":"seedance2.5-A","prompt":"test","seconds":20,"resolution":"480p","aspect_ratio":"16:9"}`)
	candidate, rewritten, _, err := DecodeVideoRoutingTarget(t.Context(), target, "default", "create", body, "application/json", task.UserId)
	require.NoError(t, err)
	assert.Equal(t, channel.Id, candidate.Channel.Id)
	assert.Equal(t, "seedance2.5-E", candidate.UpstreamModel)
	assert.Equal(t, "drama", candidate.Binding.Plugin.Meta.Key)
	assert.JSONEq(t, `{"model":"target-alias","prompt":"test","seconds":20,"resolution":"720p","aspect_ratio":"16:9"}`, string(rewritten))
	assert.Equal(t, `{"target-alias":"seedance2.5-E"}`, channel.GetModelMapping())
	for _, tc := range []struct {
		name, group, body, failure string
		target                     model.VideoRoutingTarget
	}{
		{name: "wrong group", group: "other", body: string(body), target: target, failure: "model or group"},
		{name: "unadvertised mapped model", group: "default", body: string(body), target: model.VideoRoutingTarget{ChannelID: channel.Id, Model: "seedance2.5-E"}, failure: "model or group"},
		{name: "duration outside target capability", group: "default", body: `{"model":"seedance2.5-A","prompt":"test","seconds":31,"resolution":"480p"}`, target: target, failure: "seconds must be between"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := DecodeVideoRoutingTarget(t.Context(), tc.target, tc.group, "create", []byte(tc.body), "application/json", task.UserId)
			require.ErrorContains(t, err, tc.failure)
		})
	}
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
	_, _, _, err = DecodeVideoRoutingTarget(t.Context(), target, "default", "create", body, "application/json", task.UserId)
	require.ErrorContains(t, err, "ability is unavailable")
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
	require.NoError(t, db.Model(&channel).Update("key", "").Error)
	_, _, _, err = DecodeVideoRoutingTarget(t.Context(), target, "default", "create", body, "application/json", task.UserId)
	require.ErrorContains(t, err, "no available account")
	require.NoError(t, db.Model(&channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
	_, _, _, err = DecodeVideoRoutingTarget(t.Context(), target, "default", "create", body, "application/json", task.UserId)
	require.ErrorContains(t, err, "channel is unavailable")
	yuxiboxSource, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "yuxibox", "plugin.js"))
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.Register(string(yuxiboxSource), jsplugin.Options{})
	require.NoError(t, err)
	yuxiboxSetting, yuxiboxMapping := `{"task_plugin_key":"yuxibox"}`, `{"seedance2.5-E":"seedance2.5-yx-count"}`
	yuxibox := model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "yuxibox-test-key", Models: "seedance2.5-E", Group: "default", Setting: &yuxiboxSetting, ModelMapping: &yuxiboxMapping}
	require.NoError(t, db.Create(&yuxibox).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "seedance2.5-E", ChannelId: yuxibox.Id, Enabled: true}).Error)
	for _, tc := range []struct {
		name, sourceResolution, targetResolution string
		seconds                                  int
		allowed                                  bool
	}{
		{name: "five second minimum", seconds: 5, sourceResolution: "720p", allowed: true},
		{name: "four seconds rejected", seconds: 4, sourceResolution: "720p"},
		{name: "twenty seconds upgraded", seconds: 20, sourceResolution: "480p", targetResolution: "720p", allowed: true},
		{name: "480p unsupported without rewrite", seconds: 20, sourceResolution: "480p"},
	} {
		t.Run("yuxibox "+tc.name, func(t *testing.T) {
			target := model.VideoRoutingTarget{ChannelID: yuxibox.Id, Model: "seedance2.5-E", Resolution: tc.targetResolution}
			raw := []byte(fmt.Sprintf(`{"model":"seedance2.5-A","prompt":"test","seconds":%d,"resolution":%q,"aspect_ratio":"16:9"}`, tc.seconds, tc.sourceResolution))
			candidate, execution, _, err := DecodeVideoRoutingTarget(t.Context(), target, "default", "create", raw, "application/json", task.UserId)
			if !tc.allowed {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "yuxibox", candidate.Binding.Plugin.Meta.Key)
			assert.Equal(t, "seedance2.5-yx-count", candidate.UpstreamModel)
			assert.Contains(t, string(execution), `"resolution":"720p"`)
			assert.Contains(t, string(execution), fmt.Sprintf(`"seconds":%d`, tc.seconds))
		})
	}
}

func TestVideoRoutingConfiguredTargetDimensions(t *testing.T) {
	db, task, channel := videoRoutingFixture(t)
	source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "huahuo-video", "plugin.js"))
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: "huahuo-video"})
	require.NoError(t, err)
	setting := `{"task_plugin_key":"huahuo-video","task_plugin_model_configs":{"vendor-video":{"duration":{"min":4,"max":30,"step":1,"default":5},"durations_by_resolution":{"720p":{"min":4,"max":12,"step":2,"default":4},"1080p":{"values":[5,8],"default":5}},"resolutions":["480p","720p","1080p"],"aspect_ratios":["16:9","adaptive"],"modes":["text_to_video"],"references":{"images":0,"videos":0,"audios":0},"billing_mode":"per_second"}}}`
	mapping := `{"target-alias":"vendor-video"}`
	channel.Setting, channel.ModelMapping = &setting, &mapping
	require.NoError(t, db.Save(&channel).Error)

	for _, tc := range []struct {
		name, targetResolution string
		resolutions            []string
		seconds                int
		allowed                bool
	}{
		{name: "resolution value supported", targetResolution: "1080p", seconds: 8, allowed: true},
		{name: "resolution value missing", targetResolution: "1080p", seconds: 6},
		{name: "global range cannot expand resolution values", targetResolution: "1080p", seconds: 20},
		{name: "resolution range supported", targetResolution: "720p", seconds: 10, allowed: true},
		{name: "resolution range step rejected", targetResolution: "720p", seconds: 9},
		{name: "resolution range maximum rejected", targetResolution: "720p", seconds: 14},
		{name: "unchanged resolution uses its own duration", resolutions: []string{"1080p"}, seconds: 20},
		{name: "all matching resolutions unavailable", resolutions: []string{"720p", "1080p"}, seconds: 20},
		{name: "one matching resolution supported", resolutions: []string{"720p", "1080p"}, seconds: 10, allowed: true},
		{name: "unrestricted resolutions include global range", seconds: 20, allowed: true},
		{name: "target override selects execution duration", targetResolution: "720p", resolutions: []string{"1080p"}, seconds: 10, allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := model.VideoRoutingTarget{ChannelID: channel.Id, Model: "target-alias", Resolution: tc.targetResolution}
			config := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{{ID: "dimensions", Name: "Execution dimensions", Enabled: true, OnExhausted: "fail", Match: model.VideoRoutingMatch{Resolutions: tc.resolutions, Seconds: model.VideoRoutingSeconds{Min: &tc.seconds, Max: &tc.seconds}}, Targets: []model.VideoRoutingTarget{target}}}}
			err := ValidateVideoRoutingConfig(t.Context(), config)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "no compatible mode, resolution and duration")
			}
			if tc.targetResolution != "" {
				raw := []byte(fmt.Sprintf(`{"model":"public-source","prompt":"test","seconds":%d,"resolution":"480p","aspect_ratio":"16:9"}`, tc.seconds))
				_, _, _, err := DecodeVideoRoutingTarget(t.Context(), target, "default", "create", raw, "application/json", task.UserId)
				if tc.allowed {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "duration is outside")
				}
			}
		})
	}

	for _, contentType := range []string{"application/json", "multipart/form-data"} {
		for _, ratio := range []string{"16:9", "adaptive"} {
			t.Run(contentType+" size="+ratio, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"model":"public-source","prompt":"test","seconds":8,"resolution":"480p","size":%q}`, ratio))
				requestType := contentType
				if contentType == "multipart/form-data" {
					var body bytes.Buffer
					writer := multipart.NewWriter(&body)
					for _, field := range [][2]string{{"model", "public-source"}, {"prompt", "test"}, {"seconds", "8"}, {"resolution", "480p"}, {"size", ratio}} {
						require.NoError(t, writer.WriteField(field[0], field[1]))
					}
					require.NoError(t, writer.Close())
					raw, requestType = body.Bytes(), writer.FormDataContentType()
				}
				target := model.VideoRoutingTarget{ChannelID: channel.Id, Model: "target-alias", Resolution: "720p"}
				candidate, rewritten, rewrittenType, err := DecodeVideoRoutingTarget(t.Context(), target, "default", "create", raw, requestType, task.UserId)
				require.NoError(t, err)
				assert.Equal(t, "vendor-video", candidate.UpstreamModel)
				_, body, err := MixedVideoRequestContext(rewritten, rewrittenType, "create")
				require.NoError(t, err)
				assert.Equal(t, ratio, body["size"])
				assert.Equal(t, "720p", body["resolution"])
			})
		}
	}
}

func TestVideoRoutingConfigurationRevisionAndMatching(t *testing.T) {
	_, _, channel := videoRoutingFixture(t)
	config, err := model.GetVideoRoutingConfig(t.Context())
	require.NoError(t, err)
	assert.False(t, config.Enabled)
	assert.Zero(t, config.Revision)
	minSeconds, maxSeconds := 15, 30
	rule := model.VideoRoutingRule{ID: "long-video", Name: "Long videos", Enabled: true, OnExhausted: "original", Targets: []model.VideoRoutingTarget{{ChannelID: channel.Id, Model: "target-alias", Resolution: "720p"}}, Match: model.VideoRoutingMatch{Groups: []string{"default"}, Models: []string{"seedance2.5-A"}, Resolutions: []string{"480p", "720p"}, Modes: []string{"text_to_video"}, VideoInputs: []string{"none"}, Seconds: model.VideoRoutingSeconds{Min: &minSeconds, Max: &maxSeconds, MinExclusive: true}}}
	config.Enabled, config.Rules = true, []model.VideoRoutingRule{rule}
	require.NoError(t, ValidateVideoRoutingConfig(t.Context(), config))
	saved, err := model.SaveVideoRoutingConfig(t.Context(), config, config.Revision)
	require.NoError(t, err)
	assert.EqualValues(t, 1, saved.Revision)
	loaded, err := model.GetVideoRoutingConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
	stale := config
	stale.Enabled = false
	_, err = model.SaveVideoRoutingConfig(t.Context(), stale, config.Revision)
	require.ErrorIs(t, err, model.ErrOptionRevisionConflict)
	loaded, err = model.GetVideoRoutingConfig(t.Context())
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
	for _, tc := range []struct {
		name    string
		seconds any
		matches bool
	}{
		{name: "excluded lower bound", seconds: 15},
		{name: "first allowed second", seconds: 16, matches: true},
		{name: "included upper bound", seconds: 30, matches: true},
		{name: "above range", seconds: 31},
		{name: "missing duration"},
		{name: "fractional duration", seconds: 15.5},
		{name: "invalid duration", seconds: math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := map[string]any{"seconds": tc.seconds, "resolution": "480p", "video_input": "none"}
			matches := MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "text_to_video", facts)
			assert.Equal(t, tc.matches, len(matches) == 1)
		})
	}
	facts := map[string]any{"seconds": 20, "resolution": "480p", "video_input": "none"}
	assert.Empty(t, MatchVideoRoutingRules(saved, "other", "seedance2.5-A", "text_to_video", facts))
	assert.Empty(t, MatchVideoRoutingRules(saved, "default", "other", "text_to_video", facts))
	assert.Empty(t, MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "edit_video", facts))
	facts["resolution"] = "1080p"
	assert.Empty(t, MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "text_to_video", facts))
	facts["resolution"], facts["video_input"] = "480p", "video"
	assert.Empty(t, MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "text_to_video", facts))
	facts["video_input"] = "none"
	second := rule
	second.ID = "second"
	saved.Rules = append(saved.Rules, second)
	matches := MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "text_to_video", facts)
	require.Len(t, matches, 2)
	assert.Equal(t, []string{"long-video", "second"}, []string{matches[0].ID, matches[1].ID})
	saved.Rules[0].Match.Seconds.MaxExclusive = true
	facts["seconds"] = 30
	assert.Len(t, MatchVideoRoutingRules(saved, "default", "seedance2.5-A", "text_to_video", facts), 1)
	loaded.Enabled = false
	updated, err := model.SaveVideoRoutingConfig(t.Context(), loaded, loaded.Revision)
	require.NoError(t, err)
	assert.EqualValues(t, 2, updated.Revision)
	assert.Empty(t, MatchVideoRoutingRules(updated, "default", "seedance2.5-A", "text_to_video", facts))
	duplicateChannel := config
	duplicateChannel.Rules = []model.VideoRoutingRule{rule}
	duplicateChannel.Rules[0].Targets = append([]model.VideoRoutingTarget{}, rule.Targets...)
	duplicateChannel.Rules[0].Targets = append(duplicateChannel.Rules[0].Targets, model.VideoRoutingTarget{ChannelID: channel.Id, Model: "target-alias", Resolution: "1080p"})
	require.ErrorContains(t, ValidateVideoRoutingConfig(t.Context(), duplicateChannel), "repeats a target channel")
	omitted := model.VideoRoutingConfig{Version: 1, Rules: []model.VideoRoutingRule{{ID: "draft", Name: "Draft", OnExhausted: "fail", Targets: rule.Targets}}}
	stored, err := model.SaveVideoRoutingConfig(t.Context(), omitted, updated.Revision)
	require.NoError(t, err)
	loaded, err = model.GetVideoRoutingConfig(t.Context())
	require.NoError(t, err)
	for _, current := range []model.VideoRoutingConfig{stored, loaded} {
		require.Len(t, current.Rules, 1)
		assert.NotNil(t, current.Rules[0].Match.Groups)
		assert.NotNil(t, current.Rules[0].Match.Models)
		assert.NotNil(t, current.Rules[0].Match.Resolutions)
		assert.NotNil(t, current.Rules[0].Match.Modes)
		assert.NotNil(t, current.Rules[0].Match.VideoInputs)
	}
	unavailable := model.VideoRoutingConfig{Version: 1, Rules: []model.VideoRoutingRule{rule}}
	unavailable.Rules[0].Targets = []model.VideoRoutingTarget{{ChannelID: channel.Id + 1000, Model: "target-alias", Resolution: "720p"}}
	require.NoError(t, ValidateVideoRoutingConfig(t.Context(), unavailable), "disabling the entire feature must not require live targets")
	unavailable.Enabled = true
	require.ErrorContains(t, ValidateVideoRoutingConfig(t.Context(), unavailable), "is unavailable")
}

func TestVideoRoutingPlanFreezesPublicFactsAndTargetOrder(t *testing.T) {
	db, task, channel := videoRoutingFixture(t)
	raw := []byte(`{"model":"seedance2.5-A","prompt":"test","seconds":20,"resolution":"480p","aspect_ratio":"16:9"}`)
	facts := map[string]any{"seconds": float64(20), "resolution": "480p", "video_input": "none", "requests": float64(1), "action": "generate"}
	rule := model.VideoRoutingRule{ID: "upgrade", Name: "Upgrade", Enabled: true, OnExhausted: "fail", Targets: []model.VideoRoutingTarget{{ChannelID: channel.Id + 1000, Model: "target-alias"}, {ChannelID: channel.Id, Model: "target-alias", Resolution: "720p"}}}
	snapshot, candidate, dispatch, contentType, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1, Revision: 3}, rule, "default", "seedance2.5-A", "create", raw, "application/json", facts, task.UserId, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	assert.Equal(t, channel.Id, candidate.Channel.Id)
	assert.Equal(t, 1, snapshot.TargetIndex)
	assert.EqualValues(t, 3, snapshot.Revision)
	assert.NotEmpty(t, snapshot.Targets[0].Reason)
	assert.Empty(t, snapshot.Targets[1].Reason)
	assert.Equal(t, raw, snapshot.SourceBody)
	assert.Equal(t, "480p", snapshot.SourceFacts["resolution"])
	assert.Equal(t, "application/json", contentType)
	assert.JSONEq(t, `{"model":"seedance2.5-A","prompt":"test","seconds":20,"resolution":"720p","aspect_ratio":"16:9"}`, string(dispatch))
	request := AsyncVideoRequest{Body: dispatch, ContentType: contentType, Operation: "create", ConditionalRouting: snapshot}
	assert.Equal(t, "target-alias", request.ExecutionModel("seedance2.5-A"))
	assert.Equal(t, raw, PublicVideoRoutingBody(request).Body)
	facts["resolution"] = "1080p"
	rule.Targets[1].Resolution = "1080p"
	assert.Equal(t, "480p", snapshot.SourceFacts["resolution"], "billing facts must be independent of caller state")
	assert.Equal(t, "720p", snapshot.Rule.Targets[1].Resolution, "a saved task must not observe later rule edits")
	decoded, repeated, _, err := DecodeFrozenVideoRoutingTarget(t.Context(), snapshot, 1, "default", "create", task.UserId)
	require.NoError(t, err)
	assert.Equal(t, "seedance2.5-E", decoded.UpstreamModel)
	assert.Equal(t, dispatch, repeated)
	require.NoError(t, db.Model(&channel).Update("model_mapping", `{"target-alias":"seedance2.5-F"}`).Error)
	_, _, _, err = DecodeFrozenVideoRoutingTarget(t.Context(), snapshot, 1, "default", "create", task.UserId)
	require.ErrorContains(t, err, "mapping or plugin changed")
	for _, fallback := range []string{"fail", "original"} {
		t.Run(fallback, func(t *testing.T) {
			unavailable := model.VideoRoutingRule{OnExhausted: fallback, Targets: []model.VideoRoutingTarget{{ChannelID: channel.Id + 1000, Model: "target-alias"}}}
			result, candidate, _, _, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1}, unavailable, "default", "seedance2.5-A", "create", raw, "application/json", facts, task.UserId, 0, nil)
			assert.Nil(t, candidate)
			if fallback == "fail" {
				require.ErrorContains(t, err, "no compatible")
				assert.False(t, result.Fallback)
			} else {
				require.NoError(t, err)
				assert.True(t, result.Fallback)
				assert.Equal(t, raw, result.SourceBody)
			}
		})
	}
}

func TestVideoRoutingRetryIntersectionAndOriginalFallback(t *testing.T) {
	db, task, first := videoRoutingFixture(t)
	second := first
	second.Id = 0
	require.NoError(t, db.Create(&second).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "target-alias", ChannelId: second.Id, Enabled: true}).Error)
	original := first
	original.Id, original.Models, original.ModelMapping = 0, "seedance2.5-A", nil
	require.NoError(t, db.Create(&original).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "seedance2.5-A", ChannelId: original.Id, Enabled: true}).Error)
	policy := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "mixed"), Group: "default", Platform: "mixed", PoolMode: "model", Enabled: true, AsyncEnabled: true, Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	binding := MediaPoolBindingKey(policy, "seedance2.5-A", "1K", "video")
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: binding, ChannelId: original.Id, Priority: 1}).Error)
	catalog := []ImageModelCapability{{Id: "seedance2.5-A", MediaType: "video"}}
	raw := []byte(`{"model":"seedance2.5-A","prompt":"test","seconds":20,"resolution":"480p","aspect_ratio":"16:9"}`)
	rule := model.VideoRoutingRule{ID: "upgrade", Name: "Upgrade", OnExhausted: "fail", Targets: []model.VideoRoutingTarget{{ChannelID: first.Id, Model: "target-alias", Resolution: "720p"}, {ChannelID: second.Id, Model: "target-alias", Resolution: "720p"}}}
	snapshot, _, dispatch, contentType, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1}, rule, "default", "seedance2.5-A", "create", raw, "application/json", map[string]any{"seconds": float64(20), "resolution": "480p"}, task.UserId, 0, nil)
	require.NoError(t, err)
	request := AsyncVideoRequest{Body: dispatch, ContentType: contentType, Operation: "create", ConditionalRouting: snapshot, MixedRouting: &MixedVideoRouting{Group: "default", PoolMode: "model", Model: "seedance2.5-A", Resolution: "1K", BindingKey: binding}, PendingDecision: &mediaretry.Decision{Action: mediaretry.OtherChannel}, RetryFact: &mediaretry.Fact{ChannelID: first.Id, Terminal: true}}
	job := model.AsyncMediaJob{UserId: task.UserId, TokenId: task.TokenId, Group: "default", Model: "seedance2.5-A"}
	channels, originalPool, err := ConditionalVideoRetryCandidates(t.Context(), job, request, policy, catalog)
	require.NoError(t, err)
	assert.False(t, originalPool)
	require.Len(t, channels, 1)
	assert.Equal(t, second.Id, channels[0].Id)
	rebindingSnapshot := *snapshot
	rebinding := request
	rebinding.ConditionalRouting = &rebindingSnapshot
	for range 2 {
		decoded, err := RebindConditionalVideoRequest(t.Context(), job, &rebinding, second, VideoModelOverrides{})
		require.NoError(t, err)
		assert.Equal(t, second.Id, decoded.Channel.Id)
		assert.Equal(t, 1, rebinding.ConditionalRouting.TargetIndex)
		assert.Contains(t, string(rebinding.Body), `"resolution":"720p"`)
	}
	request.PendingDecision.AllowedChannelIDs = []int{original.Id}
	channels, originalPool, err = ConditionalVideoRetryCandidates(t.Context(), job, request, policy, catalog)
	require.NoError(t, err)
	assert.False(t, originalPool)
	assert.Empty(t, channels, "strict rules must not escape to the ordinary pool")
	_, err = RebindConditionalVideoRequest(t.Context(), job, &request, original, VideoModelOverrides{})
	require.ErrorContains(t, err, "forbids original-route fallback")
	request.PendingDecision.AllowedChannelIDs = nil
	request.PendingDecision.Action = mediaretry.SameChannel
	channels, originalPool, err = ConditionalVideoRetryCandidates(t.Context(), job, request, policy, catalog)
	require.NoError(t, err)
	assert.False(t, originalPool)
	require.Len(t, channels, 1)
	assert.Equal(t, first.Id, channels[0].Id)
	request.PendingDecision.Action = mediaretry.OtherChannel
	for _, evidence := range []mediaretry.Fact{
		{Unknown: true},
		{Accepted: true},
		{Terminal: true, HasOutput: true},
	} {
		evidence.ChannelID = first.Id
		request.RetryFact = &evidence
		channels, _, err = ConditionalVideoRetryCandidates(t.Context(), job, request, policy, catalog)
		assert.Empty(t, channels, "uncertain or already productive execution must not be replayed")
		require.Error(t, err)
	}
	request.RetryFact = &mediaretry.Fact{ChannelID: first.Id, Terminal: true}
	request.PendingDecision.ExcludedChannelIDs = []int{second.Id}
	snapshot.Rule.OnExhausted = "original"
	channels, originalPool, err = ConditionalVideoRetryCandidates(t.Context(), job, request, policy, catalog)
	require.NoError(t, err)
	assert.True(t, originalPool)
	require.Len(t, channels, 1)
	assert.Equal(t, original.Id, channels[0].Id)
	request.OriginalBody, request.LocalReferencesReady = []byte("localized replay"), true
	request.Billing.Price.Quota = 123
	decoded, err := RebindConditionalVideoRequest(t.Context(), job, &request, original, VideoModelOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "seedance2.5-A", decoded.UpstreamModel)
	assert.True(t, snapshot.Fallback)
	assert.Equal(t, raw, request.Body)
	assert.Nil(t, request.OriginalBody)
	assert.False(t, request.LocalReferencesReady)
	assert.Equal(t, 123, request.Billing.Price.Quota)
	assert.Equal(t, "seedance2.5-A", request.ExecutionModel("seedance2.5-A"))
	ordinary := AsyncVideoRequest{Body: raw, ContentType: "application/json", Operation: "create"}
	decoded, err = RebindConditionalVideoRequest(t.Context(), job, &ordinary, original, VideoModelOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "seedance2.5-A", decoded.UpstreamModel)
	assert.Equal(t, raw, ordinary.Body)
	assert.Equal(t, raw, PublicVideoRoutingBody(ordinary).Body)
	weightedOriginal := original
	weightedOriginal.Id, weightedOriginal.Key = 0, "weighted-original-key"
	require.NoError(t, db.Create(&weightedOriginal).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "seedance2.5-A", ChannelId: weightedOriginal.Id, Enabled: true}).Error)
	require.NoError(t, db.Model(&model.ImageChannelPool{}).Where("binding_key = ? AND channel_id = ?", binding, original.Id).Update("priority", 0).Error)
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: binding, ChannelId: weightedOriginal.Id, Priority: 1}).Error)
	picked, err := PickVideoRetryChannel(t.Context(), job, request, "seedance2.5-A")
	require.NoError(t, err)
	assert.Equal(t, weightedOriginal.Id, picked.Id, "original-route fallback must preserve ordinary pool weights")
}

func TestVideoRoutingRetryTargetsPreserveSourceRequest(t *testing.T) {
	db, task, first := videoRoutingFixture(t)
	channels := []model.Channel{first}
	for range 2 {
		next := first
		next.Id = 0
		require.NoError(t, db.Create(&next).Error)
		require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "target-alias", ChannelId: next.Id, Enabled: true}).Error)
		channels = append(channels, next)
	}
	source := []byte(`{"model":"seedance2.5-A","prompt":"unchanged @图1","seconds":20,"resolution":"480p","aspect_ratio":"16:9","references":[{"type":"image","role":"reference","source":"https://source.test/reference.png"}]}`)
	rule := model.VideoRoutingRule{ID: "vary-resolution", Name: "Target resolutions", OnExhausted: "fail", Targets: []model.VideoRoutingTarget{
		{ChannelID: channels[0].Id, Model: "target-alias", Resolution: "720p"},
		{ChannelID: channels[1].Id, Model: "target-alias", Resolution: "1080p"},
		{ChannelID: channels[2].Id, Model: "target-alias"},
	}}
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprintf("localized=%t", local), func(t *testing.T) {
			snapshot, _, body, contentType, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1}, rule, "default", "seedance2.5-A", "create", source, "application/json", map[string]any{"seconds": float64(20), "resolution": "480p"}, task.UserId, 0, nil)
			require.NoError(t, err)
			request := AsyncVideoRequest{Body: body, ContentType: contentType, Operation: "create", ConditionalRouting: snapshot, LocalReferencesReady: local}
			request.Billing.Price.Quota = 123
			referenceURL := "https://source.test/reference.png"
			if local {
				referenceURL = "https://gateway.test/reference.png?signature=frozen"
				request.OriginalBody = body
				request.Body = bytes.ReplaceAll(body, []byte("https://source.test/reference.png"), []byte(referenceURL))
			}
			job := model.AsyncMediaJob{UserId: task.UserId, Group: "default", Model: "seedance2.5-A"}
			for _, step := range []struct {
				action, resolution string
				failed, selected   int
				candidates         []int
			}{
				{action: mediaretry.OtherChannel, resolution: "1080p", failed: 0, selected: 1, candidates: []int{channels[1].Id, channels[2].Id}},
				{action: mediaretry.SameChannel, resolution: "1080p", failed: 1, selected: 1, candidates: []int{channels[1].Id}},
				{action: mediaretry.OtherChannel, resolution: "480p", failed: 1, selected: 2, candidates: []int{channels[2].Id}},
			} {
				action := step.action
				if local {
					if action == mediaretry.OtherChannel {
						action = mediaretry.LocalOtherChannel
					} else {
						action = mediaretry.LocalSameChannel
					}
				}
				request.PendingDecision = &mediaretry.Decision{Action: action}
				request.RetryFact = &mediaretry.Fact{ChannelID: channels[step.failed].Id, Terminal: true}
				eligible, originalPool, err := ConditionalVideoRetryCandidates(t.Context(), job, request, model.ImageGroupPolicy{}, nil)
				require.NoError(t, err)
				assert.False(t, originalPool)
				ids := make([]int, 0, len(eligible))
				for _, channel := range eligible {
					ids = append(ids, channel.Id)
				}
				assert.Equal(t, step.candidates, ids)
				_, err = RebindConditionalVideoRequest(t.Context(), job, &request, channels[step.selected], VideoModelOverrides{})
				require.NoError(t, err)
				persisted, err := common.Marshal(request)
				require.NoError(t, err)
				var resumed AsyncVideoRequest
				require.NoError(t, common.Unmarshal(persisted, &resumed))
				request = resumed
				var executed map[string]any
				require.NoError(t, common.Unmarshal(request.Body, &executed))
				assert.Equal(t, "seedance2.5-A", executed["model"])
				assert.Equal(t, step.resolution, executed["resolution"])
				assert.Equal(t, float64(20), executed["seconds"])
				assert.Equal(t, "unchanged @图1", executed["prompt"])
				assert.Equal(t, "16:9", executed["aspect_ratio"])
				assert.Equal(t, []any{map[string]any{"type": "image", "role": "reference", "source": referenceURL}}, executed["references"])
				assert.Equal(t, step.selected, request.ConditionalRouting.TargetIndex)
				assert.Equal(t, source, request.ConditionalRouting.SourceBody)
				assert.Equal(t, "480p", request.ConditionalRouting.SourceFacts["resolution"])
				assert.Equal(t, 123, request.Billing.Price.Quota)
				assert.Equal(t, local, request.LocalReferencesReady)
			}
		})
	}
}

func TestVideoRoutingLocalRecoveryRetainsMediaAcrossTargetAndFallback(t *testing.T) {
	db, task, targetChannel := videoRoutingFixture(t)
	originalChannel := targetChannel
	originalChannel.Id, originalChannel.Models, originalChannel.ModelMapping = 0, "seedance2.5-A", nil
	require.NoError(t, db.Create(&originalChannel).Error)
	source := []byte(`{"model":"seedance2.5-A","prompt":"keep @图1 literal","seconds":20,"resolution":"480p","aspect_ratio":"16:9","references":[{"type":"image","role":"reference","source":"https://source.example/reference.png"}]}`)
	rule := model.VideoRoutingRule{ID: "local", Name: "Local", OnExhausted: "original", Targets: []model.VideoRoutingTarget{{ChannelID: targetChannel.Id, Model: "target-alias", Resolution: "720p"}}}
	for _, tc := range []struct {
		name, action, resolution string
		channel                  model.Channel
		fallback                 bool
	}{
		{name: "same target", action: mediaretry.LocalSameChannel, channel: targetChannel, resolution: "720p"},
		{name: "original fallback", action: mediaretry.LocalOtherChannel, channel: originalChannel, resolution: "480p", fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, _, dispatch, contentType, err := PlanVideoRouting(t.Context(), model.VideoRoutingConfig{Version: 1}, rule, "default", "seedance2.5-A", "create", source, "application/json", map[string]any{"seconds": float64(20), "resolution": "480p"}, task.UserId, 0, nil)
			require.NoError(t, err)
			localized := bytes.ReplaceAll(dispatch, []byte("https://source.example/reference.png"), []byte("https://gateway.example/local-reference.png?signature=test"))
			request := AsyncVideoRequest{Body: localized, OriginalBody: dispatch, ContentType: contentType, Operation: "create", ConditionalRouting: snapshot, PendingDecision: &mediaretry.Decision{Action: tc.action}, LocalReferencesReady: true}
			job := model.AsyncMediaJob{UserId: task.UserId, Group: "default", Model: "seedance2.5-A"}
			_, err = RebindConditionalVideoRequest(t.Context(), job, &request, tc.channel, VideoModelOverrides{})
			require.NoError(t, err)
			assert.True(t, request.LocalReferencesReady)
			assert.Equal(t, tc.fallback, snapshot.Fallback)
			var executed map[string]any
			require.NoError(t, common.Unmarshal(request.Body, &executed))
			assert.Equal(t, "seedance2.5-A", executed["model"])
			assert.Equal(t, tc.resolution, executed["resolution"])
			assert.Equal(t, float64(20), executed["seconds"])
			assert.Equal(t, "keep @图1 literal", executed["prompt"])
			assert.Contains(t, string(request.Body), "https://gateway.example/local-reference.png?signature=test")
			assert.NotContains(t, string(request.Body), "https://source.example/reference.png")
			assert.Contains(t, string(request.OriginalBody), "https://source.example/reference.png")
			assert.NotContains(t, string(request.OriginalBody), "https://gateway.example/local-reference.png")
			assert.Equal(t, source, snapshot.SourceBody)
			assert.Equal(t, "480p", snapshot.SourceFacts["resolution"])
		})
	}
}

func TestVideoRoutingSourceDefaultsMustBeUnambiguous(t *testing.T) {
	first := jsplugin.VideoProfile{Models: []string{"public"}, Modes: []jsplugin.VideoModeProfile{{Name: "text_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 4, Max: 30, Step: 1, Default: 4}, Resolutions: []string{"480p", "720p"}, DefaultResolution: "480p"}}}
	for _, tc := range []struct {
		name, raw, resolution, failure string
		seconds                        int
	}{
		{name: "ambiguous duration", raw: `{"model":"public","prompt":"test"}`, seconds: 5, resolution: "480p", failure: "seconds default is ambiguous"},
		{name: "ambiguous resolution", raw: `{"model":"public","prompt":"test"}`, seconds: 4, resolution: "720p", failure: "resolution default is ambiguous"},
		{name: "explicit request resolves ambiguity", raw: `{"model":"public","prompt":"test","seconds":20,"resolution":"480p","seed":18446744073709551615,"watermark":false}`, seconds: 5, resolution: "720p"},
		{name: "consistent defaults", raw: `{"model":"public","prompt":"test","seed":18446744073709551615}`, seconds: 4, resolution: "480p"},
		{name: "conflicting resolution aliases", raw: `{"model":"public","prompt":"test","seconds":20,"resolution":"480p","metadata":{"resolution":"720p"}}`, seconds: 4, resolution: "480p", failure: "conflicting mixed video resolutions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := jsplugin.VideoProfile{Models: []string{"public"}, Modes: []jsplugin.VideoModeProfile{{Name: "text_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 4, Max: 30, Step: 1, Default: tc.seconds}, Resolutions: []string{"480p", "720p"}, DefaultResolution: tc.resolution}}}
			normalized, contentType, body, err := NormalizeVideoRoutingSourceDefaults([]byte(tc.raw), "application/json", "create", []jsplugin.VideoProfile{first, second})
			if tc.failure != "" {
				require.ErrorContains(t, err, tc.failure)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "application/json", contentType)
			assert.Equal(t, "480p", body["resolution"])
			assert.Contains(t, string(normalized), `"seed":18446744073709551615`)
			if tc.name == "explicit request resolves ambiguity" {
				assert.Equal(t, float64(20), body["seconds"])
				assert.Contains(t, string(normalized), `"watermark":false`)
			} else {
				assert.Equal(t, float64(4), body["seconds"])
			}
		})
	}
}

func TestVideoRoutingPreviewMatchesRuntimeDimensionsAndPermissions(t *testing.T) {
	db, _, targetChannel := videoRoutingFixture(t)
	originalChannel := targetChannel
	originalChannel.Id, originalChannel.Models, originalChannel.ModelMapping = 0, "seedance2.5-A", nil
	require.NoError(t, db.Create(&originalChannel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "seedance2.5-A", ChannelId: originalChannel.Id, Enabled: true}).Error)
	policy := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "mixed"), Group: "default", Platform: "mixed", PoolMode: "model", Enabled: true, AsyncEnabled: true, Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: MediaPoolBindingKey(policy, "seedance2.5-A", "1K", "video"), Group: "default", Platform: "mixed", Mode: "model", Model: "seedance2.5-A", ChannelId: originalChannel.Id, Priority: 1}).Error)
	minimum := 15
	rule := model.VideoRoutingRule{ID: "long", Name: "Long", Enabled: true, OnExhausted: "fail", Match: model.VideoRoutingMatch{Models: []string{"seedance2.5-A"}, Resolutions: []string{"480p"}, Seconds: model.VideoRoutingSeconds{Min: &minimum, MinExclusive: true}}, Targets: []model.VideoRoutingTarget{{ChannelID: targetChannel.Id, Model: "target-alias", Resolution: "720p"}}}
	config := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{rule}}
	_, err := model.SaveVideoRoutingConfig(t.Context(), config, 0)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, provider string
		seconds        int
		conditional    bool
	}{
		{name: "excluded 15 seconds", seconds: 15},
		{name: "included 16 seconds", seconds: 16, conditional: true},
		{name: "explicit provider bypass", seconds: 20, provider: "drama"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := PreviewMediaRouting(t.Context(), MediaRoutingPreviewRequest{Group: "default", Platform: "mixed", Provider: tc.provider, MediaType: "video", Model: "seedance2.5-A", Resolution: "480p", Operation: "text_to_video", Seconds: &tc.seconds})
			require.NoError(t, err)
			if !tc.conditional {
				assert.Nil(t, preview.ConditionalRouting)
				return
			}
			require.NotNil(t, preview.ConditionalRouting)
			assert.Equal(t, "long", preview.ConditionalRouting.RuleID)
			assert.Equal(t, "480p", preview.ConditionalRouting.Source["resolution"])
			assert.Equal(t, float64(tc.seconds), preview.ConditionalRouting.Source["seconds"])
			require.Len(t, preview.ConditionalRouting.Targets, 1)
			assert.Equal(t, "720p", preview.ConditionalRouting.Targets[0].Resolution)
			assert.True(t, preview.ConditionalRouting.Targets[0].Eligible)
			require.NotNil(t, preview.ConditionalRouting.SelectedTargetIndex)
			assert.Zero(t, *preview.ConditionalRouting.SelectedTargetIndex)
			require.Len(t, preview.Eligible, 1)
			assert.Equal(t, targetChannel.Id, preview.Eligible[0].ChannelID)
			assert.Equal(t, "seedance2.5-E", preview.Eligible[0].UpstreamModel)
		})
	}
	seconds := 20
	input := MediaRoutingPreviewRequest{Group: "default", Platform: "mixed", MediaType: "video", Model: "seedance2.5-A", Resolution: "480p", Operation: "text_to_video", Seconds: &seconds, VideoRoutingConfig: &config}
	parameters := map[string]any{"prompt": "preview", "seconds": float64(20), "resolution": "480p"}
	secondRule := rule
	secondRule.ID, secondRule.Name = "later", "Later"
	config.Rules = append(config.Rules, secondRule)
	var preview MediaRoutingPreview
	handled, err := PreviewConditionalVideoRouting(t.Context(), input, &preview, []string{"default"}, parameters)
	require.NoError(t, err)
	assert.True(t, handled)
	require.NotNil(t, preview.ConditionalRouting)
	assert.Equal(t, "long", preview.ConditionalRouting.RuleID)
	assert.Len(t, preview.ConditionalRouting.MatchedRules, 2)
	assert.Contains(t, preview.Warnings, "conditional_rules_overlap_first_match_wins")
	assert.Equal(t, "480p", parameters["resolution"], "preview may not rewrite billing source facts")
	// A group with no original-model capability cannot win merely because it
	// appears before another group whose source pool supports the request.
	groupSpecific := rule
	groupSpecific.ID, groupSpecific.Match.Groups = "empty-source-group", []string{"empty-source"}
	config.Rules = append([]model.VideoRoutingRule{groupSpecific}, config.Rules...)
	preview = MediaRoutingPreview{}
	_, err = PreviewConditionalVideoRouting(t.Context(), input, &preview, []string{"empty-source", "default"}, parameters)
	require.NoError(t, err)
	require.NotNil(t, preview.ConditionalRouting)
	assert.Equal(t, "long", preview.ConditionalRouting.RuleID)
	assert.NotContains(t, preview.ConditionalRouting.MatchedRules, VideoRoutingRuleMatch{ID: "empty-source-group", Name: rule.Name})
	config.Rules = config.Rules[1:]
	config.Rules[1].Match.Resolutions = []string{"720p"}
	preview = MediaRoutingPreview{}
	_, err = PreviewConditionalVideoRouting(t.Context(), input, &preview, []string{"default"}, parameters)
	require.NoError(t, err)
	assert.Len(t, preview.ConditionalRouting.MatchedRules, 1)
	assert.NotContains(t, preview.Warnings, "conditional_rules_overlap_first_match_wins")
	// A stale ability must not authorize a model removed from the channel.
	require.NoError(t, db.Model(&targetChannel).Update("models", "unrelated-model").Error)
	preview = MediaRoutingPreview{}
	handled, err = PreviewConditionalVideoRouting(t.Context(), input, &preview, []string{"default"}, parameters)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Nil(t, preview.ConditionalRouting.SelectedTargetIndex)
	assert.Contains(t, preview.ConditionalRouting.Targets[0].Reasons, "target_model_or_group_unavailable")
	config.Rules[0].OnExhausted = "original"
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	require.NotNil(t, preview.ConditionalRouting)
	assert.True(t, preview.ConditionalRouting.IsFallback)
	require.Len(t, preview.Eligible, 1)
	assert.Equal(t, originalChannel.Id, preview.Eligible[0].ChannelID)
	assert.Equal(t, "480p", preview.Resolution)
	// An omitted resolution cannot pick a rule using one provider's default
	// when another source capability supplies a different default.
	alternative := originalChannel
	alternative.Id = 0
	alternativeMapping := `{"seedance2.5-A":"seedance2.5-F"}`
	alternative.ModelMapping = &alternativeMapping
	require.NoError(t, db.Create(&alternative).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: "seedance2.5-A", ChannelId: alternative.Id, Enabled: true}).Error)
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: MediaPoolBindingKey(policy, "seedance2.5-A", "1K", "video"), Group: "default", Platform: "mixed", Mode: "model", Model: "seedance2.5-A", ChannelId: alternative.Id, Priority: 1}).Error)
	input.Resolution = ""
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	assert.Contains(t, preview.Reasons, "video_parameters_not_supported")
	assert.Nil(t, preview.ConditionalRouting)
	assert.Empty(t, preview.Eligible)
}

func TestVideoRoutingPreviewKnownVideoInputs(t *testing.T) {
	db, _, target := videoRoutingFixture(t)
	source := target
	source.Id, source.Models, source.ModelMapping = 0, "seedance2.5-A", nil
	require.NoError(t, db.Create(&source).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: source.Models, ChannelId: source.Id, Enabled: true}).Error)
	policy := model.ImageGroupPolicy{PolicyKey: ImageIdentityHash("default", "mixed"), Group: "default", Platform: "mixed", PoolMode: "model", Enabled: true, AsyncEnabled: true, Models: `[{"id":"seedance2.5-A","media_type":"video"}]`}
	require.NoError(t, db.Create(&policy).Error)
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: MediaPoolBindingKey(policy, source.Models, "1K", "video"), Group: "default", Platform: "mixed", ChannelId: source.Id, Priority: 1}).Error)
	rule := model.VideoRoutingRule{ID: "known-input", Name: "Known input", Enabled: true, Match: model.VideoRoutingMatch{Models: []string{source.Models}}, Targets: []model.VideoRoutingTarget{{ChannelID: target.Id, Model: "target-alias"}}, OnExhausted: "fail"}
	config := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{rule}}
	seconds := 10
	input := MediaRoutingPreviewRequest{Group: "default", Platform: "mixed", MediaType: "video", Model: source.Models, Operation: "reference_to_video", Resolution: "720p", Seconds: &seconds, VideoRoutingConfig: &config}
	require.NoError(t, db.Model(&target).Update("model_mapping", `{"target-alias":"seedance2.0-F-720p"}`).Error)
	for _, knownInput := range []string{"", "none", "video"} {
		t.Run("target_video_input="+knownInput, func(t *testing.T) {
			input.VideoInput = knownInput
			preview, err := PreviewMediaRouting(t.Context(), input)
			require.NoError(t, err)
			require.NotNil(t, preview.ConditionalRouting)
			require.Len(t, preview.ConditionalRouting.Targets, 1)
			assert.Equal(t, knownInput != "video", preview.ConditionalRouting.Targets[0].Eligible)
			assert.Contains(t, preview.Warnings, "conditional_preview_dimensions_only_materials_not_validated")
			if knownInput == "video" {
				assert.Empty(t, preview.Eligible)
				assert.Contains(t, preview.ConditionalRouting.Targets[0].Reasons, "video reference input is not supported by the selected mode")
			}
		})
	}
	require.NoError(t, db.Model(&target).Update("model_mapping", `{"target-alias":"seedance2.5-E"}`).Error)
	input.VideoInput = "video"
	preview, err := PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, preview.Eligible, 1)
	assert.Equal(t, target.Id, preview.Eligible[0].ChannelID)
	// A separate source group accepting only images cannot win a video-input
	// rule before the group with an actual video-capable source model.
	imageOnly := source
	imageOnly.Id, imageOnly.Group = 0, "image-only"
	imageMapping := `{"seedance2.5-A":"seedance2.0-F-720p"}`
	imageOnly.ModelMapping = &imageMapping
	require.NoError(t, db.Create(&imageOnly).Error)
	require.NoError(t, db.Create(&model.Ability{Group: imageOnly.Group, Model: source.Models, ChannelId: imageOnly.Id, Enabled: true}).Error)
	imagePolicy := policy
	imagePolicy.Group = imageOnly.Group
	require.NoError(t, db.Create(&model.ImageChannelPool{BindingKey: MediaPoolBindingKey(imagePolicy, source.Models, "1K", "video"), Group: imageOnly.Group, Platform: "mixed", ChannelId: imageOnly.Id, Priority: 1}).Error)
	firstRule := rule
	firstRule.ID, firstRule.Match.Groups = "image-only-first", []string{imageOnly.Group}
	config.Rules = []model.VideoRoutingRule{firstRule, rule}
	parameters := map[string]any{"prompt": "preview", "seconds": float64(10), "resolution": "720p"}
	preview = MediaRoutingPreview{}
	_, err = PreviewConditionalVideoRouting(t.Context(), input, &preview, []string{imageOnly.Group, "default"}, parameters)
	require.NoError(t, err)
	require.NotNil(t, preview.ConditionalRouting)
	assert.Equal(t, rule.ID, preview.ConditionalRouting.RuleID)
	assert.Len(t, preview.ConditionalRouting.MatchedRules, 1)
	// If the public source itself loses video support, the complete preview
	// must not expose a compatible target or an ordinary eligible channel.
	require.NoError(t, db.Model(&source).Update("model_mapping", imageMapping).Error)
	preview, err = PreviewMediaRouting(t.Context(), input)
	require.NoError(t, err)
	assert.Nil(t, preview.ConditionalRouting)
	assert.Empty(t, preview.Eligible)
	assert.Contains(t, preview.Reasons, "video_parameters_not_supported")
	requiredVideo := jsplugin.VideoProfile{Modes: []jsplugin.VideoModeProfile{{Name: "reference_to_video", Inputs: []jsplugin.VideoInputProfile{{Name: "video", Kind: "video", Sources: []string{"url"}, MaxItems: 1, Required: true}}}}}
	_, err = videoRoutingPreviewProfileForInputs(requiredVideo, "reference_to_video", "none")
	require.ErrorContains(t, err, "required by the selected mode")
	for _, knownInput := range []string{"", "video"} {
		_, err = videoRoutingPreviewProfileForInputs(requiredVideo, "reference_to_video", knownInput)
		require.NoError(t, err)
	}
	assert.Len(t, requiredVideo.Modes[0].Inputs, 1, "preview must preserve the real capability's input requirements")
}

func TestVideoRoutingAdmissionDefaultsOnlyForPotentialMatches(t *testing.T) {
	profiles := make([]jsplugin.VideoProfile, 0, 2)
	for _, seconds := range []int{4, 5} {
		base := jsplugin.VideoModeProfile{Name: "text_to_video", Duration: &jsplugin.VideoDurationProfile{Min: 4, Max: 30, Step: 1, Default: seconds}, Resolutions: []string{"480p", "720p"}, DefaultResolution: "480p"}
		reference := base
		reference.Name, reference.Inputs = "reference_to_video", []jsplugin.VideoInputProfile{{Name: "image", Kind: "image", Sources: []string{"url"}, MaxItems: 1, Required: true}}
		profiles = append(profiles, jsplugin.VideoProfile{Models: []string{"public"}, Modes: []jsplugin.VideoModeProfile{base, reference}})
	}
	for _, tc := range []struct {
		name, body string
		match      model.VideoRoutingMatch
		ambiguous  bool
	}{
		{name: "reference rule ignores text request", match: model.VideoRoutingMatch{Modes: []string{"reference_to_video"}}},
		{name: "resolution misses", match: model.VideoRoutingMatch{Resolutions: []string{"720p"}}},
		{name: "reference video misses", match: model.VideoRoutingMatch{VideoInputs: []string{"video"}}},
		{name: "seconds misses", match: model.VideoRoutingMatch{Seconds: model.VideoRoutingSeconds{Min: common.GetPointer(16)}}},
		{name: "one default matches", match: model.VideoRoutingMatch{Seconds: model.VideoRoutingSeconds{Min: common.GetPointer(4), Max: common.GetPointer(4)}}, ambiguous: true},
		{name: "model misses", match: model.VideoRoutingMatch{Models: []string{"other"}}},
		{name: "group misses", match: model.VideoRoutingMatch{Groups: []string{"other"}}},
		{name: "reference rule matches reference request", body: `{"model":"public","prompt":"test","image":"https://source.test/reference.png"}`, match: model.VideoRoutingMatch{Modes: []string{"reference_to_video"}}, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.body
			if raw == "" {
				raw = `{"model":"public","prompt":"test"}`
			}
			config := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{{ID: "conditional", Enabled: true, Match: tc.match}}}
			_, _, normalized, err := NormalizeVideoRoutingAdmissionDefaults(config, "public", []string{"default"}, []byte(raw), "application/json", "create", map[string][]jsplugin.VideoProfile{"default": profiles}, "")
			if tc.ambiguous {
				require.ErrorContains(t, err, "seconds default is ambiguous")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, float64(4), normalized["seconds"], "nonmatching rules retain ordinary source default selection")
			assert.Equal(t, "480p", normalized["resolution"])
		})
	}
	for _, matchingSeconds := range []int{4, 5} {
		t.Run(fmt.Sprintf("group-specific seconds=%d", matchingSeconds), func(t *testing.T) {
			config := model.VideoRoutingConfig{Version: 1, Enabled: true, Rules: []model.VideoRoutingRule{{ID: "grouped", Enabled: true, Match: model.VideoRoutingMatch{Groups: []string{"g1"}, Seconds: model.VideoRoutingSeconds{Min: &matchingSeconds, Max: &matchingSeconds}}}}}
			grouped := map[string][]jsplugin.VideoProfile{"g1": {profiles[1]}, "g2": {profiles[0]}}
			_, _, normalized, err := NormalizeVideoRoutingAdmissionDefaults(config, "public", []string{"g1", "g2"}, []byte(`{"model":"public","prompt":"test"}`), "application/json", "create", grouped, "")
			if matchingSeconds == 5 {
				require.ErrorContains(t, err, "seconds default is ambiguous")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, float64(5), normalized["seconds"], "another group's default must not create a possible rule match")
		})
	}
}
