package dto

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
)

// TaskVideoModelConfig is channel-owned capability data, keyed by upstream model.
// Nil reference counts mean unknown and must be completed before saving.
type TaskVideoModelConfig struct {
	Duration     TaskVideoDuration   `json:"duration"`
	Resolutions  []string            `json:"resolutions"`
	AspectRatios []string            `json:"aspect_ratios"`
	Modes        []string            `json:"modes"`
	References   TaskVideoReferences `json:"references"`
	BillingMode  string              `json:"billing_mode"`
}

type TaskVideoDuration struct {
	Min     int   `json:"min,omitempty"`
	Max     int   `json:"max,omitempty"`
	Step    int   `json:"step,omitempty"`
	Values  []int `json:"values,omitempty"`
	Default int   `json:"default,omitempty"`
}

type TaskVideoReferences struct {
	Images *int `json:"images"`
	Videos *int `json:"videos"`
	Audios *int `json:"audios"`
}

const DefaultSudashuiCatalogURL = "https://catalog.sudashuiapi.com/v1/models"

func (c TaskVideoModelConfig) Validate(maxSeconds int) error {
	if c.BillingMode != "per_second" && c.BillingMode != "per_request" {
		return fmt.Errorf("billing_mode must be per_second or per_request")
	}
	d := c.Duration
	if len(d.Values) > 0 {
		if d.Min != 0 || d.Max != 0 || d.Step != 0 || len(d.Values) > maxSeconds {
			return fmt.Errorf("duration requires values or min/max/step")
		}
		seen := map[int]bool{}
		for _, v := range d.Values {
			if v < 1 || v > maxSeconds {
				return fmt.Errorf("duration exceeds host limits")
			}
			if seen[v] {
				return fmt.Errorf("duplicate duration value")
			}
			seen[v] = true
		}
	} else if d.Min < 1 || d.Max < d.Min || d.Max > maxSeconds || d.Step < 1 || d.Step > maxSeconds {
		return fmt.Errorf("duration requires valid min/max/step or values")
	}
	if d.Default != 0 && !c.AllowsDuration(d.Default) {
		return fmt.Errorf("invalid default duration")
	}
	if len(c.Resolutions) == 0 || len(c.Resolutions) > 32 || len(c.AspectRatios) == 0 || len(c.AspectRatios) > 16 || len(c.Modes) == 0 || len(c.Modes) > 4 {
		return fmt.Errorf("resolutions, aspect_ratios and modes are required")
	}
	for _, resolution := range c.Resolutions {
		if resolution == "" || len(resolution) > 32 || strings.ContainsFunc(resolution, unicode.IsSpace) || strings.ContainsFunc(resolution, unicode.IsControl) {
			return fmt.Errorf("invalid resolution")
		}
	}
	for _, ratio := range c.AspectRatios {
		if !slices.Contains([]string{"1:1", "3:4", "4:3", "9:16", "16:9", "21:9", "adaptive"}, ratio) {
			return fmt.Errorf("invalid aspect ratio: %s", ratio)
		}
	}
	for _, n := range []*int{c.References.Images, c.References.Videos, c.References.Audios} {
		if n == nil || *n < 0 || *n > 64 {
			return fmt.Errorf("reference limits must be specified between 0 and 64")
		}
	}
	for _, mode := range c.Modes {
		if !slices.Contains([]string{"text_to_video", "image_to_video", "reference_to_video", "first_last_frame"}, mode) {
			return fmt.Errorf("invalid mode: %s", mode)
		}
		if mode == "image_to_video" && *c.References.Images < 1 {
			return fmt.Errorf("image_to_video requires image support")
		}
		if mode == "reference_to_video" && *c.References.Images+*c.References.Videos+*c.References.Audios == 0 {
			return fmt.Errorf("reference_to_video requires reference support")
		}
	}
	return nil
}

func (c TaskVideoModelConfig) AllowsDuration(seconds int) bool {
	d := c.Duration
	if len(d.Values) > 0 {
		return slices.Contains(d.Values, seconds)
	}
	return d.Step > 0 && seconds >= d.Min && seconds <= d.Max && (seconds-d.Min)%d.Step == 0
}

func (s *ChannelSettings) ValidateTaskVideoModels(maxSeconds int) error {
	if s.TaskPluginModelSourceURL == "" && len(s.TaskPluginModelConfigs) == 0 {
		return nil
	}
	if s.TaskPluginKey != "sudashui" {
		return fmt.Errorf("dynamic model settings require the sudashui plugin")
	}
	if s.TaskPluginModelSourceURL != "" {
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
		if err := config.Validate(maxSeconds); err != nil {
			return fmt.Errorf("model %s: %w", name, err)
		}
	}
	return nil
}
