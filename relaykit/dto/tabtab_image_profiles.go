package dto

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// TabtabImageModelProfiles maps exact upstream model names to built-in TabTab
// Task API parameter contracts. The template name is never sent upstream.
type TabtabImageModelProfiles map[string]string

var tabtabImageModels = []string{
	"gemini-nano-banana-2.1", "gemini-3-pro-image", "gemini-3.1-flash-image", "gemini-3.1-flash-lite-image",
	"gpt-image-2", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2-r", "gpt-image-2.5-r",
	"seedream-5-pro", "seedream-5.5", "mj",
}

func (profiles TabtabImageModelProfiles) Template(model string) string {
	if slices.Contains(tabtabImageModels, model) {
		return model
	}
	if template := profiles[model]; slices.Contains(tabtabImageModels, template) {
		return template
	}
	return ""
}

func (profiles TabtabImageModelProfiles) SupportedModels() []string {
	models := slices.Clone(tabtabImageModels)
	for model := range profiles {
		if template := profiles.Template(model); template != "" && template != model {
			models = append(models, model)
		}
	}
	slices.Sort(models[len(tabtabImageModels):])
	return models
}

func (profiles TabtabImageModelProfiles) Validate() error {
	if len(profiles) > 128 {
		return fmt.Errorf("tabtab_image_model_profiles supports at most 128 entries")
	}
	for model, template := range profiles {
		if len(model) == 0 || len(model) > 191 || strings.ContainsAny(model, ",*") || strings.ContainsFunc(model, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			return fmt.Errorf("invalid TabTab image model name %q", model)
		}
		if slices.Contains(tabtabImageModels, model) {
			return fmt.Errorf("cannot override built-in TabTab image model %q", model)
		}
		if !slices.Contains(tabtabImageModels, template) {
			return fmt.Errorf("invalid TabTab image profile %q for %q", template, model)
		}
	}
	return nil
}
