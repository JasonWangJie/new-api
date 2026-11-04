package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// RewriteVideoRoutingRequest derives an execution request from the normalized
// public request. Only the model and an explicitly configured output resolution
// change; the caller retains the public request for pricing and fallback.
func RewriteVideoRoutingRequest(raw []byte, contentType, operation, targetModel, resolution string) ([]byte, string, error) {
	if strings.TrimSpace(targetModel) == "" {
		return nil, "", errors.New("video routing target model is required")
	}
	if resolution != "" && NormalizeVideoResolution(resolution) != resolution {
		return nil, "", errors.New("video routing target resolution must be canonical")
	}
	if _, _, err := MixedVideoRequestContext(raw, contentType, operation); err != nil {
		return nil, "", err
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "", err
	}
	var fields map[string]json.RawMessage
	if mediaType == "application/json" {
		if err := common.Unmarshal(raw, &fields); err != nil {
			return nil, "", err
		}
		if fields == nil {
			return nil, "", errors.New("video routing request must be an object")
		}
	} else {
		fields = make(map[string]json.RawMessage)
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, "", err
			}
			name := part.FormName()
			if part.FileName() != "" || name != "model" && name != "resolution" && name != "size" && name != "metadata" {
				part.Close()
				continue
			}
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return nil, "", err
			}
			if name == "metadata" {
				fields[name] = data
			} else {
				fields[name], err = common.Marshal(string(data))
				if err != nil {
					return nil, "", err
				}
			}
		}
	}
	fields["model"], err = common.Marshal(targetModel)
	if err != nil {
		return nil, "", err
	}
	changed := map[string]bool{"model": true}
	if resolution != "" {
		containers := []map[string]json.RawMessage{fields}
		var metadata map[string]json.RawMessage
		if rawMetadata, present := fields["metadata"]; present {
			if err := common.Unmarshal(rawMetadata, &metadata); err != nil {
				return nil, "", errors.New("video routing metadata must be an object")
			}
			if metadata != nil {
				containers = append(containers, metadata)
			}
		}
		for index, container := range containers {
			for _, key := range []string{"resolution", "size"} {
				value, present := container[key]
				if !present {
					continue
				}
				var original string
				if err := common.Unmarshal(value, &original); err != nil {
					return nil, "", fmt.Errorf("video routing %s must be a string", key)
				}
				if key == "size" && mixedVideoSizeAspectRatio(original) != "" {
					continue
				}
				replacement, err := videoRoutingResolutionValue(original, resolution)
				if err != nil {
					return nil, "", err
				}
				container[key], err = common.Marshal(replacement)
				if err != nil {
					return nil, "", err
				}
				if index == 0 {
					changed[key] = true
				} else {
					changed["metadata"] = true
				}
			}
		}
		if !changed["resolution"] && !changed["size"] && !changed["metadata"] {
			fields["resolution"], err = common.Marshal(resolution)
			if err != nil {
				return nil, "", err
			}
			changed["resolution"] = true
		}
		if changed["metadata"] {
			fields["metadata"], err = common.Marshal(metadata)
			if err != nil {
				return nil, "", err
			}
		}
	}
	if mediaType == "application/json" {
		encoded, err := common.Marshal(fields)
		return encoded, contentType, err
	}
	var output bytes.Buffer
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(params["boundary"]); err != nil {
		return nil, "", err
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	written := make(map[string]bool)
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", err
		}
		name := part.FormName()
		replace := part.FileName() == "" && changed[name]
		if replace {
			part.Header.Del("Content-Transfer-Encoding")
			part.Header.Del("Content-Length")
		}
		destination, err := writer.CreatePart(part.Header)
		if err != nil {
			part.Close()
			return nil, "", err
		}
		if replace {
			value := string(fields[name])
			if name != "metadata" {
				if err := common.Unmarshal(fields[name], &value); err != nil {
					part.Close()
					return nil, "", err
				}
			}
			_, err = io.WriteString(destination, value)
			written[name] = true
		} else {
			_, err = io.Copy(destination, part)
		}
		part.Close()
		if err != nil {
			return nil, "", err
		}
	}
	for _, name := range []string{"model", "resolution"} {
		if changed[name] && !written[name] {
			var value string
			if err := common.Unmarshal(fields[name], &value); err != nil {
				return nil, "", err
			}
			if err := writer.WriteField(name, value); err != nil {
				return nil, "", err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return output.Bytes(), contentType, nil
}

// videoRoutingResolutionValue preserves an explicit pixel aspect ratio exactly.
// Rounding, cropping and changing an unrelated request dimension are forbidden.
func videoRoutingResolutionValue(original, resolution string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(original))
	left, right, pixels := strings.Cut(strings.ReplaceAll(value, "*", "x"), "x")
	if !pixels {
		if NormalizeVideoResolution(value) == "" && value != "720px" {
			return "", errors.New("video routing source resolution is invalid")
		}
		return resolution, nil
	}
	width, widthErr := strconv.ParseUint(left, 10, 32)
	height, heightErr := strconv.ParseUint(right, 10, 32)
	if widthErr != nil || heightErr != nil || width == 0 || height == 0 {
		return "", errors.New("video routing source pixel size is invalid")
	}
	edge := map[string]uint64{"480p": 480, "720p": 720, "1080p": 1080, "2k": 1440, "4k": 2160}[resolution]
	short := min(width, height)
	if edge == 0 || width*edge%short != 0 || height*edge%short != 0 {
		return "", errors.New("video routing cannot preserve the exact pixel aspect ratio")
	}
	result := fmt.Sprintf("%dx%d", width*edge/short, height*edge/short)
	if NormalizeVideoResolution(result) != resolution {
		return "", errors.New("video routing pixel size does not represent the target resolution")
	}
	return result, nil
}
