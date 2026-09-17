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

	_ "golang.org/x/image/webp"
)

type ImageBytes struct {
	Data        []byte
	ContentType string
	Checksum    string
	Width       int
	Height      int
}

func ValidateImageBytes(data []byte, declared string, maxBytes, maxPixels int64) (ImageBytes, error) {
	if len(data) == 0 || maxBytes <= 0 || int64(len(data)) > maxBytes {
		return ImageBytes{}, errors.New("image byte limit exceeded or empty image")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return ImageBytes{}, errors.New("invalid image header")
	}
	if maxPixels <= 0 || int64(cfg.Height) > maxPixels || int64(cfg.Width) > maxPixels/int64(cfg.Height) {
		return ImageBytes{}, errors.New("image pixel limit exceeded")
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
		return ImageBytes{}, errors.New("only PNG, JPEG and WebP images are supported")
	}
	if !validImageContainer(data, format) {
		return ImageBytes{}, errors.New("invalid image container or trailing data")
	}
	if declared != "" {
		actual, _, err := mime.ParseMediaType(declared)
		if err != nil || !strings.EqualFold(actual, contentType) {
			return ImageBytes{}, errors.New("declared image MIME does not match its bytes")
		}
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return ImageBytes{}, errors.New("image could not be fully decoded")
	}
	sum := sha256.Sum256(data)
	return ImageBytes{Data: data, ContentType: contentType, Checksum: hex.EncodeToString(sum[:]), Width: cfg.Width, Height: cfg.Height}, nil
}

func validImageContainer(data []byte, format string) bool {
	if format == "webp" {
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" && uint64(binary.LittleEndian.Uint32(data[4:8]))+8 == uint64(len(data))
	}
	if format == "png" {
		if len(data) < 20 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
			return false
		}
		for offset := 8; offset+12 <= len(data); {
			length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
			if length > uint64(len(data)-offset-12) {
				return false
			}
			end := offset + 12 + int(length)
			if string(data[offset+4:offset+8]) == "IEND" {
				return length == 0 && end == len(data)
			}
			offset = end
		}
		return false
	}
	if format != "jpeg" || len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return false
	}
	position := 2
	for position < len(data) {
		if data[position] != 0xff {
			return false
		}
		for position < len(data) && data[position] == 0xff {
			position++
		}
		if position >= len(data) {
			return false
		}
		marker := data[position]
		position++
		if marker == 0xd9 {
			return position == len(data)
		}
		if marker == 0x00 || marker == 0xd8 {
			return false
		}
		if marker == 0x01 || marker >= 0xd0 && marker <= 0xd7 {
			continue
		}
		if position+2 > len(data) {
			return false
		}
		length := int(binary.BigEndian.Uint16(data[position : position+2]))
		if length < 2 || length > len(data)-position {
			return false
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
				return false
			}
			if data[position] == 0x00 || data[position] >= 0xd0 && data[position] <= 0xd7 {
				position++
				continue
			}
			position = start
			break
		}
	}
	return false
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
			return ImageBytes{}, errors.New("invalid or oversized image data URI")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(payload)
		if err != nil {
			return ImageBytes{}, errors.New("invalid base64 image")
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
		return ImageBytes{}, errors.New("image reference byte limit exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, cfg.DownloadMaxBytes+1))
	if err != nil {
		return ImageBytes{}, errors.New("image reference read failed")
	}
	return ValidateImageBytes(data, response.Header.Get("Content-Type"), cfg.DownloadMaxBytes, cfg.DownloadMaxPixels)
}

func (image ImageBytes) DataURL() string {
	return "data:" + image.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
}
