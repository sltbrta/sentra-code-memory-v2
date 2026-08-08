package localbootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
)

type jsonKind uint8

const (
	jsonObject jsonKind = iota
	jsonArray
	jsonString
	jsonUnsigned
)

type jsonShape struct {
	kind       jsonKind
	fields     map[string]*jsonShape
	element    *jsonShape
	requireAll bool
}

var (
	stringShape   = &jsonShape{kind: jsonString}
	unsignedShape = &jsonShape{kind: jsonUnsigned}
	evidenceShape = objectShape(map[string]*jsonShape{
		"namespace": stringShape,
		"value":     stringShape,
	})
	relationshipShape = objectShape(map[string]*jsonShape{
		"object":   stringShape,
		"relation": stringShape,
		"user":     stringShape,
	})
	limitsShape = optionalObjectShape(map[string]*jsonShape{
		"bytes":  unsignedShape,
		"frames": unsignedShape,
	})
	grantShape = objectShape(map[string]*jsonShape{
		"id":               stringShape,
		"action":           stringShape,
		"evidence":         evidenceShape,
		"fence":            unsignedShape,
		"nonce":            stringShape,
		"expires_at":       stringShape,
		"revocation_epoch": unsignedShape,
		"limits":           limitsShape,
	})
	manifestShape = objectShape(map[string]*jsonShape{
		"version":              unsignedShape,
		"state_root":           stringShape,
		"socket_path":          stringShape,
		"database_path":        stringShape,
		"object_root":          stringShape,
		"approved_source_root": stringShape,
		"principal":            stringShape,
		"tenant":               stringShape,
		"session":              stringShape,
		"brain":                stringShape,
		"keychain_service":     stringShape,
		"key_epoch":            unsignedShape,
		"key_reference":        stringShape,
		"max_connections":      unsignedShape,
		"max_requests":         unsignedShape,
		"frame_bytes":          unsignedShape,
		"max_read_bytes":       unsignedShape,
		"revocation_epoch":     unsignedShape,
		"relationships":        {kind: jsonArray, element: relationshipShape},
		"issued_grants":        {kind: jsonArray, element: grantShape},
	})
)

func objectShape(fields map[string]*jsonShape) *jsonShape {
	return &jsonShape{kind: jsonObject, fields: fields, requireAll: true}
}

func optionalObjectShape(fields map[string]*jsonShape) *jsonShape {
	return &jsonShape{kind: jsonObject, fields: fields}
}

// validateManifestJSON rejects alternate spellings, duplicate keys, omitted
// fields, nulls, and wrong container types before decoding into Go zero values.
// The table intentionally describes the single v1 schema instead of using a
// generic reflection-based validator at this authority boundary.
func validateManifestJSON(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := validateJSONValue(decoder, manifestShape); err != nil {
		return ErrInvalidManifest
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidManifest
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder, shape *jsonShape) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch shape.kind {
	case jsonObject:
		return validateJSONObject(decoder, token, shape)
	case jsonArray:
		return validateJSONArray(decoder, token, shape.element)
	case jsonString:
		if _, ok := token.(string); !ok {
			return ErrInvalidManifest
		}
	case jsonUnsigned:
		number, ok := token.(json.Number)
		if !ok {
			return ErrInvalidManifest
		}
		if _, err := strconv.ParseUint(number.String(), 10, 64); err != nil {
			return ErrInvalidManifest
		}
	default:
		return ErrInvalidManifest
	}
	return nil
}

func validateJSONObject(decoder *json.Decoder, token json.Token, shape *jsonShape) error {
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return ErrInvalidManifest
	}
	seen := make(map[string]struct{}, len(shape.fields))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return ErrInvalidManifest
		}
		field, known := shape.fields[key]
		if !known {
			return ErrInvalidManifest
		}
		if _, duplicate := seen[key]; duplicate {
			return ErrInvalidManifest
		}
		seen[key] = struct{}{}
		if err := validateJSONValue(decoder, field); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' ||
		(shape.requireAll && len(seen) != len(shape.fields)) {
		return ErrInvalidManifest
	}
	return nil
}

func validateJSONArray(decoder *json.Decoder, token json.Token, element *jsonShape) error {
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return ErrInvalidManifest
	}
	for decoder.More() {
		if err := validateJSONValue(decoder, element); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != ']' {
		return ErrInvalidManifest
	}
	return nil
}
