package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	filterdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func taskVideoBindingTestChannel(name, unit string) Channel {
	config := dto.TaskVideoModelConfig{Duration: dto.TaskVideoDuration{Values: []int{5}, Default: 5}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video"}, References: dto.TaskVideoReferences{Images: common.GetPointer(0), Videos: common.GetPointer(0), Audios: common.GetPointer(0)}, BillingMode: unit}
	alternate := config
	if unit == "per_second" {
		alternate.BillingMode = "per_request"
	} else {
		alternate.BillingMode = "per_second"
	}
	channel := Channel{Type: constant.ChannelTypeTaskPlugin, Key: "test-key", Status: common.ChannelStatusEnabled, Name: name, Models: name, Group: "default"}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "huahuo-video", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"vendor-model": config, "alternate": alternate}})
	mapping, _ := common.Marshal(map[string]string{name: "vendor-model"})
	channel.ModelMapping = common.GetPointer(string(mapping))
	return channel
}

func TestWolongModelDefaultsAndConfiguredCatalog(t *testing.T) {
	previous := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "wolong", "plugin.js"))
	require.NoError(t, err)
	plugin, err := jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: "wolong"})
	require.NoError(t, err)
	channel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: "haya-mo-seedance-25"}
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "wolong"})
	_, defaults, err := channel.TaskVideoModelConfig(channel.Models)
	require.NoError(t, err)
	assert.Equal(t, "per_request", defaults.BillingMode)
	assert.Equal(t, 5, defaults.Duration.Default)
	assert.Equal(t, "16:9", defaults.AspectRatios[0])
	_, _, err = channel.TaskVideoModelConfig("future-model")
	require.ErrorContains(t, err, "requires a saved capability")
	custom := defaults
	custom.PresetModel = ""
	custom.BillingMode = "per_second"
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "wolong", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model": custom, channel.Models: custom}})
	_, _, err = channel.TaskVideoModelConfig(channel.Models)
	require.ErrorContains(t, err, "built-in billing unit")
	channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "wolong", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model": custom}})
	channel.Models = "haya-mo-seedance-25,public-future"
	mapping := `{"public-future":"future-model"}`
	channel.ModelMapping = &mapping
	catalog, err := taskVideoModelCatalog([]Channel{channel}, "wolong")
	require.NoError(t, err)
	assert.Equal(t, defaults, catalog["haya-mo-seedance-25"])
	assert.Equal(t, custom, catalog["public-future"])
	assert.Equal(t, custom, catalog["future-model"])
	mutated := jsplugin.WithTaskVideoModels(plugin, catalog)
	assert.Contains(t, mutated.Meta.Models, "public-future")
	_, config, err := channel.TaskVideoModelConfig("haya-mo-seedance-25")
	require.NoError(t, err)
	assert.Equal(t, defaults, config)
}

func TestPresetVideoConfigRestoresFrozenContracts(t *testing.T) {
	previous := jsplugin.DefaultRegistry
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	t.Cleanup(func() { jsplugin.DefaultRegistry = previous })
	for _, key := range dto.ChannelConfiguredTaskVideoPlugins() {
		if !dto.PresetTaskVideoPlugin(key) {
			continue
		}
		t.Run(key, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", key, "plugin.js"))
			require.NoError(t, err)
			plugin, err := jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: key})
			require.NoError(t, err)
			if key == "yuxibox" {
				const name = "minimax-h3-quality"
				channel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: name}
				channel.SetSetting(dto.ChannelSettings{TaskPluginKey: key, VideoResolutionModelMapping: dto.VideoResolutionModelMapping{name: {"720p": name}}})
				catalog, err := taskVideoModelCatalog([]Channel{channel}, key)
				require.NoError(t, err)
				discovery := jsplugin.WithTaskVideoModels(plugin, catalog)
				expected, _ := plugin.Meta.VideoProfileForModel(name)
				profile, found := discovery.Meta.VideoProfileForModel(name)
				require.True(t, found)
				assert.Equal(t, expected, profile)
				usage, _ := discovery.Meta.UsageForModel(name)
				assert.Contains(t, usage, "billing_period")
			}
			for _, name := range plugin.Meta.Models {
				settings := dto.ChannelSettings{TaskPluginKey: key}
				defaults, err := ResolveTaskVideoModelConfig(settings, name)
				require.NoError(t, err, name)
				encoded, err := common.Marshal(defaults)
				require.NoError(t, err)
				var frozen dto.TaskVideoModelConfig
				require.NoError(t, common.Unmarshal(encoded, &frozen))
				assert.Empty(t, frozen.PresetModel)
				settings.TaskPluginModelConfigs = map[string]dto.TaskVideoModelConfig{name: frozen}
				require.NoError(t, settings.ValidateTaskVideoModels(3600))
				restored, err := ResolveTaskVideoModelConfig(settings, name)
				require.NoError(t, err)
				assert.Equal(t, defaults, restored)
				assert.False(t, IsConfiguredTaskVideoModel(settings, name))
				selected := jsplugin.WithTaskVideoModels(plugin, map[string]dto.TaskVideoModelConfig{"public-alias": restored})
				expected, _ := plugin.Meta.VideoProfileForModel(name)
				actual, found := selected.Meta.VideoProfileForModel("public-alias")
				require.True(t, found)
				expected.Models = []string{"public-alias"}
				assert.Equal(t, expected, actual)
				if key != "wolong" {
					frozen.Duration = dto.TaskVideoDuration{Values: []int{3600}}
					settings.TaskPluginModelConfigs[name] = frozen
					_, err = ResolveTaskVideoModelConfig(settings, name)
					require.ErrorContains(t, err, "built-in capabilities")
				}
			}
			var members []Channel
			unit := ""
			for _, name := range plugin.Meta.Models {
				defaults, err := ResolveTaskVideoModelConfig(dto.ChannelSettings{TaskPluginKey: key}, name)
				require.NoError(t, err)
				if unit != "" && unit != defaults.BillingMode {
					continue
				}
				unit = defaults.BillingMode
				mapping, err := common.Marshal(map[string]string{"shared-public": name})
				require.NoError(t, err)
				channel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: "shared-public", ModelMapping: common.GetPointer(string(mapping))}
				channel.SetSetting(dto.ChannelSettings{TaskPluginKey: key})
				members = append(members, channel)
				if len(members) == 2 {
					break
				}
			}
			if len(members) == 2 {
				catalog, err := taskVideoModelCatalog(members, key)
				require.NoError(t, err)
				assert.Empty(t, catalog["shared-public"].PresetModel, "different source presets cannot claim one shared provenance")
				discovery := jsplugin.WithTaskVideoModels(plugin, catalog)
				for _, member := range members {
					upstream, config, err := member.TaskVideoModelConfig("shared-public")
					require.NoError(t, err)
					selected := jsplugin.WithTaskVideoModels(discovery, map[string]dto.TaskVideoModelConfig{"shared-public": config, upstream: config})
					expected, _ := plugin.Meta.VideoProfileForModel(upstream)
					actual, found := selected.Meta.VideoProfileForModel("shared-public")
					require.True(t, found)
					expected.Models = []string{"shared-public"}
					assert.Equal(t, expected, actual)
					assert.False(t, jsplugin.IsCustomTaskVideoModel(selected, "shared-public"))
				}
			}
			if key == "drama" {
				members = nil
				for _, name := range []string{"seedance2.0-F", "seedance2.5-G"} {
					mapping, err := common.Marshal(map[string]string{"shared-rich": name})
					require.NoError(t, err)
					channel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: "shared-rich", ModelMapping: common.GetPointer(string(mapping))}
					channel.SetSetting(dto.ChannelSettings{TaskPluginKey: key})
					members = append(members, channel)
				}
				catalog, err := taskVideoModelCatalog(members, key)
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"seedance2.0-F", "seedance2.5-G"}, catalog["shared-rich"].PresetModels)
				discovery := jsplugin.WithTaskVideoModels(plugin, catalog)
				require.NoError(t, jsplugin.ValidateV1Meta(discovery.Meta))
				profile, found := discovery.Meta.VideoProfileForModel("shared-rich")
				require.True(t, found)
				modes := map[string]jsplugin.VideoModeProfile{}
				for _, mode := range profile.Modes {
					modes[mode.Name] = mode
				}
				assert.Contains(t, modes, "edit_video")
				assert.Contains(t, modes, "extend_video")
				assert.True(t, modes["reference_to_video"].Options.GenerateAudio)
				assert.True(t, modes["reference_to_video"].Options.ReturnLastFrame)
				assert.Contains(t, modes["reference_to_video"].Inputs[0].Sources, "asset")
				schema, _ := discovery.Meta.UsageForModel("shared-rich")
				assert.Contains(t, schema, "tokens")
				assert.Contains(t, schema, "video_input")
				assert.False(t, jsplugin.IsCustomTaskVideoModel(discovery, "shared-rich"))
				for _, member := range members {
					upstream, config, err := member.TaskVideoModelConfig("shared-rich")
					require.NoError(t, err)
					selected := jsplugin.WithTaskVideoModels(discovery, map[string]dto.TaskVideoModelConfig{"shared-rich": config})
					expected, _ := plugin.Meta.VideoProfileForModel(upstream)
					actual, _ := selected.Meta.VideoProfileForModel("shared-rich")
					expected.Models = []string{"shared-rich"}
					assert.Equal(t, expected, actual)
				}
				custom := dto.TaskVideoModelConfig{
					Duration:    dto.TaskVideoDuration{Values: []int{30}, Default: 30},
					Resolutions: []string{"future4k"}, AspectRatios: []string{"32:9"}, Modes: []string{"text_to_video"},
					References: dto.TaskVideoReferences{Images: common.GetPointer(0), Videos: common.GetPointer(0), Audios: common.GetPointer(0)}, BillingMode: "per_second",
				}
				mapping, err := common.Marshal(map[string]string{"shared-rich": "future-model"})
				require.NoError(t, err)
				customChannel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Models: "shared-rich", ModelMapping: common.GetPointer(string(mapping))}
				customChannel.SetSetting(dto.ChannelSettings{TaskPluginKey: key, TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-model": custom}})
				members = append(members, customChannel)
				for _, order := range [][]Channel{members, {members[2], members[0], members[1]}} {
					catalog, err = taskVideoModelCatalog(order, key)
					require.NoError(t, err)
					assert.True(t, catalog["shared-rich"].CustomModelUnion)
					discovery = jsplugin.WithTaskVideoModels(plugin, catalog)
					require.NoError(t, jsplugin.ValidateV1Meta(discovery.Meta))
					profile, found = discovery.Meta.VideoProfileForModel("shared-rich")
					require.True(t, found)
					clear(modes)
					for _, mode := range profile.Modes {
						modes[mode.Name] = mode
					}
					assert.Contains(t, modes, "edit_video")
					assert.Contains(t, modes, "extend_video")
					assert.True(t, modes["reference_to_video"].Options.GenerateAudio)
					assert.True(t, modes["reference_to_video"].Options.ReturnLastFrame)
					assert.Contains(t, modes["reference_to_video"].Inputs[0].Sources, "asset")
					assert.Contains(t, modes["text_to_video"].Resolutions, "future4k")
					assert.Contains(t, modes["text_to_video"].Duration.Values, 30)
					schema, _ = discovery.Meta.UsageForModel("shared-rich")
					assert.Contains(t, schema, "tokens")
					assert.Contains(t, schema["resolution"].Enum, "future4k")
					upstream, preset, err := members[0].TaskVideoModelConfig("shared-rich")
					require.NoError(t, err)
					selectedPreset := jsplugin.WithTaskVideoModels(discovery, map[string]dto.TaskVideoModelConfig{"shared-rich": preset})
					expected, _ := plugin.Meta.VideoProfileForModel(upstream)
					actual, _ := selectedPreset.Meta.VideoProfileForModel("shared-rich")
					expected.Models = []string{"shared-rich"}
					assert.Equal(t, expected, actual)
					selected := jsplugin.WithTaskVideoModels(discovery, map[string]dto.TaskVideoModelConfig{"shared-rich": custom})
					profile, found = selected.Meta.VideoProfileForModel("shared-rich")
					require.True(t, found)
					require.Len(t, profile.Modes, 1)
					assert.Equal(t, "text_to_video", profile.Modes[0].Name)
					assert.Equal(t, []string{"future4k"}, profile.Modes[0].Resolutions)
					schema, _ = selected.Meta.UsageForModel("shared-rich")
					assert.NotContains(t, schema, "tokens")
					assert.True(t, jsplugin.IsCustomTaskVideoModel(selected, "shared-rich"))
				}
			}
		})
	}
}

func TestTaskVideoModelBindingWrites(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "bindings.db") + "?_pragma=busy_timeout(10000)")
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "video_binding_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(4)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			previousDB, previousType := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() { DB = previousDB; common.SetMainDatabaseType(previousType) })
			require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}, &Option{}, &TaskPlugin{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Ability{}, &Channel{}, &Option{}, &TaskPlugin{})) })
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			t.Run("video resolution mapping saves and validates final models", func(t *testing.T) {
				const public, h3 = "mapped-video", "inmind/video/minimax-h3-2"
				channel := taskVideoBindingTestChannel(public, "per_second")
				channel.ModelMapping = nil
				settings := channel.GetSetting()
				small, large := settings.TaskPluginModelConfigs["vendor-model"], settings.TaskPluginModelConfigs["vendor-model"]
				small.Resolutions = []string{"480p"}
				large.Resolutions = []string{"768p", "2k"}
				settings.TaskPluginModelConfigs = map[string]dto.TaskVideoModelConfig{"small": small, h3: large}
				settings.VideoResolutionModelMapping = dto.VideoResolutionModelMapping{public: {"480p": "small", "720p": h3, "2k": h3}}
				channel.SetSetting(settings)
				require.NoError(t, channel.Insert())
				t.Cleanup(func() { require.NoError(t, channel.Delete()) })
				var loaded Channel
				require.NoError(t, db.First(&loaded, channel.Id).Error)
				assert.JSONEq(t, *channel.Setting, *loaded.Setting)
				catalog, err := taskVideoModelCatalog([]Channel{loaded}, "huahuo-video")
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"480p", "720p", "2k"}, catalog[public].Resolutions)
				assert.Equal(t, []string{"480p"}, catalog["small"].Resolutions)
				assert.Equal(t, []string{"768p", "2k"}, catalog[h3].Resolutions)
				upstream, config, err := loaded.TaskVideoResolutionModelConfig(public, "720p")
				require.NoError(t, err)
				assert.Equal(t, h3, upstream)
				assert.Equal(t, large, config)
				_, _, err = loaded.TaskVideoResolutionModelConfig(public, "1080p")
				require.ErrorContains(t, err, "1080p")
				patch := Channel{Id: loaded.Id}
				settings.VideoResolutionModelMapping[public]["480p"] = h3
				patch.SetSetting(settings)
				require.ErrorContains(t, patch.Update(), "480p")
				settings.VideoResolutionModelMapping[public]["480p"] = "small"
				small.BillingMode = "per_request"
				settings.TaskPluginModelConfigs["small"] = small
				patch.SetSetting(settings)
				require.ErrorContains(t, patch.Update(), "incompatible billing units")
				require.ErrorContains(t, (&Channel{Id: loaded.Id, Models: "different-model"}).Update(), public)
				require.NoError(t, db.First(&loaded, channel.Id).Error)
				assert.JSONEq(t, *channel.Setting, *loaded.Setting)
				ordinary := `{"mapped-video":"inmind/video/minimax-h3-2"}`
				require.NoError(t, (&Channel{Id: loaded.Id, ModelMapping: &ordinary}).Update())
				require.NoError(t, db.First(&loaded, channel.Id).Error)
				catalog, err = taskVideoModelCatalog([]Channel{loaded}, "huahuo-video")
				require.NoError(t, err)
				assert.Equal(t, []string{"768p", "2k"}, catalog[public].Resolutions)
				badOrdinary := `{"mapped-video":"missing-ordinary-model"}`
				require.ErrorContains(t, (&Channel{Id: loaded.Id, ModelMapping: &badOrdinary}).Update(), "missing-ordinary-model")
			})
			t.Run("batch insert associates generated channel IDs with abilities", func(t *testing.T) {
				channels := make([]Channel, 51)
				for i := range channels {
					channels[i] = Channel{
						Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
						Name: fmt.Sprintf("batch-channel-%d", i), Key: "test-key",
						Models: fmt.Sprintf("batch-model-%d", i), Group: "batch-a,batch-b",
					}
				}
				require.NoError(t, BatchInsertChannels(channels))
				var saved []Channel
				require.NoError(t, db.Where("name LIKE ?", "batch-channel-%").Find(&saved).Error)
				require.Len(t, saved, len(channels))
				ids := make([]int, 0, len(saved))
				for _, channel := range saved {
					require.NotZero(t, channel.Id)
					ids = append(ids, channel.Id)
					var abilities []Ability
					require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
					require.Len(t, abilities, 2)
					for _, ability := range abilities {
						assert.Equal(t, channel.Models, ability.Model)
						assert.Contains(t, []string{"batch-a", "batch-b"}, ability.Group)
						assert.True(t, ability.Enabled)
					}
					assert.NotEqual(t, abilities[0].Group, abilities[1].Group)
				}
				var orphanCount int64
				require.NoError(t, db.Model(&Ability{}).Where("channel_id = ? AND model LIKE ?", 0, "batch-model-%").Count(&orphanCount).Error)
				assert.Zero(t, orphanCount)
				require.NoError(t, db.Where("channel_id IN ?", ids).Delete(&Ability{}).Error)
				require.NoError(t, db.Where("id IN ?", ids).Delete(&Channel{}).Error)
			})
			t.Run("disabled preset catalog", func(t *testing.T) {
				previousRegistry := jsplugin.DefaultRegistry
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
				source, err := os.ReadFile(filepath.Join("..", "plugins", "tasks", "wolong", "plugin.js"))
				require.NoError(t, err)
				_, err = jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: "wolong"})
				require.NoError(t, err)
				channel := Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "preset-test", Name: "disabled-preset-test", Group: "default", Models: "haya-mo-seedance-25"}
				channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "wolong"})
				require.NoError(t, channel.Insert())
				t.Cleanup(func() { require.NoError(t, channel.Delete()) })
				jsplugin.DefaultRegistry.SetDisabledFactoryKeys([]string{"wolong"})
				for _, overridden := range []bool{false, true} {
					if overridden {
						override := strings.ReplaceAll(string(source), `defaultAspectRatio: "16:9"`, `defaultAspectRatio: "9:16"`)
						_, err = jsplugin.DefaultRegistry.Register(override, jsplugin.Options{Key: "wolong"})
						require.NoError(t, err)
						jsplugin.DefaultRegistry.SetEnabled(false)
					}
					_, active := jsplugin.DefaultRegistry.Get("wolong")
					require.False(t, active)
					require.NoError(t, channel.ValidateTaskVideoModelBindings())
					channel.Name += "-updated"
					require.NoError(t, channel.Update())
					snapshot, err := GetTaskPluginSyncSnapshot()
					require.NoError(t, err)
					config, found := snapshot.VideoModels["wolong"][channel.Models]
					require.True(t, found)
					expected := "16:9"
					if overridden {
						expected = "9:16"
					}
					assert.Equal(t, expected, config.AspectRatios[0])
					assert.Equal(t, channel.Models, config.PresetModel)
					_, active = jsplugin.DefaultRegistry.Get("wolong")
					assert.False(t, active, "catalog reads must not reenable execution")
				}

				// A newly started node has not registered the saved override yet.
				jsplugin.DefaultRegistry = jsplugin.NewRegistry()
				_, err = jsplugin.DefaultRegistry.RegisterFactory(string(source), jsplugin.Options{Key: "wolong"})
				require.NoError(t, err)
				jsplugin.DefaultRegistry.SetDisabledFactoryKeys([]string{"wolong"})
				overrideSource := strings.ReplaceAll(string(source), "haya-mo-seedance-25", "future-override-only")
				compiled, err := jsplugin.CompilePlugin(overrideSource, jsplugin.Options{Key: "wolong"})
				require.NoError(t, err)
				stored := TaskPlugin{Key: "wolong", APIVersion: 1, Version: compiled.Meta.Version, Source: overrideSource, SourceHash: compiled.Engine.SourceFingerprint(), Enabled: true, Active: true}
				require.NoError(t, db.Create(&stored).Error)
				t.Cleanup(func() { require.NoError(t, db.Delete(&stored).Error) })
				channel.Models = "future-override-only"
				require.NoError(t, db.Model(&channel).Update("models", channel.Models).Error)
				snapshot, err := GetTaskPluginSyncSnapshot()
				require.NoError(t, err)
				assert.Equal(t, channel.Models, snapshot.VideoModels["wolong"][channel.Models].PresetModel)
				assert.Empty(t, jsplugin.DefaultRegistry.OverridePlugins(), "snapshot must not publish the desired override")
				_, active := jsplugin.DefaultRegistry.Get("wolong")
				assert.False(t, active)

				// An invalid replacement retains the registered incumbent contract.
				_, err = jsplugin.DefaultRegistry.Register(overrideSource, jsplugin.Options{Key: "wolong"})
				require.NoError(t, err)
				jsplugin.DefaultRegistry.SetEnabled(false)
				require.NoError(t, db.Model(&stored).Updates(map[string]any{"source": "invalid javascript", "source_hash": "invalid"}).Error)
				snapshot, err = GetTaskPluginSyncSnapshot()
				require.NoError(t, err)
				assert.Equal(t, channel.Models, snapshot.VideoModels["wolong"][channel.Models].PresetModel)
			})

			for _, writes := range [][2]string{{"batch", "batch"}, {"insert", "update"}, {"insert", "tag"}, {"update", "tag"}, {"batch", "upstream sync"}} {
				t.Run(writes[0]+" against "+writes[1], func(t *testing.T) {
					name := writes[0] + "-" + writes[1]
					writers := make([]func() error, 0, 2)
					for index, operation := range writes {
						unit := []string{"per_second", "per_request"}[index]
						candidate := taskVideoBindingTestChannel(name, unit)
						require.NoError(t, candidate.ValidateTaskVideoModelBindings(), "both preflight checks succeed before either writer")
						switch operation {
						case "batch":
							writers = append(writers, func() error { return BatchInsertChannels([]Channel{candidate}) })
						case "insert":
							writers = append(writers, func() error { return candidate.Insert() })
						case "update", "tag", "upstream sync":
							existing := taskVideoBindingTestChannel(fmt.Sprintf("%s-before-%d", name, index), unit)
							existing.Tag = common.GetPointer(existing.Name)
							mapping, err := common.Marshal(map[string]string{existing.Models: "vendor-model", candidate.Models: "vendor-model"})
							require.NoError(t, err)
							existing.ModelMapping = common.GetPointer(string(mapping))
							require.NoError(t, existing.Insert())
							if operation == "update" {
								patch := Channel{Id: existing.Id, Models: candidate.Models, ModelMapping: candidate.ModelMapping}
								writers = append(writers, func() error { return patch.Update() })
							} else if operation == "upstream sync" {
								existing.Models = candidate.Models
								writers = append(writers, func() error { return existing.UpdateUpstreamModelSettings(dto.ChannelOtherSettings{}, true) })
							} else {
								writers = append(writers, func() error {
									return EditChannelByTag(*existing.Tag, nil, candidate.ModelMapping, &candidate.Models, nil, nil, nil, nil, nil)
								})
							}
						}
					}
					start, ready, results := make(chan struct{}), make(chan struct{}, 2), make(chan error, 2)
					var workers sync.WaitGroup
					for _, write := range writers {
						workers.Go(func() { ready <- struct{}{}; <-start; results <- write() })
					}
					<-ready
					<-ready
					close(start)
					workers.Wait()
					errorsSeen := []error{<-results, <-results}
					succeeded := 0
					for _, err := range errorsSeen {
						if err == nil {
							succeeded++
						} else {
							assert.ErrorContains(t, err, "incompatible billing units")
						}
					}
					assert.Equal(t, 1, succeeded)
					var channels, abilities int64
					require.NoError(t, db.Model(&Channel{}).Where("models = ?", name).Count(&channels).Error)
					require.NoError(t, db.Model(&Ability{}).Where("model = ?", name).Count(&abilities).Error)
					assert.EqualValues(t, 1, channels)
					assert.EqualValues(t, 1, abilities)
					_, err := GetTaskPluginSyncSnapshot()
					require.NoError(t, err)
				})
			}

			t.Run("batch conflict rolls back every channel and ability", func(t *testing.T) {
				batch := []Channel{taskVideoBindingTestChannel("batch-conflict", "per_second"), taskVideoBindingTestChannel("batch-conflict", "per_request")}
				require.ErrorContains(t, BatchInsertChannels(batch), "incompatible billing units")
				var count int64
				require.NoError(t, db.Model(&Channel{}).Where("models = ?", "batch-conflict").Count(&count).Error)
				assert.Zero(t, count)
				require.NoError(t, db.Model(&Ability{}).Where("model = ?", "batch-conflict").Count(&count).Error)
				assert.Zero(t, count)
			})

			for _, field := range []string{"setting", "model_mapping", "type"} {
				t.Run("partial update "+field, func(t *testing.T) {
					name := "partial-" + field
					first := taskVideoBindingTestChannel(name, "per_second")
					require.NoError(t, first.Insert())
					second := taskVideoBindingTestChannel(name, "per_second")
					if field == "type" {
						second = taskVideoBindingTestChannel(name, "per_request")
						second.Type = constant.ChannelTypeOpenAI
					}
					require.NoError(t, second.Insert())
					patch := Channel{Id: second.Id, Name: "must-not-persist"}
					switch field {
					case "setting":
						conflicting := taskVideoBindingTestChannel(name, "per_request")
						patch.Setting = conflicting.Setting
					case "model_mapping":
						mapping, err := common.Marshal(map[string]string{name: "alternate"})
						require.NoError(t, err)
						patch.ModelMapping = common.GetPointer(string(mapping))
					case "type":
						patch.Type = constant.ChannelTypeTaskPlugin
					}
					require.ErrorContains(t, patch.Update(), "incompatible billing units")
					var loaded Channel
					require.NoError(t, db.First(&loaded, second.Id).Error)
					assert.Equal(t, second, loaded)
					valid := Channel{Id: second.Id, Name: "renamed"}
					require.NoError(t, valid.Update())
					assert.Equal(t, "renamed", valid.Name)
					assert.Equal(t, second.Models, valid.Models)
					assert.Equal(t, second.Setting, valid.Setting)
				})
			}

			t.Run("tag members are validated together", func(t *testing.T) {
				for index, unit := range []string{"per_second", "per_request"} {
					channel := taskVideoBindingTestChannel(fmt.Sprintf("tag-before-%d", index), unit)
					channel.Tag = common.GetPointer("tag-conflict")
					mapping, err := common.Marshal(map[string]string{channel.Models: "vendor-model", "tag-shared": "vendor-model"})
					require.NoError(t, err)
					channel.ModelMapping = common.GetPointer(string(mapping))
					require.NoError(t, channel.Insert())
				}
				require.ErrorContains(t, EditChannelByTag("tag-conflict", common.GetPointer("must-not-persist"), nil, common.GetPointer("tag-shared"), nil, nil, nil, nil, nil), "incompatible billing units")
				var channels []Channel
				require.NoError(t, db.Where("tag = ?", "tag-conflict").Order("id").Find(&channels).Error)
				require.Len(t, channels, 2)
				assert.Equal(t, "tag-before-0", channels[0].Models)
				assert.Equal(t, "tag-before-1", channels[1].Models)
			})

			t.Run("upstream sync validates live configuration and clears models", func(t *testing.T) {
				peer := taskVideoBindingTestChannel("upstream-shared", "per_second")
				require.NoError(t, peer.Insert())
				channel := taskVideoBindingTestChannel("upstream-before", "per_request")
				channel.Type = constant.ChannelTypeOpenAI
				mapping := `{"upstream-before":"vendor-model","upstream-shared":"vendor-model","upstream-compatible":"vendor-model"}`
				channel.ModelMapping = &mapping
				require.NoError(t, channel.Insert())
				stale := channel
				live := Channel{Id: channel.Id, Type: constant.ChannelTypeTaskPlugin, Name: "current-name"}
				require.NoError(t, live.Update())
				stale.Models = "upstream-shared"
				settings := dto.ChannelOtherSettings{UpstreamModelUpdateLastDetectedModels: []string{"upstream-compatible"}}
				require.ErrorContains(t, stale.UpdateUpstreamModelSettings(settings, true), "incompatible billing units")
				var unchanged Channel
				require.NoError(t, db.First(&unchanged, channel.Id).Error)
				assert.Equal(t, live, unchanged, "failed synchronization must preserve live fields and pending settings")
				stale.Models = "upstream-compatible"
				require.NoError(t, stale.UpdateUpstreamModelSettings(settings, true))
				assert.Equal(t, "current-name", stale.Name)
				assert.Equal(t, constant.ChannelTypeTaskPlugin, stale.Type)
				assert.Equal(t, live.ModelMapping, stale.ModelMapping)
				assert.Equal(t, settings, stale.GetOtherSettings())
				var abilities []Ability
				require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilities).Error)
				require.Len(t, abilities, 1)
				assert.Equal(t, "upstream-compatible", abilities[0].Model)
				stale.Models = ""
				require.NoError(t, stale.UpdateUpstreamModelSettings(dto.ChannelOtherSettings{}, true))
				var cleared Channel
				require.NoError(t, db.First(&cleared, channel.Id).Error)
				assert.Empty(t, cleared.Models)
				var count int64
				require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", channel.Id).Count(&count).Error)
				assert.Zero(t, count)
			})

			t.Run("ability failure rolls back channel writes", func(t *testing.T) {
				existing := taskVideoBindingTestChannel("ability-original", "per_second")
				existing.Tag = common.GetPointer("ability-tag")
				require.NoError(t, existing.Insert())
				var beforeChannels []Channel
				var beforeAbilities []Ability
				require.NoError(t, db.Order("id").Find(&beforeChannels).Error)
				require.NoError(t, db.Order("channel_id, model").Find(&beforeAbilities).Error)
				failure := errors.New("ability storage unavailable")
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("video-binding-ability-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "video_binding_abilities" {
						tx.AddError(failure)
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove("video-binding-ability-failure")) })
				candidate := taskVideoBindingTestChannel("ability-new", "per_second")
				require.ErrorIs(t, candidate.Insert(), failure)
				candidate.Id = 0
				require.ErrorIs(t, BatchInsertChannels([]Channel{candidate}), failure)
				patch := Channel{Id: existing.Id, Models: candidate.Models, ModelMapping: candidate.ModelMapping}
				require.ErrorIs(t, patch.Update(), failure)
				require.ErrorContains(t, EditChannelByTag(*existing.Tag, nil, candidate.ModelMapping, &candidate.Models, nil, nil, nil, nil, nil), failure.Error())
				require.ErrorIs(t, existing.UpdateUpstreamModelSettings(dto.ChannelOtherSettings{UpstreamModelUpdateLastDetectedModels: []string{"not-committed"}}, true), failure)
				var afterChannels []Channel
				var afterAbilities []Ability
				require.NoError(t, db.Order("id").Find(&afterChannels).Error)
				require.NoError(t, db.Order("channel_id, model").Find(&afterAbilities).Error)
				assert.Equal(t, beforeChannels, afterChannels)
				assert.Equal(t, beforeAbilities, afterAbilities)
			})
			_, err = GetTaskPluginSyncSnapshot()
			require.NoError(t, err)
		})
	}
}

func TestSudashuiSettingsAndTaskSnapshotDatabaseRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "sudashui.db"))
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			channels, tasks := db.Table("sudashui_config_channels").Session(&gorm.Session{}), db.Table("sudashui_config_tasks").Session(&gorm.Session{})
			require.NoError(t, channels.AutoMigrate(&Channel{}))
			require.NoError(t, tasks.AutoMigrate(&Task{}))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable("sudashui_config_tasks", "sudashui_config_channels"))
			})
			images, videos, audios := 3, 0, 0
			config := dto.TaskVideoModelConfig{Duration: dto.TaskVideoDuration{Values: []int{4, 8}}, Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"}, Modes: []string{"text_to_video", "reference_to_video"}, References: dto.TaskVideoReferences{Images: &images, Videos: &videos, Audios: &audios}, BillingMode: "per_request"}
			channel := Channel{Type: constant.ChannelTypeTaskPlugin, Key: "fixture", Models: "future-video"}
			channel.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelSourceURL: dto.DefaultSudashuiCatalogURL, TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-video": config}})
			require.NoError(t, channel.ValidateSettings())
			require.NoError(t, channels.Create(&channel).Error)
			task := Task{TaskID: "task_sudashui_snapshot", Platform: "sudashui", PrivateData: TaskPrivateData{Execution: &TaskExecutionSnapshot{TaskPlugin: &TaskPluginSnapshot{Key: "sudashui", Version: "1.0.0"}, VideoModelConfig: &config}}}
			require.NoError(t, tasks.Create(&task).Error)
			// Existing tables and JSON data survive a second startup/migration.
			require.NoError(t, channels.AutoMigrate(&Channel{}))
			require.NoError(t, tasks.AutoMigrate(&Task{}))
			var loaded Channel
			require.NoError(t, channels.First(&loaded, channel.Id).Error)
			assert.Equal(t, channel.GetSetting(), loaded.GetSetting())
			changed := config
			changed.BillingMode, changed.Duration = "per_second", dto.TaskVideoDuration{Values: []int{4}}
			loaded.SetSetting(dto.ChannelSettings{TaskPluginKey: "sudashui", TaskPluginModelConfigs: map[string]dto.TaskVideoModelConfig{"future-video": changed}})
			require.NoError(t, channels.Where("id = ?", channel.Id).Update("setting", loaded.Setting).Error)
			var frozen Task
			require.NoError(t, tasks.First(&frozen, task.ID).Error)
			require.NotNil(t, frozen.PrivateData.Execution.VideoModelConfig)
			assert.Equal(t, config, *frozen.PrivateData.Execution.VideoModelConfig)
			var version string
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
		})
	}
}

func TestChannelOtherSettingsReadOnly(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "settings.db"))
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(os.Getenv("TEST_POSTGRES_DSN"))
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "readonly_settings_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			previousDB, previousType := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() { DB = previousDB; common.SetMainDatabaseType(previousType) })
			require.NoError(t, db.AutoMigrate(&Channel{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&Channel{})) })
			query := "SELECT version()"
			if dialect == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			for _, tc := range []struct {
				name, settings string
				channelType    int
				want           dto.ChannelOtherSettings
			}{
				{name: "malformed JSON", channelType: constant.ChannelTypeOpenAI, settings: `{"disable_task_polling_sleep":`},
				{name: "valid fields survive a field type error", channelType: constant.ChannelTypeOpenAI, settings: `{"disable_task_polling_sleep":true,"upstream_model_update_last_check_time":"invalid"}`, want: dto.ChannelOtherSettings{DisableTaskPollingSleep: true}},
				{name: "empty settings", channelType: constant.ChannelTypeOpenAI},
				{name: "valid settings", channelType: constant.ChannelTypeOpenAI, settings: `{"disable_task_polling_sleep":true,"upstream_model_update_last_detected_models":["pending-model"]}`, want: dto.ChannelOtherSettings{DisableTaskPollingSleep: true, UpstreamModelUpdateLastDetectedModels: []string{"pending-model"}}},
				{name: "malformed settings retain preset", channelType: constant.ChannelTypeVLLM, settings: `{"advanced_custom":`, want: dto.ChannelOtherSettings{AdvancedCustom: common.GetAdvancedCustomPreset(constant.ChannelTypeVLLM)}},
				{name: "preset overrides stored custom routes", channelType: constant.ChannelTypeSGLang, settings: `{"disable_task_polling_sleep":true,"advanced_custom":{"routes":[]}}`, want: dto.ChannelOtherSettings{DisableTaskPollingSleep: true, AdvancedCustom: common.GetAdvancedCustomPreset(constant.ChannelTypeSGLang)}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					channel := Channel{Type: tc.channelType, Name: tc.name, Key: "preserved-key", Status: common.ChannelStatusEnabled, Models: "public-model", Group: "default", UsedQuota: 37, OtherSettings: tc.settings, Setting: common.GetPointer(`{"proxy":"https://proxy.example"}`), ModelMapping: common.GetPointer(`{"public-model":"upstream-model"}`)}
					require.NoError(t, db.Create(&channel).Error)
					var before Channel
					require.NoError(t, db.First(&before, channel.Id).Error)
					var projected Channel
					require.NoError(t, db.Select("id", "type", "settings").First(&projected, channel.Id).Error)
					snapshot := projected
					assert.Equal(t, tc.want, projected.GetOtherSettings())
					assert.Equal(t, snapshot, projected, "reading settings must not mutate the caller's snapshot")
					var after Channel
					require.NoError(t, db.First(&after, channel.Id).Error)
					assert.Equal(t, before, after, "reading projected settings must preserve every persisted field")
				})
			}
		})
	}
}

func TestChannelValidateSettingsRejectsInvalidHTTPTransport(t *testing.T) {
	tests := []struct {
		name    string
		setting dto.ChannelSettings
		wantErr string
	}{
		{
			name:    "auto with shards is valid",
			setting: dto.ChannelSettings{HTTPProtocol: "auto", HTTP2ConnectionShards: 4},
		},
		{
			name:    "http1 with shards greater than one rejected",
			setting: dto.ChannelSettings{HTTPProtocol: "http1", HTTP2ConnectionShards: 2},
			wantErr: "http2_connection_shards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{}
			channel.SetSetting(tt.setting)
			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestAdvancedCustomChannelRequiresModelListRouteOnlyWhenUpdateChecksEnabled(t *testing.T) {
	inferenceRoute := dto.AdvancedCustomRoute{
		IncomingPath: "/v1/chat/completions",
		UpstreamPath: "/v1/chat/completions",
		Converter:    "none",
	}

	tests := []struct {
		name          string
		checksEnabled bool
		routes        []dto.AdvancedCustomRoute
		wantErr       string
	}{
		{
			name:   "legacy channel without discovery route remains valid",
			routes: []dto.AdvancedCustomRoute{inferenceRoute},
		},
		{
			name:          "enabled checks require discovery route",
			checksEnabled: true,
			routes:        []dto.AdvancedCustomRoute{inferenceRoute},
			wantErr:       dto.AdvancedCustomModelListPath,
		},
		{
			name:          "enabled checks accept discovery route",
			checksEnabled: true,
			routes: []dto.AdvancedCustomRoute{
				inferenceRoute,
				{
					IncomingPath: dto.AdvancedCustomModelListPath,
					UpstreamPath: dto.AdvancedCustomModelListPath,
					Converter:    "none",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{Type: constant.ChannelTypeAdvancedCustom}
			channel.SetOtherSettings(dto.ChannelOtherSettings{
				UpstreamModelUpdateCheckEnabled: tt.checksEnabled,
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: tt.routes,
				},
			})

			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestInferencePresetSettingsAndDatabaseRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "presets.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			table := db.Table("inference_preset_channels").Session(&gorm.Session{})
			require.NoError(t, table.AutoMigrate(&Channel{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable("inference_preset_channels")) })
			var version string
			if dialect == "sqlite" {
				require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
			} else {
				require.NoError(t, db.Raw("select version()").Scan(&version).Error)
			}
			t.Logf("%s version: %s", dialect, version)
			for _, channelType := range []int{constant.ChannelTypeVLLM, constant.ChannelTypeSGLang} {
				t.Run(fmt.Sprint(channelType), func(t *testing.T) {
					channel := &Channel{Type: channelType, Key: "EMPTY", Name: "inference", Status: common.ChannelStatusEnabled}
					require.NoError(t, channel.ValidateSettings())
					require.NotNil(t, channel.GetOtherSettings().AdvancedCustom)
					require.NoError(t, table.Create(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						assert.Equal(t, channelType, loaded.Type)
						assert.Empty(t, loaded.OtherSettings)
						defaults := loaded.GetOtherSettings().AdvancedCustom
						require.NotNil(t, defaults)
						assert.True(t, defaults.SupportsPath("/v1/messages"))
						assert.Empty(t, loaded.OtherSettings, "reading defaults must not rewrite saved settings")
						defaults.Routes[0].UpstreamPath = "/changed-locally"
						assert.Equal(t, "/v1/chat/completions", loaded.GetOtherSettings().AdvancedCustom.Routes[0].UpstreamPath)
					}
					settings := dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/chat/completions", UpstreamPath: "/custom/chat", Models: []string{"allowed"}, Auth: &dto.AdvancedCustomRouteAuth{Type: "none"}}}}}
					channel.SetOtherSettings(settings)
					require.NoError(t, channel.ValidateSettings())
					require.NoError(t, table.Save(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						actual := loaded.GetOtherSettings().AdvancedCustom
						require.Equal(t, common.GetAdvancedCustomPreset(channelType), actual, "named channels must ignore editable advanced_custom overrides")
						assert.Equal(t, channel.OtherSettings, loaded.OtherSettings)
						for _, tc := range []struct {
							path, model string
							allowed     bool
						}{
							{"/v1/chat/completions", "allowed", true},
							{"/v1/chat/completions", "other", true},
							{"/v1/messages", "allowed", true},
							{"/v1/images/generations", "allowed", false},
						} {
							ok, _ := ChannelSatisfiesFilters(&loaded, tc.model, []filterdto.ChannelFilter{{Kind: filterdto.FilterRequestPath, RequestPath: tc.path}})
							assert.Equal(t, tc.allowed, ok)
						}
						endpoints := getPricingEndpointTypesForAbility(AbilityWithChannel{ChannelType: channelType, Ability: Ability{Model: "allowed", ChannelId: loaded.Id}}, map[int]*dto.AdvancedCustomConfig{loaded.Id: actual})
						expectedEndpoints := []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse, constant.EndpointTypeAnthropic, constant.EndpointTypeEmbeddings}
						if channelType == constant.ChannelTypeSGLang {
							expectedEndpoints = append(expectedEndpoints, constant.EndpointTypeJinaRerank)
						}
						assert.ElementsMatch(t, expectedEndpoints, endpoints)
					}
				})
			}
		})
	}
}

func TestUpstreamAsyncSettingsDatabaseRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "upstream-async.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			versionQuery := map[string]string{
				"sqlite":   "SELECT sqlite_version()",
				"mysql":    "SELECT VERSION()",
				"postgres": "SHOW server_version",
			}[dialect]
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("%s version: %s", dialect, version)
			table := db.Table("upstream_async_channels").Session(&gorm.Session{})
			require.NoError(t, table.AutoMigrate(&Channel{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable("upstream_async_channels")) })

			var settings dto.ChannelOtherSettings
			require.NoError(t, common.UnmarshalJsonStr(`{"upstream_async":{"profiles":[{"id":"vendor-video","media_type":"video","models":["mapped-video"],"operations":["generate"],"submit":{"task_id_path":"data.task_id","request":{"method":"POST","path":"/videos","query":{"source":"studio"},"headers":{"Authorization":"Bearer {api_key}"},"body":{"model":"{upstream_model}","prompt":"{request.prompt}"}}},"poll":{"request":{"method":"POST","path":"/tasks/{task_id}","headers":{"Authorization":"Bearer {api_key}"},"body":{"model":"{upstream_model}"}},"response":{"status_path":"data.status","status_values":{"queued":["queued"],"succeeded":[true],"failed":[false]},"result_path":"data.output.url","actual_seconds_path":"data.output.duration","usage_paths":{"total_tokens":"usage.total_tokens"}}}}]}}`, &settings))
			channel := &Channel{Type: constant.ChannelTypeOpenAI, Key: "secret", Name: "dynamic", Status: common.ChannelStatusEnabled}
			channel.SetOtherSettings(settings)
			require.NoError(t, channel.ValidateSettings())
			require.NoError(t, table.Create(channel).Error)

			var loaded Channel
			require.NoError(t, table.First(&loaded, channel.Id).Error)
			profile, matched := loaded.GetOtherSettings().UpstreamAsync.Match("video", "mapped-video", "generate")
			require.True(t, matched)
			require.NotNil(t, profile)
			assert.Equal(t, "vendor-video", profile.ID)
			require.NotNil(t, profile.Submit.Request)
			assert.Equal(t, "/videos", profile.Submit.Request.Path)
			assert.Equal(t, "studio", profile.Submit.Request.Query["source"])
			assert.Equal(t, "{request.prompt}", profile.Submit.Request.Body.(map[string]any)["prompt"])
			assert.Equal(t, "Bearer {api_key}", profile.Poll.Request.Headers["Authorization"])
			assert.Equal(t, "data.output.duration", profile.Poll.Response.ActualSecondsPath)
			profile.Submit.Request.Headers["Authorization"] = "changed"
			profile.Poll.Request.Headers["Authorization"] = "changed"
			fresh, matched := loaded.GetOtherSettings().UpstreamAsync.Match("video", "mapped-video", "generate")
			require.True(t, matched)
			assert.Equal(t, "Bearer {api_key}", fresh.Poll.Request.Headers["Authorization"])
			assert.Equal(t, "Bearer {api_key}", fresh.Submit.Request.Headers["Authorization"])
			assert.Equal(t, "data.output.duration", fresh.Poll.Response.ActualSecondsPath)
			for _, mode := range []string{"inline", "passthrough", ""} {
				loaded.Type = constant.ChannelTypeXai
				loaded.SetSetting(dto.ChannelSettings{VideoReferenceTransportMode: mode})
				require.NoError(t, loaded.ValidateSettings())
				require.NoError(t, table.Save(&loaded).Error)
				var roundTrip Channel
				require.NoError(t, table.First(&roundTrip, loaded.Id).Error)
				assert.Equal(t, mode, roundTrip.GetSetting().VideoReferenceTransportMode)
			}
		})
	}
}

func TestVideoReferenceTransportChannelValidation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kind         int
		plugin, mode string
		valid        bool
	}{
		{"legacy", constant.ChannelTypeOpenAI, "", "", true},
		{"passthrough", constant.ChannelTypeOpenAI, "", "passthrough", true},
		{"xai inline", constant.ChannelTypeXai, "", "inline", true},
		{"xai plugin", constant.ChannelTypeTaskPlugin, "xai", "inline", true},
		{"drama unsupported", constant.ChannelTypeTaskPlugin, "drama", "inline", false},
		{"non xai", constant.ChannelTypeOpenAI, "xai", "inline", false},
		{"invalid mode", constant.ChannelTypeXai, "", "local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := Channel{Type: tc.kind}
			channel.SetSetting(dto.ChannelSettings{TaskPluginKey: tc.plugin, VideoReferenceTransportMode: tc.mode})
			if tc.valid {
				require.NoError(t, channel.ValidateSettings())
			} else {
				require.ErrorContains(t, channel.ValidateSettings(), "video_reference_transport_mode")
			}
		})
	}
}
