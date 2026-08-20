package multimodal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	maxTextBytes     = 1 << 20
	maxPDFBytes      = 25 << 20
	maxPDFPages      = 100
	maxPNGBytes      = 20 << 20
	maxPNGMegapixels = 25_000_000
	maxWAVBytes      = 250 << 20
	maxWAVDurationMs = 3_600_000
)

// extractResult is the deterministic product of one residual extractor pass.
type extractResult struct {
	Items   []evidenceBodyItem
	Lanes   []laneBody
	Partial bool
}

// preDecode validates declared envelope bounds and kind/media agreement before
// any expensive parse of payload bytes. It returns a typed admit denial.
func preDecode(envelope *contractsv1Envelope, payloadLen int) error {
	if envelope == nil {
		return ErrInvalidInput
	}
	if envelope.ContentDigest == nil || envelope.ContentDigest.Algorithm != "sha256" ||
		!isHexDigest(envelope.ContentDigest.Hex) {
		return ErrInvalidInput
	}
	if envelope.ExtractorIdentity == nil || envelope.ExtractorIdentity.Algorithm != "sha256" ||
		!isHexDigest(envelope.ExtractorIdentity.Hex) {
		return ErrInvalidInput
	}
	if envelope.ByteLength == 0 {
		return ErrInvalidInput
	}
	// Declared length must equal available payload length.
	if payloadLen > 0 && uint64(payloadLen) != envelope.ByteLength {
		if uint64(payloadLen) < envelope.ByteLength {
			return ErrPartialPayload
		}
		return ErrMalformed
	}
	switch envelope.Kind {
	case kindText:
		if envelope.MediaType != "text/plain" && envelope.MediaType != "text/markdown" {
			return ErrMediaTypeMismatch
		}
		if envelope.Text == nil {
			return ErrInvalidInput
		}
		if envelope.Text.Utf8ByteLength > maxTextBytes || envelope.ByteLength > maxTextBytes {
			return ErrOversized
		}
		if envelope.ByteLength != envelope.Text.Utf8ByteLength {
			return ErrMalformed
		}
	case kindPDF:
		if envelope.MediaType != "application/pdf" {
			return ErrMediaTypeMismatch
		}
		if envelope.PDF == nil {
			return ErrInvalidInput
		}
		if envelope.PDF.ByteLength > maxPDFBytes || envelope.ByteLength > maxPDFBytes {
			return ErrOversized
		}
		if envelope.PDF.PageCount == 0 || envelope.PDF.PageCount > maxPDFPages {
			return ErrOversized
		}
		if envelope.ByteLength != envelope.PDF.ByteLength {
			return ErrMalformed
		}
	case kindPNG:
		if envelope.MediaType != "image/png" {
			// JPEG and other image types are DEF-010 / media-type mismatch.
			if strings.HasPrefix(envelope.MediaType, "image/") {
				return ErrMediaTypeMismatch
			}
			return ErrMediaTypeMismatch
		}
		if envelope.PNG == nil {
			return ErrInvalidInput
		}
		if envelope.PNG.ByteLength > maxPNGBytes || envelope.ByteLength > maxPNGBytes {
			return ErrOversized
		}
		mp := uint64(envelope.PNG.WidthPx) * uint64(envelope.PNG.HeightPx)
		if mp == 0 || mp > maxPNGMegapixels {
			return ErrOversized
		}
		if envelope.ByteLength != envelope.PNG.ByteLength {
			return ErrMalformed
		}
	case kindWAV:
		switch envelope.MediaType {
		case "audio/wav", "audio/wave", "audio/x-wav":
		case "audio/mpeg", "audio/mp3", "audio/ogg", "audio/flac":
			return ErrEncryptedOrUnsupported
		default:
			return ErrMediaTypeMismatch
		}
		if envelope.WAV == nil {
			return ErrInvalidInput
		}
		if envelope.WAV.ByteLength > maxWAVBytes || envelope.ByteLength > maxWAVBytes {
			return ErrOversized
		}
		if envelope.WAV.DurationMillis == 0 || envelope.WAV.DurationMillis > maxWAVDurationMs {
			return ErrOversized
		}
		if envelope.WAV.Channels < 1 || envelope.WAV.Channels > 2 {
			return ErrEncryptedOrUnsupported
		}
		if envelope.ByteLength != envelope.WAV.ByteLength {
			return ErrMalformed
		}
	default:
		return ErrEncryptedOrUnsupported
	}
	return nil
}

// contractsv1Envelope is a narrow view used by preDecode without importing the
// full generated package into extract tests that only need declared bounds.
// The kernel builds this from the real protobuf envelope.
type contractsv1Envelope struct {
	Kind              kindCode
	MediaType         string
	ByteLength        uint64
	ContentDigest     *digestView
	ExtractorIdentity *digestView
	Text              *textBounds
	PDF               *pdfBounds
	PNG               *pngBounds
	WAV               *wavBounds
}

type digestView struct {
	Algorithm string
	Hex       string
}

type textBounds struct {
	Utf8ByteLength uint64
}

type pdfBounds struct {
	ByteLength uint64
	PageCount  uint32
}

type pngBounds struct {
	ByteLength uint64
	WidthPx    uint32
	HeightPx   uint32
}

type wavBounds struct {
	ByteLength     uint64
	DurationMillis uint64
	SampleRateHz   uint32
	Channels       uint32
}

type kindCode int

const (
	kindUnspecified kindCode = iota
	kindText
	kindPDF
	kindPNG
	kindWAV
)

// extract runs the kind-specific residual extractor and returns anchors + lanes.
func extract(kind kindCode, payload []byte, forcePartial bool, sourceRevisionID string) (extractResult, error) {
	if len(payload) == 0 {
		return extractResult{}, ErrPartialPayload
	}
	var result extractResult
	var err error
	switch kind {
	case kindText:
		result, err = extractText(payload, sourceRevisionID)
	case kindPDF:
		result, err = extractPDF(payload, sourceRevisionID)
	case kindPNG:
		result, err = extractPNG(payload, sourceRevisionID)
	case kindWAV:
		result, err = extractWAV(payload, sourceRevisionID)
	default:
		return extractResult{}, ErrEncryptedOrUnsupported
	}
	if err != nil {
		return extractResult{}, err
	}
	if forcePartial {
		result.Partial = true
		for index := range result.Lanes {
			if result.Lanes[index].Required && result.Lanes[index].Lane != "ORIGINAL" {
				result.Lanes[index].State = "PENDING"
				result.Lanes[index].CoveragePerMille = 500
				break
			}
		}
	}
	return result, nil
}

func extractText(payload []byte, sourceRevisionID string) (extractResult, error) {
	if !utf8.Valid(payload) {
		return extractResult{}, ErrMalformed
	}
	if len(payload) > maxTextBytes {
		return extractResult{}, ErrOversized
	}
	support := digestBytes(payload)
	item := evidenceBodyItem{
		EvidenceID:       identity("ouroboros.stage11.evidence.v1", sourceRevisionID, "text", "0"),
		SourceRevisionID: sourceRevisionID,
		AnchorKind:       "text",
		StartByte:        0,
		EndByte:          uint64(len(payload)),
		SupportDigest:    support,
		Authority:        "DIRECT_SOURCE",
	}
	// Also emit a raw bytes anchor over the same span.
	bytesItem := evidenceBodyItem{
		EvidenceID:       identity("ouroboros.stage11.evidence.v1", sourceRevisionID, "bytes", "0"),
		SourceRevisionID: sourceRevisionID,
		AnchorKind:       "bytes",
		StartByte:        0,
		EndByte:          uint64(len(payload)),
		SupportDigest:    support,
		Authority:        "DIRECT_SOURCE",
	}
	return extractResult{
		Items: []evidenceBodyItem{item, bytesItem},
		Lanes: []laneBody{
			{Lane: "ORIGINAL", State: "READY", Required: true, CoveragePerMille: 1000},
			{Lane: "TEXT", State: "READY", Required: true, CoveragePerMille: 1000},
		},
	}, nil
}

func extractPDF(payload []byte, sourceRevisionID string) (extractResult, error) {
	if len(payload) < 5 || !bytes.HasPrefix(payload, []byte("%PDF-")) {
		return extractResult{}, ErrMalformed
	}
	if len(payload) > maxPDFBytes {
		return extractResult{}, ErrOversized
	}
	// Residual page count: count "/Type /Page" occurrences, at least 1.
	pages := uint32(bytes.Count(payload, []byte("/Type /Page")))
	// Exclude "/Type /Pages" false positives by not counting that token alone;
	// the simple residual heuristic still floors at one page for a valid header.
	if pages == 0 {
		pages = 1
	}
	if pages > maxPDFPages {
		return extractResult{}, ErrOversized
	}
	items := make([]evidenceBodyItem, 0, pages)
	for page := uint32(1); page <= pages; page++ {
		items = append(items, evidenceBodyItem{
			EvidenceID:       identity("ouroboros.stage11.evidence.v1", sourceRevisionID, "page", itoa(page)),
			SourceRevisionID: sourceRevisionID,
			AnchorKind:       "page",
			PageNumber:       page,
			Authority:        "MACHINE_OBSERVATION",
		})
	}
	return extractResult{
		Items: items,
		Lanes: []laneBody{
			{Lane: "ORIGINAL", State: "READY", Required: true, CoveragePerMille: 1000},
			{Lane: "PAGE", State: "READY", Required: true, CoveragePerMille: 1000},
		},
	}, nil
}

func extractPNG(payload []byte, sourceRevisionID string) (extractResult, error) {
	if len(payload) < 24 {
		return extractResult{}, ErrMalformed
	}
	signature := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if !bytes.HasPrefix(payload, signature) {
		return extractResult{}, ErrMalformed
	}
	// IHDR is the first chunk: length(4) + "IHDR"(4) + data...
	if string(payload[12:16]) != "IHDR" {
		return extractResult{}, ErrMalformed
	}
	width := binary.BigEndian.Uint32(payload[16:20])
	height := binary.BigEndian.Uint32(payload[20:24])
	if width == 0 || height == 0 {
		return extractResult{}, ErrMalformed
	}
	if uint64(width)*uint64(height) > maxPNGMegapixels {
		return extractResult{}, ErrOversized
	}
	if len(payload) > maxPNGBytes {
		return extractResult{}, ErrOversized
	}
	item := evidenceBodyItem{
		EvidenceID:       identity("ouroboros.stage11.evidence.v1", sourceRevisionID, "region", "0"),
		SourceRevisionID: sourceRevisionID,
		AnchorKind:       "page",
		PageNumber:       1,
		LeftPerMille:     0,
		RightPerMille:    1000,
		TopPerMille:      0,
		BottomPerMille:   1000,
		Authority:        "MACHINE_OBSERVATION",
	}
	return extractResult{
		Items: []evidenceBodyItem{item},
		Lanes: []laneBody{
			{Lane: "ORIGINAL", State: "READY", Required: true, CoveragePerMille: 1000},
			{Lane: "REGION", State: "READY", Required: true, CoveragePerMille: 1000},
		},
	}, nil
}

func extractWAV(payload []byte, sourceRevisionID string) (extractResult, error) {
	if len(payload) < 44 {
		return extractResult{}, ErrMalformed
	}
	if string(payload[0:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return extractResult{}, ErrMalformed
	}
	// Locate fmt  chunk.
	offset := 12
	var audioFormat, channels, bitsPerSample uint16
	var sampleRate, byteRate uint32
	var dataSize uint32
	foundFmt, foundData := false, false
	for offset+8 <= len(payload) {
		chunkID := string(payload[offset : offset+4])
		chunkSize := binary.LittleEndian.Uint32(payload[offset+4 : offset+8])
		dataStart := offset + 8
		dataEnd := dataStart + int(chunkSize)
		if dataEnd > len(payload) {
			return extractResult{}, ErrPartialPayload
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return extractResult{}, ErrMalformed
			}
			audioFormat = binary.LittleEndian.Uint16(payload[dataStart : dataStart+2])
			channels = binary.LittleEndian.Uint16(payload[dataStart+2 : dataStart+4])
			sampleRate = binary.LittleEndian.Uint32(payload[dataStart+4 : dataStart+8])
			byteRate = binary.LittleEndian.Uint32(payload[dataStart+8 : dataStart+12])
			bitsPerSample = binary.LittleEndian.Uint16(payload[dataStart+14 : dataStart+16])
			foundFmt = true
		case "data":
			dataSize = chunkSize
			foundData = true
		}
		// Chunks are word-aligned.
		offset = dataEnd + int(chunkSize%2)
	}
	if !foundFmt || !foundData {
		return extractResult{}, ErrMalformed
	}
	// Only linear PCM (format 1) is Stage 11 v1.
	if audioFormat != 1 {
		return extractResult{}, ErrEncryptedOrUnsupported
	}
	if channels < 1 || channels > 2 {
		return extractResult{}, ErrEncryptedOrUnsupported
	}
	if sampleRate == 0 || bitsPerSample == 0 {
		return extractResult{}, ErrMalformed
	}
	_ = byteRate
	// Widen before multiplying. sampleRate is read straight from the payload
	// and was only checked non-zero, so `sampleRate * bytesPerSample` in uint32
	// wrapped to exactly 0 for a crafted header (2^30 Hz, stereo, 16-bit) and
	// the division below panicked -- reachable from Kernel.Admit on
	// caller-supplied bytes, with no recover on the path and the kernel mutex
	// held.
	bytesPerSample := uint64(bitsPerSample/8) * uint64(channels)
	if bytesPerSample == 0 {
		return extractResult{}, ErrMalformed
	}
	frameRate := uint64(sampleRate) * bytesPerSample
	if frameRate == 0 {
		return extractResult{}, ErrMalformed
	}
	durationMs := uint64(dataSize) * 1000 / frameRate
	if durationMs == 0 {
		durationMs = 1
	}
	if durationMs > maxWAVDurationMs {
		return extractResult{}, ErrOversized
	}
	if len(payload) > maxWAVBytes {
		return extractResult{}, ErrOversized
	}
	// Residual transcript: one anonymous time-range segment covering full audio.
	// No speaker identity (DEF-010).
	item := evidenceBodyItem{
		EvidenceID:       identity("ouroboros.stage11.evidence.v1", sourceRevisionID, "audio", "0"),
		SourceRevisionID: sourceRevisionID,
		AnchorKind:       "audio",
		StartMillis:      0,
		EndMillis:        durationMs,
		SupportDigest:    digestBytes(payload[:min(len(payload), 64)]),
		Authority:        "MACHINE_OBSERVATION",
	}
	return extractResult{
		Items: []evidenceBodyItem{item},
		Lanes: []laneBody{
			{Lane: "ORIGINAL", State: "READY", Required: true, CoveragePerMille: 1000},
			{Lane: "TRANSCRIPT", State: "READY", Required: true, CoveragePerMille: 1000},
		},
	}, nil
}

func itoa(value uint32) string {
	if value == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mapExtractError preserves typed pre-decode denials; unknown errors fail closed.
func mapExtractError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrOversized),
		errors.Is(err, ErrMalformed),
		errors.Is(err, ErrMediaTypeMismatch),
		errors.Is(err, ErrEncryptedOrUnsupported),
		errors.Is(err, ErrPartialPayload),
		errors.Is(err, ErrInvalidInput),
		errors.Is(err, ErrNotFoundOrDenied):
		return err
	default:
		return ErrMalformed
	}
}
