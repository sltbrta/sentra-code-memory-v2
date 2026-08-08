package productsec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// validateSessionID requires a single path segment (no traversal / separators).
// Returns the base name to use under sessions/.
func validateSessionID(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("productsec: empty session id")
	}
	if sessionID == "." || sessionID == ".." {
		return "", fmt.Errorf("productsec: invalid session id")
	}
	if strings.Contains(sessionID, "/") || strings.Contains(sessionID, `\`) {
		return "", fmt.Errorf("productsec: invalid session id")
	}
	if strings.ContainsRune(sessionID, filepath.Separator) {
		return "", fmt.Errorf("productsec: invalid session id")
	}
	base := filepath.Base(sessionID)
	if base != sessionID || base == "." || base == ".." || base == "" {
		return "", fmt.Errorf("productsec: invalid session id")
	}
	return base, nil
}

// SealSession appends an encrypted session turn frame under sessions/ (SEC-003).
// Key is derived from owner string + brain dir (local profile; not KMS).
func SealSession(dir, owner, sessionID, role, content string) error {
	if dir == "" {
		return fmt.Errorf("productsec: seal session requires dir and session")
	}
	sid, err := validateSessionID(sessionID)
	if err != nil {
		return err
	}
	key := deriveKey(owner, dir)
	plain := []byte(role + "\n" + content)
	sealed, err := seal(key, plain)
	if err != nil {
		return err
	}
	sessDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(sessDir, sid+".sealed")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(base64.StdEncoding.EncodeToString(sealed) + "\n")
	return err
}

// OpenSealedSession returns decrypted turn payloads for a session (export path).
func OpenSealedSession(dir, owner, sessionID string) ([]string, error) {
	sid, err := validateSessionID(sessionID)
	if err != nil {
		return nil, err
	}
	key := deriveKey(owner, dir)
	path := filepath.Join(dir, "sessions", sid+".sealed")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range splitLines(string(raw)) {
		if line == "" {
			continue
		}
		bin, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, err
		}
		plain, err := unseal(key, bin)
		if err != nil {
			return nil, err
		}
		out = append(out, string(plain))
	}
	return out, nil
}

func deriveKey(owner, dir string) []byte {
	h := sha256.Sum256([]byte("ouroboros.productsec.v1|" + owner + "|" + dir))
	return h[:]
}

func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func unseal(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("productsec: short sealed frame")
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
