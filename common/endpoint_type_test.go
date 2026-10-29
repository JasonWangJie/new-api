package common

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestXAIModelEndpointTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []constant.EndpointType
	}{
		{"grok-imagine", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"grok-imagine-image-2.0", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"grok-imagine-image-quality", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"grok-2-image-1212", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"grok-imagine-video", []constant.EndpointType{constant.EndpointTypeOpenAIVideo}},
		{"grok-imagine-video-1.5-preview", []constant.EndpointType{constant.EndpointTypeOpenAIVideo}},
		{"grok-4", []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, GetEndpointTypesByChannelType(constant.ChannelTypeXai, tc.name))
		})
	}
	assert.Equal(t, []constant.EndpointType{constant.EndpointTypeOpenAI}, GetEndpointTypesByChannelType(constant.ChannelTypeOpenAI, "grok-imagine-video"), "other channel types retain their own inference")
}

func TestDedicatedImageModelEndpointTypes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		channelType int
		model       string
		want        []constant.EndpointType
	}{
		{"openai", constant.ChannelTypeOpenAI, "gpt-image-2", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"newapi", constant.ChannelTypeNewAPI, "gpt-image-2", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"sub2api", constant.ChannelTypeSub2API, "gpt-image-2", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"azure", constant.ChannelTypeAzure, "gpt-image-1", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"openrouter", constant.ChannelTypeOpenRouter, "flux-1", []constant.EndpointType{constant.EndpointTypeImageGeneration}},
		{"gemini_native_image", constant.ChannelTypeGemini, "imagen-3.0-generate-002", []constant.EndpointType{constant.EndpointTypeImageGeneration, constant.EndpointTypeGemini}},
		{"vertex_native_image", constant.ChannelTypeVertexAi, "imagen-4.0-generate-001", []constant.EndpointType{constant.EndpointTypeImageGeneration, constant.EndpointTypeGemini}},
		{"gemini_multimodal", constant.ChannelTypeGemini, "gemini-3.1-flash-image", []constant.EndpointType{constant.EndpointTypeGemini, constant.EndpointTypeOpenAI}},
		{"newapi_text", constant.ChannelTypeNewAPI, "gpt-4o", []constant.EndpointType{
			constant.EndpointTypeOpenAI,
			constant.EndpointTypeOpenAIResponse,
			constant.EndpointTypeOpenAIResponseCompact,
			constant.EndpointTypeAnthropic,
			constant.EndpointTypeGemini,
			constant.EndpointTypeOpenAIAlphaSearch,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, GetEndpointTypesByChannelType(tc.channelType, tc.model))
		})
	}
}
