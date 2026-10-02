package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"

	keychain "github.com/keybase/go-keychain"
)

var ErrNotFound = errors.New("credential not found")

type Store interface {
	Get(ctx context.Context, account, service string) (string, error)
	Put(ctx context.Context, account, service, secret string) error
}

type backend interface {
	Get(account, service string) (string, error)
	Put(account, service, secret string) error
}

type Keychain struct {
	backend backend
}

func (k Keychain) native() backend {
	if k.backend != nil {
		return k.backend
	}
	return nativeBackend{}
}

func (k Keychain) Get(ctx context.Context, account, service string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	secret, err := k.native().Get(account, service)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(secret) == "" {
		return "", ErrNotFound
	}
	return secret, nil
}

func (k Keychain) Put(ctx context.Context, account, service, secret string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(secret) == "" {
		return errors.New("refuse to store an empty credential")
	}
	if err := k.native().Put(account, service, secret); err != nil {
		return fmt.Errorf("write macOS Keychain: %w", err)
	}
	return nil
}

type nativeBackend struct{}

func (nativeBackend) Get(account, service string) (string, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(service)
	query.SetAccount(account)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)
	results, err := keychain.QueryItem(query)
	if err != nil {
		return "", fmt.Errorf("read macOS Keychain: %w", err)
	}
	if len(results) == 0 {
		return "", ErrNotFound
	}
	if len(results) != 1 {
		return "", errors.New("read macOS Keychain: multiple matching credentials")
	}
	return string(results[0].Data), nil
}

func (nativeBackend) Put(account, service, secret string) error {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(service)
	query.SetAccount(account)

	update := keychain.NewItem()
	update.SetData([]byte(secret))
	if err := keychain.UpdateItem(query, update); err == nil {
		return nil
	} else if err != keychain.ErrorItemNotFound {
		return err
	}

	item := keychain.NewGenericPassword(service, account, service, []byte(secret), "")
	return keychain.AddItem(item)
}

// Delete removes only the exact account/service pair.
func (k Keychain) Delete(ctx context.Context, account, service string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(service)
	query.SetAccount(account)
	err := keychain.DeleteItem(query)
	if err == keychain.ErrorItemNotFound {
		return nil
	}
	return err
}
