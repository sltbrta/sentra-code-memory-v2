//go:build darwin

package localauthority

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/keyring"
)

type readOnlyKeychainRunner struct {
	calls [][]string
	fill  byte
}

func (r *readOnlyKeychainRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) == 0 || args[0] != "find-generic-password" {
		return nil, errors.New("unexpected keychain mutation")
	}
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{r.fill}, keyring.RootKeyBytes))
	return []byte(encoded + "\n"), nil
}

func TestDarwinCompositionRejectsChangedMaterialForSameReference(t *testing.T) {
	config, _ := durableTestConfig(t.TempDir())
	first, err := openDarwin(context.Background(), DarwinConfig{
		Durable: config, KeychainService: "com.ouroboros.test",
	}, &readOnlyKeychainRunner{fill: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openDarwin(context.Background(), DarwinConfig{
		Durable: config, KeychainService: "com.ouroboros.test",
	}, &readOnlyKeychainRunner{fill: 2}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("changed Keychain material = %v", err)
	}
}

func TestDarwinCompositionReadsExistingKeychainItemWithoutMutation(t *testing.T) {
	config, _ := durableTestConfig(t.TempDir())
	runner := &readOnlyKeychainRunner{}
	runtime, err := openDarwin(context.Background(), DarwinConfig{
		Durable: config, KeychainService: "com.ouroboros.test",
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "find-generic-password" ||
		runner.calls[1][0] != "find-generic-password" {
		t.Fatalf("Keychain calls = %v", runner.calls)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call[0], "add-") || strings.HasPrefix(call[0], "delete-") {
			t.Fatalf("constructor mutated Keychain: %v", call)
		}
	}
}

func TestDarwinCompositionRejectsInvalidServiceBeforeKeychainAccess(t *testing.T) {
	config, _ := durableTestConfig(t.TempDir())
	runner := &readOnlyKeychainRunner{}
	if _, err := openDarwin(context.Background(), DarwinConfig{
		Durable: config, KeychainService: "bad\nservice",
	}, runner); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid service = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid constructor accessed Keychain: %v", runner.calls)
	}
}
