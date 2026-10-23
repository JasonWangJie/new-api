package volcengine

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// PrepareImageSubmission validates the final Ark payload after channel overrides
// and keeps native group generation aligned with the quantity reserved by the host.
func PrepareImageSubmission(data []byte, upstreamModel string, count int, asynchronous bool) ([]byte, error) {
	model := strings.TrimPrefix(strings.ToLower(upstreamModel), "doubao-")
	if !strings.HasPrefix(model, "seedream-") {
		return data, nil
	}
	var fields map[string]common.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("Seedream request must be a JSON object")
	}
	var submittedModel string
	if common.Unmarshal(fields["model"], &submittedModel) != nil || submittedModel != upstreamModel {
		return nil, fmt.Errorf("outbound image model conflicts with the selected Seedream model mapping")
	}
	singleImage := strings.HasPrefix(model, "seedream-5-0-pro-") || strings.HasPrefix(model, "seedream-5-0-flash-")
	maximum := 15
	if singleImage {
		maximum = 1
	}
	if count < 1 || count > maximum {
		return nil, fmt.Errorf("%s supports between 1 and %d generated images", upstreamModel, maximum)
	}
	for _, name := range []string{"n", "batch_size", "num_outputs", "create_count"} {
		if raw, exists := fields[name]; exists {
			var quantity uint
			if common.Unmarshal(raw, &quantity) != nil || quantity < 1 || quantity > uint(maximum) || int(quantity) != count {
				return nil, fmt.Errorf("%s conflicts with the validated Seedream image count", name)
			}
		}
	}
	if raw, exists := fields["parameters"]; exists {
		var parameters map[string]common.RawMessage
		if common.Unmarshal(raw, &parameters) != nil || parameters == nil {
			return nil, fmt.Errorf("Seedream parameters must be an object")
		}
		if raw, exists := parameters["n"]; exists {
			var quantity uint
			if common.Unmarshal(raw, &quantity) != nil || quantity < 1 || quantity > uint(maximum) || int(quantity) != count {
				return nil, fmt.Errorf("parameters.n conflicts with the validated Seedream image count")
			}
		}
	}
	mode, hasMode := fields["sequential_image_generation"]
	options, hasOptions := fields["sequential_image_generation_options"]
	if singleImage && (hasMode || hasOptions) {
		return nil, fmt.Errorf("%s does not support sequential_image_generation parameters", upstreamModel)
	}
	var sequential string
	if hasMode && (common.Unmarshal(mode, &sequential) != nil || sequential != "auto" && sequential != "disabled") {
		return nil, fmt.Errorf("sequential_image_generation must be auto or disabled")
	}
	var groupOptions map[string]common.RawMessage
	if hasOptions {
		if common.Unmarshal(options, &groupOptions) != nil || groupOptions == nil {
			return nil, fmt.Errorf("sequential_image_generation_options must be an object")
		}
		if raw, exists := groupOptions["max_images"]; exists {
			var quantity uint
			if common.Unmarshal(raw, &quantity) != nil || quantity < 1 || quantity > uint(maximum) || int(quantity) != count {
				return nil, fmt.Errorf("sequential_image_generation_options.max_images conflicts with the validated Seedream image count")
			}
		}
	}
	if !singleImage && (count > 1 || sequential == "auto" || hasOptions) {
		if sequential == "disabled" && count > 1 {
			return nil, fmt.Errorf("sequential_image_generation disabled conflicts with n greater than one")
		}
		if groupOptions == nil {
			groupOptions = make(map[string]common.RawMessage)
		}
		groupOptions["max_images"], _ = common.Marshal(count)
		fields["sequential_image_generation_options"], _ = common.Marshal(groupOptions)
		if sequential != "disabled" {
			fields["sequential_image_generation"] = common.RawMessage(`"auto"`)
		}
	}
	for _, name := range []string{"stream", "web_search", "layer_decomposition", "watermark"} {
		raw, exists := fields[name]
		if !exists {
			continue
		}
		var enabled bool
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || common.Unmarshal(raw, &enabled) != nil {
			return nil, fmt.Errorf("%s must be a boolean", name)
		}
		if enabled && (name == "stream" && (asynchronous || singleImage) || name == "web_search" && (singleImage || !strings.HasPrefix(model, "seedream-5-0-"))) {
			return nil, fmt.Errorf("%s is not supported by this Seedream image request", name)
		}
		if enabled && name == "layer_decomposition" && asynchronous {
			return nil, fmt.Errorf("layer_decomposition is not supported by asynchronous image tasks")
		}
	}
	var references []string
	for _, name := range []string{"image", "image_urls", "images"} {
		raw, exists := fields[name]
		if !exists {
			continue
		}
		if len(references) > 0 {
			return nil, fmt.Errorf("use only one Seedream reference image field")
		}
		var single string
		if common.Unmarshal(raw, &single) == nil {
			references = []string{single}
		} else if common.Unmarshal(raw, &references) != nil {
			return nil, fmt.Errorf("%s must contain a reference URL or an array of reference URLs", name)
		}
		for _, reference := range references {
			if strings.TrimSpace(reference) == "" {
				return nil, fmt.Errorf("Seedream reference images must not be empty")
			}
		}
		delete(fields, name)
	}
	referenceLimit := 14
	if singleImage {
		referenceLimit = 10
	}
	if len(references) > referenceLimit {
		return nil, fmt.Errorf("%s supports at most %d reference images", upstreamModel, referenceLimit)
	}
	if !singleImage && len(references)+count > 15 {
		return nil, fmt.Errorf("Seedream reference images and generated images must total at most 15")
	}
	if len(references) > 0 {
		fields["image"], _ = common.Marshal(references)
	}
	if raw, exists := fields["mask"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("Seedream does not support mask inputs; describe the edit in the prompt")
	}
	return common.Marshal(fields)
}
