package xai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImagineImageRequest(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, want string
	}{
		{"explicit geometry", "/v1/images/generations", `{"model":"grok-imagine-image-2.0","prompt":"sea","n":2,"size":"1536x1024","resolution":"1K","aspect_ratio":"16:9","quality":"low"}`, `{"model":"grok-imagine-image-2.0","prompt":"sea","n":2,"resolution":"1k","aspect_ratio":"16:9","quality":"low"}`},
		{"pixel size", "/v1/images/generations", `{"model":"grok-imagine-image","prompt":"sea","size":"2048x2048"}`, `{"model":"grok-imagine-image","prompt":"sea","n":1,"resolution":"2k","aspect_ratio":"1:1"}`},
		{"explicit immediate response", "/v1/images/generations", `{"model":"grok-imagine-image","prompt":"sea","deferred":false}`, `{"model":"grok-imagine-image","prompt":"sea","n":1,"deferred":false}`},
		{"single reference", "/v1/images/edits", `{"model":"grok-imagine-image-quality","prompt":"sketch","images":[{"image_url":"https://cdn.example/in.png"}],"response_format":"url"}`, `{"model":"grok-imagine-image-quality","prompt":"sketch","n":1,"response_format":"url","image":{"url":"https://cdn.example/in.png","type":"image_url"}}`},
		{"multiple references", "/v1/images/edits", `{"model":"grok-imagine","prompt":"combine","images":["https://cdn.example/a.png",{"url":"data:image/png;base64,AA=="}]}`, `{"model":"grok-imagine","prompt":"combine","n":1,"images":[{"url":"https://cdn.example/a.png","type":"image_url"},{"url":"data:image/png;base64,AA==","type":"image_url"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request dto.ImageRequest
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &request))
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
			ctx.Request.Header.Set("Content-Type", "application/json")
			converted, err := (&Adaptor{}).ConvertImageRequest(ctx, nil, request)
			require.NoError(t, err)
			encoded, err := common.Marshal(converted)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(encoded))
		})
	}
	for _, tc := range []struct{ name, body, message string }{
		{"too many outputs", `{"model":"grok-imagine-image","n":11}`, "at most 10"},
		{"oversized count", `{"model":"grok-imagine-image","n":18446744073686646784}`, "n must be"},
		{"4k resolution", `{"model":"grok-imagine-image","resolution":"4K"}`, "only 1k or 2k"},
		{"4k pixels", `{"model":"grok-imagine-image","size":"4096x4096"}`, "unsupported"},
		{"invalid scalar", `{"model":"grok-imagine-image","resolution":0}`, "must be a string"},
		{"native deferred response", `{"model":"grok-imagine-image","deferred":true}`, "use the gateway asynchronous image endpoints"},
		{"invalid deferred scalar", `{"model":"grok-imagine-image","deferred":1}`, "deferred must be a boolean"},
		{"missing reference", `{"model":"grok-imagine-image"}`, "image is required"},
		{"reference limit", `{"model":"grok-imagine-image","images":["https://cdn.example/1.png","https://cdn.example/2.png","https://cdn.example/3.png","https://cdn.example/4.png"]}`, "at most 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request dto.ImageRequest
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &request))
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
			_, err := (&Adaptor{}).ConvertImageRequest(ctx, nil, request)
			require.ErrorContains(t, err, tc.message)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
		})
	}
	assert.True(t, (&Adaptor{}).ImageCapability("grok-imagine-image").Edit)
	assert.False(t, (&Adaptor{}).ImageCapability("grok-2-image-1212").Edit)
}
