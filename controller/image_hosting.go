package controller

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

func GetImageHostingConfiguration(c *gin.Context) {
	cfg, err := service.GetImageHostingConfig(c.Request.Context())
	if err != nil {
		imageManagementError(c, http.StatusServiceUnavailable, errors.New("Image hosting configuration is unavailable"))
		return
	}
	imageManagementData(c, struct {
		service.ImageHostingConfig
		Runtime service.ImageHostingRuntimeStatus `json:"runtime"`
	}{cfg, service.GetImageHostingRuntimeStatus(cfg)})
}

func UpdateImageHostingConfiguration(c *gin.Context) {
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || contentType != "application/json" {
		imageManagementError(c, http.StatusUnsupportedMediaType, errors.New("Image hosting configuration requires application/json"))
		return
	}
	// Cross-site cookie requests must never replace an administrator's upload
	// credential. Non-browser API clients may omit Origin and still need RootAuth.
	if c.GetHeader("Sec-Fetch-Site") == "cross-site" {
		imageManagementError(c, http.StatusForbidden, errors.New("Cross-site image hosting configuration is not allowed"))
		return
	}
	if origin := c.GetHeader("Origin"); origin != "" {
		parsed, parseErr := url.Parse(origin)
		configured, _ := url.Parse(system_setting.ServerAddress)
		expectedScheme := "http"
		if c.Request.TLS != nil {
			expectedScheme = "https"
		}
		if configured != nil && strings.EqualFold(configured.Host, c.Request.Host) && (configured.Scheme == "http" || configured.Scheme == "https") {
			expectedScheme = configured.Scheme
		}
		valid := parseErr == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/") && (parsed.Scheme == "https" || parsed.Scheme == "http") && (strings.EqualFold(parsed.Host, c.Request.Host) && parsed.Scheme == expectedScheme || configured != nil && strings.EqualFold(parsed.Host, configured.Host) && strings.EqualFold(parsed.Scheme, configured.Scheme))
		if !valid {
			imageManagementError(c, http.StatusForbidden, errors.New("Cross-site image hosting configuration is not allowed"))
			return
		}
	}
	input := struct {
		service.ImageHostingConfig
		Revision     *int64 `json:"revision"`
		Token        string `json:"token"`
		ClearToken   bool   `json:"clear_token"`
		VideoEnabled bool   `json:"video_enabled"`
	}{ImageHostingConfig: service.DefaultImageHostingConfig()}
	input.ExtraFields = nil
	if err := common.DecodeJson(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20), &input); err != nil {
		imageManagementError(c, http.StatusBadRequest, errors.New("Invalid image hosting configuration"))
		return
	}
	if input.VideoEnabled {
		imageManagementError(c, http.StatusBadRequest, errors.New("External video uploads are not supported; videos use local storage"))
		return
	}
	if input.Revision == nil {
		imageManagementError(c, http.StatusPreconditionRequired, errors.New("Configuration revision is required. Reload it before saving"))
		return
	}
	cfg, err := service.SaveImageHostingConfig(c.Request.Context(), input.ImageHostingConfig, input.Token, input.ClearToken, *input.Revision)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, model.ErrOptionRevisionConflict) {
			status = http.StatusConflict
		}
		imageManagementError(c, status, err)
		return
	}
	model.RecordAuditLog(c, model.AuditLog{
		UserId: c.GetInt("id"), Username: c.GetString("username"), ActorRole: c.GetInt("role"),
		Category: model.AuditCategorySecurity, Action: "image_hosting.update", Content: "Image hosting configuration updated", Success: true,
		Other: model.AuditOther{Op: &model.AuditOperation{Action: "image_hosting.update", Params: model.AuditFields{"revision": cfg.Revision, "enabled": cfg.Enabled, "image_enabled": cfg.ImageEnabled, "token_replaced": input.Token != "", "token_cleared": input.ClearToken}}},
	})
	imageManagementData(c, struct {
		service.ImageHostingConfig
		Runtime service.ImageHostingRuntimeStatus `json:"runtime"`
	}{cfg, service.GetImageHostingRuntimeStatus(cfg)})
}
