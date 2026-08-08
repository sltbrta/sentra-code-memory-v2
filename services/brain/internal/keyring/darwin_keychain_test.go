//go:build darwin

package keyring

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"testing"

	"github.com/sltbrta/sentra-code-memory-v2/services/internal/contracts"
)

func TestDarwinKeychainUsesExplicitArgumentsAndStdin(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{results: []runResult{
		{err: ErrKeychainItemNotFound},
		{},
		{output: encodedRoot(1)},
	}}
	store, err := NewDarwinKeychain("com.ouroboros.test", runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := KeychainReference{Account: "tenant-a/epoch-1"}
	secret := bytes.Repeat([]byte{7}, RootKeyBytes)
	if err := store.Store(ctx, ref, secret); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls[1].args; !reflect.DeepEqual(got, []string{"add-generic-password", "-a", ref.Account, "-s", "com.ouroboros.test", "-w"}) {
		t.Fatalf("Store args = %#v", got)
	}
	if bytes.Contains([]byte(joinArgs(runner.calls[1].args)), secret) || !bytes.Contains(runner.calls[1].stdin, []byte("BwcH")) {
		t.Fatal("Store must pass encoded secret over stdin, never argv")
	}
	loaded, err := store.Load(ctx, ref)
	if err != nil || !bytes.Equal(loaded, bytes.Repeat([]byte{1}, RootKeyBytes)) {
		t.Fatalf("Load() = (%v, %v)", loaded, err)
	}
	if got := runner.calls[2].args; !reflect.DeepEqual(got, []string{"find-generic-password", "-a", ref.Account, "-s", "com.ouroboros.test", "-w"}) {
		t.Fatalf("Load args = %#v", got)
	}
}

func TestDarwinKeychainStoreIsCreateOnlyAndExactRetrySafe(t *testing.T) {
	t.Parallel()
	ref := KeychainReference{Account: "8:tenant-a7:epoch-1"}
	secret := bytes.Repeat([]byte{7}, RootKeyBytes)

	exactRunner := &recordingRunner{results: []runResult{{output: encodedRoot(7)}}}
	exact, _ := NewDarwinKeychain("com.ouroboros.test", exactRunner)
	if err := exact.Store(context.Background(), ref, secret); err != nil || len(exactRunner.calls) != 1 {
		t.Fatalf("exact retry = %v, calls = %d", err, len(exactRunner.calls))
	}

	conflictRunner := &recordingRunner{results: []runResult{{output: encodedRoot(8)}}}
	conflict, _ := NewDarwinKeychain("com.ouroboros.test", conflictRunner)
	if err := conflict.Store(context.Background(), ref, secret); !errors.Is(err, ErrKeyConflict) || len(conflictRunner.calls) != 1 {
		t.Fatalf("conflicting retry = %v, calls = %d", err, len(conflictRunner.calls))
	}

	raceRunner := &recordingRunner{results: []runResult{
		{err: ErrKeychainItemNotFound}, {err: errors.New("duplicate")}, {output: encodedRoot(7)},
	}}
	raceStore, _ := NewDarwinKeychain("com.ouroboros.test", raceRunner)
	if err := raceStore.Store(context.Background(), ref, secret); err != nil {
		t.Fatalf("exact concurrent creator retry = %v", err)
	}
	for _, call := range raceRunner.calls {
		for _, arg := range call.args {
			if arg == "-U" {
				t.Fatal("Store must never request Keychain overwrite")
			}
		}
	}
}

func TestLengthPrefixedKeychainAccountsDoNotCollide(t *testing.T) {
	t.Parallel()
	left := encodeAccount("team/user", "key")
	right := encodeAccount("team", "user/key")
	if left == right {
		t.Fatal("opaque Keychain account components collided")
	}
}

func TestDarwinKeychainRejectsMalformedReferencesAndReturnsStaticErrors(t *testing.T) {
	t.Parallel()
	store, err := NewDarwinKeychain("com.ouroboros.test", &recordingRunner{err: errors.New("secret upstream detail")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), KeychainReference{Account: ""}); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("empty account error = %v", err)
	}
	if _, err := store.Load(context.Background(), KeychainReference{Account: "tenant-a"}); !errors.Is(err, ErrKeychainUnavailable) || bytes.Contains([]byte(err.Error()), []byte("upstream")) {
		t.Fatalf("runner error = %v", err)
	}
}

func TestDarwinKeychainLoadClearsMutableCommandOutput(t *testing.T) {
	t.Parallel()
	runner := &ownedOutputRunner{output: encodedRoot(4)}
	store, err := NewDarwinKeychain("com.ouroboros.test", runner)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), KeychainReference{Account: "tenant-a"})
	if err != nil || !bytes.Equal(loaded, bytes.Repeat([]byte{4}, RootKeyBytes)) {
		t.Fatalf("Load() = (%v, %v)", loaded, err)
	}
	if !bytes.Equal(runner.output, make([]byte, len(runner.output))) {
		t.Fatal("Load must clear mutable Keychain command output")
	}
}

func TestDarwinResolverBindsTenantReferenceToKeychainAccount(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{results: []runResult{{output: encodedRoot(1)}}}
	store, err := NewDarwinKeychain("com.ouroboros.test", runner)
	if err != nil {
		t.Fatal(err)
	}
	tenant := identifierForTest("tenant", "tenant-a")
	reference := contracts.KeyReference{
		Root: identifierForTest("key-root", "tenant-a"), KeyID: identifierForTest("key", "epoch-1"), Epoch: 1,
	}
	resolver, err := NewDarwinResolver(staticReferences{current: reference}, store)
	if err != nil {
		t.Fatal(err)
	}
	material, err := resolver.Current(context.Background(), tenant)
	if err != nil || material.Reference.Epoch != 1 {
		t.Fatalf("Current() = (%+v, %v)", material, err)
	}
	want := []string{"find-generic-password", "-a", "8:tenant-a7:epoch-1", "-s", "com.ouroboros.test", "-w"}
	if !reflect.DeepEqual(runner.calls[0].args, want) {
		t.Fatalf("resolver args = %#v", runner.calls[0].args)
	}
}

type staticReferences struct {
	current contracts.KeyReference
}

func (s staticReferences) CurrentReference(context.Context, contracts.Identifier) (contracts.KeyReference, error) {
	return s.current, nil
}

func (s staticReferences) Reference(_ context.Context, _ contracts.Identifier, epoch uint64) (contracts.KeyReference, error) {
	reference := s.current
	reference.Epoch = epoch
	return reference, nil
}

func identifierForTest(namespace, value string) contracts.Identifier {
	return contracts.Identifier{Namespace: namespace, Value: value}
}

type recordedCall struct {
	args  []string
	stdin []byte
}

type recordingRunner struct {
	calls   []recordedCall
	results []runResult
	output  []byte
	err     error
}

type ownedOutputRunner struct {
	output []byte
}

func (r *ownedOutputRunner) Run(context.Context, []string, []byte) ([]byte, error) {
	return r.output, nil
}

func (r *recordingRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, error) {
	r.calls = append(r.calls, recordedCall{args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	if len(r.results) > 0 {
		result := r.results[0]
		r.results = r.results[1:]
		return append([]byte(nil), result.output...), result.err
	}
	return append([]byte(nil), r.output...), r.err
}

type runResult struct {
	output []byte
	err    error
}

func encodedRoot(value byte) []byte {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(RootKeyBytes)+1)
	base64.StdEncoding.Encode(encoded, bytes.Repeat([]byte{value}, RootKeyBytes))
	encoded[len(encoded)-1] = '\n'
	return encoded
}

func joinArgs(args []string) string {
	var joined bytes.Buffer
	for _, arg := range args {
		joined.WriteString(arg)
	}
	return joined.String()
}
