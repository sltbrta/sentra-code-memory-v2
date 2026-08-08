package artifactvault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

const (
	frameMagic       = "OUROFRM1"
	frameHeaderBytes = 8 + 8 + 4 + 12 + 48 + 12
)

func encryptFrame(random io.Reader, root []byte, record GenerationRecord, index uint32, plaintext []byte) ([]byte, error) {
	if len(root) != keyring.RootKeyBytes || len(plaintext) == 0 {
		return nil, ErrInvalid
	}
	dek := make([]byte, keyring.RootKeyBytes)
	defer clear(dek)
	if _, err := io.ReadFull(random, dek); err != nil {
		return nil, fmt.Errorf("artifactvault: generate data key: %w", err)
	}
	rootAEAD, err := newAEAD(root)
	if err != nil {
		return nil, err
	}
	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	wrapNonce := make([]byte, rootAEAD.NonceSize())
	dataNonce := make([]byte, dataAEAD.NonceSize())
	if _, err := io.ReadFull(random, wrapNonce); err != nil {
		return nil, fmt.Errorf("artifactvault: generate wrap nonce: %w", err)
	}
	if _, err := io.ReadFull(random, dataNonce); err != nil {
		return nil, fmt.Errorf("artifactvault: generate frame nonce: %w", err)
	}
	aad := frameAAD(record, index)
	wrapped := rootAEAD.Seal(nil, wrapNonce, dek, aad)
	ciphertext := dataAEAD.Seal(nil, dataNonce, plaintext, aad)
	envelope := make([]byte, frameHeaderBytes+len(ciphertext))
	copy(envelope[:8], frameMagic)
	binary.BigEndian.PutUint64(envelope[8:16], record.Manifest.KeyEpoch)
	binary.BigEndian.PutUint32(envelope[16:20], uint32(len(plaintext)))
	copy(envelope[20:32], wrapNonce)
	copy(envelope[32:80], wrapped)
	copy(envelope[80:92], dataNonce)
	copy(envelope[92:], ciphertext)
	return envelope, nil
}

func decryptFrame(root []byte, record GenerationRecord, frame FrameRecord, envelope []byte) ([]byte, error) {
	if len(root) != keyring.RootKeyBytes || len(envelope) < frameHeaderBytes || string(envelope[:8]) != frameMagic {
		return nil, ErrCorrupt
	}
	if binary.BigEndian.Uint64(envelope[8:16]) != record.Manifest.KeyEpoch || binary.BigEndian.Uint32(envelope[16:20]) != frame.Length {
		return nil, ErrCorrupt
	}
	rootAEAD, err := newAEAD(root)
	if err != nil {
		return nil, err
	}
	aad := frameAAD(record, frame.Index)
	dek, err := rootAEAD.Open(nil, envelope[20:32], envelope[32:80], aad)
	if err != nil {
		return nil, ErrCorrupt
	}
	defer clear(dek)
	dataAEAD, err := newAEAD(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataAEAD.Open(nil, envelope[80:92], envelope[92:], aad)
	if err != nil || len(plaintext) != int(frame.Length) {
		clear(plaintext)
		return nil, ErrCorrupt
	}
	return plaintext, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: initialize AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("artifactvault: initialize GCM: %w", err)
	}
	return aead, nil
}

func frameAAD(record GenerationRecord, index uint32) []byte {
	return []byte(encodeComposite(
		"tenant", record.Manifest.Tenant.Value,
		"kind", "artifact-frame",
		"locator", record.Locator,
		"artifact", record.Manifest.Artifact.Value,
		"generation", uintString(record.Manifest.Generation),
		"frame", uintString(uint64(index)),
	))
}

func digestBytes(data []byte) contracts.Digest {
	digest := sha256.Sum256(data)
	return contracts.Digest{Algorithm: "sha256", Hex: hex.EncodeToString(digest[:])}
}

func verifyDigest(expected contracts.Digest, data []byte) error {
	if expected.Algorithm != "sha256" || len(expected.Hex) != sha256.Size*2 {
		return ErrCorrupt
	}
	actual := digestBytes(data)
	if actual.Hex != expected.Hex {
		return ErrCorrupt
	}
	return nil
}
