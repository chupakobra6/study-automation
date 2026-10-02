package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memoryStore struct {
	account string
	service string
	secret  string
}

func (m *memoryStore) Get(context.Context, string, string) (string, error) {
	return m.secret, nil
}

func (m *memoryStore) Put(_ context.Context, account, service, secret string) error {
	m.account, m.service, m.secret = account, service, secret
	return nil
}

func TestAuthorizeUsesLoopbackReadonlyAndStoresOnlyRefreshToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/token":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("grant_type") != "authorization_code" || request.Form.Get("code") != "test-code" {
				t.Fatalf("unexpected token exchange: %v", request.Form)
			}
			_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "access", "refresh_token": "refresh"})
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(writer).Encode(map[string]string{"emailAddress": "student@example.com"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	clientPath := filepath.Join(t.TempDir(), "client.json")
	clientJSON := `{"installed":{"client_id":"client-id","client_secret":"client-secret","auth_uri":"https://accounts.example/auth","token_uri":"` + server.URL + `/token"}}`
	if err := os.WriteFile(clientPath, []byte(clientJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{}
	openBrowser := func(target string) error {
		parsed, err := url.Parse(target)
		if err != nil {
			return err
		}
		if parsed.Query().Get("scope") != readonlyScope || parsed.Query().Get("prompt") != "consent" {
			t.Fatalf("unsafe or incomplete OAuth request: %s", target)
		}
		callback := parsed.Query().Get("redirect_uri") + "?state=" + url.QueryEscape(parsed.Query().Get("state")) + "&code=test-code"
		go func() {
			response, err := http.Get(callback)
			if err == nil {
				response.Body.Close()
			}
		}()
		return nil
	}
	err := Authorize(context.Background(), AuthorizeOptions{
		Account: "student@example.com", KeychainService: "study-gmail", ClientPath: clientPath,
		Store: store, HTTP: server.Client(), OpenBrowser: openBrowser, Timeout: time.Second,
		APIBase: server.URL + "/gmail/v1", allowTestEndpoints: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.account != "student@example.com" || store.service != "study-gmail" || store.secret != "refresh" {
		t.Fatalf("unexpected stored credential: %#v", store)
	}
}

func TestRefreshAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("refresh_token") != "refresh" || request.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("unexpected refresh request")
		}
		_ = json.NewEncoder(writer).Encode(map[string]string{"access_token": "new-access"})
	}))
	defer server.Close()
	token, err := RefreshAccessToken(context.Background(), server.Client(), OAuthClient{
		ClientID: "id", ClientSecret: "secret", TokenURI: server.URL,
	}, "refresh")
	if err != nil {
		t.Fatal(err)
	}
	if token != "new-access" {
		t.Fatalf("access token = %q", token)
	}
}

func TestValidateOAuthClientRejectsNonGoogleEndpoints(t *testing.T) {
	err := ValidateOAuthClient(OAuthClient{
		AuthURI:  "https://accounts.example/auth",
		TokenURI: "https://oauth2.googleapis.com/token",
	})
	if err == nil {
		t.Fatal("expected non-Google authorization endpoint to be rejected")
	}
}
