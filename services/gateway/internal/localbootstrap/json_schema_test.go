package localbootstrap

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestLoadAcceptsExplicitZeroRevocationEpochs(t *testing.T) {
	manifest := validBootstrap(t)
	manifest.RevocationEpoch = 0
	for index := range manifest.IssuedGrants {
		manifest.IssuedGrants[index].RevocationEpoch = 0
	}
	path, _, digest := writeBootstrap(t, manifest)
	if _, err := Load(Options{
		ManifestPath: path, ExpectedSHA256: digest, Now: func() time.Time { return fixedNow },
	}); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsAmbiguousObjectKeys(t *testing.T) {
	payload := marshaledBootstrap(t)
	tests := map[string][]byte{
		"duplicate top-level key": replaceRequired(t, payload,
			[]byte(`"version":1`), []byte(`"version":1,"version":1`)),
		"duplicate grant key": replaceRequired(t, payload,
			[]byte(`"id":"grant-read"`), []byte(`"id":"grant-read","id":"grant-read"`)),
		"duplicate evidence key": replaceRequired(t, payload,
			[]byte(`"namespace":"evidence","value":"evidence-a"`),
			[]byte(`"namespace":"evidence","namespace":"evidence","value":"evidence-a"`)),
		"duplicate limits key": replaceRequired(t, payload,
			[]byte(`"limits":{"bytes":1024}`), []byte(`"limits":{"bytes":1024,"bytes":1024}`)),
		"case alias top-level": replaceRequired(t, payload,
			[]byte(`"version":1`), []byte(`"Version":1`)),
		"case alias grant": replaceRequired(t, payload,
			[]byte(`"revocation_epoch":3,"limits":{"bytes":1024}`),
			[]byte(`"revocation_Epoch":3,"limits":{"bytes":1024}`)),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, candidate, 0o600)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func TestLoadRejectsOmittedRequiredEpochs(t *testing.T) {
	payload := marshaledBootstrap(t)
	tests := map[string][]byte{
		"manifest revocation epoch": replaceRequired(t, payload,
			[]byte(`,"revocation_epoch":3,"relationships"`), []byte(`,"relationships"`)),
		"grant revocation epoch": replaceRequired(t, payload,
			[]byte(`,"revocation_epoch":3,"limits":{"bytes":1024}`),
			[]byte(`,"limits":{"bytes":1024}`)),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, candidate, 0o600)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func TestLoadRejectsWrongNestedContainersAndNulls(t *testing.T) {
	payload := marshaledBootstrap(t)
	tests := map[string][]byte{
		"relationships object": replaceRequired(t, payload,
			payloadSection(t, payload, `"relationships":[`, `],"issued_grants"`),
			[]byte(`"relationships":{}`)),
		"grant limits array": replaceRequired(t, payload,
			[]byte(`"limits":{"bytes":1024}`), []byte(`"limits":[]`)),
		"null security scalar": replaceRequired(t, payload,
			[]byte(`"revocation_epoch":3`), []byte(`"revocation_epoch":null`)),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			path, digest := writeRaw(t, candidate, 0o600)
			assertLoadError(t, path, digest, ErrInvalidManifest)
		})
	}
}

func marshaledBootstrap(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(validBootstrap(t))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return payload
}

func replaceRequired(t *testing.T, payload, old, replacement []byte) []byte {
	t.Helper()
	if !bytes.Contains(payload, old) {
		t.Fatalf("test fixture does not contain %q", old)
	}
	return bytes.Replace(payload, old, replacement, 1)
}

func payloadSection(t *testing.T, payload []byte, start, end string) []byte {
	t.Helper()
	startIndex := bytes.Index(payload, []byte(start))
	endIndex := bytes.Index(payload, []byte(end))
	if startIndex < 0 || endIndex < startIndex {
		t.Fatalf("test fixture does not contain section %q through %q", start, end)
	}
	return payload[startIndex : endIndex+1]
}
