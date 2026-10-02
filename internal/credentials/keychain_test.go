package credentials

import (
	"context"
	"errors"
	"testing"

	keychain "github.com/keybase/go-keychain"
)

type fakeBackend struct {
	account string
	service string
	secret  string
	err     error
}

func TestSafeFailureKeepsOSStatusAndDoesNotClaimEveryDenialIsLocked(t *testing.T) {
	for _, test := range []struct {
		err    error
		locked bool
		kind   string
		status int
	}{
		{keychain.ErrorInteractionNotAllowed, false, "interaction-required", -25308},
		{keychain.ErrorInteractionNotAllowed, true, "locked", -25308},
		{keychain.ErrorAuthFailed, false, "access-denied", -25293},
		{keychain.ErrorItemNotFound, false, "not-found", -25300},
		{errors.New("secret-bearing-native-error"), false, "keychain-error", 0},
	} {
		response := failureResponse(test.err, test.locked)
		if response.Error != test.kind || response.OSStatus != test.status || response.Secret != "" {
			t.Fatalf("unexpected failure: %+v", response)
		}
	}
}

func (f *fakeBackend) Get(account, service string) (string, error) {
	f.account = account
	f.service = service
	return f.secret, f.err
}

func (f *fakeBackend) Put(account, service, secret string) error {
	f.account = account
	f.service = service
	f.secret = secret
	return f.err
}

func TestPutPassesSecretOnlyToNativeBackend(t *testing.T) {
	backend := &fakeBackend{}
	store := Keychain{backend: backend}
	const secret = "refresh-token-sensitive"
	if err := store.Put(context.Background(), "me@example.com", "service", secret); err != nil {
		t.Fatal(err)
	}
	if backend.account != "me@example.com" || backend.service != "service" || backend.secret != secret {
		t.Fatalf("unexpected backend call: %+v", backend)
	}
}

func TestGetMapsEmptyCredentialToNotFound(t *testing.T) {
	store := Keychain{backend: &fakeBackend{}}
	if _, err := store.Get(context.Background(), "me@example.com", "service"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestKeychainHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &fakeBackend{}
	store := Keychain{backend: backend}
	if err := store.Put(ctx, "me@example.com", "service", "secret"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if backend.secret != "" {
		t.Fatal("backend was called after cancellation")
	}
}
