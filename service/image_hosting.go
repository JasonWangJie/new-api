package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
	"gorm.io/gorm"
)

const imageHostingOption = "image_hosting_config"

var (
	ErrImageHostingDisabled    = errors.New("image hosting is disabled")
	ErrImageHostingCircuitOpen = errors.New("image hosting circuit is open")
	ErrImageHostingBusy        = errors.New("image hosting upload capacity is busy")
	ErrImageHostingUpload      = errors.New("image hosting upload failed")
)

// ImageHostingConfig contains no usable credential. The token is encrypted in
// the option envelope and can only be replaced or cleared by an administrator.
type ImageHostingConfig struct {
	Revision            int64          `json:"revision"`
	Enabled             bool           `json:"enabled"`
	ImageEnabled        bool           `json:"image_enabled"`
	BaseURL             string         `json:"base_url"`
	FileUploadPath      string         `json:"file_upload_path"`
	URLUploadPath       string         `json:"url_upload_path"`
	FileUploadMethod    string         `json:"file_upload_method"`
	URLUploadMethod     string         `json:"url_upload_method"`
	FileField           string         `json:"file_field"`
	URLField            string         `json:"url_field"`
	ResponseURLPath     string         `json:"response_url_path"`
	TokenHeader         string         `json:"token_header"`
	TokenPrefix         string         `json:"token_prefix"`
	ExtraFields         map[string]any `json:"extra_fields"`
	TimeoutSeconds      int            `json:"timeout_seconds"`
	Retries             int            `json:"retries"`
	Concurrency         int            `json:"concurrency"`
	FailureThreshold    int            `json:"failure_threshold"`
	CooldownSeconds     int            `json:"cooldown_seconds"`
	URLFallback         string         `json:"url_fallback"`
	AllowPrivateNetwork bool           `json:"allow_private_network"`
	HasToken            bool           `json:"has_token"`
}

type imageHostingEnvelope struct {
	ImageHostingConfig
	TokenCipher []byte `json:"token_cipher,omitempty"`
}

func DefaultImageHostingConfig() ImageHostingConfig {
	return ImageHostingConfig{ImageEnabled: true, FileUploadPath: "/api/v1/upload", URLUploadPath: "/api/v1/upload/url", FileUploadMethod: http.MethodPost, URLUploadMethod: http.MethodPost, FileField: "file", URLField: "url", ResponseURLPath: "data.links.url", TokenHeader: "Authorization", TokenPrefix: "Bearer ", ExtraFields: map[string]any{"visibility": "public"}, TimeoutSeconds: 20, Retries: 1, Concurrency: 4, FailureThreshold: 3, CooldownSeconds: 60, URLFallback: "local"}
}

func getImageHostingEnvelope(ctx context.Context) (imageHostingEnvelope, error) {
	envelope := imageHostingEnvelope{ImageHostingConfig: DefaultImageHostingConfig()}
	var option model.Option
	err := model.DB.WithContext(ctx).Where(map[string]any{"key": imageHostingOption}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return envelope, nil
	}
	if err != nil {
		return envelope, err
	}
	// JSON object decoding merges into existing maps; an explicitly empty map
	// must not silently restore the default public visibility on a saved config.
	envelope.ExtraFields = nil
	if err := common.UnmarshalJsonStr(option.Value, &envelope); err != nil {
		return envelope, errors.New("image hosting configuration is invalid")
	}
	envelope.HasToken = len(envelope.TokenCipher) > 0
	return envelope, envelope.ImageHostingConfig.Validate()
}

func GetImageHostingConfig(ctx context.Context) (ImageHostingConfig, error) {
	envelope, err := getImageHostingEnvelope(ctx)
	return envelope.ImageHostingConfig, err
}

func (cfg ImageHostingConfig) Validate() error {
	if cfg.Revision < 0 || cfg.Revision > model.MaxOptionRevision {
		return model.ErrOptionRevisionInvalid
	}
	if cfg.BaseURL != "" {
		parsed, err := url.Parse(cfg.BaseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || len(cfg.BaseURL) > 2048 {
			return errors.New("image hosting requires an HTTPS base URL without credentials or query")
		}
		if ip, err := netip.ParseAddr(parsed.Hostname()); err == nil && !imageHostingAddressAllowed(ip, cfg.AllowPrivateNetwork) {
			return errors.New("image hosting address is not allowed")
		}
	} else if cfg.Enabled {
		return errors.New("image hosting base URL is required")
	}
	for _, endpoint := range []string{cfg.FileUploadPath, cfg.URLUploadPath} {
		parsed, err := url.Parse(endpoint)
		if err != nil || !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(endpoint, "\r\n\\") || len(endpoint) > 512 {
			return errors.New("image hosting upload paths must be relative paths starting with a slash")
		}
	}
	for _, method := range []string{cfg.FileUploadMethod, cfg.URLUploadMethod} {
		if method != http.MethodPost && method != http.MethodPut {
			return errors.New("image hosting upload methods must be POST or PUT")
		}
	}
	for _, field := range []string{cfg.FileField, cfg.URLField} {
		if !httpguts.ValidHeaderFieldName(field) || len(field) > 64 {
			return errors.New("image hosting upload field names are invalid")
		}
	}
	if !httpguts.ValidHeaderFieldName(cfg.TokenHeader) || len(cfg.TokenHeader) > 64 || !httpguts.ValidHeaderFieldValue(cfg.TokenPrefix) || len(cfg.TokenPrefix) > 64 || slices.Contains([]string{"host", "cookie", "set-cookie", "content-length", "content-type", "connection", "transfer-encoding", "proxy-authorization", "proxy-authenticate"}, strings.ToLower(cfg.TokenHeader)) || strings.HasPrefix(strings.ToLower(cfg.TokenHeader), "x-forwarded-") {
		return errors.New("image hosting token header or prefix is invalid")
	}
	if cfg.ResponseURLPath == "" || len(cfg.ResponseURLPath) > 128 || strings.ContainsAny(cfg.ResponseURLPath, "\r\n @#|!") {
		return errors.New("image hosting response URL path is invalid")
	}
	if len(cfg.ExtraFields) > 16 {
		return errors.New("too many image hosting extra fields")
	}
	for name, value := range cfg.ExtraFields {
		sensitiveName := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
		fieldValue, err := imageHostingFormValue(value)
		if err != nil || !httpguts.ValidHeaderFieldName(name) || len(name) > 64 || len(fieldValue) > 4096 || name == cfg.FileField || name == cfg.URLField || strings.ContainsAny(fieldValue, "\x00\r\n") || strings.Contains(sensitiveName, "token") || strings.Contains(sensitiveName, "secret") || strings.Contains(sensitiveName, "password") || strings.Contains(sensitiveName, "api_key") || strings.Contains(sensitiveName, "access_key") || strings.Contains(sensitiveName, "authorization") || strings.Contains(sensitiveName, "credential") {
			return errors.New("image hosting extra fields are invalid")
		}
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 120 || cfg.Retries < 0 || cfg.Retries > 3 || cfg.Concurrency < 1 || cfg.Concurrency > 32 || cfg.FailureThreshold < 1 || cfg.FailureThreshold > 100 || cfg.CooldownSeconds < 1 || cfg.CooldownSeconds > 3600 {
		return errors.New("image hosting timeout, retry or circuit limits are invalid")
	}
	if cfg.URLFallback != "local" && cfg.URLFallback != "upstream" {
		return errors.New("image hosting URL fallback must be local or upstream")
	}
	return nil
}

func imageHostingFormValue(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	if number, ok := value.(float64); ok && (math.IsNaN(number) || math.IsInf(number, 0)) {
		return "", errors.New("image hosting extra fields require finite scalar values")
	}
	if number, ok := value.(float32); ok && (math.IsNaN(float64(number)) || math.IsInf(float64(number), 0)) {
		return "", errors.New("image hosting extra fields require finite scalar values")
	}
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		data, err := common.Marshal(value)
		if err != nil {
			return "", errors.New("image hosting extra fields require finite scalar values")
		}
		return string(data), nil
	default:
		return "", errors.New("image hosting extra fields require string, number or boolean values")
	}
}

func SaveImageHostingConfig(ctx context.Context, cfg ImageHostingConfig, token string, clearToken bool, expectedRevision int64) (ImageHostingConfig, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.TokenHeader = http.CanonicalHeaderKey(cfg.TokenHeader)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	if token != "" && (len(token) > 4096 || !httpguts.ValidHeaderFieldValue(token) || strings.ContainsAny(token, "\r\n")) || token != "" && clearToken {
		return cfg, errors.New("image hosting token is invalid")
	}
	previous, err := getImageHostingEnvelope(ctx)
	if err != nil {
		return cfg, err
	}
	if previous.Revision != expectedRevision {
		return cfg, model.ErrOptionRevisionConflict
	}
	if previous.HasToken && token == "" && !clearToken && (previous.BaseURL != cfg.BaseURL || previous.FileUploadPath != cfg.FileUploadPath || previous.URLUploadPath != cfg.URLUploadPath) {
		return cfg, errors.New("replace the image hosting token when changing upload endpoints")
	}
	envelope := imageHostingEnvelope{ImageHostingConfig: cfg, TokenCipher: previous.TokenCipher}
	if clearToken {
		envelope.TokenCipher = nil
	}
	if token != "" {
		envelope.TokenCipher, err = EncryptImagePayload([]byte(token), "image-hosting-token:"+cfg.BaseURL)
		if err != nil {
			return cfg, err
		}
	}
	envelope.HasToken = len(envelope.TokenCipher) > 0
	if cfg.Enabled && !envelope.HasToken {
		return cfg, errors.New("image hosting upload token is required")
	}
	data, err := common.Marshal(envelope)
	if err != nil {
		return cfg, err
	}
	revision, err := model.SaveVersionedOption(ctx, imageHostingOption, string(data), &expectedRevision)
	if err != nil {
		return cfg, err
	}
	envelope.Revision = revision
	return envelope.ImageHostingConfig, nil
}

func ValidateHostedImageURL(raw string) (*url.URL, error) {
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, errors.New("image hosting returned an invalid image URL")
	}
	return ValidateImageReferenceURL(raw)
}

func imageHostingAddressAllowed(ip netip.Addr, allowPrivate bool) bool {
	return ImagePublicIP(ip) || allowPrivate && (ip.IsPrivate() || ip.IsLoopback())
}

// This circuit is node-local and shared by all image workers on this node.
// A revision change opens a fresh generation; late completions cannot mutate it.
var imageHostingRuntime = struct {
	sync.Mutex
	key       string
	epoch     uint64
	failures  int
	inFlight  int
	openUntil time.Time
	probe     bool
}{}

type imageHostingUploadClaim struct {
	key   string
	epoch uint64
	probe bool
}

type ImageHostingRuntimeStatus struct {
	State             string `json:"state"`
	Failures          int    `json:"failures"`
	InFlight          int    `json:"in_flight"`
	RetryAfterSeconds int    `json:"retry_after_seconds"`
}

func GetImageHostingRuntimeStatus(configs ...ImageHostingConfig) ImageHostingRuntimeStatus {
	imageHostingRuntime.Lock()
	defer imageHostingRuntime.Unlock()
	if len(configs) > 0 && imageHostingRuntime.key != fmt.Sprintf("%d:%s", configs[0].Revision, configs[0].BaseURL) {
		return ImageHostingRuntimeStatus{State: "closed"}
	}
	status := ImageHostingRuntimeStatus{State: "closed", Failures: imageHostingRuntime.failures, InFlight: imageHostingRuntime.inFlight}
	if !imageHostingRuntime.openUntil.IsZero() {
		status.State = "open"
		status.RetryAfterSeconds = max(0, int(time.Until(imageHostingRuntime.openUntil).Seconds())+1)
		if imageHostingRuntime.probe || time.Now().After(imageHostingRuntime.openUntil) {
			status.State = "half_open"
		}
	}
	return status
}

func acquireImageHostingUpload(cfg ImageHostingConfig) (imageHostingUploadClaim, error) {
	key := fmt.Sprintf("%d:%s", cfg.Revision, cfg.BaseURL)
	imageHostingRuntime.Lock()
	defer imageHostingRuntime.Unlock()
	if imageHostingRuntime.key != key {
		imageHostingRuntime.key, imageHostingRuntime.failures, imageHostingRuntime.inFlight = key, 0, 0
		imageHostingRuntime.openUntil, imageHostingRuntime.probe = time.Time{}, false
		imageHostingRuntime.epoch++
	}
	if !imageHostingRuntime.openUntil.IsZero() {
		if time.Now().Before(imageHostingRuntime.openUntil) || imageHostingRuntime.probe {
			return imageHostingUploadClaim{}, ErrImageHostingCircuitOpen
		}
	}
	if imageHostingRuntime.inFlight >= cfg.Concurrency {
		return imageHostingUploadClaim{}, ErrImageHostingBusy
	}
	if !imageHostingRuntime.openUntil.IsZero() {
		imageHostingRuntime.probe = true
	}
	imageHostingRuntime.inFlight++
	return imageHostingUploadClaim{key: key, epoch: imageHostingRuntime.epoch, probe: imageHostingRuntime.probe}, nil
}

func completeImageHostingUpload(claim imageHostingUploadClaim, cfg ImageHostingConfig, success, canceled bool) {
	imageHostingRuntime.Lock()
	defer imageHostingRuntime.Unlock()
	if imageHostingRuntime.key != claim.key {
		return
	}
	imageHostingRuntime.inFlight--
	if imageHostingRuntime.epoch != claim.epoch {
		return
	}
	if claim.probe {
		imageHostingRuntime.probe = false
	}
	if canceled {
		return
	}
	if success {
		imageHostingRuntime.failures, imageHostingRuntime.openUntil = 0, time.Time{}
		if claim.probe {
			imageHostingRuntime.epoch++
		}
		return
	}
	imageHostingRuntime.failures++
	if imageHostingRuntime.failures >= cfg.FailureThreshold {
		imageHostingRuntime.openUntil = time.Now().Add(time.Duration(cfg.CooldownSeconds) * time.Second)
		imageHostingRuntime.epoch++
	}
}

func imageHostingHTTPClient(cfg ImageHostingConfig) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(cfg.TimeoutSeconds) * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("image hosting address lookup failed")
		}
		for _, ip := range addresses {
			if !imageHostingAddressAllowed(ip, cfg.AllowPrivateNetwork) {
				return nil, errors.New("image hosting address is not allowed")
			}
		}
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err != nil {
				continue
			}
			peer, err := netip.ParseAddrPort(conn.RemoteAddr().String())
			if err != nil || peer.Addr().Unmap() != ip.Unmap() {
				_ = conn.Close()
				return nil, errors.New("image hosting connected to an unexpected address")
			}
			return conn, nil
		}
		return nil, errors.New("image hosting connection failed")
	}}
	return &http.Client{Transport: transport, Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func imageHostingProbeHTTPClient(cfg ImageHostingConfig) *http.Client {
	cfg.AllowPrivateNetwork = false
	client := imageHostingHTTPClient(cfg)
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("image hosting direct-link redirect limit exceeded")
		}
		_, err := ValidateHostedImageURL(request.URL.String())
		return err
	}
	return client
}

// UploadHostedImage only uploads an already validated generation result. Provider
// headers are deliberately absent: private upstream URLs must use the file path.
// The parent operation has one bounded deadline across all retry attempts.
func UploadHostedImage(ctx context.Context, stableIdentity, sourceURL string, image ImageBytes) (resultURL string, resultErr error) {
	envelope, err := getImageHostingEnvelope(ctx)
	if err != nil {
		return "", err
	}
	cfg := envelope.ImageHostingConfig
	if !cfg.Enabled || !cfg.ImageEnabled {
		return "", ErrImageHostingDisabled
	}
	if len(image.Data) == 0 || int64(len(image.Data)) > 64<<20 || !strings.HasPrefix(image.ContentType, "image/") {
		return "", ErrImageHostingUpload
	}
	token, err := DecryptImagePayload(envelope.TokenCipher, "image-hosting-token:"+cfg.BaseURL)
	if err != nil {
		return "", ErrImageHostingUpload
	}
	client := imageHostingHTTPClient(cfg)
	defer client.CloseIdleConnections()
	probeClient := imageHostingProbeHTTPClient(cfg)
	defer probeClient.CloseIdleConnections()
	return uploadHostedImage(ctx, cfg, string(token), stableIdentity, sourceURL, image, client, probeClient)
}

func uploadHostedImage(ctx context.Context, cfg ImageHostingConfig, token, stableIdentity, sourceURL string, image ImageBytes, client, probeClient *http.Client) (resultURL string, resultErr error) {
	key, err := acquireImageHostingUpload(cfg)
	if err != nil {
		return "", err
	}
	defer func() { completeImageHostingUpload(key, cfg, resultErr == nil, ctx.Err() != nil) }()
	uploadCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	useURL := sourceURL != "" && PublicUpstreamAsyncResultURL(sourceURL)
	if useURL {
		_, err = ValidateImageReferenceURL(sourceURL)
		useURL = err == nil
	}
	digest := sha256.Sum256([]byte(stableIdentity + ":" + image.Checksum))
	idempotencyKey := fmt.Sprintf("new-api-image-%x", digest[:])
	for attempt := range cfg.Retries + 1 {
		var body bytes.Buffer
		method, endpoint, contentType := cfg.FileUploadMethod, cfg.FileUploadPath, ""
		if useURL {
			payload := make(map[string]any, len(cfg.ExtraFields)+1)
			for name, value := range cfg.ExtraFields {
				payload[name] = value
			}
			payload[cfg.URLField] = sourceURL
			data, marshalErr := common.Marshal(payload)
			if marshalErr != nil {
				return "", ErrImageHostingUpload
			}
			_, _ = body.Write(data)
			method, endpoint, contentType = cfg.URLUploadMethod, cfg.URLUploadPath, "application/json"
		} else {
			writer := multipart.NewWriter(&body)
			for name, value := range cfg.ExtraFields {
				fieldValue, err := imageHostingFormValue(value)
				if err != nil {
					return "", ErrImageHostingUpload
				}
				if err := writer.WriteField(name, fieldValue); err != nil {
					return "", ErrImageHostingUpload
				}
			}
			filename := "image" + imageStorageExtension(image.ContentType)
			part, err := writer.CreateFormFile(cfg.FileField, filename)
			if err != nil {
				return "", ErrImageHostingUpload
			}
			if _, err := part.Write(image.Data); err != nil {
				return "", ErrImageHostingUpload
			}
			if err := writer.Close(); err != nil {
				return "", ErrImageHostingUpload
			}
			contentType = writer.FormDataContentType()
		}
		request, err := http.NewRequestWithContext(uploadCtx, method, strings.TrimRight(cfg.BaseURL, "/")+endpoint, &body)
		if err != nil {
			return "", ErrImageHostingUpload
		}
		request.Header.Set("Content-Type", contentType)
		request.Header.Set(cfg.TokenHeader, cfg.TokenPrefix+token)
		request.Header.Set("Idempotency-Key", idempotencyKey)
		response, err := client.Do(request)
		retryable := err != nil
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
			_ = response.Body.Close()
			retryable = response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
			if response.StatusCode >= 200 && response.StatusCode < 300 && readErr == nil && len(data) <= 1<<20 && gjson.ValidBytes(data) {
				success, errorValue, code := gjson.GetBytes(data, "success"), gjson.GetBytes(data, "error"), gjson.GetBytes(data, "code")
				if success.Type == gjson.False || errorValue.Exists() && errorValue.Type != gjson.Null && errorValue.Type != gjson.False && errorValue.String() != "" || code.Exists() && !slices.Contains([]string{"0", "200", "ok", "success"}, strings.ToLower(code.String())) {
					return "", ErrImageHostingUpload
				}
				candidate := gjson.GetBytes(data, cfg.ResponseURLPath)
				if candidate.Type == gjson.String {
					if _, err := ValidateHostedImageURL(candidate.String()); err == nil && probeHostedImage(uploadCtx, probeClient, candidate.String()) == nil {
						return candidate.String(), nil
					}
				}
			}
		}
		if !retryable || attempt == cfg.Retries || uploadCtx.Err() != nil {
			return "", ErrImageHostingUpload
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-uploadCtx.Done():
			timer.Stop()
			return "", ErrImageHostingUpload
		case <-timer.C:
		}
	}
	return "", ErrImageHostingUpload
}

// Probe without upload credentials: a successful upload can still return a
// private image, review placeholder, expired link or an HTML share page.
func probeHostedImage(ctx context.Context, client *http.Client, raw string) error {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		request, err := http.NewRequestWithContext(ctx, method, raw, nil)
		if err != nil {
			return ErrImageHostingUpload
		}
		if method == http.MethodGet {
			request.Header.Set("Range", "bytes=0-511")
		}
		response, err := client.Do(request)
		if err != nil {
			return ErrImageHostingUpload
		}
		if method == http.MethodHead && response.StatusCode == http.StatusMethodNotAllowed {
			_ = response.Body.Close()
			continue
		}
		contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
		valid := (response.StatusCode == http.StatusOK || method == http.MethodGet && response.StatusCode == http.StatusPartialContent) && parseErr == nil && slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif", "image/svg+xml"}, contentType) && response.ContentLength != 0 && response.ContentLength <= 64<<20
		if valid && method == http.MethodGet {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 512))
			valid = readErr == nil && len(data) > 0
		}
		_ = response.Body.Close()
		if valid {
			return nil
		}
		return ErrImageHostingUpload
	}
	return ErrImageHostingUpload
}

func imageStorageExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	default:
		return ".png"
	}
}
