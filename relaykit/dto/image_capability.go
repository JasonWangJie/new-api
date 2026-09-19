package dto

import "strings"

// ImageCapability describes the image operations implemented by an adaptor.
// Model availability remains subject to the host's configured channel abilities.
type ImageCapability struct {
	Provider       string   `json:"provider"`
	Protocol       string   `json:"protocol"`
	Generate       bool     `json:"supports_generation"`
	Models         []string `json:"models,omitempty"`
	Parameters     []string `json:"supported_parameters,omitempty"`
	EditFormat     string   `json:"edit_format,omitempty"`
	Edit           bool     `json:"supports_edit"`
	ReferenceField string   `json:"reference_field,omitempty"`
}

// DeclaresModel matches adaptor-declared families for catalog discovery. A
// gateway alias is tested after its configured upstream mapping is applied.
func (capability ImageCapability) DeclaresModel(model string) bool {
	model = strings.ToLower(model)
	for _, pattern := range capability.Models {
		pattern = strings.ToLower(pattern)
		if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
			if strings.HasPrefix(model, prefix) {
				return true
			}
			continue
		}
		if model == pattern {
			return true
		}
	}
	return false
}
