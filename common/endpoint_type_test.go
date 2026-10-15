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
