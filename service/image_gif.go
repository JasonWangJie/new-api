package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image/gif"
)

// Scan frame descriptors before DecodeAll allocates their pixels. The budget
// counts the whole logical canvas per frame, bounding both decoding memory and
// animation compositing work even when frames only update small rectangles.
func validateGIFOutput(data []byte, maxPixels int64) (ImageBytes, error) {
	if len(data) < 13 {
		return ImageBytes{}, newImageValidationError("invalid GIF header")
	}
	width, height := int(binary.LittleEndian.Uint16(data[6:8])), int(binary.LittleEndian.Uint16(data[8:10]))
	pixels := int64(width) * int64(height)
	if pixels <= 0 || maxPixels <= 0 || pixels > maxPixels {
		return ImageBytes{}, newImageValidationError("image pixel limit exceeded")
	}
	position, frames := 13, 0
	if data[10]&0x80 != 0 {
		position += 3 * (1 << ((data[10] & 7) + 1))
	}
	end := 0
scan:
	for position < len(data) {
		block := data[position]
		position++
		switch block {
		case 0x3b:
			end = position
			break scan
		case 0x21:
			// Extension label; its payload uses GIF sub-block framing.
			if position >= len(data) {
				return ImageBytes{}, newImageValidationError("invalid GIF extension")
			}
			position++
		case 0x2c:
			if len(data)-position < 9 {
				return ImageBytes{}, newImageValidationError("invalid GIF frame")
			}
			left := int(binary.LittleEndian.Uint16(data[position:]))
			top := int(binary.LittleEndian.Uint16(data[position+2:]))
			frameWidth := int(binary.LittleEndian.Uint16(data[position+4:]))
			frameHeight := int(binary.LittleEndian.Uint16(data[position+6:]))
			if frameWidth <= 0 || frameHeight <= 0 || left+frameWidth > width || top+frameHeight > height {
				return ImageBytes{}, newImageValidationError("invalid GIF frame bounds")
			}
			frames++
			if frames > 512 || int64(frames) > maxPixels/pixels {
				return ImageBytes{}, newImageValidationError("GIF animation frame or pixel limit exceeded")
			}
			packed := data[position+8]
			position += 9
			if packed&0x80 != 0 {
				position += 3 * (1 << ((packed & 7) + 1))
			}
			if position >= len(data) {
				return ImageBytes{}, newImageValidationError("invalid GIF frame data")
			}
			position++ // LZW code size, validated by DecodeAll below.
		default:
			return ImageBytes{}, newImageValidationError("invalid GIF block")
		}
		for {
			if position >= len(data) {
				return ImageBytes{}, newImageValidationError("truncated GIF data")
			}
			length := int(data[position])
			position++
			if length == 0 {
				break
			}
			if length > len(data)-position {
				return ImageBytes{}, newImageValidationError("truncated GIF data")
			}
			position += length
		}
	}
	if end == 0 || frames == 0 || len(data)-end > maxNormalizedImageTrailingBytes {
		return ImageBytes{}, newImageValidationError("invalid GIF container or excessive trailing data")
	}
	canonical := data[:end]
	decoded, err := gif.DecodeAll(bytes.NewReader(canonical))
	if err != nil || len(decoded.Image) != frames || decoded.Config.Width != width || decoded.Config.Height != height {
		return ImageBytes{}, newImageValidationError("GIF could not be fully decoded")
	}
	if end != len(data) {
		canonical = bytes.Clone(canonical)
	}
	sum := sha256.Sum256(canonical)
	return ImageBytes{Data: canonical, ContentType: "image/gif", Checksum: hex.EncodeToString(sum[:]), Width: width, Height: height}, nil
}
