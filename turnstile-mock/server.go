package turnstilemock

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

//nolint:gosec // These are public deterministic provider fixtures, never production credentials.
const (
	WidgetScriptPath   = "/turnstile/v0/api.js"
	VerificationPath   = "/turnstile/v0/siteverify"
	SuccessSitekey     = "1x00000000000000000000AA"
	InvalidSitekey     = "2x00000000000000000000AB"
	ExpiredSitekey     = "3x00000000000000000000BB"
	ProviderSitekey    = "4x00000000000000000000CC"
	TestSecret         = "1x0000000000000000000000000000000AA"
	SuccessToken       = "turnstile-mock-success-token"
	InvalidToken       = "turnstile-mock-invalid-token"
	ExpiredToken       = "turnstile-mock-expired-token"
	ProviderErrorToken = "turnstile-mock-provider-error-token"
	readHeaderTimeout  = 5 * time.Second
	maxRequestBytes    = 64 << 10
)

//go:embed api.js
var widgetScript []byte

type verificationResponse struct {
	Success     bool     `json:"success"`
	ChallengeTS string   `json:"challenge_ts,omitempty"`
	Hostname    string   `json:"hostname,omitempty"`
	Action      string   `json:"action,omitempty"`
	ErrorCodes  []string `json:"error-codes,omitempty"`
}

type Server struct{}

func NewServer(addr string) (*http.Server, error) {
	if addr == "" {
		return nil, errors.New("new Turnstile mock server: listener address is empty")
	}
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}, nil
}

func NewHandler() *Server {
	return &Server{}
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/healthz":
		if request.Method != http.MethodGet {
			writeJSON(writer, http.StatusMethodNotAllowed, verificationResponse{Success: false})
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	case WidgetScriptPath:
		server.serveWidgetScript(writer, request)
	case VerificationPath:
		server.verifyToken(writer, request)
	default:
		writeJSON(writer, http.StatusNotFound, verificationResponse{Success: false})
	}
}

func (server *Server) serveWidgetScript(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeJSON(writer, http.StatusMethodNotAllowed, verificationResponse{Success: false})
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(widgetScript)
}

func (server *Server) verifyToken(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, verificationResponse{Success: false})
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	// #nosec G120 -- MaxBytesReader above bounds both form encodings.
	if err := request.ParseMultipartForm(maxRequestBytes); err != nil &&
		!errors.Is(err, http.ErrNotMultipart) {
		writeJSON(writer, http.StatusBadRequest, verificationResponse{
			Success:    false,
			ErrorCodes: []string{"bad-request"},
		})
		return
	}
	if request.FormValue("secret") != TestSecret {
		writeJSON(writer, http.StatusOK, verificationResponse{
			Success:    false,
			ErrorCodes: []string{"invalid-input-secret"},
		})
		return
	}

	switch request.FormValue("response") {
	case SuccessToken:
		writeJSON(writer, http.StatusOK, verificationResponse{
			Success:     true,
			ChallengeTS: "2026-01-01T00:00:00Z",
			Hostname:    "turnstile.mock",
			Action:      "test",
		})
	case ExpiredToken:
		writeJSON(writer, http.StatusOK, verificationResponse{
			Success:    false,
			ErrorCodes: []string{"timeout-or-duplicate"},
		})
	case ProviderErrorToken:
		writeJSON(writer, http.StatusServiceUnavailable, verificationResponse{
			Success:    false,
			ErrorCodes: []string{"internal-error"},
		})
	default:
		writeJSON(writer, http.StatusOK, verificationResponse{
			Success:    false,
			ErrorCodes: []string{"invalid-input-response"},
		})
	}
}

func writeJSON(writer http.ResponseWriter, status int, value verificationResponse) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		slog.Debug("write Turnstile mock response", "error", err)
	}
}
