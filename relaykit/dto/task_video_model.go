package dto

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// TaskVideoModelConfig is channel-owned capability data, keyed by upstream model.
// Nil reference counts mean unknown and must be completed before saving.
type TaskVideoModelConfig struct {
	// PresetModel is host-owned provenance, never accepted from or persisted as JSON.
	PresetModel           string                       `json:"-"`
	PresetModels          []string                     `json:"-"`
	CustomModelUnion      bool                         `json:"-"`
	Duration              TaskVideoDuration            `json:"duration"`
	DurationsByResolution map[string]TaskVideoDuration `json:"durations_by_resolution,omitempty"`
	Resolutions           []string                     `json:"resolutions"`
	AspectRatios          []string                     `json:"aspect_ratios"`
	Modes                 []string                     `json:"modes"`
	References            TaskVideoReferences          `json:"references"`
	BillingMode           string                       `json:"billing_mode"`
}

type TaskVideoDuration struct {
	Min     int   `json:"min,omitempty"`
	Max     int   `json:"max,omitempty"`
	Step    int   `json:"step,omitempty"`
	Values  []int `json:"values,omitempty"`
	Default int   `json:"default,omitempty"`
}

type TaskVideoReferences struct {
	Images       *int `json:"images"`
	Videos       *int `json:"videos"`
	Audios       *int `json:"audios"`
	ImageUpload  bool `json:"image_upload,omitempty"`
	ImageDataURI bool `json:"image_data_uri,omitempty"`
}

const DefaultSudashuiCatalogURL = "https://catalog.sudashuiapi.com/v1/models"

var taskVideoResolutionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)

func ChannelConfiguredTaskVideoPlugin(key string) bool {
	return key == "sudashui" || key == "huahuo-video" || PresetTaskVideoPlugin(key)
}

func PresetTaskVideoPlugin(key string) bool {
	switch key {
	case "wolong", "drama", "yali", "tabtab", "nldw", "yuxibox", "tianyue":
		return true
	default:
		return false
	}
}

func ChannelConfiguredTaskVideoPlugins() []string {
	return []string{"sudashui", "huahuo-video", "wolong", "drama", "yali", "tabtab", "nldw", "yuxibox", "tianyue"}
}

func (c TaskVideoModelConfig) Validate(maxSeconds int) error {
	return c.ValidateForPlugin("sudashui", maxSeconds)
}

func (c TaskVideoModelConfig) ValidateForPlugin(pluginKey string, maxSeconds int) error {
	if c.BillingMode != "per_second" && c.BillingMode != "per_request" {
		return fmt.Errorf("billing_mode must be per_second or per_request")
	}
	if err := c.Duration.validate(maxSeconds); err != nil {
		return err
	}
	if len(c.Resolutions) == 0 || len(c.Resolutions) > 32 || len(c.AspectRatios) == 0 || len(c.AspectRatios) > 16 || len(c.Modes) == 0 || len(c.Modes) > 4 {
		return fmt.Errorf("resolutions, aspect_ratios and modes are required")
	}
	for _, resolution := range c.Resolutions {
		if resolution == "" || len(resolution) > 32 || strings.ContainsFunc(resolution, unicode.IsSpace) || strings.ContainsFunc(resolution, unicode.IsControl) {
			return fmt.Errorf("invalid resolution")
		}
		if PresetTaskVideoPlugin(pluginKey) && !taskVideoResolutionPattern.MatchString(resolution) {
			return fmt.Errorf("invalid video resolution")
		}
	}
	resolutions := make(map[string]bool, len(c.Resolutions))
	for _, resolution := range c.Resolutions {
		resolutions[strings.ToLower(resolution)] = true
	}
	allowed := make(map[int]bool, len(c.Duration.Values))
	for _, seconds := range c.Duration.Values {
		allowed[seconds] = true
	}
	for resolution, duration := range c.DurationsByResolution {
		if resolution != strings.ToLower(resolution) || !resolutions[resolution] {
			return fmt.Errorf("duration resolution %q must match a lowercase enabled resolution", resolution)
		}
		if err := duration.validate(maxSeconds); err != nil {
			return fmt.Errorf("duration for %s: %w", resolution, err)
		}
		for _, seconds := range duration.values() {
			if len(allowed) > 0 && !allowed[seconds] || len(allowed) == 0 && !c.AllowsDuration(seconds) {
				return fmt.Errorf("duration for %s exceeds the model duration capability", resolution)
			}
		}
	}
	for _, ratio := range c.AspectRatios {
		valid := slices.Contains([]string{"1:1", "3:4", "4:3", "9:16", "16:9", "21:9", "adaptive"}, ratio)
		if pluginKey == "wolong" && ratio == "adaptive" {
			valid = false
		}
		if PresetTaskVideoPlugin(pluginKey) && (ratio == "auto" || pluginKey == "drama" && ratio == "Auto") {
			valid = true
		}
		if !valid && (pluginKey == "huahuo-video" || PresetTaskVideoPlugin(pluginKey)) {
			left, right, ratioFormat := strings.Cut(ratio, ":")
			valid = ratioFormat && taskVideoRatioComponent(left) && taskVideoRatioComponent(right)
		}
		if !valid {
			return fmt.Errorf("invalid aspect ratio: %s", ratio)
		}
	}
	for _, n := range []*int{c.References.Images, c.References.Videos, c.References.Audios} {
		if n == nil || *n < 0 || *n > 64 {
			return fmt.Errorf("reference limits must be specified between 0 and 64")
		}
	}
	if (c.References.ImageUpload || c.References.ImageDataURI) && *c.References.Images == 0 {
		return fmt.Errorf("image uploads or data URIs require reference image support")
	}
	if PresetTaskVideoPlugin(pluginKey) && (c.References.ImageUpload || c.References.ImageDataURI) {
		return fmt.Errorf("channel-configured video references require public URLs")
	}
	if pluginKey == "yali" && *c.References.Videos > 0 {
		return fmt.Errorf("Yali video does not support reference videos")
	}
	for _, mode := range c.Modes {
		if !slices.Contains([]string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame"}, mode) {
			return fmt.Errorf("invalid mode: %s", mode)
		}
		if (pluginKey == "yali" || pluginKey == "nldw") && mode == "first_last_frame" {
			return fmt.Errorf("%s does not support mode %s", pluginKey, mode)
		}
		if pluginKey == "tianyue" && mode == "first_last_frame" {
			return fmt.Errorf("Tianyue video does not support first_last_frame")
		}
		if mode == "image_to_video" && *c.References.Images < 1 {
			return fmt.Errorf("image_to_video requires image support")
		}
		if PresetTaskVideoPlugin(pluginKey) && mode == "first_last_frame" && *c.References.Images < 2 {
			return fmt.Errorf("first_last_frame requires at least two images")
		}
		if mode == "reference_to_video" && *c.References.Images+*c.References.Videos+*c.References.Audios == 0 {
			return fmt.Errorf("reference_to_video requires reference support")
		}
	}
	return nil
}

func taskVideoRatioComponent(part string) bool {
	n, err := strconv.ParseUint(part, 10, 16)
	return err == nil && n > 0 && n <= 999 && strconv.FormatUint(n, 10) == part
}

func (c TaskVideoModelConfig) AllowsDuration(seconds int) bool {
	return c.Duration.Allows(seconds)
}

func (d TaskVideoDuration) Allows(seconds int) bool {
	if len(d.Values) > 0 {
		return slices.Contains(d.Values, seconds)
	}
	return d.Step > 0 && seconds >= d.Min && seconds <= d.Max && (seconds-d.Min)%d.Step == 0
}

func (d TaskVideoDuration) values() []int {
	if len(d.Values) > 0 {
		return d.Values
	}
	values := make([]int, 0, 1+(d.Max-d.Min)/d.Step)
	for seconds := d.Min; seconds <= d.Max; seconds += d.Step {
		values = append(values, seconds)
	}
	return values
}

func (d TaskVideoDuration) validate(maxSeconds int) error {
	if len(d.Values) > 0 {
		if d.Min != 0 || d.Max != 0 || d.Step != 0 || len(d.Values) > maxSeconds {
			return fmt.Errorf("duration requires values or min/max/step")
		}
		seen := make(map[int]bool, len(d.Values))
		for _, value := range d.Values {
			if value < 1 || value > maxSeconds {
				return fmt.Errorf("duration exceeds host limits")
			}
			if seen[value] {
				return fmt.Errorf("duplicate duration value")
			}
			seen[value] = true
		}
	} else if d.Min < 1 || d.Max < d.Min || d.Max > maxSeconds || d.Step < 1 || d.Step > maxSeconds {
		return fmt.Errorf("duration requires valid min/max/step or values")
	}
	if d.Default != 0 && !d.Allows(d.Default) {
		return fmt.Errorf("invalid default duration")
	}
	return nil
}

func (s *ChannelSettings) ValidateTaskVideoModels(maxSeconds int) error {
	if s.TaskPluginModelSourceURL == "" && len(s.TaskPluginModelConfigs) == 0 {
		return nil
	}
	if !ChannelConfiguredTaskVideoPlugin(s.TaskPluginKey) {
		return fmt.Errorf("dynamic video model settings require a channel-configured video plugin")
	}
	if s.TaskPluginModelSourceURL != "" {
		if s.TaskPluginKey != "sudashui" {
			return fmt.Errorf("the model catalog URL is only supported by the sudashui plugin")
		}
		u, err := url.Parse(s.TaskPluginModelSourceURL)
		if err != nil || u.Hostname() == "" || u.Scheme != "https" && u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return fmt.Errorf("model catalog must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
	if len(s.TaskPluginModelConfigs) > 1000 {
		return fmt.Errorf("too many model configurations")
	}
	seenModels := map[string]bool{}
	for name, config := range s.TaskPluginModelConfigs {
		if name == "" || len(name) > 255 || name != strings.TrimSpace(name) || strings.ContainsAny(name, ",*") || strings.ContainsFunc(name, unicode.IsControl) {
			return fmt.Errorf("invalid upstream model name")
		}
		folded := strings.ToLower(name)
		if seenModels[folded] {
			return fmt.Errorf("model names must be unique ignoring case")
		}
		seenModels[folded] = true
		if s.TaskPluginKey == "sudashui" && (len(config.DurationsByResolution) > 0 || config.References.ImageUpload || config.References.ImageDataURI) {
			return fmt.Errorf("model %s: resolution-specific durations and image upload settings require huahuo-video", name)
		}
		if s.TaskPluginKey == "huahuo-video" && slices.Contains(config.Modes, "first_last_frame") && (config.References.Images == nil || *config.References.Images < 1) {
			return fmt.Errorf("model %s: first_last_frame requires image support", name)
		}
		if err := config.ValidateForPlugin(s.TaskPluginKey, maxSeconds); err != nil {
			return fmt.Errorf("model %s: %w", name, err)
		}
	}
	return nil
}
