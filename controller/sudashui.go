package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
)

type sudashuiModelDetail struct {
	dto.TaskVideoModelConfig
	DisplayName string   `json:"display_name,omitempty"`
	Price       *float64 `json:"price,omitempty"`
	FinalPrice  *float64 `json:"final_price,omitempty"`
	Currency    string   `json:"currency,omitempty"`
}

func refreshTaskVideoRouting() {
	if err := syncTaskPluginsOnceContext(context.Background()); err != nil {
		common.SysError("refresh task video routing: " + err.Error())
	}
}

// fetchChannelModelPreview is only used by explicit administrator fetch APIs.
// Background upstream checks continue to read the saved model list.
func fetchChannelModelPreview(ctx context.Context, channel *model.Channel) ([]string, map[string]sudashuiModelDetail, error) {
	settings := channel.GetSetting()
	if channel.Type != constant.ChannelTypeTaskPlugin || settings.TaskPluginKey != "sudashui" {
		ids, err := fetchChannelUpstreamModelIDs(channel)
		return ids, nil, err
	}
	address := sudashuiCatalogURL(settings.TaskPluginModelSourceURL)
	if err := settings.ValidateTaskVideoModels(relaycommon.MaxTaskDurationSeconds); err != nil {
		return nil, nil, err
	}
	if err := service.ValidateSSRFProtectedFetchURL(address); err != nil {
		return nil, nil, err
	}
	client, err := service.NewProxyHttpClient(settings.Proxy)
	if err != nil {
		return nil, nil, err
	}
	if settings.Proxy == "" {
		client = service.GetSSRFProtectedHTTPClient()
	}
	if client == nil {
		return nil, nil, fmt.Errorf("HTTP client is not initialized")
	}
	requestClient := *client
	requestClient.Timeout = 30 * time.Second
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	key := ""
	for index, candidate := range channel.GetKeys() {
		status := channel.ChannelInfo.MultiKeyStatusList[index]
		if status == 0 || status == common.ChannelStatusEnabled {
			key = candidate
			break
		}
	}
	if key == "" {
		return nil, nil, fmt.Errorf("no usable catalog API key")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := requestClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("model catalog request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("model catalog returned HTTP %d", resp.StatusCode)
	}
	const maxCatalogBytes = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil || len(body) > maxCatalogBytes {
		return nil, nil, fmt.Errorf("model catalog response is unreadable or too large")
	}
	var catalog struct {
		Success *bool `json:"success"`
		Data    []struct {
			ID   string `json:"id"`
			Info struct {
				sudashuiModelDetail
				Modality   string `json:"modality"`
				Resolution string `json:"resolution"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := common.Unmarshal(body, &catalog); err != nil || catalog.Data == nil || catalog.Success != nil && !*catalog.Success {
		return nil, nil, fmt.Errorf("invalid model catalog response")
	}
	details := map[string]sudashuiModelDetail{}
	ids := []string{}
	for _, entry := range catalog.Data {
		if entry.Info.Modality != "video" {
			continue
		}
		if entry.ID == "" || entry.ID != strings.TrimSpace(entry.ID) || len(entry.ID) > 255 || strings.ContainsAny(entry.ID, ",*\r\n") {
			return nil, nil, fmt.Errorf("invalid catalog model name")
		}
		if _, duplicate := details[entry.ID]; duplicate {
			return nil, nil, fmt.Errorf("duplicate catalog model name")
		}
		detail := entry.Info.sudashuiModelDetail
		if len(detail.Resolutions) == 0 && entry.Info.Resolution != "" {
			detail.Resolutions = []string{entry.Info.Resolution}
		}
		// The catalog does not publish modes reliably. Do not guess them.
		details[entry.ID] = detail
		ids = append(ids, entry.ID)
	}
	return normalizeModelNames(ids), details, nil
}

func sudashuiCatalogURL(address string) string {
	if address == "" {
		return dto.DefaultSudashuiCatalogURL
	}
	return address
}

func buildSudashuiModelPreviewChannel(req fetchModelsRequest) (*model.Channel, error) {
	key, _, _ := strings.Cut(strings.TrimSpace(req.Key), "\n")
	key = strings.TrimSpace(key)
	channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin}
	var savedSettings dto.ChannelSettings
	if req.ChannelID > 0 {
		saved, err := model.GetChannelById(req.ChannelID, key == "")
		if err != nil {
			return nil, err
		}
		savedSettings = saved.GetSetting()
		if saved.Type != constant.ChannelTypeTaskPlugin || savedSettings.TaskPluginKey != "sudashui" {
			return nil, fmt.Errorf("channel is not a Sudashui channel")
		}
		channel = saved
	}
	if key != "" {
		channel.Key = key
		channel.ChannelInfo = model.ChannelInfo{}
	}
	settings := savedSettings
	settings.TaskPluginKey = "sudashui"
	if req.TaskPluginModelSourceURL != nil {
		settings.TaskPluginModelSourceURL = strings.TrimSpace(*req.TaskPluginModelSourceURL)
	}
	if req.Proxy != nil {
		settings.Proxy = strings.TrimSpace(*req.Proxy)
	}
	if req.ChannelID > 0 && key == "" &&
		(sudashuiCatalogURL(settings.TaskPluginModelSourceURL) != sudashuiCatalogURL(savedSettings.TaskPluginModelSourceURL) || settings.Proxy != savedSettings.Proxy) {
		return nil, fmt.Errorf("enter an API key to preview a different model catalog URL or proxy")
	}
	// Preview reads the catalog without using or saving the channel's capability configuration.
	settings.TaskPluginModelConfigs = nil
	channel.SetSetting(settings)
	return channel, nil
}
