// study-keychain is installed once at a stable path. Do not rebuild it as part
// of normal collector updates: macOS authorizes this executable's identity.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chupakobra6/study-automation/internal/credentials"
)

func main() {
	authorize := len(os.Args) == 2 && os.Args[1] == "--authorize"
	if len(os.Args) > 1 && !authorize {
		fmt.Fprintln(os.Stderr, "usage: study-keychain [--authorize]; request JSON on stdin")
		os.Exit(2)
	}
	response := credentials.Response{}
	var request credentials.Request
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&request); err != nil {
		response.Error = "invalid-request"
	} else {
		response = handle(request, authorize, operations{
			store: credentials.Keychain{}, configure: credentials.ConfigureInteraction,
			grant: credentials.AuthorizeSelf, failure: credentials.FailureResponse,
			probe: func(ctx context.Context, account, service string) (credentials.Response, error) {
				path, err := os.Executable()
				if err != nil {
					return credentials.Response{}, err
				}
				return (credentials.Helper{Path: path}).Probe(ctx, account, service)
			},
		})
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		os.Exit(1)
	}
}

type credentialStore interface {
	credentials.Store
	Delete(context.Context, string, string) error
}

type operations struct {
	store     credentialStore
	configure func(bool) error
	grant     func(context.Context, string, string) error
	probe     func(context.Context, string, string) (credentials.Response, error)
	failure   func(error, string, string) credentials.Response
}

func handle(request credentials.Request, authorize bool, ops operations) credentials.Response {
	if !allowedService(request.Service) || strings.TrimSpace(request.Account) == "" || strings.ContainsRune(request.Account, 0) {
		return credentials.Response{Error: "invalid-scope"}
	}
	switch request.Operation {
	case "get", "check", "put", "delete":
	default:
		return credentials.Response{Error: "invalid-operation"}
	}
	if err := ops.configure(authorize); err != nil {
		return ops.failure(err, request.Account, request.Service)
	}
	ctx := context.Background()
	response := credentials.Response{}
	var err error
	switch request.Operation {
	case "get", "check":
		// Interactive setup changes trust; only a fresh background process proves it.
		if !authorize {
			response.Secret, err = ops.store.Get(ctx, request.Account, request.Service)
		}
	case "put":
		err = ops.store.Put(ctx, request.Account, request.Service, request.Secret)
	case "delete":
		err = ops.store.Delete(ctx, request.Account, request.Service)
	}
	if err != nil {
		return ops.failure(err, request.Account, request.Service)
	}
	if request.Operation == "check" {
		response.Secret = ""
	}
	if !authorize {
		return response
	}
	if request.Operation != "delete" {
		if err := ops.grant(ctx, request.Account, request.Service); err != nil {
			return ops.failure(err, request.Account, request.Service)
		}
		// Decrypt ACL and partition authorization are separate native checks.
		// One interactive read lets macOS confirm this executable's partition;
		// discard the secret and prove persistence in a new background process.
		if _, err := ops.store.Get(ctx, request.Account, request.Service); err != nil {
			return ops.failure(err, request.Account, request.Service)
		}
	}
	response, err = ops.probe(ctx, request.Account, request.Service)
	response.Secret = ""
	if err != nil {
		return credentials.Response{Error: "authorization-probe-failed"}
	}
	if request.Operation == "delete" {
		if response.Error == "not-found" {
			return credentials.Response{}
		}
		if response.Error == "" {
			return credentials.Response{Error: "delete-not-confirmed"}
		}
		return response
	}
	response.Authorized = response.Error == ""
	return response
}

func allowedService(service string) bool {
	return service == "codex-study-gmail-oauth" || service == "codex-study-yandex-imap" || service == "codex-study-lms"
}
