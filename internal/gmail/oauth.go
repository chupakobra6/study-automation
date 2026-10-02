package gmail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chupakobra6/study-automation/internal/credentials"
)

const readonlyScope = "https://www.googleapis.com/auth/gmail.readonly"

type OAuthClient struct {
	ClientID     string
	ClientSecret string
	AuthURI      string
	TokenURI     string
}

type oauthClientFile struct {
	Installed *oauthClientJSON `json:"installed"`
}

type oauthClientJSON struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthURI      string `json:"auth_uri"`
	TokenURI     string `json:"token_uri"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func LoadOAuthClient(path string) (OAuthClient, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("read OAuth client: %w", err)
	}
	var file oauthClientFile
	if err := json.Unmarshal(data, &file); err != nil {
		return OAuthClient{}, fmt.Errorf("parse OAuth client: %w", err)
	}
	if file.Installed == nil {
		return OAuthClient{}, errors.New("OAuth client must have Desktop app type")
	}
	client := OAuthClient{
		ClientID:     strings.TrimSpace(file.Installed.ClientID),
		ClientSecret: strings.TrimSpace(file.Installed.ClientSecret),
		AuthURI:      strings.TrimSpace(file.Installed.AuthURI),
		TokenURI:     strings.TrimSpace(file.Installed.TokenURI),
	}
	if client.ClientID == "" || client.ClientSecret == "" || client.AuthURI == "" || client.TokenURI == "" {
		return OAuthClient{}, errors.New("OAuth Desktop client JSON is incomplete")
	}
	return client, nil
}

func ValidateOAuthClient(client OAuthClient) error {
	auth, err := url.Parse(client.AuthURI)
	if err != nil || auth.Scheme != "https" || auth.Hostname() != "accounts.google.com" {
		return errors.New("OAuth authorization endpoint is not an official Google endpoint")
	}
	token, err := url.Parse(client.TokenURI)
	if err != nil || token.Scheme != "https" || (token.Hostname() != "oauth2.googleapis.com" && token.Hostname() != "accounts.google.com") {
		return errors.New("OAuth token endpoint is not an official Google endpoint")
	}
	return nil
}

func ImportOAuthClient(source, target string) error {
	if _, err := LoadOAuthClient(source); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".gmail-oauth-client-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, target)
}

func RefreshAccessToken(ctx context.Context, httpClient *http.Client, client OAuthClient, refreshToken string) (string, error) {
	values := url.Values{
		"client_id":     {client.ClientID},
		"client_secret": {client.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	token, err := exchange(ctx, httpClient, client.TokenURI, values)
	if err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", errors.New("Google token refresh returned no access token")
	}
	return token.AccessToken, nil
}

type AuthorizeOptions struct {
	Account            string
	KeychainService    string
	ClientPath         string
	Store              credentials.Store
	HTTP               *http.Client
	OpenBrowser        func(string) error
	Timeout            time.Duration
	APIBase            string
	allowTestEndpoints bool
}

func Authorize(ctx context.Context, options AuthorizeOptions) error {
	client, err := LoadOAuthClient(options.ClientPath)
	if err != nil {
		return err
	}
	if !options.allowTestEndpoints {
		if err := ValidateOAuthClient(client); err != nil {
			return err
		}
	}
	if options.Store == nil {
		return errors.New("credential store is required")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start OAuth callback: %w", err)
	}
	defer listener.Close()
	redirectURI := "http://" + listener.Addr().String() + "/oauth2/callback"
	state, err := randomState()
	if err != nil {
		return err
	}
	type callbackResult struct {
		code string
		err  error
	}
	result := make(chan callbackResult, 1)
	var callbackOnce sync.Once
	complete := func(value callbackResult) {
		callbackOnce.Do(func() { result <- value })
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/callback", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("state") != state {
			http.Error(writer, "OAuth state mismatch", http.StatusBadRequest)
			complete(callbackResult{err: errors.New("OAuth state mismatch")})
			return
		}
		if oauthErr := query.Get("error"); oauthErr != "" {
			http.Error(writer, "Authorization was not completed", http.StatusBadRequest)
			complete(callbackResult{err: fmt.Errorf("Google authorization: %s", oauthErr)})
			return
		}
		code := query.Get("code")
		if code == "" {
			http.Error(writer, "Authorization code is missing", http.StatusBadRequest)
			complete(callbackResult{err: errors.New("authorization code is missing")})
			return
		}
		_, _ = io.WriteString(writer, "Gmail read-only authorization completed. You can close this tab.")
		complete(callbackResult{code: code})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Shutdown(context.Background())

	authValues := url.Values{
		"client_id":     {client.ClientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {readonlyScope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	authURL := client.AuthURI + "?" + authValues.Encode()
	openBrowser := options.OpenBrowser
	if openBrowser == nil {
		openBrowser = func(target string) error { return exec.Command("/usr/bin/open", target).Start() }
	}
	if err := openBrowser(authURL); err != nil {
		return fmt.Errorf("open Google authorization: %w", err)
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	var callback callbackResult
	select {
	case callback = <-result:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return errors.New("Google authorization timed out")
	}
	if callback.err != nil {
		return callback.err
	}
	token, err := exchange(ctx, options.HTTP, client.TokenURI, url.Values{
		"client_id":     {client.ClientID},
		"client_secret": {client.ClientSecret},
		"code":          {callback.code},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return err
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return errors.New("Google authorization returned incomplete tokens")
	}
	api := &Client{HTTP: options.HTTP, APIBase: options.APIBase, AccessToken: token.AccessToken}
	profile, err := api.Profile(ctx)
	if err != nil {
		return fmt.Errorf("verify Gmail profile: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(profile.EmailAddress), strings.TrimSpace(options.Account)) {
		return fmt.Errorf("authorized Gmail account is %q, expected %q", profile.EmailAddress, options.Account)
	}
	if err := options.Store.Put(ctx, options.Account, options.KeychainService, token.RefreshToken); err != nil {
		return err
	}
	return nil
}

func exchange(ctx context.Context, httpClient *http.Client, endpoint string, values url.Values) (tokenResponse, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := httpClient.Do(request)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("Google token request: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return tokenResponse{}, err
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return tokenResponse{}, fmt.Errorf("decode Google token response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || token.Error != "" {
		return tokenResponse{}, fmt.Errorf("Google token endpoint rejected request: %s %s", token.Error, token.Description)
	}
	return token, nil
}

func randomState() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
