package googleoauthmock_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	googleoauthmock "github.com/meoyawn/mock-servers/google-oauth-mock"
)

const callbackURL = "https://example.test/callback"

func TestYouTubeAuthorizationRequiresOfflineConsentAndPKCE(t *testing.T) {
	t.Parallel()
	server, client := newOAuthServer(t)
	code := authorizeYouTube(t, server, client)
	if scope := exchangeYouTubeCode(
		t,
		server,
		client,
		code,
	); scope != googleoauthmock.YouTubeScope {
		t.Fatalf("scope = %q", scope)
	}
}

func TestYouTubeAuthorizationCanReturnInsufficientGrantedScope(t *testing.T) {
	t.Parallel()
	server, client := newOAuthServer(t)
	control := oauthRequest(
		t,
		client,
		http.MethodPost,
		server.URL+"/__mock__/youtube-grant",
		"application/json",
		bytes.NewBufferString(`{"scopes":["https://www.googleapis.com/auth/youtube.readonly"]}`),
	)
	defer control.Body.Close()
	if control.StatusCode != http.StatusNoContent {
		t.Fatalf("control status = %d", control.StatusCode)
	}
	code := authorizeYouTube(t, server, client)
	if scope := exchangeYouTubeCode(
		t,
		server,
		client,
		code,
	); scope != googleoauthmock.YouTubeReadScope {
		t.Fatalf("scope = %q", scope)
	}
}

func newOAuthServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	handler, err := googleoauthmock.New(googleoauthmock.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return server, client
}

func authorizeYouTube(t *testing.T, server *httptest.Server, client *http.Client) string {
	t.Helper()
	digest := sha256.Sum256([]byte("verifier"))
	authorize := server.URL + "/authorize?" + url.Values{
		"client_id":              {"test-google-client"},
		"redirect_uri":           {callbackURL},
		"response_type":          {"code"},
		"scope":                  {googleoauthmock.YouTubeScope},
		"state":                  {"state"},
		"access_type":            {"offline"},
		"include_granted_scopes": {"false"},
		"prompt":                 {"consent"},
		"code_challenge_method": {
			"S256",
		},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])},
	}.Encode()
	response := oauthRequest(t, client, http.MethodGet, authorize, "", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", response.StatusCode)
	}
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return callback.Query().Get("code")
}

func exchangeYouTubeCode(
	t *testing.T,
	server *httptest.Server,
	client *http.Client,
	code string,
) string {
	t.Helper()
	token := oauthRequest(
		t,
		client,
		http.MethodPost,
		server.URL+"/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(url.Values{
			"client_id": {"test-google-client"}, "client_secret": {"test-google-secret"},
			"grant_type": {"authorization_code"}, "redirect_uri": {callbackURL},
			"code": {code}, "code_verifier": {"verifier"},
		}.Encode()),
	)
	defer token.Body.Close()
	if token.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d", token.StatusCode)
	}
	var payload struct {
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(token.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Scope
}

func oauthRequest(
	t *testing.T,
	client *http.Client,
	method, address, contentType string,
	body io.Reader,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, address, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
