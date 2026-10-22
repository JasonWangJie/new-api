package system_setting

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
	"golang.org/x/net/idna"
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

// CanvasOrigin is the single representation used for configured redirects and
// grant fingerprints. It accepts an origin, never a caller-selected path.
func CanvasOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || strings.ContainsAny(value, "\\#") || strings.ContainsFunc(value, unicode.IsSpace) {
		return "", errors.New("invalid Infinite Canvas URL")
	}
	hostname := strings.ToLower(u.Hostname())
	loopback := false
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if ip.Zone() != "" {
			return "", errors.New("invalid Infinite Canvas URL")
		}
		loopback = ip.Unmap().IsLoopback()
		hostname = ip.String()
		if ip.Is4In6() {
			// Browser URL origins render mapped IPv4 addresses in hexadecimal.
			bytes := ip.As16()
			hostname = fmt.Sprintf("::ffff:%x:%x", uint16(bytes[12])<<8|uint16(bytes[13]), uint16(bytes[14])<<8|uint16(bytes[15]))
		}
	} else {
		hostname, err = idna.Lookup.ToASCII(hostname)
		if err != nil || hostname == "" || strings.HasPrefix(u.Host, "[") || strings.ContainsAny(hostname, "%[]:") {
			return "", errors.New("invalid Infinite Canvas URL")
		}
		lastLabel := strings.TrimSuffix(hostname, ".")
		if index := strings.LastIndexByte(lastLabel, '.'); index >= 0 {
			lastLabel = lastLabel[index+1:]
		}
		hexLabel, hexadecimal := strings.CutPrefix(lastLabel, "0x")
		if strings.Trim(lastLabel, "0123456789") == "" || hexadecimal && hexLabel != "" && strings.Trim(hexLabel, "0123456789abcdef") == "" {
			// Reject abbreviated/hexadecimal IPs that browsers reinterpret.
			return "", errors.New("invalid Infinite Canvas URL")
		}
	}
	if u.Scheme != "https" {
		if u.Scheme != "http" || hostname != "localhost" && !loopback {
			return "", errors.New("Infinite Canvas requires HTTPS, except loopback development")
		}
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 0 || number > 65535 {
			return "", errors.New("invalid Infinite Canvas URL")
		}
		port = strconv.Itoa(number)
		if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
			port = ""
		}
	}
	u.Host = hostname
	if strings.Contains(hostname, ":") {
		u.Host = "[" + hostname + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(hostname, port)
	}
	u.Path = ""
	u.RawPath = ""
	return u.String(), nil
}

// CanvasCallbackURL accepts a fixed deployment base, never a caller-selected redirect.
func CanvasCallbackURL(settings InfiniteCanvasSettings) (string, error) {
	origin, err := CanvasOrigin(settings.URL)
	if err != nil {
		return "", err
	}
	return origin + "/auth/callback", nil
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
