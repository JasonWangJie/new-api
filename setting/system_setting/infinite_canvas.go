package system_setting

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

type InfiniteCanvasSettings struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	ClientID string `json:"client_id"`
}

var infiniteCanvasSettings = InfiniteCanvasSettings{ClientID: "infinite-canvas"}

func init() {
	config.GlobalConfig.Register("infinite_canvas", &infiniteCanvasSettings)
}

func InfiniteCanvasSettingsSnapshot() InfiniteCanvasSettings {
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()
	return infiniteCanvasSettings
}

// CanvasCallbackURL accepts a fixed deployment base, never a caller-selected redirect.
func CanvasCallbackURL(settings InfiniteCanvasSettings) (string, error) {
	u, err := url.Parse(settings.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || strings.ContainsAny(settings.URL, "\\\r\n\t #") {
		return "", errors.New("invalid Infinite Canvas URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("Infinite Canvas requires HTTPS, except loopback development")
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/auth/callback"
	u.RawPath = ""
	return u.String(), nil
}

func CanvasClientSecret() string { return os.Getenv("INFINITE_CANVAS_CLIENT_SECRET") }

func ValidateInfiniteCanvasOption(key, value string) error {
	switch key {
	case "infinite_canvas.url":
		if value == "" {
			return nil
		}
		_, err := CanvasCallbackURL(InfiniteCanvasSettings{URL: value})
		return err
	case "infinite_canvas.client_id":
		if len(value) == 0 || len(value) > 64 || strings.ContainsAny(value, ":/\\\r\n\t ") {
			return errors.New("invalid Infinite Canvas client id")
		}
	case "infinite_canvas.enabled":
		if value != "true" && value != "false" {
			return errors.New("Infinite Canvas enabled must be boolean")
		}
		if value == "true" {
			settings := InfiniteCanvasSettingsSnapshot()
			if _, err := CanvasCallbackURL(settings); err != nil {
				return err
			}
			if len(CanvasClientSecret()) < 32 || !common.RedisEnabled || common.RDB == nil {
				return errors.New("Infinite Canvas requires Redis and a client secret of at least 32 characters")
			}
		}
	default:
		if strings.HasPrefix(key, "infinite_canvas.") {
			return errors.New("unsupported Infinite Canvas setting")
		}
	}
	return nil
}
