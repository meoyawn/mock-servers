package turnstilemock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWidgetScriptInstallsDeterministicAPI(t *testing.T) {
	t.Parallel()
	mock := httptest.NewServer(NewHandler())
	defer mock.Close()

	response := request(t, mock, http.MethodGet, WidgetScriptPath+"?render=explicit", nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read widget script: %v", err)
	}
	for _, expected := range []string{
		"window.turnstile",
		"render(container, options)",
		"remove(id)",
		"reset(id)",
		SuccessSitekey,
		InvalidSitekey,
		ExpiredSitekey,
		ProviderSitekey,
		SuccessToken,
		InvalidToken,
		ExpiredToken,
		ProviderErrorToken,
	} {
		if !strings.Contains(string(body), expected) {
			t.Fatalf("widget script missing %q", expected)
		}
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("widget script status = %d, want 200", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/javascript; charset=utf-8" {
		t.Fatalf("widget script Content-Type = %q", response.Header.Get("Content-Type"))
	}
}

//nolint:funlen // Keep the provider protocol cases and complete response assertions together.
func TestSiteverifyScenarios(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name      string
		secret    string
		token     string
		status    int
		success   bool
		errorCode string
	}{
		{
			name:    "success",
			secret:  TestSecret,
			token:   SuccessToken,
			status:  http.StatusOK,
			success: true,
		},
		{
			name:      "invalid token",
			secret:    TestSecret,
			token:     InvalidToken,
			status:    http.StatusOK,
			errorCode: "invalid-input-response",
		},
		{
			name:      "expired token",
			secret:    TestSecret,
			token:     ExpiredToken,
			status:    http.StatusOK,
			errorCode: "timeout-or-duplicate",
		},
		{
			name:      "provider error",
			secret:    TestSecret,
			token:     ProviderErrorToken,
			status:    http.StatusServiceUnavailable,
			errorCode: "internal-error",
		},
		{
			name:      "invalid secret",
			secret:    "wrong-secret",
			token:     SuccessToken,
			status:    http.StatusOK,
			errorCode: "invalid-input-secret",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			mock := httptest.NewServer(NewHandler())
			defer mock.Close()
			form := url.Values{
				"secret":   {testCase.secret},
				"response": {testCase.token},
			}
			response := request(
				t,
				mock,
				http.MethodPost,
				VerificationPath,
				strings.NewReader(form.Encode()),
			)
			defer response.Body.Close()
			var result verificationResponse
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatalf("decode verification response: %v", err)
			}
			if response.StatusCode != testCase.status || result.Success != testCase.success {
				t.Fatalf(
					"verification status=%d success=%v, want status=%d success=%v",
					response.StatusCode,
					result.Success,
					testCase.status,
					testCase.success,
				)
			}
			if testCase.errorCode != "" &&
				(len(result.ErrorCodes) != 1 || result.ErrorCodes[0] != testCase.errorCode) {
				t.Fatalf("verification errors = %v, want %q", result.ErrorCodes, testCase.errorCode)
			}
		})
	}
}

func request(
	t *testing.T,
	mock *httptest.Server,
	method string,
	path string,
	body io.Reader,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, mock.URL+path, body)
	if err != nil {
		t.Fatalf("build mock request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := mock.Client().Do(request)
	if err != nil {
		t.Fatalf("request Turnstile mock: %v", err)
	}
	return response
}
