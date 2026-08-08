package artifactvault

import (
	"strconv"
	"strings"
)

func encodeComposite(parts ...string) string {
	var encoded strings.Builder
	for _, part := range parts {
		encoded.WriteString(strconv.Itoa(len(part)))
		encoded.WriteByte(':')
		encoded.WriteString(part)
	}
	return encoded.String()
}

func decodeComposite(encoded string, count int) ([]string, error) {
	original := encoded
	parts := make([]string, 0, count)
	for len(encoded) > 0 && len(parts) < count {
		separator := strings.IndexByte(encoded, ':')
		if separator <= 0 {
			return nil, ErrInvalid
		}
		length, err := strconv.Atoi(encoded[:separator])
		if err != nil || length < 0 || length > len(encoded)-separator-1 {
			return nil, ErrInvalid
		}
		start := separator + 1
		parts = append(parts, encoded[start:start+length])
		encoded = encoded[start+length:]
	}
	if len(parts) != count || encoded != "" || encodeComposite(parts...) != original {
		return nil, ErrInvalid
	}
	return parts, nil
}
