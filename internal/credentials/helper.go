package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

type Request struct {
	Operation string `json:"operation"`
	Account   string `json:"account"`
	Service   string `json:"service"`
	Secret    string `json:"secret,omitempty"`
}

type Response struct {
	Secret     string `json:"secret,omitempty"`
	Error      string `json:"error,omitempty"`
	OSStatus   int    `json:"os_status,omitempty"`
	Authorized bool   `json:"authorized,omitempty"`
}

// Helper keeps the Keychain caller's executable identity stable when the main
// collector is rebuilt. Secrets travel only through a private process pipe.
type Helper struct{ Path string }

func (h Helper) invoke(ctx context.Context, request Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	data, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	command := exec.CommandContext(ctx, h.Path)
	command.Stdin = bytes.NewReader(data)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		return Response{}, fmt.Errorf("credential helper unavailable; run make install in automation/: %w", err)
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return Response{}, errors.New("invalid credential helper response")
	}
	return response, nil
}

// Probe starts a new process without interactive authorization and returns no secret.
func (h Helper) Probe(ctx context.Context, account, service string) (Response, error) {
	response, err := h.invoke(ctx, Request{Operation: "check", Account: account, Service: service})
	response.Secret = ""
	return response, err
}

func (h Helper) call(ctx context.Context, request Request) (Response, error) {
	response, err := h.invoke(ctx, request)
	if err != nil {
		return Response{}, err
	}
	if response.Error == "not-found" {
		return Response{}, ErrNotFound
	}
	if response.Error != "" {
		return Response{}, fmt.Errorf("Keychain %s (OSStatus %d); unlock the keychain if locked, or authorize study-keychain and verify its noninteractive probe", response.Error, response.OSStatus)
	}
	return response, nil
}

func (h Helper) Get(ctx context.Context, account, service string) (string, error) {
	response, err := h.call(ctx, Request{Operation: "get", Account: account, Service: service})
	return response.Secret, err
}

func (h Helper) Put(ctx context.Context, account, service, secret string) error {
	_, err := h.call(ctx, Request{Operation: "put", Account: account, Service: service, Secret: secret})
	return err
}
