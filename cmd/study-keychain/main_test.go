package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chupakobra6/study-automation/internal/credentials"
)

type fakeStore struct {
	calls  *[]string
	getErr error
}

func (s fakeStore) Get(context.Context, string, string) (string, error) {
	*s.calls = append(*s.calls, "get")
	return "must-not-leak", s.getErr
}
func (s fakeStore) Put(context.Context, string, string, string) error {
	*s.calls = append(*s.calls, "put")
	return nil
}
func (s fakeStore) Delete(context.Context, string, string) error {
	*s.calls = append(*s.calls, "delete")
	return nil
}

func testOperations(calls *[]string, probe credentials.Response) operations {
	return operations{
		store: fakeStore{calls: calls},
		configure: func(allowed bool) error {
			if allowed {
				*calls = append(*calls, "interactive")
			} else {
				*calls = append(*calls, "background")
			}
			return nil
		},
		grant: func(_ context.Context, account, service string) error {
			if account != "me@example.test" || service != "codex-study-gmail-oauth" {
				return errors.New("wrong exact scope")
			}
			*calls = append(*calls, "grant-self")
			return nil
		},
		probe: func(_ context.Context, account, service string) (credentials.Response, error) {
			*calls = append(*calls, "new-background-process")
			return probe, nil
		},
		failure: func(error, string, string) credentials.Response { return credentials.Response{Error: "safe-error"} },
	}
}

func TestAuthorizeRequiresFreshBackgroundProbe(t *testing.T) {
	request := credentials.Request{Operation: "check", Account: "me@example.test", Service: "codex-study-gmail-oauth"}
	for _, blocked := range []bool{false, true} {
		calls := []string{}
		probe := credentials.Response{Secret: "must-not-leak"}
		if blocked {
			probe.Error, probe.OSStatus = "interaction-required", -25308
		}
		response := handle(request, true, testOperations(&calls, probe))
		if !reflect.DeepEqual(calls, []string{"interactive", "grant-self", "get", "new-background-process"}) {
			t.Fatalf("calls: %v", calls)
		}
		if response.Secret != "" || response.Authorized == blocked {
			t.Fatalf("response: %+v", response)
		}
		if blocked && (response.Error != "interaction-required" || response.OSStatus != -25308) {
			t.Fatalf("lost probe failure: %+v", response)
		}
	}
}

func TestBackgroundReadCannotGrantTrust(t *testing.T) {
	calls := []string{}
	response := handle(credentials.Request{Operation: "check", Account: "me@example.test", Service: "codex-study-gmail-oauth"}, false, testOperations(&calls, credentials.Response{}))
	if response.Secret != "" || response.Error != "" || response.Authorized {
		t.Fatalf("response: %+v", response)
	}
	if !reflect.DeepEqual(calls, []string{"background", "get"}) {
		t.Fatalf("calls: %v", calls)
	}
}

func TestInvalidScopeNeverTouchesKeychain(t *testing.T) {
	for _, request := range []credentials.Request{
		{Operation: "check", Account: "me@example.test", Service: "unrelated-credential"},
		{Operation: "check", Account: "me@example.test\x00other", Service: "codex-study-gmail-oauth"},
		{Operation: "unknown", Account: "me@example.test", Service: "codex-study-gmail-oauth"},
	} {
		calls := []string{}
		response := handle(request, true, testOperations(&calls, credentials.Response{}))
		if len(calls) != 0 || response.Error == "" {
			t.Fatalf("response=%+v calls=%v", response, calls)
		}
	}
}

func TestFailedGrantStopsBeforeProbe(t *testing.T) {
	calls := []string{}
	ops := testOperations(&calls, credentials.Response{})
	ops.grant = func(context.Context, string, string) error { return errors.New("access denied") }
	response := handle(credentials.Request{Operation: "check", Account: "me@example.test", Service: "codex-study-gmail-oauth"}, true, ops)
	if response.Error != "safe-error" || !reflect.DeepEqual(calls, []string{"interactive"}) {
		t.Fatalf("response=%+v calls=%v", response, calls)
	}
}

func TestFailedInteractiveReadStopsBeforeProbe(t *testing.T) {
	calls := []string{}
	ops := testOperations(&calls, credentials.Response{})
	ops.store = fakeStore{calls: &calls, getErr: errors.New("partition authorization denied")}
	response := handle(credentials.Request{Operation: "check", Account: "me@example.test", Service: "codex-study-gmail-oauth"}, true, ops)
	if response.Error != "safe-error" || response.Secret != "" || response.Authorized {
		t.Fatalf("response: %+v", response)
	}
	if !reflect.DeepEqual(calls, []string{"interactive", "grant-self", "get"}) {
		t.Fatalf("calls: %v", calls)
	}
}
