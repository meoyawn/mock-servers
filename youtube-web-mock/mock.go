// Package youtubewebmock provides a non-forwarding HTTPS CONNECT proxy for
// deterministic YouTube client E2E traffic. It only serves the narrow local surface
// configured by FixtureDirectory and never opens an upstream connection.
package youtubewebmock

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Independent provider SAPISID authentication contract requires SHA-1.
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	youtubeWebHost        = "www.youtube.com"
	decimalBase           = 10
	privateFileMode       = 0o600
	controlBodyLimit      = 64 << 10
	anonymousVisitor      = "listenbox-anonymous-visitor"
	playerTimestamp       = 12345
	playlistSuffixLength  = 10
	certificateSerialBits = 128
	certificateLifetime   = 24 * time.Hour
	playlistBodyLimit     = 256 << 10
	readHeaderTimeout     = 5 * time.Second
	requestTimeout        = 15 * time.Second
)

// Config defines the closed, local-only mock boundary.
type Config struct {
	Address               string
	CAFile                string
	FixtureDirectory      string
	CloneIdentifierPlayer bool
	// PlayerScript overrides the player and requires stream URL deciphering.
	PlayerScript string
}

// Request records one locally served upstream-shaped request for E2E assertions.
type Request struct {
	Host             string `json:"host"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Query            string `json:"query"`
	Range            string `json:"range"`
	HasAuthorization bool   `json:"has_authorization"`
	HasCookie        bool   `json:"has_cookie"`
	ValidCookieAuth  bool   `json:"valid_cookie_auth"`
	Client           string `json:"client"`
}

// Server owns the CONNECT listener and its generated E2E trust root.
type Server struct {
	listener net.Listener
	server   *http.Server
	tls      *tls.Config
	fixtures string

	mu                      sync.Mutex
	requests                map[string][]Request
	gates                   map[string]chan struct{}
	playlists               map[string]PlaylistFixture
	videoTitles             map[string]string
	videoArtwork            map[string]string
	channels                map[string]ChannelFixture
	videoDates              map[string]videoDates
	failedRanges            map[string]int64
	mediaWindows            map[string]chan int64
	unavailableVideos       map[string]bool
	playbackSubreasons      map[string]string
	sabrVideos              map[string]bool
	mobileSABR              map[string]bool
	embeddingDisabled       map[string]bool
	loginRequiredVideos     map[string]string
	forbiddenMedia          map[string]bool
	refusedAudioVariants    map[string]bool
	mobileForbiddenProbes   map[string]bool
	embeddedForbiddenMedia  map[string]bool
	nativeForbiddenMedia    map[string]bool
	nativeUnavailableVideos map[string]bool
	cookieSessions          map[string]*CookieSession
	playerDisconnects       map[string]bool
	rateLimitedVideos       map[string]bool
	nativeRateLimited       map[string]bool
	liveOfflineVideos       map[string]bool
	fixturePlayers          map[string]map[string]any
	cloneIdentifierPlayer   bool
	playerScript            string
}

// Start creates a generated CA and a strict local proxy. CAFile is consumed by
// the CLI SSL_CERT_FILE setting in E2E; production never uses this server.
func Start(ctx context.Context, config Config) (*Server, error) {
	if config.Address == "" || config.CAFile == "" || config.FixtureDirectory == "" {
		return nil, errors.New("YouTube web mock requires address, CA file, and fixture directory")
	}
	if err := VerifyFixtures(ctx, config.FixtureDirectory); err != nil {
		return nil, err
	}
	certificate, caPEM, err := newCertificate()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(config.CAFile, caPEM, privateFileMode); err != nil {
		return nil, fmt.Errorf("write YouTube web mock CA: %w", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", config.Address)
	if err != nil {
		return nil, err
	}
	mock := &Server{
		listener:              listener,
		fixtures:              config.FixtureDirectory,
		cloneIdentifierPlayer: config.CloneIdentifierPlayer,
		requests:              make(map[string][]Request),
		gates:                 make(map[string]chan struct{}),
		playlists:             make(map[string]PlaylistFixture),
		videoTitles:           make(map[string]string),
		videoArtwork:          make(map[string]string),
		tls: &tls.Config{
			MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate},
		},
	}
	mock.server = &http.Server{Handler: mock, ReadHeaderTimeout: readHeaderTimeout}
	mock.unavailableVideos = make(map[string]bool)
	mock.playerScript = config.PlayerScript
	mock.channels = make(map[string]ChannelFixture)
	mock.videoDates = make(map[string]videoDates)
	mock.playbackSubreasons = make(map[string]string)
	mock.sabrVideos = make(map[string]bool)
	mock.mobileSABR = make(map[string]bool)
	mock.embeddingDisabled = make(map[string]bool)
	mock.loginRequiredVideos = make(map[string]string)
	mock.forbiddenMedia = make(map[string]bool)
	mock.refusedAudioVariants = make(map[string]bool)
	mock.mobileForbiddenProbes = make(map[string]bool)
	mock.embeddedForbiddenMedia = make(map[string]bool)
	mock.nativeForbiddenMedia = make(map[string]bool)
	mock.nativeUnavailableVideos = make(map[string]bool)
	mock.cookieSessions = make(map[string]*CookieSession)
	mock.playerDisconnects = make(map[string]bool)
	mock.rateLimitedVideos = make(map[string]bool)
	mock.nativeRateLimited = make(map[string]bool)
	mock.liveOfflineVideos = make(map[string]bool)
	mock.fixturePlayers = make(map[string]map[string]any)
	go func() { _ = mock.server.Serve(listener) }()
	return mock, nil
}

// Addr returns the listener address suitable for the CLI HTTPS_PROXY setting.
func (server *Server) Addr() string { return server.listener.Addr().String() }

// Close terminates the local mock without touching external resources.
func (server *Server) Close(ctx context.Context) error {
	server.mu.Lock()
	for gateContext := range server.gates {
		close(server.gates[gateContext])
		delete(server.gates, gateContext)
	}
	for context, window := range server.mediaWindows {
		close(window)
		delete(server.mediaWindows, context)
	}
	server.mu.Unlock()
	return server.server.Shutdown(ctx)
}

// Requests returns an immutable snapshot of served upstream-shaped requests.
func (server *Server) Requests(context string) []Request {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]Request(nil), server.requests[context]...)
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method + " " + request.URL.Path {
	case http.MethodGet + " " + "/healthz":
		writer.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet + " " + "/__mock__/requests":
		context := request.URL.Query().Get("context")
		if context == "" {
			http.Error(writer, "request context is required", http.StatusBadRequest)
			return
		}
		server.serveRequests(writer, context)
		return
	case http.MethodPost + " " + "/__mock__/playlist":
		server.configurePlaylist(writer, request)
		return
	case http.MethodPost + " " + "/__mock__/channel":
		server.configureChannel(writer, request)
		return
	case http.MethodPost + " " + "/__mock__/gate":
		server.configureGate(writer, request)
		return
	case http.MethodPost + " " + "/__mock__/range-failure":
		server.configureRangeFailure(writer, request)
		return
	case http.MethodPost + " " + "/__mock__/media-window":
		server.configureMediaWindow(writer, request)
		return
	}
	if request.Method != http.MethodConnect {
		http.Error(writer, "YouTube web mock requires CONNECT", http.StatusBadRequest)
		return
	}
	server.serveConnect(writer, request)
}

func (server *Server) serveRequests(writer http.ResponseWriter, context string) {
	body, err := json.Marshal(struct {
		Requests []Request `json:"requests"`
	}{Requests: server.Requests(context)})
	if err != nil {
		http.Error(writer, "encode requests", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	// A failed write means the peer has gone away; there is no response to retry.
	if _, err := writer.Write(append(body, '\n')); err != nil {
		return
	}
}

func (server *Server) serveConnect(writer http.ResponseWriter, request *http.Request) {
	host, _, err := net.SplitHostPort(request.Host)
	if err != nil || !allowedHost(host) {
		http.Error(writer, "unknown CONNECT host", http.StatusForbidden)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		http.Error(writer, "CONNECT unsupported", http.StatusInternalServerError)
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer connection.Close()
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	tlsConnection := tls.Server(connection, server.tls)
	_ = tlsConnection.SetDeadline(time.Now().Add(requestTimeout))
	if err := tlsConnection.HandshakeContext(request.Context()); err != nil {
		return
	}
	defer tlsConnection.Close()
	reader := bufio.NewReader(tlsConnection)
	proxied, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	defer proxied.Body.Close()
	_ = tlsConnection.SetDeadline(time.Time{})
	if !allowedHost(proxied.Host) {
		writeProxyResponse(tlsConnection, http.StatusForbidden, "unknown HTTPS host")
		return
	}
	server.record(proxied)
	server.waitForGate(requestContext(proxied))
	server.serveProxied(tlsConnection, proxied)
}

func (server *Server) configureGate(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Context  string   `json:"context"`
		Contexts []string `json:"contexts"`
		Enabled  bool     `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, controlBodyLimit)).Decode(&input); err != nil {
		http.Error(writer, "gate context is required", http.StatusBadRequest)
		return
	}
	if input.Context != "" {
		input.Contexts = append(input.Contexts, input.Context)
	}
	if len(input.Contexts) == 0 {
		http.Error(writer, "gate context is required", http.StatusBadRequest)
		return
	}
	for _, context := range input.Contexts {
		if strings.TrimSpace(context) == "" {
			http.Error(writer, "gate context is required", http.StatusBadRequest)
			return
		}
	}
	server.mu.Lock()
	for _, context := range input.Contexts {
		if existing := server.gates[context]; existing != nil {
			close(existing)
			delete(server.gates, context)
		}
		if input.Enabled {
			server.gates[context] = make(chan struct{})
		}
	}
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) waitForGate(context string) {
	if context == "" {
		return
	}
	server.mu.Lock()
	gate := server.gates[context]
	server.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (server *Server) record(request *http.Request) {
	server.recordFor(requestContext(request), request)
}

func (server *Server) recordFor(context string, request *http.Request) {
	if context == "" {
		return
	}
	server.mu.Lock()
	server.requests[context] = append(server.requests[context], Request{
		ValidCookieAuth: validCookieAuth(request),
		Host:            request.Host,
		Method:          request.Method,
		Path:            request.URL.Path,
		Query:           request.URL.RawQuery,
		Range:           request.Header.Get("Range"),
		HasAuthorization: request.Header.Get(
			"Authorization",
		) != "",
		HasCookie: request.Header.Get("Cookie") != "",
		Client:    request.Header.Get("X-Youtube-Client-Name"),
	})
	server.mu.Unlock()
}

func requestContext(request *http.Request) string {
	context := request.URL.Query().Get("context")
	if context == "" {
		if fixture, _, found := splitFixtureReference(request.URL.Query().Get("fixture")); found {
			context = fixture
		}
	}
	if context == "" {
		segments := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(segments) >= 2 && segments[len(segments)-1] == "videoplayback" {
			if decoded, err := url.PathUnescape(segments[len(segments)-2]); err == nil {
				context = decoded
			}
		}
	}
	if context == "" && request.URL.Path == "/watch" {
		context = request.URL.Query().Get("v")
	}
	return context
}

func (server *Server) serveProxied(writer io.Writer, request *http.Request) {
	switch request.URL.Path {
	case "/sw.js_data":
		serveServiceWorker(writer, request)
	case "/":
		writeProxyResponse(writer, http.StatusOK, "<!doctype html>")
	case "/iframe_api":
		writeProxyResponse(writer, http.StatusOK, `var player="player\/deadbeef\/";`)
	case "/s/player/deadbeef/player_es6.vflset/en_US/base.js":
		if server.playerScript != "" {
			server.recordFor("player-script", request)
			writeProxyResponse(writer, http.StatusOK, server.playerScript)
			return
		}
		if server.cloneIdentifierPlayer {
			server.recordFor("player-script", request)
			writeProxyResponse(
				writer,
				http.StatusOK,
				`(function(g){var foo=1;var stamp=function(){return {signatureTimestamp:12345,foo};};})(this);`,
			)
			return
		}
		writeProxyResponse(
			writer,
			http.StatusOK,
			`(function(g){var stamp=function(){return {signatureTimestamp:12345}};})(this);`,
		)
	case "/watch":
		videoID := request.URL.Query().Get("v")
		encoded, err := json.Marshal(server.playerResponse(videoID))
		if err != nil {
			writeProxyResponse(writer, http.StatusInternalServerError, "encode player")
			return
		}
		writeProxyResponse(
			writer,
			http.StatusOK,
			"<!doctype html><script>var ytInitialPlayerResponse="+string(encoded)+";</script>",
		)
	case "/youtubei/v1/player":
		server.servePlayer(writer, request)
	case "/youtubei/v1/browse":
		server.servePlaylist(writer, request)
	default:
		server.serveMediaPath(writer, request)
	}
}

func (server *Server) serveMediaPath(writer io.Writer, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/@") {
		server.serveChannel(writer, request)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/embed/") {
		server.serveEmbedPage(writer, request)
		return
	}
	if !strings.HasSuffix(request.URL.Path, "/videoplayback") {
		if strings.HasPrefix(request.URL.Path, "/720/") ||
			strings.HasPrefix(request.URL.Path, "/1080/") {
			server.serveMedia(writer, request)
			return
		}
		writeProxyResponse(writer, http.StatusNotFound, "unknown YouTube mock path")
		return
	}
	server.serveMedia(writer, request)
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func (server *Server) playerResponse(videoID string) map[string]any {
	response := server.rawPlayerResponse(videoID)
	if server.playerScript != "" {
		rewritePlayerStreamURLs(response, func(source string) string { return source + "&n=fixture" })
	}
	server.mu.Lock()
	title, artwork := server.videoTitles[videoID], server.videoArtwork[videoID]
	dates := server.videoDates[videoID]
	server.mu.Unlock()
	if original, ok := response["videoDetails"].(map[string]string); ok {
		details := make(map[string]any, len(original)+1)
		for key, value := range original {
			details[key] = value
		}
		if title != "" {
			details["title"] = title
		}
		if artwork != "" {
			details["thumbnail"] = map[string]any{
				"thumbnails": []any{map[string]any{"url": artwork, "width": 1400, "height": 1400}},
			}
		}
		response["videoDetails"] = details
	}
	if dates.published == "" {
		dates.published = "2024-05-06T12:30:00Z"
	}
	if dates.uploaded == "" {
		dates.uploaded = "2024-05-06"
	}
	response["microformat"] = map[string]any{"playerMicroformatRenderer": map[string]any{
		"publishDate": dates.published, "uploadDate": dates.uploaded,
		"thumbnail": map[string]any{"thumbnails": []any{}}, "lengthSeconds": "2",
	}}
	return response
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func (server *Server) rawPlayerResponse(videoID string) map[string]any {
	// Gate at the response boundary as well as the recorded-request boundary.
	// Clients can discover the same player through either /watch or the player
	// API, but every usable extraction must obtain this response.
	server.waitForGate(videoID)
	server.mu.Lock()
	unavailable := server.unavailableVideos[videoID]
	subreason := server.playbackSubreasons[videoID]
	sabr := server.sabrVideos[videoID]
	loginRequired := server.loginRequiredVideos[videoID]
	refusedAudioVariant := server.refusedAudioVariants[videoID]
	fixturePlayer := server.fixturePlayers[videoID]
	rateLimited := server.rateLimitedVideos[videoID]
	liveOffline := server.liveOfflineVideos[videoID]
	server.mu.Unlock()
	if rateLimited {
		return rateLimitedPlayerResponse()
	}
	if liveOffline {
		return liveOfflinePlayerResponse(videoID)
	}
	if fixturePlayer != nil {
		return fixturePlayer
	}
	if loginRequired != "" {
		return map[string]any{"playabilityStatus": map[string]string{
			"status": "LOGIN_REQUIRED", "reason": loginRequired,
		}}
	}
	if unavailable {
		return configuredUnavailableResponse(subreason)
	}
	if refusedAudioVariant {
		return refusedAudioVariantPlayerResponse(videoID)
	}
	return selectedPlayerResponse(videoID, sabr)
}

func selectedPlayerResponse(videoID string, sabr bool) map[string]any {
	if strings.HasPrefix(videoID, "u") {
		return unavailablePlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "c") {
		return claimedContentPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "n") {
		return nativeHLSPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "w") {
		return opusDASHPlayerResponse(videoID)
	}
	if sabr {
		return sabrPlayerResponse(videoID)
	}
	return ordinaryPlayerResponse(videoID)
}

func ordinaryPlayerResponse(videoID string) map[string]any {
	if strings.HasPrefix(videoID, "x") || strings.HasPrefix(videoID, "z") {
		return renditionPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "d") {
		return dashPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "l") || strings.HasPrefix(videoID, "s") {
		return lowResolutionPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "a") {
		return audioOnlyPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "o") {
		return opusPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "q") || strings.HasPrefix(videoID, "m") {
		return audioFallbackPlayerResponse(videoID)
	}
	if strings.HasPrefix(videoID, "i") {
		return incompatiblePlayerResponse(videoID)
	}
	return progressivePlayerResponse(videoID)
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func progressivePlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox deterministic fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{"formats": []map[string]any{
			{
				"itag":            18,
				"url":             fixtureURL("fallback/720.mp4", videoID),
				"mimeType":        "video/mp4; codecs=\"avc1.64001F, mp4a.40.2\"",
				"bitrate":         1_000_000,
				"width":           1280,
				"height":          720,
				"fps":             30,
				"qualityLabel":    "720p",
				"audioQuality":    "AUDIO_QUALITY_MEDIUM",
				"audioSampleRate": "48000",
				"audioChannels":   2,
			},
			{
				"itag":            22,
				"url":             fixtureURL("fallback/1080.mp4", videoID),
				"mimeType":        "video/mp4; codecs=\"avc1.64001F, mp4a.40.2\"",
				"bitrate":         2_000_000,
				"width":           1920,
				"height":          1080,
				"fps":             30,
				"qualityLabel":    "1080p",
				"audioQuality":    "AUDIO_QUALITY_MEDIUM",
				"audioSampleRate": "48000",
				"audioChannels":   2,
			},
		}},
	}
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func unavailablePlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{
			"status": "ERROR",
			"reason": "Video unavailable",
		},
		"videoDetails": map[string]string{
			"videoId": videoID,
		},
	}
}

const rateLimitedReason = "This content isn't available, try again later."

// rateLimitedPlayerResponse is YouTube's answer to every player request from a
// session it has rate-limited for up to an hour.
//
//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func rateLimitedPlayerResponse() map[string]any {
	return map[string]any{"playabilityStatus": map[string]any{
		"status": "ERROR", "reason": "Video unavailable",
		"errorScreen": map[string]any{"playerErrorMessageRenderer": map[string]any{
			"reason":    map[string]string{"simpleText": "Video unavailable"},
			"subreason": map[string]any{"runs": []any{map[string]string{"text": rateLimitedReason}}},
		}},
	}}
}

func liveOfflinePlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]any{
			"status": "LIVE_STREAM_OFFLINE", "reason": "This live event will begin in a few moments.",
		},
		"videoDetails": map[string]any{
			"videoId": videoID, "title": "Scheduled stream", "isLiveContent": true, "isUpcoming": true,
		},
	}
}

func claimedContentPlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]any{
			"status": "ERROR",
			"reason": "Video unavailable",
			"errorScreen": map[string]any{"playerErrorMessageRenderer": map[string]any{
				"reason": map[string]string{"simpleText": "Video unavailable"},
				"subreason": map[string]any{"runs": []any{
					map[string]string{"text": "It was blocked due to the claimed content by "},
					map[string]string{"text": "WMG."},
				}},
			}},
		},
		"videoDetails": map[string]string{
			"videoId": videoID,
		},
	}
}

//nolint:mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func incompatiblePlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox incompatible fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{"formats": []map[string]any{
			{
				"itag":            18,
				"url":             fixtureURL("fallback/720.mp4", videoID),
				"mimeType":        "video/mp4; codecs=\"vp09.00.51.08, opus\"",
				"bitrate":         1_000_000,
				"width":           1280,
				"height":          720,
				"fps":             30,
				"qualityLabel":    "720p",
				"audioQuality":    "AUDIO_QUALITY_MEDIUM",
				"audioSampleRate": "48000",
				"audioChannels":   2,
			},
		}},
	}
}

//nolint:mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func nativeHLSPlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox native HLS fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{
			"hlsManifestUrl": fixtureURL("native-hls/master.m3u8", videoID),
			"formats": []map[string]any{
				{
					"itag":            18,
					"url":             fixtureURL("fallback/720.mp4", videoID),
					"mimeType":        "video/mp4; codecs=\"avc1.64001F, mp4a.40.2\"",
					"bitrate":         1_000_000,
					"width":           1280,
					"height":          720,
					"fps":             30,
					"qualityLabel":    "720p",
					"audioQuality":    "AUDIO_QUALITY_MEDIUM",
					"audioSampleRate": "48000",
					"audioChannels":   2,
				},
			},
		},
	}
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func dashFormats(videoID string) []map[string]any {
	return []map[string]any{
		{
			"itag":         136,
			"url":          fixtureURL("dash-range/720/manifest-stream0.mp4", videoID),
			"mimeType":     "video/mp4; codecs=\"avc1.64001F\"",
			"bitrate":      1_000_000,
			"width":        1280,
			"height":       720,
			"fps":          30,
			"quality":      "hd720",
			"qualityLabel": "720p",
		},
		{
			"itag":         137,
			"url":          fixtureURL("dash-range/1080/manifest-stream0.mp4", videoID),
			"mimeType":     "video/mp4; codecs=\"avc1.640028\"",
			"bitrate":      2_000_000,
			"width":        1920,
			"height":       1080,
			"fps":          30,
			"quality":      "hd1080",
			"qualityLabel": "1080p",
		},
		{
			"itag":            140,
			"url":             fixtureURL("dash-range/1080/manifest-stream1.mp4", videoID),
			"mimeType":        "audio/mp4; codecs=\"mp4a.40.2\"",
			"bitrate":         128_000,
			"audioQuality":    "AUDIO_QUALITY_MEDIUM",
			"audioSampleRate": "48000",
			"audioChannels":   2,
		},
	}
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func dashPlayerResponse(videoID string) map[string]any {
	return dashPlayerWithStreaming(videoID, map[string]any{"adaptiveFormats": dashFormats(videoID)})
}

func refusedAudioVariantPlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	variant := maps.Clone(formats[2])
	variant["isDrc"], variant["bitrate"] = true, 128_001
	variant["url"] = fixtureURL("dash-range/1080/manifest-stream1.mp4", videoID) + "&xtags=drc%3D1"
	return dashPlayerWithStreaming(videoID, map[string]any{"adaptiveFormats": append(formats, variant)})
}

func dashPlayerWithStreaming(videoID string, streaming map[string]any) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox DASH fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": streaming,
	}
}

//nolint:mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func audioOnlyPlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox AAC-only fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{"adaptiveFormats": []map[string]any{{
			"itag": 140, "url": fixtureURL("dash-range/1080/manifest-stream1.mp4", videoID),
			"mimeType": "audio/mp4; codecs=\"mp4a.40.2\"", "bitrate": 128_000,
			"audioQuality": "AUDIO_QUALITY_MEDIUM", "audioSampleRate": "48000", "audioChannels": 2,
		}}},
	}
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func opusPlayerResponse(videoID string) map[string]any {
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox Opus-only fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{"adaptiveFormats": []map[string]any{{
			"itag": 251, "url": fixtureURL("audio/opus.webm", videoID),
			"mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 128_000,
			"audioQuality": "AUDIO_QUALITY_MEDIUM", "audioSampleRate": "48000", "audioChannels": 2,
		}}},
	}
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func audioFallbackPlayerResponse(videoID string) map[string]any {
	opusPath := "audio/opus.webm"
	if strings.HasPrefix(videoID, "m") {
		opusPath = "audio/missing-opus.webm"
	}
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Listenbox audio download fallback fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
		},
		"streamingData": map[string]any{
			"adaptiveFormats": []map[string]any{
				{
					"itag":            140,
					"url":             fixtureURL("audio/missing-aac.m4a", videoID),
					"mimeType":        "audio/mp4; codecs=\"mp4a.40.2\"",
					"bitrate":         128_000,
					"audioQuality":    "AUDIO_QUALITY_MEDIUM",
					"audioSampleRate": "48000",
					"audioChannels":   2,
				},
				{
					"itag":            251,
					"url":             fixtureURL(opusPath, videoID),
					"mimeType":        "audio/webm; codecs=\"opus\"",
					"bitrate":         128_000,
					"audioQuality":    "AUDIO_QUALITY_MEDIUM",
					"audioSampleRate": "48000",
					"audioChannels":   2,
				},
			},
			"formats": []map[string]any{
				{
					"itag":            22,
					"url":             fixtureURL("fallback/1080.mp4", videoID),
					"mimeType":        "video/mp4; codecs=\"avc1.640028, mp4a.40.2\"",
					"bitrate":         2_000_000,
					"width":           1920,
					"height":          1080,
					"fps":             30,
					"qualityLabel":    "1080p",
					"audioQuality":    "AUDIO_QUALITY_MEDIUM",
					"audioSampleRate": "48000",
					"audioChannels":   2,
				},
			},
		},
	}
}

func fixtureURL(name string, context string) string {
	return "https://r1.googlevideo.com/videoplayback?fixture=" + url.QueryEscape(
		context+"--"+name,
	) + ""
}

func (server *Server) serveMedia(writer io.Writer, request *http.Request) {
	name, context := mediaFixtureReference(request)
	if request.Method == http.MethodHead && strings.HasPrefix(name, "dash-range/") {
		writeProxyResponse(writer, http.StatusForbidden, "YouTube DASH media HEAD is forbidden")
		return
	}
	if reason, forbidden := server.mediaForbidden(request, context); forbidden {
		writeProxyResponse(writer, http.StatusForbidden, reason)
		return
	}
	if name == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
		writeProxyResponse(writer, http.StatusNotFound, "unknown media fixture")
		return
	}
	directory, relative := server.fixtures, name
	if source, found := strings.CutPrefix(name, "source/"); found {
		path, err := server.sourceFixturePath(source)
		if err != nil {
			writeProxyResponse(writer, http.StatusNotFound, "unknown source fixture")
			return
		}
		// Source fixtures are an exact allowlist in the sibling corpus.
		directory, relative = filepath.Dir(path), filepath.Base(path)
	}
	file, err := openMediaFixture(directory, relative)
	if err != nil {
		writeProxyResponse(writer, http.StatusNotFound, "unknown media fixture")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 {
		writeProxyResponse(writer, http.StatusInternalServerError, "invalid media fixture")
		return
	}
	server.serveMediaContent(writer, request, file, name, context, info.Size())
}

func (server *Server) serveMediaContent(writer io.Writer, request *http.Request,
	file *os.File, name, context string, size int64,
) {
	start, end, partial, err := byteRange(request.Header.Get("Range"), size)
	if err != nil {
		writeProxyResponse(writer, http.StatusRequestedRangeNotSatisfiable, "invalid range")
		return
	}
	server.mu.Lock()
	failedStart, fail := server.failedRanges[context]
	fail = fail && start == failedStart
	if fail {
		delete(server.failedRanges, context)
	}
	server.mu.Unlock()
	if fail {
		writeProxyResponse(writer, http.StatusServiceUnavailable, "injected interrupted range")
		return
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		writeProxyResponse(writer, http.StatusInternalServerError, "seek fixture")
		return
	}
	status := http.StatusOK
	// ServeHTTP closes the CONNECT tunnel after this response. Declare that
	// before clients can pool the connection for their next media request.
	headers := fmt.Sprintf(
		"Connection: close\r\nContent-Type: %s\r\nAccept-Ranges: bytes\r\nContent-Length: %d\r\nETag: \"%s-%d\"\r\n",
		fixtureContentType(name),
		end-start,
		name,
		size,
	)
	if partial {
		status = http.StatusPartialContent
		headers += fmt.Sprintf("Content-Range: bytes %d-%d/%d\r\n", start, end-1, size)
	}
	if request.Method == http.MethodHead {
		_, _ = fmt.Fprintf(
			writer,
			"HTTP/1.1 %d %s\r\n%s\r\n",
			status,
			http.StatusText(status),
			headers,
		)
		return
	}
	if strings.HasSuffix(name, ".m3u8") {
		serveMediaPlaylist(writer, file, name, context, status)
		return
	}
	_, _ = fmt.Fprintf(writer, "HTTP/1.1 %d %s\r\n%s\r\n", status, http.StatusText(status), headers)
	if start == 0 && end-start > 1 {
		server.copyMediaWindows(writer, file, end-start, context)
		return
	}
	_, _ = io.CopyN(writer, file, end-start)
}

func fixtureContentType(name string) string {
	switch filepath.Ext(name) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".mpd":
		return "application/dash+xml"
	case ".m4s":
		return "video/iso.segment"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "audio/webm"
	default:
		return "application/octet-stream"
	}
}

func rewritePlaylist(name string, body []byte, context string) []byte {
	directory := filepath.ToSlash(filepath.Dir(filepath.FromSlash(name)))
	rewritten := make([]string, 0, strings.Count(string(body), "\n")+1)
	for line := range strings.SplitSeq(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
			if uri, ok := hlsURI(trimmed); ok {
				line = strings.Replace(
					line,
					uri,
					fixtureURL(filepath.ToSlash(filepath.Join(directory, uri)), context),
					1,
				)
			}
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			line = fixtureURL(filepath.ToSlash(filepath.Join(directory, trimmed)), context)
		}
		rewritten = append(rewritten, line)
	}
	return []byte(strings.Join(rewritten, "\n"))
}

func hlsURI(line string) (string, bool) {
	_, after, found := strings.Cut(line, "URI=\"")
	if !found {
		return "", false
	}
	value, _, found := strings.Cut(after, "\"")
	return value, found && value != ""
}

func writeProxyResponse(writer io.Writer, status int, body string) {
	_, _ = fmt.Fprintf(
		writer,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status,
		http.StatusText(status),
		len(body),
		body,
	)
}

func writeJSON(writer io.Writer, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		writeProxyResponse(writer, http.StatusInternalServerError, "encode response")
		return
	}
	_, _ = fmt.Fprintf(
		writer,
		"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		http.StatusOK,
		http.StatusText(http.StatusOK),
		len(body),
		body,
	)
}

func allowedHost(value string) bool {
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	return host == "youtube.com" || host == youtubeWebHost || host == "m.youtube.com" ||
		host == "music.youtube.com" ||
		strings.HasSuffix(host, ".googlevideo.com")
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistResponse(suffix string) map[string]any {
	if len(suffix) < playlistSuffixLength {
		suffix = "1234567890"
	}
	suffix = suffix[:playlistSuffixLength]
	ids := []string{"f" + suffix, "d" + suffix, "f" + suffix}
	entries := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, map[string]any{"playlistVideoRenderer": map[string]any{
			"videoId":    id,
			"isPlayable": true,
			"title": map[string]any{
				"runs": []map[string]string{{"text": "Local video " + id}},
				"accessibility": map[string]any{
					"accessibilityData": map[string]string{"label": "Local video " + id},
				},
			},
			"lengthSeconds":      "2",
			"lengthText":         map[string]string{"simpleText": "0:02"},
			"thumbnail":          map[string]any{"thumbnails": []any{}},
			"navigationEndpoint": map[string]any{"watchEndpoint": map[string]string{"videoId": id}},
		}})
	}
	return map[string]any{
		"metadata": map[string]any{
			"playlistMetadataRenderer": map[string]any{"title": "Local YouTube playlist"},
		},
		"contents": map[string]any{
			"twoColumnBrowseResultsRenderer": map[string]any{
				"tabs": []map[string]any{
					{
						"tabRenderer": map[string]any{
							"content": map[string]any{
								"sectionListRenderer": map[string]any{
									"contents": []map[string]any{
										{
											"playlistVideoListRenderer": map[string]any{
												"contents": entries,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func byteRange(header string, size int64) (int64, int64, bool, error) {
	if header == "" {
		return 0, size, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, 0, false, errors.New("unsupported range")
	}
	value := strings.TrimPrefix(header, "bytes=")
	startText, endText, found := strings.Cut(value, "-")
	if !found || startText == "" {
		return 0, 0, false, errors.New("invalid range")
	}
	start, err := parsePositiveInt(startText)
	if err != nil || start >= size {
		return 0, 0, false, errors.New("invalid range")
	}
	end := size
	if endText != "" {
		inclusive, parseErr := parsePositiveInt(endText)
		if parseErr != nil || inclusive < start {
			return 0, 0, false, errors.New("invalid range")
		}
		end = min(inclusive+1, size)
	}
	return start, end, true, nil
}

func parsePositiveInt(value string) (int64, error) {
	parsed, err := new(big.Int).SetString(value, decimalBase)
	if !err || !parsed.IsInt64() || parsed.Sign() < 0 {
		return 0, errors.New("invalid integer")
	}
	return parsed.Int64(), nil
}

func newCertificate() (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), certificateSerialBits)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Listenbox E2E YouTube Web Mock CA"},
		NotBefore: now.Add(
			-time.Minute,
		),
		NotAfter:              now.Add(certificateLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(
		rand.Reader,
		caTemplate,
		caTemplate,
		&caKey.PublicKey,
		caKey,
	)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certificate, err := newLeafCertificate(caTemplate, caKey, now, serialLimit)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func newLeafCertificate(caTemplate *x509.Certificate, caKey *ecdsa.PrivateKey,
	now time.Time, serialLimit *big.Int,
) (tls.Certificate, error) {
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafSerial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: youtubeWebHost},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(certificateLifetime),
		DNSNames: []string{
			"youtube.com",
			"www.youtube.com",
			"m.youtube.com",
			"music.youtube.com",
			"googlevideo.com",
			"*.googlevideo.com",
		},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(
		rand.Reader,
		leafTemplate,
		caTemplate,
		&leafKey.PublicKey,
		caKey,
	)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate, err := tls.X509KeyPair(
		leafPEM,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return tls.Certificate{}, err
	}
	return certificate, nil
}

// ProxyURL returns an HTTP proxy URL for a listener address.
func ProxyURL(address string) (string, error) {
	parsed, err := url.Parse("http://" + address)
	if err != nil || parsed.Host == "" {
		return "", errors.New("invalid YouTube web mock address")
	}
	return parsed.String(), nil
}

func splitFixtureReference(value string) (string, string, bool) {
	before, after, found := strings.CutLast(value, "--")
	return before, after, found
}

// Checks the signature against the exact cookie sent on this request, including
// metadata requests after the fixture rotates SAPISID in its player response.
func validCookieAuth(request *http.Request) bool {
	signatures := strings.Fields(request.Header.Get("Authorization"))
	if len(signatures) == 0 || len(signatures)%2 != 0 {
		return false
	}
	for _, pair := range []struct{ scheme, name string }{
		{"SAPISIDHASH", "SAPISID"},
		{"SAPISID1PHASH", "__Secure-1PAPISID"},
		{"SAPISID3PHASH", "__Secure-3PAPISID"},
	} {
		cookie, err := request.Cookie(pair.name)
		if err != nil {
			if pair.name == "SAPISID" {
				return false
			}
			continue
		}
		var signature string
		for index := 0; index < len(signatures); index += 2 {
			if signatures[index] == pair.scheme {
				signature = signatures[index+1]
			}
		}
		if !validCookieSignature(signature, cookie.Value) {
			return false
		}
	}
	return true
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func serveEmbeddedPlayer(writer io.Writer, input playerRequest, disabled bool, response map[string]any) bool {
	if input.Context.Client.Name != "WEB_EMBEDDED_PLAYER" ||
		input.Context.Client.Version != "2.20261002.01.00" ||
		input.Context.ThirdParty.EmbedURL != "https://www.google.com/" ||
		input.Playback.Content.EncryptedHostFlags != "fixture-embed:"+input.VideoID {
		writeJSON(
			writer,
			map[string]any{
				"playabilityStatus": map[string]string{
					"status": "ERROR",
					"reason": "Embedder identity denied (152 - 18)",
				},
			},
		)
		return false
	}
	if disabled {
		writeJSON(writer, map[string]any{"playabilityStatus": map[string]any{
			"status": "UNPLAYABLE", "reason": "Video unavailable",
			"errorScreen": map[string]any{"playerInterstitialRenderer": map[string]any{
				"content": map[string]any{"interstitialViewModel": map[string]any{
					"title": map[string]string{"content": "Video unavailable"},
					"description": map[string]string{
						"content": "Playback on other websites has been disabled by the video owner",
					},
				}},
			}},
		}})
		return false
	}
	delete(response, "microformat")
	return true
}

func serveCreatorPlayer(writer io.Writer, request *http.Request, input playerRequest, response map[string]any) bool {
	if input.Context.Client.Name != "WEB_CREATOR" || !validCookieAuth(request) {
		writeJSON(
			writer,
			map[string]any{
				"playabilityStatus": map[string]string{
					"status": "LOGIN_REQUIRED",
					"reason": "Sign in required",
				},
			},
		)
		return false
	}
	delete(response, "microformat")
	return true
}

func serveNativePlayer(writer io.Writer, request *http.Request, state playerAccess, response map[string]any) bool {
	if request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
		writeProxyResponse(
			writer,
			http.StatusBadRequest,
			"VisionOS does not support browser credentials",
		)
		return false
	}
	if state.nativeUnavailable {
		writeJSON(
			writer,
			map[string]any{"playabilityStatus": map[string]string{
				"status": "UNPLAYABLE", "reason": "This video is not available",
			}},
		)
		return false
	}
	if state.nativeRateLimited {
		writeJSON(writer, map[string]any{"playabilityStatus": map[string]string{
			"status": "UNPLAYABLE", "reason": rateLimitedReason,
		}})
		return false
	}
	delete(response, "microformat")
	return true
}

type playerRequest struct {
	VideoID  string `json:"videoId"`
	Playback struct {
		Content struct {
			Timestamp          int    `json:"signatureTimestamp"`
			EncryptedHostFlags string `json:"encryptedHostFlags"`
		} `json:"contentPlaybackContext"`
	} `json:"playbackContext"`
	Context struct {
		Client struct {
			VisitorData string `json:"visitorData"`
			Name        string `json:"clientName"`
			Version     string `json:"clientVersion"`
		} `json:"client"`
		ThirdParty struct {
			EmbedURL string `json:"embedUrl"`
		} `json:"thirdParty"`
	} `json:"context"`
}

func (server *Server) servePlayer(writer io.Writer, request *http.Request) {
	var body playerRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, controlBodyLimit)).
		Decode(&body); err != nil ||
		body.VideoID == "" {
		writeProxyResponse(writer, http.StatusBadRequest, "missing player video ID")
		return
	}
	server.recordFor(body.VideoID, request)
	server.waitForGate(body.VideoID)
	if server.disconnectPlayer(body.VideoID) {
		// Returning without a response injects a transport failure through the
		// real embedded JS fetch; serveConnect owns and closes the tunnel.
		return
	}
	if body.Context.Client.VisitorData != anonymousVisitor {
		writeJSON(
			writer,
			map[string]any{
				"playabilityStatus": map[string]string{
					"status": "LOGIN_REQUIRED",
					"reason": "Anonymous visitor session missing",
				},
			},
		)
		return
	}
	state := server.playerAccess(body.VideoID)
	session := state.session
	client := request.Header.Get("X-Youtube-Client-Name")
	if session != nil && client != "101" && !session.authorizes(request) {
		writeJSON(writer, map[string]any{"playabilityStatus": map[string]string{
			"status": "LOGIN_REQUIRED", "reason": "Sign in to confirm you’re not a bot",
		}})
		return
	}
	response := server.playerResponse(body.VideoID)
	if body.Playback.Content.Timestamp != playerTimestamp {
		writeProxyResponse(writer, http.StatusForbidden, "Playback requires a player timestamp")
		return
	}
	if !applyPlayerClient(writer, request, body, response, state) {
		return
	}

	server.rewriteClientMedia(response, body.VideoID, client)
	if session != nil && session.Rotate != "" {
		writeRotatedPlayer(writer, response, session.Rotate)
		return
	}
	writeJSON(writer, response)
}

//nolint:mnd // Independent provider response contract: positional service-worker data.
func serveServiceWorker(writer io.Writer, request *http.Request) {
	device := make([]any, 108)
	device[0], device[1], device[13] = "en", "US", anonymousVisitor
	device[16], device[17], device[18] = "2.20260925.01.00", "Windows", "10.0"
	// Anonymous service-worker responses may omit the optional install data.
	if request.Header.Get("Cookie") != "" {
		device[61] = []any{"fixture-install-data"}
	}
	device[79] = "UTC"
	device[86], device[87] = "Chrome", "146.0.0.0"
	data, err := json.Marshal([]any{[]any{nil, nil, []any{[]any{device}, "fixture-api-key"}}})
	if err != nil {
		panic(err)
	}
	writeProxyResponse(writer, http.StatusOK, ")]}'\n"+string(data))
}

type playerAccess struct {
	session                                                *CookieSession
	sabr, mobileSABR, embeddingDisabled, nativeUnavailable bool
	nativeRateLimited                                      bool
}

func (server *Server) playerAccess(videoID string) playerAccess {
	server.mu.Lock()
	defer server.mu.Unlock()
	return playerAccess{
		session:           server.cookieSessions[videoID],
		sabr:              server.sabrVideos[videoID],
		mobileSABR:        server.mobileSABR[videoID],
		embeddingDisabled: server.embeddingDisabled[videoID],
		nativeUnavailable: server.nativeUnavailableVideos[videoID],
		nativeRateLimited: server.nativeRateLimited[videoID],
	}
}

func (session *CookieSession) authorizes(request *http.Request) bool {
	cookie, err := request.Cookie("SAPISID")
	if err != nil || !validCookieAuth(request) {
		return false
	}
	return cookie.Value == session.Expected || (session.Rotate != "" && cookie.Value == session.Rotate)
}

func applyPlayerClient(writer io.Writer, request *http.Request, input playerRequest,
	response map[string]any, state playerAccess,
) bool {
	client := request.Header.Get("X-Youtube-Client-Name")
	switch client {
	case "1", "2", "7":
		if (client == "1" && state.sabr) || (client == "2" && state.mobileSABR) {
			applySABRResponse(response)
		}
	case "56":
		return serveEmbeddedPlayer(writer, input, state.embeddingDisabled, response)
	case "62":
		return serveCreatorPlayer(writer, request, input, response)
	case "101":
		return serveNativePlayer(writer, request, state, response)
	default:
		writeProxyResponse(writer, http.StatusForbidden, "Unexpected playback client")
		return false
	}
	return true
}

func applySABRResponse(response map[string]any) {
	streaming, ok := response["streamingData"].(map[string]any)
	if !ok {
		return
	}
	streaming["serverAbrStreamingUrl"] = "https://rr1.googlevideo.com/sabr-only"
	formats, _ := streaming["adaptiveFormats"].([]map[string]any)
	for _, format := range formats {
		delete(format, "url")
	}
}

func rewritePlayerStreamURLs(response map[string]any, rewrite func(string) string) {
	streaming, ok := response["streamingData"].(map[string]any)
	if !ok {
		return
	}
	for _, key := range []string{"formats", "adaptiveFormats"} {
		formats, _ := streaming[key].([]map[string]any)
		for _, format := range formats {
			if source, ok := format["url"].(string); ok {
				format["url"] = rewrite(source)
			}
		}
	}
}

func writeRotatedPlayer(writer io.Writer, response map[string]any, cookie string) {
	encoded, err := json.Marshal(response)
	if err != nil {
		writeProxyResponse(writer, http.StatusInternalServerError, "encode response")
		return
	}
	_, _ = fmt.Fprintf(writer,
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"+
			"Set-Cookie: SAPISID=%s; Domain=.youtube.com; Path=/; Secure; HttpOnly\r\n"+
			"Set-Cookie: PREF=fixture; Domain=.youtube.com; Path=/; Secure\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", cookie, len(encoded), encoded)
}

//nolint:mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func opusDASHPlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	formats[2] = map[string]any{
		"itag": 251, "url": fixtureURL("audio/opus.webm", videoID),
		"mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 128_000,
		"audioQuality": "AUDIO_QUALITY_MEDIUM", "audioSampleRate": "48000", "audioChannels": 2,
	}
	return dashPlayerWithStreaming(videoID, map[string]any{"adaptiveFormats": formats})
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func renditionPlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	streaming := map[string]any{"adaptiveFormats": formats}
	if strings.HasPrefix(videoID, "x") {
		formats[1]["url"] = fixtureURL(
			"dash-range/1080/manifest-stream0.mp4",
			videoID,
		) + "&rendition=efficient"
		streaming["adaptiveFormats"] = append(formats, map[string]any{
			"itag": 299,
			"url": fixtureURL(
				"dash-range/1080/manifest-stream0.mp4",
				videoID,
			) + "&rendition=expensive",
			"mimeType":     "video/mp4; codecs=\"avc1.640028\"",
			"bitrate":      4_000_000,
			"width":        1920,
			"height":       1080,
			"fps":          30,
			"qualityLabel": "1080p",
		})
	} else {
		formats[2] = map[string]any{
			"itag":            251,
			"url":             fixtureURL("audio/opus.webm", videoID),
			"mimeType":        "audio/webm; codecs=\"opus\"",
			"bitrate":         256_000,
			"audioQuality":    "AUDIO_QUALITY_HIGH",
			"audioSampleRate": "48000",
			"audioChannels":   2,
		}
		streaming["formats"] = []map[string]any{
			{
				"itag":            22,
				"url":             fixtureURL("fallback/1080.mp4", videoID),
				"mimeType":        "video/mp4; codecs=\"avc1.640028, mp4a.40.2\"",
				"bitrate":         3_000_000,
				"width":           1920,
				"height":          1080,
				"fps":             30,
				"qualityLabel":    "1080p",
				"audioQuality":    "AUDIO_QUALITY_MEDIUM",
				"audioSampleRate": "48000",
				"audioChannels":   2,
			},
		}
	}
	return dashPlayerWithStreaming(videoID, streaming)
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func sabrPlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	streaming := map[string]any{"adaptiveFormats": formats}
	formats[2]["audioTrack"] = map[string]any{
		"audioIsDefault": true,
		"id":             "en.original",
		"displayName":    "English (original)",
	}
	streaming["adaptiveFormats"] = append(formats, map[string]any{
		"itag":            251,
		"url":             fixtureURL("audio/opus.webm", videoID),
		"mimeType":        "audio/webm; codecs=\"opus\"",
		"bitrate":         256000,
		"audioQuality":    "AUDIO_QUALITY_HIGH",
		"audioSampleRate": "48000",
		"audioChannels":   2,
		"audioTrack": map[string]any{
			"audioIsDefault": true,
			"id":             "en.original",
			"displayName":    "English (original)",
		},
	}, map[string]any{
		"itag": 139,
		"url": fixtureURL(
			"dash-range/1080/manifest-stream1.mp4",
			videoID,
		) + "&quality=low",
		"mimeType":        "audio/mp4; codecs=\"mp4a.40.2\"",
		"bitrate":         64000,
		"audioQuality":    "AUDIO_QUALITY_LOW",
		"audioSampleRate": "48000",
		"audioChannels":   2,
		"audioTrack": map[string]any{
			"audioIsDefault": true,
			"id":             "en.original",
			"displayName":    "English (original)",
		},
	}, map[string]any{
		"itag": 140,
		"url": fixtureURL(
			"dash-range/1080/manifest-stream1.mp4",
			videoID,
		) + "&track=fr.dubbed",
		"mimeType":        "audio/mp4; codecs=\"mp4a.40.2\"",
		"bitrate":         512000,
		"audioQuality":    "AUDIO_QUALITY_HIGH",
		"audioSampleRate": "48000",
		"audioChannels":   2,
		"audioTrack": map[string]any{
			"audioIsDefault": false,
			"id":             "fr.dubbed",
			"displayName":    "French",
		},
	})
	return dashPlayerWithStreaming(videoID, streaming)
}

func lowResolutionPlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	streaming := map[string]any{"adaptiveFormats": formats}
	streaming["adaptiveFormats"] = []map[string]any{formats[0], formats[2]}
	if strings.HasPrefix(videoID, "s") {
		// WEB can advertise supported low-resolution formats without direct URLs.
		formats[0]["width"], formats[0]["height"], formats[0]["qualityLabel"] = 854, 480, "480p"
		delete(formats[0], "url")
		delete(formats[2], "url")
	}
	return dashPlayerWithStreaming(videoID, streaming)
}

func configuredUnavailableResponse(subreason string) map[string]any {
	if subreason != "" {
		return map[string]any{"playabilityStatus": map[string]any{
			"status": "UNPLAYABLE", "reason": "Video unavailable",
			"errorScreen": map[string]any{"playerInterstitialRenderer": map[string]any{
				"content": map[string]any{"interstitialViewModel": map[string]any{
					"title":       map[string]string{"content": "Video unavailable"},
					"description": map[string]string{"content": subreason},
				}},
			}},
		}}
	}
	return map[string]any{"playabilityStatus": map[string]string{
		"status": "UNPLAYABLE", "reason": "This video is not available in your country",
	}}
}

func validCookieSignature(signature, cookie string) bool {
	timestamp, digest, ok := strings.Cut(signature, "_")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if !ok || err != nil || time.Now().Unix()-seconds > 60 || seconds-time.Now().Unix() > 60 {
		return false
	}
	expected := fmt.Sprintf(
		"%x",
		//nolint:gosec // Independent provider SAPISID signature contract requires SHA-1, not a selectable hash.
		sha1.Sum([]byte(timestamp+" "+cookie+" https://www.youtube.com")),
	)
	return digest == expected
}

func (server *Server) disconnectPlayer(videoID string) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	disconnect := server.playerDisconnects[videoID]
	delete(server.playerDisconnects, videoID)
	return disconnect
}

func (server *Server) rewriteClientMedia(response map[string]any, videoID, client string) {
	server.mu.Lock()
	mobileProbeForbidden := server.mobileForbiddenProbes[videoID]
	embeddedForbidden := server.embeddedForbiddenMedia[videoID]
	nativeForbidden := server.nativeForbiddenMedia[videoID]
	server.mu.Unlock()
	if mobileProbeForbidden || embeddedForbidden || nativeForbidden {
		rewritePlayerStreamURLs(response, func(source string) string {
			return source + "&playback_client=" + client
		})
	}
}

func mediaFixtureReference(request *http.Request) (string, string) {
	name := request.URL.Query().Get("fixture")
	context := request.URL.Query().Get("context")
	if fixtureContext, fixture, found := splitFixtureReference(name); found {
		if context == "" {
			context = fixtureContext
		}
		name = fixture
	}
	if name == "" {
		// Native HLS child playlists and fragments are deliberately relative in
		// the checked-in fixture so FFprobe can open the master locally. Resolve
		// those direct Googlevideo requests through the same fixture tree.
		relative := strings.TrimPrefix(request.URL.Path, "/")
		if strings.HasPrefix(relative, "720/") || strings.HasPrefix(relative, "1080/") {
			name = "native-hls/" + relative
		}
	}
	return name, context
}

func (server *Server) mediaForbidden(request *http.Request, context string) (string, bool) {
	server.mu.Lock()
	forbidden := server.forbiddenMedia[context]
	refusedAudioVariant := server.refusedAudioVariants[context]
	mobileProbeForbidden := server.mobileForbiddenProbes[context]
	embeddedForbidden := server.embeddedForbiddenMedia[context]
	nativeForbidden := server.nativeForbiddenMedia[context]
	server.mu.Unlock()
	if mobileProbeForbidden && request.URL.Query().Get("playback_client") == "2" {
		return "", true
	}
	if nativeForbidden && request.URL.Query().Get("playback_client") == "101" {
		return "Video unavailable", true
	}
	// A playable response and a successful first range do not prove permission
	// to download the rest. Refuse only this test's later embedded ranges.
	if forbidden || (refusedAudioVariant && request.URL.Query().Get("xtags") == "drc=1") ||
		(embeddedForbidden && request.URL.Query().Get("playback_client") == "56" &&
			!strings.HasPrefix(request.Header.Get("Range"), "bytes=0-")) {
		return "Video unavailable", true
	}
	return "", false
}

func openMediaFixture(directory, name string) (*os.File, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.FromSlash(name))
}

func serveMediaPlaylist(writer io.Writer, file io.Reader, name, context string, status int) {
	body, readErr := io.ReadAll(io.LimitReader(file, playlistBodyLimit))
	if readErr != nil {
		writeProxyResponse(writer, http.StatusInternalServerError, "read playlist")
		return
	}
	body = rewritePlaylist(name, body, context)
	headers := fmt.Sprintf(
		"Connection: close\r\nContent-Type: %s\r\nContent-Length: %d\r\n",
		fixtureContentType(name),
		len(body),
	)
	_, _ = fmt.Fprintf(
		writer,
		"HTTP/1.1 %d %s\r\n%s\r\n%s",
		status,
		http.StatusText(status),
		headers,
		body,
	)
}
