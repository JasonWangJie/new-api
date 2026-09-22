package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	_ "golang.org/x/image/webp"
)

type ImageBytes struct {
	Data        []byte
	ContentType string
	Checksum    string
	Width       int
	Height      int
}

const maxNormalizedImageTrailingBytes = 4 << 10

type imageValidationError struct{ message string }

func (err *imageValidationError) Error() string { return err.message }

func newImageValidationError(message string) error {
	return &imageValidationError{message: message}
}

func IsImageValidationError(err error) bool {
	var validationError *imageValidationError
	return errors.As(err, &validationError)
}

func ValidateImageBytes(data []byte, declared string, maxBytes, maxPixels int64) (ImageBytes, error) {
	if len(data) == 0 {
		return ImageBytes{}, newImageValidationError("empty image")
	}
	if maxBytes <= 0 || int64(len(data)) > maxBytes {
		return ImageBytes{}, newImageValidationError("image byte limit exceeded")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return ImageBytes{}, newImageValidationError("invalid image header")
	}
	if maxPixels <= 0 || int64(cfg.Height) > maxPixels || int64(cfg.Width) > maxPixels/int64(cfg.Height) {
		return ImageBytes{}, newImageValidationError("image pixel limit exceeded")
	}
	contentType := ""
	switch format {
	case "png":
		contentType = "image/png"
	case "jpeg":
		contentType = "image/jpeg"
	case "webp":
		contentType = "image/webp"
	default:
		return ImageBytes{}, newImageValidationError("only PNG, JPEG and WebP images are supported")
	}
	canonical, ok := canonicalImageContainer(data, format)
	if !ok {
		return ImageBytes{}, newImageValidationError("invalid image container or excessive trailing data")
	}
	if len(canonical) != len(data) {
		canonical = bytes.Clone(canonical)
	}
	if declared != "" {
		actual, _, err := mime.ParseMediaType(declared)
		if err != nil || !strings.EqualFold(actual, contentType) {
			return ImageBytes{}, newImageValidationError("declared image MIME does not match its bytes")
		}
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(canonical))
	if err != nil || decodedFormat != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return ImageBytes{}, newImageValidationError("image could not be fully decoded")
	}
	sum := sha256.Sum256(canonical)
	return ImageBytes{Data: canonical, ContentType: contentType, Checksum: hex.EncodeToString(sum[:]), Width: cfg.Width, Height: cfg.Height}, nil
}

// canonicalImageContainer only removes a short suffix after a fully parsed
// terminal marker. The normalized bytes are decoded again before acceptance,
// so appended payloads are discarded instead of becoming part of stored or
// upstream image data.
func canonicalImageContainer(data []byte, format string) ([]byte, bool) {
	if format == "webp" {
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return nil, false
		}
		end := uint64(binary.LittleEndian.Uint32(data[4:8])) + 8
		if end < 12 || end > uint64(len(data)) || uint64(len(data))-end > maxNormalizedImageTrailingBytes {
			return nil, false
		}
		return data[:int(end)], true
	}
	if format == "png" {
		if len(data) < 20 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return nil, false
		}
		for offset := 8; offset+12 <= len(data); {
			length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
			if length > uint64(len(data)-offset-12) {
				return nil, false
			}
			end := offset + 12 + int(length)
			if string(data[offset+4:offset+8]) == "IEND" {
				if length != 0 || len(data)-end > maxNormalizedImageTrailingBytes {
					return nil, false
				}
				return data[:end], true
			}
			offset = end
		}
		return nil, false
	}
	if format != "jpeg" || len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, false
	}
	position := 2
	for position < len(data) {
		if data[position] != 0xff {
			return nil, false
		}
		for position < len(data) && data[position] == 0xff {
			position++
		}
		if position >= len(data) {
			return nil, false
		}
		marker := data[position]
		position++
		if marker == 0xd9 {
			if len(data)-position > maxNormalizedImageTrailingBytes {
				return nil, false
			}
			return data[:position], true
		}
		if marker == 0x00 || marker == 0xd8 {
			return nil, false
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		if position+2 > len(data) {
			return nil, false
		}
		length := int(binary.BigEndian.Uint16(data[position : position+2]))
		if length < 2 || length > len(data)-position {
			return nil, false
		}
		position += length
		if marker != 0xda {
			continue
		}
		for position < len(data) {
			if data[position] != 0xff {
				position++
				continue
			}
			start := position
			for position < len(data) && data[position] == 0xff {
				position++
			}
			if position >= len(data) {
				return nil, false
			}
			if data[position] == 0x00 || data[position] >= 0xd0 && data[position] <= 0xd7 {
				position++
				continue
			}
			position = start
			break
		}
	}
	return nil, false
}

var imageBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/32"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("ff00::/8"),
}

func ImagePublicIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() {
		return false
	}
	for _, prefix := range imageBlockedNetworks {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func ValidateImageReferenceURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("image reference requires an HTTPS URL without credentials")
	}
	if ip, err := netip.ParseAddr(parsed.Hostname()); err == nil && !ImagePublicIP(ip) {
		return nil, errors.New("image reference address is not public")
	}
	return parsed, nil
}

type imageReferenceResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Every redirect gets fresh address checks. The dial uses a checked IP literal
// and verifies the connected peer before handing it to the HTTP transport.
func imageReferenceClient(cfg ImageRuntimeConfig, resolver imageReferenceResolver, dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: time.Duration(cfg.DownloadTimeout) * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := resolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("image reference DNS lookup failed")
		}
		for _, ip := range addresses {
			if !ImagePublicIP(ip) {
				return nil, errors.New("image reference resolved to a non-public address")
			}
		}
		var lastError error
		for _, ip := range addresses {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err != nil {
				lastError = err
				continue
			}
			peer, err := netip.ParseAddrPort(conn.RemoteAddr().String())
			if err != nil || !ImagePublicIP(peer.Addr()) || peer.Addr().Unmap() != ip.Unmap() {
				_ = conn.Close()
				return nil, errors.New("image reference connected to an unexpected address")
			}
			return conn, nil
		}
		return nil, lastError
	}}
	return &http.Client{Transport: transport, Timeout: time.Duration(cfg.DownloadTimeout) * time.Second, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) > cfg.DownloadRedirects {
			return errors.New("image reference redirect limit exceeded")
		}
		_, err := ValidateImageReferenceURL(request.URL.String())
		return err
	}}
}

func DownloadImageReference(ctx context.Context, raw string, cfg ImageRuntimeConfig) (ImageBytes, error) {
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		meta, payload, ok := strings.Cut(raw, ",")
		if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") || base64.StdEncoding.DecodedLen(len(payload)) > int(cfg.DownloadMaxBytes)+2 {
			return ImageBytes{}, newImageValidationError("invalid or oversized image data URI")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(payload)
		if err != nil {
			return ImageBytes{}, newImageValidationError("invalid base64 image")
		}
		return ValidateImageBytes(data, meta[5:len(meta)-7], cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
	}
	parsed, err := ValidateImageReferenceURL(raw)
	if err != nil {
		return ImageBytes{}, err
	}
	dialer := &net.Dialer{Timeout: time.Duration(cfg.DownloadTimeout) * time.Second}
	client := imageReferenceClient(cfg, net.DefaultResolver, dialer.DialContext)
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ImageBytes{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return ImageBytes{}, errors.New("image reference download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ImageBytes{}, fmt.Errorf("image reference returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > cfg.DownloadMaxBytes {
		return ImageBytes{}, newImageValidationError("image reference byte limit exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, cfg.DownloadMaxBytes+1))
	if err != nil {
		return ImageBytes{}, errors.New("image reference read failed")
	}
	return ValidateImageBytes(data, response.Header.Get("Content-Type"), cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
}

func DownloadUpstreamAsyncImages(ctx context.Context, urls []string, profile *dto.UpstreamAsyncProfile, baseURL, proxy string, values UpstreamAsyncTemplateContext, cfg ImageRuntimeConfig) ([]ImageBytes, error) {
	if len(urls) == 0 || len(urls) > dto.MaxImageN {
		return nil, fmt.Errorf("upstream async image result count must be between 1 and %d", dto.MaxImageN)
	}
	images := make([]ImageBytes, 0, len(urls))
	for _, raw := range urls {
		headers, credentialless, err := BuildUpstreamAsyncDownloadHeaders(profile, raw, baseURL, values)
		if err != nil {
			return nil, err
		}
		var image ImageBytes
		if credentialless {
			image, err = downloadPublicUpstreamAsyncImage(ctx, raw, cfg)
		} else {
			image, err = downloadCredentialedUpstreamAsyncImage(ctx, raw, baseURL, proxy, headers, cfg)
		}
		if err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, nil
}

func downloadPublicUpstreamAsyncImage(ctx context.Context, raw string, cfg ImageRuntimeConfig) (ImageBytes, error) {
	parsed, err := validatePublicUpstreamAsyncImageURL(raw)
	if err != nil {
		return ImageBytes{}, err
	}
	dialer := &net.Dialer{Timeout: time.Duration(cfg.DownloadTimeout) * time.Second}
	baseClient := imageReferenceClient(cfg, net.DefaultResolver, dialer.DialContext)
	client := *baseClient
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > cfg.DownloadRedirects {
			return errors.New("image result redirect limit exceeded")
		}
		_, err := validatePublicUpstreamAsyncImageURL(request.URL.String())
		return err
	}
	defer client.CloseIdleConnections()
	return downloadUpstreamAsyncImage(ctx, &client, parsed.String(), nil, cfg)
}

func downloadCredentialedUpstreamAsyncImage(ctx context.Context, raw, baseURL, proxy string, headers map[string]string, cfg ImageRuntimeConfig) (ImageBytes, error) {
	resultURL, err := url.Parse(raw)
	if err != nil {
		return ImageBytes{}, err
	}
	base, err := url.Parse(baseURL)
	if err != nil || !sameUpstreamAsyncOrigin(base, resultURL) {
		return ImageBytes{}, errors.New("credentialed image result must use the channel origin")
	}
	shared, err := GetHttpClientWithProxy(proxy)
	if err != nil {
		return ImageBytes{}, err
	}
	client := *shared
	client.Timeout = time.Duration(cfg.DownloadTimeout) * time.Second
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > cfg.DownloadRedirects || !sameUpstreamAsyncOrigin(base, request.URL) {
			return errors.New("credentialed image result redirect was rejected")
		}
		return nil
	}
	return downloadUpstreamAsyncImage(ctx, &client, resultURL.String(), headers, cfg)
}

func downloadUpstreamAsyncImage(ctx context.Context, client *http.Client, raw string, headers map[string]string, cfg ImageRuntimeConfig) (ImageBytes, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return ImageBytes{}, err
	}
	for name, value := range headers {
		if strings.ContainsAny(name+value, "\r\n") {
			return ImageBytes{}, errors.New("image result header is invalid")
		}
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return ImageBytes{}, errors.New("image result download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ImageBytes{}, fmt.Errorf("image result returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > cfg.DownloadMaxBytes {
		return ImageBytes{}, newImageValidationError("image result byte limit exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, cfg.DownloadMaxBytes+1))
	if err != nil {
		return ImageBytes{}, errors.New("image result read failed")
	}
	return ValidateImageBytes(data, response.Header.Get("Content-Type"), cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
}

func validatePublicUpstreamAsyncImageURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("anonymous image result requires a public HTTP(S) URL without credentials")
	}
	if ip, err := netip.ParseAddr(parsed.Hostname()); err == nil && !ImagePublicIP(ip) {
		return nil, errors.New("image result address is not public")
	}
	return parsed, nil
}

func (image ImageBytes) DataURL() string {
	return "data:" + image.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
}
