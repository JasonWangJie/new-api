package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

type AsyncImageInputPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
}

type AsyncImageRequest struct {
	Billing        *MediaBillingSnapshot        `json:"billing,omitempty"`
	Provider       string                       `json:"provider,omitempty"`
	PolicyPlatform string                       `json:"policy_platform,omitempty"`
	ClientIP       string                       `json:"client_ip,omitempty"`
	Platform       string                       `json:"platform"`
	Dialect        string                       `json:"dialect"`
	SourcePath     string                       `json:"source_path"`
	Model          string                       `json:"model"`
	Prompt         string                       `json:"prompt"`
	Kind           string                       `json:"kind"`
	Size           string                       `json:"size,omitempty"`
	Resolution     string                       `json:"resolution,omitempty"`
	AspectRatio    string                       `json:"aspect_ratio,omitempty"`
	ExplicitTier   bool                         `json:"explicit_tier"`
	Count          int                          `json:"count"`
	Parts          []AsyncImageInputPart        `json:"parts"`
	Native         map[string]common.RawMessage `json:"native,omitempty"`
}

func (request AsyncImageRequest) ReferenceImageCount() int {
	count := 0
	for _, part := range request.Parts {
		if part.Type == "image_url" {
			count++
		}
	}
	return count
}

// ReferenceURLs retains external image references for the administrator task
// view without copying embedded image data or internal input handles.
func (request AsyncImageRequest) ReferenceURLs() []string {
	urls := make([]string, 0)
	for _, part := range request.Parts {
		if part.Type != "image_url" {
			continue
		}
		value := strings.TrimSpace(part.URL)
		lower := strings.ToLower(value)
		if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
			urls = append(urls, value)
		}
	}
	return urls
}

func AsyncImageRequestHash(platform, dialect, path string, raw []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.TrimSpace(platform) + "\x00" + strings.TrimSpace(dialect) + "\x00" + strings.TrimSpace(path) + "\x00"))
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func MapOpenAIImageDimensions(resolution, ratio string) (string, error) {
	resolution = strings.ToUpper(strings.TrimSpace(resolution))
	ratio = strings.TrimSpace(ratio)
	if ratio == "" {
		ratio = "1:1"
	}
	index := slices.Index([]string{"1K", "2K", "4K"}, resolution)
	if resolution == "AUTO" {
		if ratio != "auto" && ratio != "自动" {
			if _, err := MapOpenAIImageDimensions("1K", ratio); err != nil {
				return "", err
			}
		}
		return "auto", nil
	}
	if index < 0 {
		return "", errors.New("unsupported_image_dimensions: invalid resolution")
	}
	if ratio == "auto" || ratio == "自动" {
		return "auto", nil
	}
	rows := map[string][3]string{
		"1:1": {"1024x1024", "2048x2048", "4096x4096"}, "3:2": {"1536x1024", "2048x1152", "4096x2304"}, "16:9": {"1536x1024", "2048x1152", "4096x2304"}, "2:3": {"1024x1536", "1152x2048", "2304x4096"}, "9:16": {"1024x1536", "1152x2048", "2304x4096"}, "5:4": {"1280x1024", "2048x1632", "4096x3272"}, "4:5": {"1024x1280", "1632x2048", "3272x4096"}, "4:3": {"1360x1024", "2048x1536", "4096x3072"}, "3:4": {"1024x1360", "1536x2048", "3072x4096"}, "21:9": {"2384x1024", "2048x880", "4096x1752"}, "9:21": {"1024x2384", "880x2048", "1752x4096"}, "2:1": {"2048x1024", "2048x1024", "4096x2048"}, "1:2": {"1024x2048", "1024x2048", "2048x4096"},
	}
	sizes, ok := rows[ratio]
	if !ok {
		return "", errors.New("unsupported_image_dimensions: invalid aspect_ratio")
	}
	return sizes[index], nil
}

func ImageNativeTier(width, height int, platform string) string {
	dimension := max(width, height)
	if platform == "gemini" {
		dimension = min(width, height)
	}
	if dimension <= 1024 {
		return "1K"
	}
	if dimension <= 2048 {
		return "2K"
	}
	return "4K"
}

func imageDimensions(value string) (int, int, bool) {
	value = strings.NewReplacer("X", "x", "*", "x", "×", "x").Replace(strings.TrimSpace(value))
	left, right, ok := strings.Cut(value, "x")
	if !ok {
		return 0, 0, false
	}
	width, err := strconv.Atoi(left)
	if err != nil || width <= 0 {
		return 0, 0, false
	}
	height, err := strconv.Atoi(right)
	return width, height, err == nil && height > 0
}

func normalizeAsyncOpenAIDimensions(request *AsyncImageRequest) error {
	size, resolution, ratio := strings.TrimSpace(request.Size), strings.ToUpper(strings.TrimSpace(request.Resolution)), strings.TrimSpace(request.AspectRatio)
	if strings.EqualFold(ratio, "auto") || ratio == "自动" {
		ratio = "auto"
	}
	if resolution != "" && !slices.Contains([]string{"1K", "2K", "4K", "AUTO"}, resolution) {
		return errors.New("unsupported_image_dimensions: invalid resolution")
	}
	if ratio != "" && ratio != "auto" {
		if _, err := MapOpenAIImageDimensions("1K", ratio); err != nil {
			return err
		}
	}
	if slices.Contains([]string{"1K", "2K", "4K"}, strings.ToUpper(size)) {
		if resolution == "" {
			resolution = strings.ToUpper(size)
		}
		size = ""
	}
	if strings.Contains(size, ":") || size == "自动" {
		if ratio == "" {
			ratio = size
		}
		size = ""
	}
	if strings.EqualFold(size, "auto") {
		request.Size, request.Resolution, request.AspectRatio = "auto", "AUTO", ratio
		return nil
	}
	if width, height, ok := imageDimensions(size); ok {
		if resolution != "" && resolution != "AUTO" && resolution != ImageNativeTier(width, height, "openai") {
			return errors.New("unsupported_image_dimensions: native size conflicts with resolution")
		}
		request.Size, request.Resolution, request.AspectRatio = fmt.Sprintf("%dx%d", width, height), resolution, ratio
		return nil
	}
	if size != "" {
		if resolution != "" || ratio != "" {
			return errors.New("unsupported_image_dimensions: ambiguous native size")
		}
		request.Size = size
		return nil
	}
	if ratio != "" && resolution == "" {
		return errors.New("unsupported_image_dimensions: resolution required")
	}
	if resolution != "" {
		mapped, err := MapOpenAIImageDimensions(resolution, ratio)
		if err != nil {
			return err
		}
		request.Size = mapped
		request.ExplicitTier = resolution != "AUTO"
	}
	request.Resolution, request.AspectRatio = resolution, ratio
	return nil
}

func normalizeAsyncGeminiDimensions(request *AsyncImageRequest) error {
	resolution := strings.ToUpper(strings.TrimSpace(request.Resolution))
	ratio := strings.TrimSpace(request.AspectRatio)
	size := strings.TrimSpace(request.Size)
	if resolution == "" && slices.Contains([]string{"0.5K", "1K", "2K", "4K"}, strings.ToUpper(size)) {
		resolution = strings.ToUpper(size)
	} else if ratio == "" && size != "" {
		if width, height, ok := imageDimensions(size); ok {
			a, b := width, height
			for b != 0 {
				a, b = b, a%b
			}
			ratio = fmt.Sprintf("%d:%d", width/a, height/a)
		} else {
			ratio = size
		}
	}
	if resolution != "" && !slices.Contains([]string{"0.5K", "1K", "2K", "4K"}, resolution) {
		return errors.New("unsupported_image_dimensions: invalid image_size")
	}
	if strings.EqualFold(ratio, "auto") || ratio == "自动" {
		ratio = ""
	}
	if ratio != "" && !slices.Contains([]string{"1:1", "2:3", "3:2", "4:5", "5:4", "4:3", "3:4", "16:9", "9:16", "21:9", "9:21"}, ratio) {
		return errors.New("unsupported_image_dimensions: invalid aspect_ratio")
	}
	request.Resolution, request.AspectRatio, request.Size = resolution, ratio, resolution
	request.ExplicitTier = resolution != ""
	return nil
}

func ParseAsyncImageRequest(raw []byte, contentType, path string, cfg ImageRuntimeConfig) (AsyncImageRequest, error) {
	request := AsyncImageRequest{SourcePath: path, Count: 1, Kind: "text_to_image", Dialect: "bb", Platform: "openai"}
	if strings.HasSuffix(path, "_gm") {
		request.Platform = "gemini"
	}
	if strings.HasSuffix(path, "_sc") {
		request.Platform, request.Dialect = "gemini", "sc"
	}
	fields := make(map[string]common.RawMessage)
	if strings.HasSuffix(path, "_async") {
		request.Dialect = "async"
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return request, errors.New("invalid content type")
	}
	if mediaType == "multipart/form-data" && request.Platform == "openai" {
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		multipartReferenceIndex := 0
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return request, errors.New("invalid multipart body")
			}
			name := part.FormName()
			data, readErr := io.ReadAll(io.LimitReader(part, cfg.DownloadMaxBytes+1))
			_ = part.Close()
			if part.FileName() != "" {
				multipartReferenceIndex++
				if readErr != nil || int64(len(data)) > cfg.DownloadMaxBytes {
					return request, fmt.Errorf("Reference image %d: multipart image byte limit exceeded", multipartReferenceIndex)
				}
				indexedImage := false
				if index, ok := strings.CutPrefix(name, "image["); ok {
					if index, ok = strings.CutSuffix(index, "]"); ok {
						_, indexErr := strconv.ParseUint(index, 10, 32)
						indexedImage = indexErr == nil
					}
				}
				if name != "image" && name != "image[]" && name != "mask" && !indexedImage {
					return request, errors.New("unsupported multipart file field")
				}
				if len(request.Parts) >= cfg.MaxReferences {
					return request, errors.New("too_many_reference_images_for_model")
				}
				image, err := ValidateImageBytes(data, "", cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
				if err != nil {
					return request, fmt.Errorf("Reference image %d: %w", multipartReferenceIndex, err)
				}
				if name == "mask" {
					mask, err := common.Marshal(map[string]string{"image_url": image.DataURL()})
					if err != nil {
						return request, err
					}
					fields["mask"] = mask
				} else {
					request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: image.DataURL()})
				}
				continue
			}
			if readErr != nil {
				return request, errors.New("multipart field could not be read")
			}
			if len(data) > 64<<10 {
				return request, errors.New("multipart field limit exceeded")
			}
			value := strings.TrimSpace(string(data))
			if request.Dialect == "async" && !slices.Contains([]string{"model", "prompt", "provider", "size", "quality", "response_format", "output_format", "background", "user", "aspect_ratio", "resolution"}, name) {
				var decoded any
				if common.Unmarshal(data, &decoded) == nil {
					fields[name] = common.RawMessage(data)
					continue
				}
			}
			if name == "n" || name == "stream" || name == "output_compression" || name == "partial_images" {
				fields[name] = common.RawMessage(data)
			} else {
				encoded, err := common.Marshal(value)
				if err != nil {
					return request, err
				}
				fields[name] = encoded
			}
		}
	} else {
		if mediaType != "application/json" {
			return request, errors.New("JSON content type required")
		}
		if err := common.Unmarshal(raw, &fields); err != nil || fields == nil {
			return request, errors.New("invalid JSON request body")
		}
	}
	var stream *bool
	if value, ok := fields["provider"]; ok {
		if common.Unmarshal(value, &request.Provider) != nil || !RegisteredImageProvider(request.Provider) {
			return request, errors.New("invalid image provider")
		}
		delete(fields, "provider")
	}
	if value, ok := fields["stream"]; ok && request.Dialect != "sc" {
		if err := common.Unmarshal(value, &stream); err != nil {
			return request, errors.New("stream must be a boolean")
		}
		if stream != nil && *stream {
			return request, errors.New("streaming is not supported by async image endpoints")
		}
	}
	if value, ok := fields["model"]; ok {
		if err := common.Unmarshal(value, &request.Model); err != nil {
			return request, errors.New("model must be a string")
		}
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" && request.Platform == "openai" && request.Dialect != "async" {
		request.Model = "gpt-image-2"
	}
	if err := ValidateImageModelName(request.Model); err != nil {
		return request, err
	}
	if request.Platform == "gemini" && request.Dialect == "bb" {
		var body struct {
			Messages []struct {
				Role    string            `json:"role"`
				Content common.RawMessage `json:"content"`
			} `json:"messages"`
			ExtraBody struct {
				Google struct {
					ImageConfig struct {
						ImageSize   string `json:"image_size"`
						AspectRatio string `json:"aspect_ratio"`
					} `json:"image_config"`
				} `json:"google"`
			} `json:"extra_body"`
		}
		if err := common.Unmarshal(raw, &body); err != nil || len(body.Messages) == 0 {
			return request, errors.New("messages are required")
		}
		var prompts []string
		for _, message := range body.Messages {
			if message.Role != "user" {
				return request, errors.New("only user messages are supported")
			}
			var text string
			if common.Unmarshal(message.Content, &text) == nil {
				text = strings.TrimSpace(text)
				if text == "" {
					return request, errors.New("message content is empty")
				}
				request.Parts = append(request.Parts, AsyncImageInputPart{Type: "text", Text: text})
				prompts = append(prompts, text)
				continue
			}
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			if err := common.Unmarshal(message.Content, &parts); err != nil || len(parts) == 0 {
				return request, errors.New("invalid message content")
			}
			for _, part := range parts {
				if part.Type == "text" {
					text = strings.TrimSpace(part.Text)
					if text == "" {
						return request, errors.New("empty text part")
					}
					request.Parts = append(request.Parts, AsyncImageInputPart{Type: "text", Text: text})
					prompts = append(prompts, text)
				} else if part.Type == "image_url" && strings.TrimSpace(part.ImageURL.URL) != "" {
					request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: strings.TrimSpace(part.ImageURL.URL)})
				} else {
					return request, errors.New("unsupported or empty content part")
				}
			}
		}
		request.Prompt = strings.Join(prompts, "\n")
		request.Resolution, request.AspectRatio = body.ExtraBody.Google.ImageConfig.ImageSize, body.ExtraBody.Google.ImageConfig.AspectRatio
	} else {
		for name, target := range map[string]*string{"prompt": &request.Prompt, "size": &request.Size, "resolution": &request.Resolution, "aspect_ratio": &request.AspectRatio} {
			if value, ok := fields[name]; ok {
				if err := common.Unmarshal(value, target); err != nil {
					return request, fmt.Errorf("%s must be a string", name)
				}
			}
		}
		if request.Platform == "gemini" {
			if value, ok := fields["ratio"]; ok && request.AspectRatio == "" {
				if err := common.Unmarshal(value, &request.AspectRatio); err != nil {
					return request, errors.New("ratio must be a string")
				}
			}
		}
		var urls []string
		if value, ok := fields["image_urls"]; ok {
			if err := common.Unmarshal(value, &urls); err != nil {
				return request, errors.New("image_urls must be an array")
			}
		}
		if value, present := fields["image"]; present && request.Dialect == "async" {
			var one string
			var multiple []string
			if common.Unmarshal(value, &one) == nil {
				multiple = []string{one}
			} else if common.Unmarshal(value, &multiple) != nil {
				return request, errors.New("image must be a URL or array of URLs")
			}
			urls = append(urls, multiple...)
			delete(fields, "image")
		}
		for _, value := range urls {
			value = strings.TrimSpace(value)
			if value == "" {
				if request.Dialect == "sc" {
					return request, errors.New("image_urls must not contain empty values")
				}
				continue
			}
			request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: value})
		}
		if request.Platform == "openai" {
			var images []struct {
				URL    string `json:"image_url"`
				FileId string `json:"file_id"`
			}
			if value, ok := fields["images"]; ok {
				if err := common.Unmarshal(value, &images); err != nil {
					return request, errors.New("invalid images array")
				}
			}
			for _, image := range images {
				if image.FileId != "" {
					return request, errors.New("file_id references are not supported")
				}
				if value := strings.TrimSpace(image.URL); value != "" {
					request.Parts = append(request.Parts, AsyncImageInputPart{Type: "image_url", URL: value})
				}
			}
			if value, ok := fields["n"]; ok {
				if err := common.Unmarshal(value, &request.Count); err != nil || request.Count < 1 || request.Count > dto.MaxImageN {
					return request, fmt.Errorf("n must be an integer between 1 and %d", dto.MaxImageN)
				}
			}
			if request.Dialect == "async" {
				quantities := make(map[string]common.RawMessage)
				for _, key := range []string{"batch_size", "num_outputs"} {
					if raw, exists := fields[key]; exists {
						quantities[key] = raw
					}
				}
				if raw, exists := fields["input"]; exists {
					var input map[string]common.RawMessage
					if err := common.Unmarshal(raw, &input); err != nil {
						return request, errors.New("invalid provider image input")
					}
					if count, exists := input["num_outputs"]; exists {
						quantities["input.num_outputs"] = count
					}
				}
				if raw, exists := fields["sequential_image_generation_options"]; exists {
					var options map[string]common.RawMessage
					if err := common.Unmarshal(raw, &options); err != nil {
						return request, errors.New("invalid sequential image options")
					}
					if count, exists := options["max_images"]; exists {
						quantities["sequential_image_generation_options.max_images"] = count
					}
				}
				for name, raw := range quantities {
					var count uint
					if common.Unmarshal(raw, &count) != nil || count < 1 || count > dto.MaxImageN {
						return request, fmt.Errorf("%s must be an integer between 1 and %d", name, dto.MaxImageN)
					}
					if _, exists := fields["n"]; exists && int(count) != request.Count {
						return request, fmt.Errorf("%s conflicts with image n", name)
					}
					if request.Count != 1 && int(count) != request.Count {
						return request, errors.New("conflicting image quantities")
					}
					request.Count = int(count)
				}
			}
			if value, present := fields["parameters"]; present {
				var parameters dto.ImageBillingParameters
				if err := common.Unmarshal(value, &parameters); err != nil {
					return request, errors.New("invalid image parameters")
				}
				quantity := dto.ImageRequest{N: common.GetPointer(uint(request.Count)), BillingParameters: &parameters}
				count, err := quantity.ImageCount(true)
				if err != nil {
					return request, err
				}
				request.Count = int(count)
			}
			request.Native = fields
		}
		request.Prompt = strings.TrimSpace(request.Prompt)
		request.Parts = append(request.Parts, AsyncImageInputPart{Type: "text", Text: request.Prompt})
	}
	if request.Prompt == "" || len(request.Prompt) > 64<<10 {
		return request, errors.New("prompt is required and must not exceed 64 KiB")
	}
	references := request.ReferenceImageCount()
	if references > cfg.MaxReferences {
		return request, errors.New("too_many_reference_images_for_model")
	}
	if references > 0 {
		request.Kind = "image_to_image"
	}
	if (strings.HasSuffix(path, "edits_oa") || strings.HasSuffix(path, "edits_async")) && references == 0 {
		return request, errors.New("reference images are required for edits")
	}
	if _, ok := fields["mask"]; ok && references == 0 {
		return request, errors.New("mask requires reference images")
	}
	if raw, ok := fields["mask"]; ok {
		var mask struct {
			URL string `json:"image_url"`
		}
		var maskURL string
		if request.Dialect == "async" && common.Unmarshal(raw, &maskURL) == nil {
			mask.URL = maskURL
		} else if err := common.Unmarshal(raw, &mask); err != nil {
			return request, errors.New("mask.image_url is required")
		}
		if mask.URL == "" {
			return request, errors.New("mask.image_url is required")
		}
		request.Parts = append(request.Parts, AsyncImageInputPart{Type: "mask", URL: mask.URL})
	}
	referenceIndex := 0
	for _, part := range request.Parts {
		if part.Type != "image_url" && part.Type != "mask" {
			continue
		}
		referenceIndex++
		if !strings.HasPrefix(strings.ToLower(part.URL), "data:") {
			if IsGatewayImageReference(part.URL) {
				continue
			}
			if _, err := ValidateImageReferenceURL(part.URL); err != nil {
				return request, fmt.Errorf("Reference image %d: %w", referenceIndex, err)
			}
			continue
		}
		if _, err := DownloadImageReference(context.Background(), part.URL, cfg); err != nil {
			return request, fmt.Errorf("Reference image %d: %w", referenceIndex, err)
		}
	}
	if request.Platform == "openai" {
		if request.Dialect != "async" || request.Size == "" {
			if err := normalizeAsyncOpenAIDimensions(&request); err != nil {
				return request, err
			}
		}
		if request.Dialect != "async" {
			delete(request.Native, "resolution")
			delete(request.Native, "aspect_ratio")
		}
		delete(request.Native, "image_urls")
		delete(request.Native, "images")
		if request.Count != 1 {
			request.Native["n"], err = common.Marshal(request.Count)
			if err != nil {
				return request, err
			}
		}
		for name, value := range map[string]string{"model": request.Model, "prompt": request.Prompt, "size": request.Size} {
			if value != "" {
				data, err := common.Marshal(value)
				if err != nil {
					return request, err
				}
				request.Native[name] = data
			}
		}
	} else if err := normalizeAsyncGeminiDimensions(&request); err != nil {
		return request, err
	}
	return request, nil
}
