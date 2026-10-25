package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"os"
	"strings"

	mp4 "github.com/abema/go-mp4"
	"github.com/mewkiz/flac"
	"github.com/tcolgate/mp3"
)

// ValidateMediaInputFile checks complete container boundaries and media tracks;
// filenames and response MIME types never select the parser. Original bytes
// remain unchanged, including for images with removable trailing metadata.
func ValidateMediaInputFile(ctx context.Context, file *os.File, size int64, kind, declared string, imageCfg ImageRuntimeConfig) (string, string, error) {
	if size <= 0 {
		return "", "", errors.New("empty reference media")
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	var header [16]byte
	n, _ := file.ReadAt(header[:], 0)
	contentType, extension := "", ""
	var err error
	if kind == "image" {
		if size > imageCfg.DownloadMaxBytes {
			return "", "", errors.New("reference image byte limit exceeded")
		}
		data, readErr := io.ReadAll(io.NewSectionReader(file, 0, size))
		if readErr != nil {
			return "", "", readErr
		}
		image, validateErr := ValidateImageBytes(data, "", imageCfg.DownloadMaxBytes, imageCfg.DownloadMaxPixels)
		if validateErr != nil || len(image.Data) != len(data) {
			return "", "", errors.New("reference image has invalid or trailing data")
		}
		contentType = image.ContentType
		extension = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp"}[contentType]
	} else if kind == "video" || kind == "audio" {
		switch {
		case n >= 12 && string(header[4:8]) == "ftyp":
			contentType, extension, err = validateMediaISOContainer(ctx, file, size, kind)
		case n >= 4 && bytes.Equal(header[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
			err = validateMediaWebM(ctx, file, size, kind)
			contentType, extension = kind+"/webm", ".webm"
		case kind == "audio" && n >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WAVE":
			err = validateMediaPCMContainer(ctx, file, size, false)
			contentType, extension = "audio/wav", ".wav"
		case kind == "audio" && n >= 12 && string(header[:4]) == "FORM" && (string(header[8:12]) == "AIFF" || string(header[8:12]) == "AIFC"):
			err = validateMediaPCMContainer(ctx, file, size, true)
			contentType, extension = "audio/aiff", ".aiff"
		case kind == "audio" && n >= 4 && string(header[:4]) == "fLaC":
			stream, parseErr := flac.New(&mediaValidationReader{ctx: ctx, reader: io.NewSectionReader(file, 0, size)})
			if parseErr != nil {
				err = parseErr
				break
			}
			var samples uint64
			for {
				frame, frameErr := stream.ParseNext()
				if frameErr == io.EOF {
					break
				}
				if frameErr != nil {
					err = frameErr
					break
				}
				samples += uint64(frame.BlockSize)
			}
			if samples == 0 || stream.Info.SampleRate == 0 || stream.Info.NSamples > 0 && samples != stream.Info.NSamples {
				err = errors.New("invalid or truncated FLAC reference")
			}
			contentType, extension = "audio/flac", ".flac"
		case kind == "audio" && n >= 4 && string(header[:4]) == "OggS":
			err = validateMediaOgg(ctx, file, size)
			contentType, extension = "audio/ogg", ".ogg"
		case kind == "audio" && n >= 2 && header[0] == 0xff && header[1]&0xf6 == 0xf0:
			err = validateMediaADTS(ctx, file, size)
			contentType, extension = "audio/aac", ".aac"
		case kind == "audio" && n >= 3 && (string(header[:3]) == "ID3" || header[0] == 0xff && header[1]&0xe0 == 0xe0):
			err = validateMediaMP3(ctx, file, size)
			contentType, extension = "audio/mpeg", ".mp3"
		default:
			err = errors.New("unsupported reference media format")
		}
	} else {
		err = errors.New("unsupported reference media kind")
	}
	if err != nil {
		return "", "", err
	}
	if declared != "" {
		actual, _, parseErr := mime.ParseMediaType(declared)
		actual = strings.ToLower(actual)
		aliases := map[string]string{"audio/x-wav": "audio/wav", "audio/wave": "audio/wav", "audio/x-flac": "audio/flac", "audio/mp3": "audio/mpeg", "audio/x-aiff": "audio/aiff", "audio/x-m4a": "audio/mp4", "audio/opus": "audio/ogg", "application/ogg": "audio/ogg"}
		if canonical := aliases[actual]; canonical != "" {
			actual = canonical
		}
		if parseErr != nil || actual != "application/octet-stream" && actual != contentType {
			return "", "", errors.New("reference media MIME does not match its bytes")
		}
	}
	return contentType, extension, ctx.Err()
}

type mediaValidationReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *mediaValidationReader) Read(target []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(target)
}

func validateMediaISOContainer(ctx context.Context, file *os.File, size int64, kind string) (string, string, error) {
	var header [16]byte
	ftyp, moov, mdat, track := false, false, false, false
	quickTime := false
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		if size-offset < 8 {
			return "", "", errors.New("truncated media container")
		}
		if _, err := file.ReadAt(header[:8], offset); err != nil {
			return "", "", err
		}
		length, headerSize := int64(binary.BigEndian.Uint32(header[:4])), int64(8)
		if length == 1 {
			if _, err := file.ReadAt(header[8:], offset+8); err != nil || binary.BigEndian.Uint64(header[8:]) > uint64(size-offset) {
				return "", "", errors.New("invalid extended media box")
			}
			length, headerSize = int64(binary.BigEndian.Uint64(header[8:])), 16
		} else if length == 0 {
			length = size - offset
		}
		if length < headerSize || length > size-offset {
			return "", "", errors.New("invalid media box bounds")
		}
		switch string(header[4:8]) {
		case "ftyp":
			if length < headerSize+8 {
				return "", "", errors.New("invalid media file type")
			}
			if _, err := file.ReadAt(header[:4], offset+headerSize); err != nil {
				return "", "", err
			}
			quickTime, ftyp = string(header[:4]) == "qt  ", true
		case "moov":
			moov = length > headerSize
		case "mdat":
			mdat = length > headerSize
		}
		offset += length
	}
	boxes := 0
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	_, err := mp4.ReadBoxStructure(file, func(handle *mp4.ReadHandle) (any, error) {
		boxes++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if boxes > 100000 || len(handle.Path) > 16 || handle.BoxInfo.Offset > uint64(size) || handle.BoxInfo.Size > uint64(size)-handle.BoxInfo.Offset {
			return nil, errors.New("invalid media metadata bounds")
		}
		switch handle.BoxInfo.Type {
		case mp4.BoxTypeMoov(), mp4.BoxTypeTrak(), mp4.BoxTypeMdia(), mp4.BoxTypeMinf(), mp4.BoxTypeStbl():
			return handle.Expand()
		case mp4.BoxTypeHdlr():
			if handle.BoxInfo.Size > 64<<10 || len(handle.Path) != 4 || handle.Path[0] != mp4.BoxTypeMoov() || handle.Path[1] != mp4.BoxTypeTrak() || handle.Path[2] != mp4.BoxTypeMdia() {
				return nil, errors.New("invalid media track metadata")
			}
			box, _, err := handle.ReadPayload()
			if err != nil {
				return nil, err
			}
			if handler, ok := box.(*mp4.Hdlr); ok {
				track = track || kind == "video" && string(handler.HandlerType[:]) == "vide" || kind == "audio" && string(handler.HandlerType[:]) == "soun"
			}
		}
		return nil, nil
	})
	if err != nil || !ftyp || !moov || !mdat || !track {
		return "", "", errors.New("unsupported or damaged media container or track")
	}
	if kind == "audio" {
		return "audio/mp4", ".m4a", nil
	}
	if quickTime {
		return "video/quicktime", ".mov", nil
	}
	return "video/mp4", ".mp4", nil
}

func validateMediaPCMContainer(ctx context.Context, file *os.File, size int64, aiff bool) error {
	var header [32]byte
	if _, err := file.ReadAt(header[:12], 0); err != nil {
		return err
	}
	var order binary.ByteOrder = binary.LittleEndian
	if aiff {
		order = binary.BigEndian
	}
	if int64(order.Uint32(header[4:8]))+8 != size {
		return errors.New("truncated audio container")
	}
	format, payload := false, false
	var blockSize, expectedBytes int64
	aifc := aiff && string(header[8:12]) == "AIFC"
	for offset := int64(12); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size-offset < 8 {
			return errors.New("truncated audio chunk")
		}
		if _, err := file.ReadAt(header[:8], offset); err != nil {
			return err
		}
		name, length := string(header[:4]), int64(order.Uint32(header[4:8]))
		if length > size-offset-8 {
			return errors.New("invalid audio chunk bounds")
		}
		start := offset + 8
		if !aiff && name == "fmt " {
			if length < 16 {
				return errors.New("invalid WAV format")
			}
			if _, err := file.ReadAt(header[:16], start); err != nil {
				return err
			}
			encoding, channels, rate := order.Uint16(header[:2]), order.Uint16(header[2:4]), order.Uint32(header[4:8])
			blockSize = int64(order.Uint16(header[12:14]))
			bits := order.Uint16(header[14:16])
			format = channels > 0 && channels <= 32 && rate > 0 && rate <= 768000 && bits > 0 && bits <= 64 && blockSize == int64(channels)*int64((bits+7)/8) && uint64(order.Uint32(header[8:12])) == uint64(rate)*uint64(blockSize)
			if encoding == 0xfffe {
				var extra [24]byte
				if length < 40 {
					return errors.New("truncated extensible WAV format")
				}
				if _, err := file.ReadAt(extra[:], start+16); err != nil {
					return err
				}
				encoding = order.Uint16(extra[8:10])
				format = format && order.Uint16(extra[:2]) >= 22 && bytes.Equal(extra[10:], []byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
			}
			format = format && (encoding == 1 || encoding == 3 && (bits == 32 || bits == 64))
		}
		if aiff && name == "COMM" {
			if length < 18 {
				return errors.New("invalid AIFF format")
			}
			if _, err := file.ReadAt(header[:18], start); err != nil {
				return err
			}
			channels, frames, bits := order.Uint16(header[:2]), order.Uint32(header[2:6]), order.Uint16(header[6:8])
			format = channels > 0 && channels <= 32 && frames > 0 && bits > 0 && bits <= 64 && order.Uint16(header[8:10])&0x7fff > 0
			blockSize = int64(channels) * int64((bits+7)/8)
			expectedBytes = int64(frames) * blockSize
			if aifc {
				if length < 22 {
					return errors.New("truncated AIFC format")
				}
				if _, err := file.ReadAt(header[:4], start+18); err != nil {
					return err
				}
				format = format && (string(header[:4]) == "NONE" || string(header[:4]) == "sowt" || string(header[:4]) == "fl32" || string(header[:4]) == "fl64")
			}
		}
		if !aiff && name == "data" {
			payload = length > 0 && blockSize > 0 && length%blockSize == 0
		}
		if aiff && name == "SSND" {
			if length < 9 {
				return errors.New("invalid AIFF sound data")
			}
			if _, err := file.ReadAt(header[:8], start); err != nil {
				return err
			}
			payload = expectedBytes > 0 && length-8-int64(order.Uint32(header[:4])) == expectedBytes
		}
		offset = start + length + (length & 1)
		if offset > size {
			return errors.New("truncated audio padding")
		}
	}
	if !format || !payload {
		return errors.New("audio container has no valid samples")
	}
	return nil
}

func validateMediaMP3(ctx context.Context, file *os.File, size int64) error {
	start, end := int64(0), size
	var header [10]byte
	_, _ = file.ReadAt(header[:], 0)
	if string(header[:3]) == "ID3" {
		if header[3] < 2 || header[3] > 4 || header[6]&0x80 != 0 || header[7]&0x80 != 0 || header[8]&0x80 != 0 || header[9]&0x80 != 0 {
			return errors.New("invalid ID3 tag")
		}
		start = 10 + (int64(header[6]) << 21) + (int64(header[7]) << 14) + (int64(header[8]) << 7) + int64(header[9])
		if header[3] == 4 && header[5]&0x10 != 0 {
			start += 10
		}
	}
	if size >= 128 {
		_, _ = file.ReadAt(header[:3], size-128)
		if string(header[:3]) == "TAG" {
			end -= 128
		}
	}
	if start >= end {
		return errors.New("MP3 has no frames")
	}
	decoder := mp3.NewDecoder(&mediaValidationReader{ctx: ctx, reader: io.NewSectionReader(file, start, end-start)})
	var consumed int64
	for consumed < end-start {
		var frame mp3.Frame
		skipped := 0
		if err := decoder.Decode(&frame, &skipped); err != nil || skipped != 0 || frame.Size() <= 0 {
			return errors.New("invalid or truncated MP3 frame")
		}
		consumed += int64(frame.Size())
	}
	if consumed != end-start {
		return errors.New("invalid MP3 frame bounds")
	}
	return nil
}

func validateMediaADTS(ctx context.Context, file *os.File, size int64) error {
	var header [7]byte
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		if size-offset < 7 {
			return errors.New("truncated AAC frame")
		}
		if _, err := file.ReadAt(header[:], offset); err != nil {
			return err
		}
		length := int64(header[3]&3)<<11 | int64(header[4])<<3 | int64(header[5]>>5)
		headerLength := int64(7)
		if header[1]&1 == 0 {
			headerLength = 9
		}
		if header[0] != 0xff || header[1]&0xf6 != 0xf0 || header[2]>>2&15 >= 13 || length <= headerLength || length > size-offset {
			return errors.New("invalid AAC ADTS frame")
		}
		offset += length
	}
	return nil
}

// Ogg pages are bounded, checksummed and sequenced. The first complete packet
// must identify Opus or Vorbis, and a complete terminal audio page is required.
func validateMediaOgg(ctx context.Context, file *os.File, size int64) error {
	var serial, sequence uint32
	codec, packets, eos := "", 0, false
	var packet []byte
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var header [27]byte
		if _, err := file.ReadAt(header[:], offset); err != nil {
			return errors.New("truncated Ogg page")
		}
		if string(header[:4]) != "OggS" || header[4] != 0 || eos {
			return errors.New("invalid Ogg page")
		}
		segments := make([]byte, int(header[26]))
		if _, err := file.ReadAt(segments, offset+27); err != nil {
			return err
		}
		length := 0
		for _, n := range segments {
			length += int(n)
		}
		page := make([]byte, 27+len(segments)+length)
		if int64(len(page)) > size-offset {
			return errors.New("truncated Ogg payload")
		}
		if _, err := file.ReadAt(page, offset); err != nil {
			return err
		}
		wantCRC := binary.LittleEndian.Uint32(page[22:26])
		clear(page[22:26])
		var crc uint32
		for _, b := range page {
			crc ^= uint32(b) << 24
			for range 8 {
				if crc&0x80000000 != 0 {
					crc = crc<<1 ^ 0x04c11db7
				} else {
					crc <<= 1
				}
			}
		}
		if crc != wantCRC {
			return errors.New("invalid Ogg checksum")
		}
		currentSerial, currentSequence := binary.LittleEndian.Uint32(header[14:18]), binary.LittleEndian.Uint32(header[18:22])
		if offset == 0 {
			serial, sequence = currentSerial, currentSequence
			if header[5]&2 == 0 || header[5]&1 != 0 || sequence != 0 {
				return errors.New("invalid Ogg initial page")
			}
		} else if serial != currentSerial || currentSequence != sequence+1 || header[5]&2 != 0 {
			return errors.New("invalid Ogg stream sequence")
		} else {
			sequence = currentSequence
		}
		if (len(packet) > 0) != (header[5]&1 != 0) {
			return errors.New("invalid Ogg continuation")
		}
		position := 27 + len(segments)
		for _, n := range segments {
			if len(packet)+int(n) > 1<<20 {
				return errors.New("Ogg packet exceeds validation limit")
			}
			packet = append(packet, page[position:position+int(n)]...)
			position += int(n)
			if n == 255 {
				continue
			}
			packets++
			if packets == 1 {
				switch {
				case len(packet) >= 19 && string(packet[:8]) == "OpusHead" && packet[8] <= 15 && packet[9] > 0:
					codec = "opus"
					channels, mapping := int(packet[9]), packet[18]
					if mapping == 0 && (channels > 2 || len(packet) != 19) || mapping != 0 && (len(packet) != 21+channels || packet[19] == 0 || packet[20] > packet[19] || int(packet[19])+int(packet[20]) > 255) {
						return errors.New("invalid Opus channel mapping")
					}
				case len(packet) == 30 && bytes.Equal(packet[:7], []byte{1, 'v', 'o', 'r', 'b', 'i', 's'}) && binary.LittleEndian.Uint32(packet[7:11]) == 0 && packet[11] > 0 && binary.LittleEndian.Uint32(packet[12:16]) > 0 && packet[28]&15 >= 6 && packet[28]>>4 <= 13 && packet[28]&15 <= packet[28]>>4 && packet[29]&1 == 1:
					codec = "vorbis"
				default:
					return errors.New("unsupported Ogg audio codec")
				}
			} else if packets == 2 {
				prefix := "OpusTags"
				if codec == "vorbis" {
					prefix = "\x03vorbis"
				}
				if len(packet) < len(prefix)+8 || string(packet[:len(prefix)]) != prefix {
					return errors.New("invalid Ogg audio comment header")
				}
				position := len(prefix)
				vendorSize := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
				position += 4
				if vendorSize > uint64(len(packet)-position-4) {
					return errors.New("truncated Ogg vendor metadata")
				}
				position += int(vendorSize)
				comments := binary.LittleEndian.Uint32(packet[position : position+4])
				position += 4
				for range comments {
					if len(packet)-position < 4 {
						return errors.New("truncated Ogg comment length")
					}
					length := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
					position += 4
					if length > uint64(len(packet)-position) {
						return errors.New("truncated Ogg comment")
					}
					position += int(length)
				}
				if codec == "vorbis" && (position >= len(packet) || packet[position]&1 != 1) {
					return errors.New("missing Vorbis comment framing bit")
				}
			} else if codec == "vorbis" && packets == 3 {
				if len(packet) < 8 || !bytes.Equal(packet[:7], []byte{5, 'v', 'o', 'r', 'b', 'i', 's'}) {
					return errors.New("invalid Vorbis setup header")
				}
			} else if len(packet) == 0 {
				return errors.New("empty Ogg audio packet")
			}
			packet = packet[:0]
		}
		eos = header[5]&4 != 0
		offset += int64(len(page))
	}
	minimum := 3
	if codec == "vorbis" {
		minimum = 4
	}
	if !eos || len(packet) != 0 || packets < minimum {
		return errors.New("incomplete Ogg audio stream")
	}
	return nil
}

type mediaEBMLValidation struct {
	ctx     context.Context
	file    *os.File
	kind    string
	nodes   int
	docType bool
	header  bool
	segment bool
	tracks  map[uint64]bool
	blocks  map[uint64]int
}

func validateMediaWebM(ctx context.Context, file *os.File, size int64, kind string) error {
	validation := mediaEBMLValidation{ctx: ctx, file: file, kind: kind, tracks: make(map[uint64]bool), blocks: make(map[uint64]int)}
	if err := validation.elements(0, size, 0, 0); err != nil {
		return err
	}
	matching := false
	for number := range validation.blocks {
		if _, exists := validation.tracks[number]; !exists {
			return errors.New("WebM media block references an unknown track")
		}
	}
	for number, compatible := range validation.tracks {
		matching = matching || compatible && validation.blocks[number] > 0
	}
	if !validation.header || !validation.segment || !validation.docType || !matching {
		return errors.New("WebM has no complete matching media track")
	}
	return nil
}

func (validation *mediaEBMLValidation) elements(start, end int64, depth int, parent uint64) error {
	if depth > 16 {
		return errors.New("WebM nesting exceeds validation limit")
	}
	trackType, trackNumber, codec := uint64(0), uint64(0), ""
	for offset := start; offset < end; {
		validation.nodes++
		if validation.nodes > 100000 {
			return errors.New("WebM metadata exceeds validation limit")
		}
		if err := validation.ctx.Err(); err != nil {
			return err
		}
		id, idBytes, _, err := readMediaEBMLInteger(validation.file, offset, true)
		if err != nil {
			return err
		}
		length, lengthBytes, unknown, err := readMediaEBMLInteger(validation.file, offset+int64(idBytes), false)
		if err != nil {
			return err
		}
		payload := offset + int64(idBytes+lengthBytes)
		if payload > end || !unknown && length > uint64(end-payload) {
			return errors.New("truncated WebM element")
		}
		next := end
		if !unknown {
			next = payload + int64(length)
		}
		if unknown && id != 0x18538067 && id != 0x1f43b675 {
			return errors.New("invalid unknown WebM element size")
		}
		switch id {
		case 0x1a45dfa3, 0x18538067, 0x1549a966, 0x1654ae6b, 0xe0, 0xe1, 0x1f43b675, 0xa0, 0xae:
			validParent := id == 0x1a45dfa3 && parent == 0 && !validation.header && offset == 0 || id == 0x18538067 && parent == 0 && validation.header && !validation.segment || (id == 0x1549a966 || id == 0x1654ae6b || id == 0x1f43b675) && parent == 0x18538067 || id == 0xae && parent == 0x1654ae6b || (id == 0xe0 || id == 0xe1) && parent == 0xae || id == 0xa0 && parent == 0x1f43b675
			if !validParent {
				return errors.New("invalid WebM element hierarchy")
			}
			validation.header = validation.header || id == 0x1a45dfa3
			validation.segment = validation.segment || id == 0x18538067
			if err := validation.elements(payload, next, depth+1, id); err != nil {
				return err
			}
		case 0x4282, 0x86:
			if next-payload > 128 {
				return errors.New("invalid WebM codec metadata")
			}
			data := make([]byte, next-payload)
			if _, err := validation.file.ReadAt(data, payload); err != nil {
				return err
			}
			if id == 0x4282 {
				if parent != 0x1a45dfa3 || validation.docType || string(data) != "webm" {
					return errors.New("invalid WebM document type")
				}
				validation.docType = true
			} else if parent == 0xae {
				codec = string(data)
			}
		case 0x83, 0xd7:
			if parent != 0xae || next-payload < 1 || next-payload > 8 {
				return errors.New("invalid WebM track type")
			}
			var data [8]byte
			if _, err := validation.file.ReadAt(data[8-(next-payload):], payload); err != nil {
				return err
			}
			if id == 0x83 {
				trackType = binary.BigEndian.Uint64(data[:])
			} else {
				trackNumber = binary.BigEndian.Uint64(data[:])
			}
		case 0xa1, 0xa3:
			if (id == 0xa1 && parent != 0xa0 || id == 0xa3 && parent != 0x1f43b675) || next-payload < 5 {
				return errors.New("truncated WebM media block")
			}
			number, trackBytes, unknownTrack, err := readMediaEBMLInteger(validation.file, payload, false)
			if err != nil || unknownTrack || number == 0 || next-payload <= int64(trackBytes+3) {
				return errors.New("invalid WebM media block")
			}
			var flags [1]byte
			if _, err := validation.file.ReadAt(flags[:], payload+int64(trackBytes)+2); err != nil {
				return err
			}
			if flags[0]&6 != 0 {
				// Lacing is validated separately; accepting its marker alone would
				// permit truncated frame-size tables to masquerade as valid media.
				if err := validateMediaWebMLacing(validation.file, payload+int64(trackBytes)+3, next, flags[0]&6); err != nil {
					return err
				}
			}
			validation.blocks[number]++
		}
		offset = next
	}
	if parent == 0xae {
		if trackNumber == 0 || codec == "" || trackType == 0 {
			return errors.New("incomplete WebM track")
		}
		if _, exists := validation.tracks[trackNumber]; exists {
			return errors.New("duplicate WebM track")
		}
		validation.tracks[trackNumber] = validation.kind == "video" && trackType == 1 && (codec == "V_VP8" || codec == "V_VP9" || codec == "V_AV1") || validation.kind == "audio" && trackType == 2 && (codec == "A_OPUS" || codec == "A_VORBIS")
	}
	return nil
}

func readMediaEBMLInteger(file *os.File, offset int64, identifier bool) (uint64, int, bool, error) {
	var data [8]byte
	if _, err := file.ReadAt(data[:1], offset); err != nil {
		return 0, 0, false, err
	}
	width, mask := 1, byte(0x80)
	for width <= 8 && data[0]&mask == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || identifier && width > 4 {
		return 0, 0, false, errors.New("invalid WebM variable integer")
	}
	if _, err := file.ReadAt(data[:width], offset); err != nil {
		return 0, 0, false, err
	}
	value := uint64(data[0] &^ mask)
	if identifier {
		value = uint64(data[0])
	}
	for _, b := range data[1:width] {
		value = value<<8 | uint64(b)
	}
	return value, width, !identifier && value == uint64(1)<<(7*width)-1, nil
}

func validateMediaWebMLacing(file *os.File, start, end int64, mode byte) error {
	var count [1]byte
	if _, err := file.ReadAt(count[:], start); err != nil {
		return err
	}
	frames, position := int(count[0])+1, start+1
	if mode == 4 {
		if (end-position)%int64(frames) != 0 || end-position < int64(frames) {
			return errors.New("invalid fixed WebM lacing")
		}
		return nil
	}
	var total, previous int64
	for index := range frames - 1 {
		var length int64
		if mode == 2 {
			for {
				if position >= end {
					return errors.New("truncated WebM lace table")
				}
				if _, err := file.ReadAt(count[:], position); err != nil {
					return err
				}
				position++
				length += int64(count[0])
				if count[0] < 255 {
					break
				}
			}
		} else {
			value, width, unknown, err := readMediaEBMLInteger(file, position, false)
			if err != nil || unknown {
				return errors.New("invalid WebM EBML lacing")
			}
			position += int64(width)
			length = int64(value)
			if index > 0 {
				length += previous - (int64(1)<<(7*width-1) - 1)
			}
		}
		if length <= 0 || length > end-position || total > end-position-length {
			return errors.New("invalid WebM lace length")
		}
		total += length
		previous = length
	}
	if total >= end-position {
		return errors.New("truncated WebM laced payload")
	}
	return nil
}
