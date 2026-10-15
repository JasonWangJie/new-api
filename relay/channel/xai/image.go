package xai

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Imagine uses resolution/aspect_ratio rather than OpenAI's pixel-size field.
// Keep this projection provider-specific so legacy image channels stay intact.
func convertImagineImageRequest(c *gin.Context, request dto.ImageRequest) (map[string]any, error) {
	if c != nil && c.Request != nil && strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/") {
		return nil, errors.New("xAI image editing requires JSON; asynchronous image uploads are converted to JSON by the worker")
	}
	count, err := request.ImageCount(false)
	if err != nil {
		return nil, err
	}
	if count > 10 {
		return nil, errors.New("xAI Imagine supports at most 10 output images")
	}
	var deferred *bool
	if raw, exists := request.Extra["deferred"]; exists {
		if common.Unmarshal(raw, &deferred) != nil {
			return nil, errors.New("deferred must be a boolean")
		}
		if deferred != nil && *deferred {
			return nil, errors.New("use the gateway asynchronous image endpoints instead of upstream deferred responses")
		}
	}
	resolution, ratio := "", ""
	for field, target := range map[string]*string{"resolution": &resolution, "aspect_ratio": &ratio} {
		if raw, exists := request.Extra[field]; exists {
			if err := common.Unmarshal(raw, target); err != nil {
				return nil, fmt.Errorf("%s must be a string", field)
			}
			*target = strings.TrimSpace(*target)
		}
	}
	resolution = strings.ToLower(resolution)
	size := strings.ToLower(strings.TrimSpace(request.Size))
	if size == "4k" {
		return nil, errors.New("xAI Imagine supports only 1k or 2k resolution")
	}
	if size == "1k" || size == "2k" {
		if resolution != "" && resolution != size {
			return nil, errors.New("size conflicts with resolution")
		}
		resolution = size
	} else if size != "" && size != "auto" {
		left, right, found := strings.Cut(size, "x")
		width, widthErr := strconv.Atoi(left)
		height, heightErr := strconv.Atoi(right)
		if !found || widthErr != nil || heightErr != nil || width < 1 || height < 1 || max(width, height) > 3072 {
			return nil, errors.New("unsupported xAI Imagine size; use resolution 1k or 2k and aspect_ratio")
		}
		if resolution == "" {
			resolution = strings.ToLower(service.ImageNativeTier(width, height, "openai"))
		}
		if ratio == "" {
			a, b := width, height
			for b != 0 {
				a, b = b, a%b
			}
			ratio = fmt.Sprintf("%d:%d", width/a, height/a)
		}
	}
	if resolution != "" && !slices.Contains([]string{"1k", "2k"}, resolution) {
		return nil, errors.New("xAI Imagine supports only 1k or 2k resolution")
	}
	if ratio != "" && !slices.Contains([]string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20", "21:9", "5:2"}, ratio) {
		return nil, errors.New("unsupported xAI Imagine aspect_ratio")
	}
	body := map[string]any{"model": request.Model, "prompt": request.Prompt, "n": count}
	if deferred != nil {
		body["deferred"] = *deferred
	}
	if resolution != "" {
		body["resolution"] = resolution
	}
	if ratio != "" {
		body["aspect_ratio"] = ratio
	}
	if request.ResponseFormat != "" {
		body["response_format"] = request.ResponseFormat
	}
	if request.Quality != "" && request.Model == "grok-imagine-image-2.0" {
		if !slices.Contains([]string{"auto", "low", "medium"}, request.Quality) {
			return nil, errors.New("xAI Imagine 2.0 quality must be auto, low or medium")
		}
		body["quality"] = request.Quality
	}
	var references []any
	if len(request.Images) > 0 && string(request.Images) != "null" {
		if common.Unmarshal(request.Images, &references) != nil {
			return nil, errors.New("images must be an array")
		}
	} else if len(request.Image) > 0 && string(request.Image) != "null" {
		var image any
		if common.Unmarshal(request.Image, &image) != nil {
			return nil, errors.New("image must be a URL or image object")
		}
		references = []any{image}
	}
	if len(references) > 3 {
		return nil, errors.New("xAI image editing supports at most 3 reference images on this gateway")
	}
	images := make([]map[string]string, 0, len(references))
	for _, reference := range references {
		url := ""
		switch image := reference.(type) {
		case string:
			url = strings.TrimSpace(image)
		case map[string]any:
			url, _ = image["url"].(string)
			if url == "" {
				url, _ = image["image_url"].(string)
			}
			url = strings.TrimSpace(url)
		}
		if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "data:image/") {
			return nil, errors.New("image references must use HTTPS image URLs or image data URIs")
		}
		images = append(images, map[string]string{"url": url, "type": "image_url"})
	}
	if len(images) == 1 {
		body["image"] = images[0]
	} else if len(images) > 1 {
		body["images"] = images
	}
	if c != nil && c.Request != nil && strings.HasSuffix(c.Request.URL.Path, "/edits") && len(images) == 0 {
		return nil, errors.New("image is required for xAI image editing")
	}
	if len(request.Mask) > 0 && string(request.Mask) != "null" {
		return nil, errors.New("xAI image editing does not support masks")
	}
	return body, nil
}
