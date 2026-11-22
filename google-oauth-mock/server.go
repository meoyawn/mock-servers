// Package googleoauthmock provides a deterministic Google OAuth boundary for tests.
package googleoauthmock

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

const (
	YouTubeReadScope     = "https://www.googleapis.com/auth/youtube.readonly"
	YouTubeWriteScope    = "https://www.googleapis.com/auth/youtube.force-ssl"
	YouTubeScope         = YouTubeWriteScope
	defaultSubject       = "default"
	tokenLifetimeSeconds = 3600
)

type Config struct {
	ClientID     string
	ClientSecret string
	AccessToken  string
	RefreshToken string
}

func DefaultConfig() Config {
	return Config{
		ClientID: "test-google-client", ClientSecret: "test-google-secret",
		AccessToken: "test-token", RefreshToken: "test-refresh-token",
	}
}

type authorizationCode struct {
	challenge       string
	redirectURI     string
	scopes          []string
	youtube         bool
	identitySubject string
	subject         string
	tokenSubject    string
}

type youtubeGrant struct {
	scopes           []string
	omitRefreshToken bool
}

type State struct {
	AuthorizationCount       int64    `json:"authorizationCount"`
	RedirectURIs             []string `json:"redirectURIs"`
	IdentityScopes           []string `json:"identityScopes"`
	YouTubeScopes            []string `json:"youtubeScopes"`
	GrantedYouTubeScopes     []string `json:"grantedYouTubeScopes"`
	OmitYouTubeRefreshToken  bool     `json:"omitYouTubeRefreshToken"`
	IncrementalAuthorization bool     `json:"incrementalAuthorization"`
	PKCEVerified             bool     `json:"pkceVerified"`
	OfflineConsent           bool     `json:"offlineConsent"`
	RefreshCount             int64    `json:"refreshCount"`
	RevocationCount          int64    `json:"revocationCount"`
	RevokedRefreshTokens     []string `json:"revokedRefreshTokens"`
}

type Server struct {
	mu                       sync.Mutex
	config                   Config
	codes                    map[string]authorizationCode
	identityTokens           map[string]string
	state                    State
	grantedYouTubeScopes     []string
	omitYouTubeRefreshToken  bool
	grants                   map[string]youtubeGrant
	youtubeGrantConfigured   bool
	refreshSubjects          map[string]string
	revokedRefreshTokens     map[string]bool
	nextRefreshFailureStatus int
	nextRefreshFailureError  string
	nextRefreshFailureToken  string
	refreshGate              chan struct{}
	refreshGateTarget        int
	refreshGateWaiting       int
	refreshGateToken         string
	mux                      *http.ServeMux
}

func New(config Config) (*Server, error) {
	if config.ClientID == "" || config.ClientSecret == "" || config.AccessToken == "" ||
		config.RefreshToken == "" {
		return nil, errors.New(
			"create Google OAuth mock: client credentials and tokens must not be empty",
		)
	}
	server := &Server{
		config:               config,
		codes:                make(map[string]authorizationCode),
		mux:                  http.NewServeMux(),
		identityTokens:       make(map[string]string),
		grantedYouTubeScopes: []string{YouTubeScope},
		grants: make(
			map[string]youtubeGrant,
		),
		refreshSubjects:      make(map[string]string),
		revokedRefreshTokens: make(map[string]bool),
	}
	server.mux.HandleFunc(
		"GET /healthz",
		func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) },
	)
	server.mux.HandleFunc("GET /authorize", server.authorize)
	server.mux.HandleFunc("POST /token", server.token)
	server.mux.HandleFunc("POST /revoke", server.revoke)
	server.mux.HandleFunc("GET /userinfo", server.userinfo)
	server.mux.HandleFunc("GET /__mock__/state", server.mockState)
	server.mux.HandleFunc("POST /__mock__/reset", server.reset)
	server.mux.HandleFunc("POST /__mock__/youtube-grant", server.youtubeGrant)
	server.mux.HandleFunc("POST /__mock__/external-revoke", server.externalRevoke)
	server.mux.HandleFunc("POST /__mock__/next-refresh-failure", server.nextRefreshFailure)
	server.mux.HandleFunc("POST /__mock__/refresh-gate", server.configureRefreshGate)
	server.mux.HandleFunc("POST /__mock__/refresh-grant", server.configureRefreshGrant)
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) authorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	redirectURI, err := parseAuthorization(query, server.config.ClientID)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	scopes := strings.Fields(query.Get("scope"))
	youtube := isYouTubeScope(scopes)
	if err := validateAuthorizationScopes(query, scopes, youtube); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	challenge := query.Get("code_challenge")
	server.mu.Lock()
	server.state.AuthorizationCount++
	identitySubject := strings.ToLower(rand.Text())
	code := "code-" + identitySubject
	// The default catalogue remains available to existing browser journeys. E2E
	// cases that need mutable OAuth state provide their own subject cookie and
	// therefore receive a distinct token/catalogue pair.
	tokenSubject := defaultSubject
	if cookie, cookieErr := request.Cookie(
		"youtube_mock_subject",
	); cookieErr == nil &&
		cookie.Value != "" {
		tokenSubject = cookie.Value
	}
	server.codes[code] = authorizationCode{
		challenge:       challenge,
		redirectURI:     redirectURI.String(),
		scopes:          append([]string(nil), scopes...),
		youtube:         youtube,
		identitySubject: identitySubject,
		subject:         query.Get("state"),
		tokenSubject:    tokenSubject,
	}
	if youtube {
		server.state.YouTubeScopes = append([]string(nil), scopes...)
		server.state.RedirectURIs = append(server.state.RedirectURIs, redirectURI.String())
		server.state.IncrementalAuthorization = false
		server.state.OfflineConsent = true
	} else {
		server.state.IdentityScopes = append([]string(nil), scopes...)
	}
	server.mu.Unlock()
	redirectQuery := redirectURI.Query()
	redirectQuery.Set("code", code)
	redirectQuery.Set("state", query.Get("state"))
	redirectURI.RawQuery = redirectQuery.Encode()
	// OAuth deliberately redirects to the caller-supplied callback on this local test boundary.
	// Production callback registration is enforced by the application, not this fixture.
	//nolint:gosec // G710: arbitrary callback origins are required by isolated E2E servers.
	http.Redirect(writer, request, redirectURI.String(), http.StatusFound)
}

func (server *Server) token(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil ||
		request.Form.Get("client_id") != server.config.ClientID ||
		request.Form.Get("client_secret") != server.config.ClientSecret {
		writeOAuthError(writer, "invalid_client", http.StatusUnauthorized)
		return
	}
	switch request.Form.Get("grant_type") {
	case "authorization_code":
		server.exchangeCode(writer, request.Form)
	case "refresh_token":
		server.refreshToken(writer, request)
	default:
		writeOAuthError(writer, "unsupported_grant_type", http.StatusBadRequest)
	}
}

func parseAuthorization(query url.Values, clientID string) (*url.URL, error) {
	redirectURI, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirectURI.Scheme == "" || redirectURI.Host == "" ||
		query.Get(
			"client_id",
		) != clientID || query.Get("response_type") != "code" || query.Get("state") == "" {
		return nil, errors.New("invalid OAuth authorization request")
	}
	return redirectURI, nil
}

func isYouTubeScope(scopes []string) bool {
	return len(scopes) == 1 && (scopes[0] == YouTubeReadScope || scopes[0] == YouTubeWriteScope)
}

func validateAuthorizationScopes(query url.Values, scopes []string, youtube bool) error {
	if !youtube && !slices.Equal(scopes, []string{"openid", "profile", "email"}) {
		return errors.New("unsupported OAuth scope")
	}
	if youtube &&
		(query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" ||
			query.Get("access_type") != "offline" || query.Get("include_granted_scopes") != "false" ||
			query.Get("prompt") != "consent") {
		return errors.New(
			"YouTube OAuth requires S256 PKCE, offline access, explicit scopes, and consent",
		)
	}
	return nil
}

func (server *Server) refreshToken(writer http.ResponseWriter, request *http.Request) {
	refreshToken := request.Form.Get("refresh_token")
	server.mu.Lock()
	status, failure := server.takeRefreshFailureLocked(refreshToken)
	if status != 0 {
		server.mu.Unlock()
		writeOAuthError(writer, failure, status)
		return
	}
	subject := server.refreshSubjects[refreshToken]
	if refreshToken == server.config.RefreshToken && !server.revokedRefreshTokens[refreshToken] {
		subject = defaultSubject
	}
	if subject == "" || server.revokedRefreshTokens[refreshToken] {
		gate := server.arriveRefreshGateLocked(refreshToken)
		server.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-request.Context().Done():
				return
			}
		}
		writeOAuthError(writer, "invalid_grant", http.StatusBadRequest)
		return
	}
	server.state.RefreshCount++
	server.mu.Unlock()
	accessToken := server.config.AccessToken
	if subject != defaultSubject {
		accessToken += "-" + subject
	}
	writeJSON(writer, map[string]any{
		"access_token": accessToken, "expires_in": tokenLifetimeSeconds, "token_type": "Bearer",
	})
}

func (server *Server) takeRefreshFailureLocked(refreshToken string) (int, string) {
	if server.nextRefreshFailureToken != refreshToken {
		return 0, ""
	}
	status, failure := server.nextRefreshFailureStatus, server.nextRefreshFailureError
	server.nextRefreshFailureStatus = 0
	server.nextRefreshFailureError = ""
	server.nextRefreshFailureToken = ""
	return status, failure
}

func (server *Server) arriveRefreshGateLocked(refreshToken string) <-chan struct{} {
	if server.refreshGateToken != refreshToken || server.refreshGate == nil {
		return nil
	}
	gate := server.refreshGate
	server.refreshGateWaiting++
	if server.refreshGateWaiting == server.refreshGateTarget {
		close(gate)
		server.refreshGate = nil
		server.refreshGateTarget = 0
		server.refreshGateWaiting = 0
		server.refreshGateToken = ""
	}
	return gate
}

func (server *Server) revoke(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		writeOAuthError(writer, "invalid_request", http.StatusBadRequest)
		return
	}
	refreshToken := request.Form.Get("token")
	server.mu.Lock()
	_, known := server.refreshSubjects[refreshToken]
	if refreshToken == server.config.RefreshToken {
		known = true
	}
	if known && !server.revokedRefreshTokens[refreshToken] {
		server.revokedRefreshTokens[refreshToken] = true
		delete(server.refreshSubjects, refreshToken)
		server.state.RevocationCount++
		server.state.RevokedRefreshTokens = append(server.state.RevokedRefreshTokens, refreshToken)
		server.mu.Unlock()
		writer.WriteHeader(http.StatusOK)
		return
	}
	server.mu.Unlock()
	writeOAuthError(writer, "invalid_token", http.StatusBadRequest)
}

func (server *Server) externalRevoke(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.RefreshToken == "" {
		http.Error(writer, "invalid external revocation control", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	server.revokedRefreshTokens[input.RefreshToken] = true
	delete(server.refreshSubjects, input.RefreshToken)
	server.state.RevokedRefreshTokens = append(
		server.state.RevokedRefreshTokens,
		input.RefreshToken,
	)
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) nextRefreshFailure(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Error        string `json:"error"`
		RefreshToken string `json:"refreshToken"`
		Status       int    `json:"status"`
	}
	if err := json.NewDecoder(request.Body).
		Decode(&input); err != nil ||
		input.RefreshToken == "" ||
		!validRefreshFailure(input.Status, input.Error) {
		http.Error(writer, "invalid refresh failure control", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	server.nextRefreshFailureStatus = input.Status
	server.nextRefreshFailureError = input.Error
	server.nextRefreshFailureToken = input.RefreshToken
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func validRefreshFailure(status int, code string) bool {
	revoked := status == http.StatusBadRequest &&
		(code == "invalid_grant" || code == "invalid_token")
	transient := status >= http.StatusInternalServerError && status <= 599 &&
		code == "temporarily_unavailable"
	return revoked || transient
}

func (server *Server) configureRefreshGate(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
		Target       int    `json:"target"`
	}
	if err := json.NewDecoder(request.Body).
		Decode(&input); err != nil || input.RefreshToken == "" || input.Target < 2 ||
		input.Target > 10 {
		http.Error(writer, "invalid refresh gate control", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	if server.refreshGate != nil {
		close(server.refreshGate)
	}
	server.refreshGate = make(chan struct{})
	server.refreshGateTarget = input.Target
	server.refreshGateWaiting = 0
	server.refreshGateToken = input.RefreshToken
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) configureRefreshGrant(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
		Subject      string `json:"subject"`
	}
	if err := json.NewDecoder(request.Body).
		Decode(&input); err != nil || input.RefreshToken == "" ||
		input.Subject == "" {
		http.Error(writer, "invalid refresh grant control", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	server.refreshSubjects[input.RefreshToken] = input.Subject
	delete(server.revokedRefreshTokens, input.RefreshToken)
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) exchangeCode(writer http.ResponseWriter, form url.Values) {
	server.mu.Lock()
	code, ok := server.codes[form.Get("code")]
	if ok {
		delete(server.codes, form.Get("code"))
	}
	server.mu.Unlock()
	if !ok || form.Get("redirect_uri") != code.redirectURI {
		writeOAuthError(writer, "invalid_grant", http.StatusBadRequest)
		return
	}
	if code.youtube {
		digest := sha256.Sum256([]byte(form.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(digest[:])
		if challenge != code.challenge {
			writeOAuthError(writer, "invalid_grant", http.StatusBadRequest)
			return
		}
		server.mu.Lock()
		server.state.PKCEVerified = true
		server.mu.Unlock()
	}
	accessToken := server.config.AccessToken
	if code.youtube && code.tokenSubject != defaultSubject {
		accessToken += "-" + code.tokenSubject
	}
	if !code.youtube {
		accessToken += "-identity-" + code.identitySubject
		server.mu.Lock()
		server.identityTokens[accessToken] = code.identitySubject
		server.mu.Unlock()
	}
	payload := map[string]any{
		"access_token": accessToken,
		"expires_in":   tokenLifetimeSeconds,
		"token_type":   "Bearer",
	}
	if code.youtube {
		server.addYouTubeGrant(payload, code)
	}
	writeJSON(writer, payload)
}

func (server *Server) grantLocked(code authorizationCode) youtubeGrant {
	if grant, ok := server.grants[code.subject]; ok {
		return grant
	}
	if grant, ok := server.grants[code.tokenSubject]; ok {
		return grant
	}
	if server.youtubeGrantConfigured {
		return youtubeGrant{
			scopes:           server.grantedYouTubeScopes,
			omitRefreshToken: server.omitYouTubeRefreshToken,
		}
	}
	return youtubeGrant{scopes: code.scopes}
}

func (server *Server) addYouTubeGrant(payload map[string]any, code authorizationCode) {
	server.mu.Lock()
	defer server.mu.Unlock()
	grant := server.grantLocked(code)
	granted := slices.Clone(grant.scopes)
	grantKind := "write"
	if len(code.scopes) == 1 && code.scopes[0] == YouTubeReadScope {
		grantKind = "read"
	}
	refreshToken := server.config.RefreshToken + "-" + code.tokenSubject + "-" + grantKind
	server.refreshSubjects[refreshToken] = code.tokenSubject
	server.state.GrantedYouTubeScopes = granted
	if !grant.omitRefreshToken {
		payload["refresh_token"] = refreshToken
	}
	payload["scope"] = strings.Join(granted, " ")
}

func (server *Server) userinfo(writer http.ResponseWriter, request *http.Request) {
	accessToken, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	server.mu.Lock()
	subject := server.identityTokens[accessToken]
	server.mu.Unlock()
	if !bearer || subject == "" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSON(writer, map[string]any{
		"sub":            "google-sub-" + subject,
		"email":          "person-" + subject + "@example.com",
		"email_verified": true,
		"name":           "Person " + subject,
		"picture":        "https://example.test/avatar-" + subject + ".png",
	})
}

func (server *Server) mockState(writer http.ResponseWriter, request *http.Request) {
	server.mu.Lock()
	state := server.state
	if refreshToken := request.URL.Query().Get("refreshToken"); refreshToken != "" {
		state.RevocationCount = 0
		state.RevokedRefreshTokens = nil
		for _, revoked := range server.state.RevokedRefreshTokens {
			if revoked == refreshToken {
				state.RevocationCount++
				state.RevokedRefreshTokens = append(state.RevokedRefreshTokens, revoked)
			}
		}
	}
	server.mu.Unlock()
	writeJSON(writer, state)
}

func (server *Server) reset(writer http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	server.codes = make(map[string]authorizationCode)
	server.identityTokens = make(map[string]string)
	server.state = State{}
	server.grantedYouTubeScopes = []string{YouTubeWriteScope}
	server.omitYouTubeRefreshToken = false
	server.grants = make(map[string]youtubeGrant)
	server.youtubeGrantConfigured = false
	server.refreshSubjects = make(map[string]string)
	server.revokedRefreshTokens = make(map[string]bool)
	server.nextRefreshFailureStatus = 0
	server.nextRefreshFailureError = ""
	server.nextRefreshFailureToken = ""
	if server.refreshGate != nil {
		close(server.refreshGate)
	}
	server.refreshGate = nil
	server.refreshGateTarget = 0
	server.refreshGateWaiting = 0
	server.refreshGateToken = ""
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) youtubeGrant(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Scopes                   []string `json:"scopes"`
		OmitRefreshToken         bool     `json:"omitRefreshToken"`
		AuthorizationTransaction string   `json:"authorizationTransaction"`
		Subject                  string   `json:"subject"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, "invalid YouTube grant control", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	grant := youtubeGrant{
		scopes:           append([]string(nil), input.Scopes...),
		omitRefreshToken: input.OmitRefreshToken,
	}
	switch {
	case input.AuthorizationTransaction != "":
		server.grants[input.AuthorizationTransaction] = grant
	case input.Subject != "":
		server.grants[input.Subject] = grant
	default:
		server.grantedYouTubeScopes = grant.scopes
		server.omitYouTubeRefreshToken = grant.omitRefreshToken
		server.youtubeGrantConfigured = true
	}
	server.state.OmitYouTubeRefreshToken = input.OmitRefreshToken
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		slog.Error("write Google OAuth mock response", "error", err)
	}
}

func writeOAuthError(writer http.ResponseWriter, code string, status int) {
	writer.WriteHeader(status)
	writeJSON(writer, map[string]string{"error": code})
}
