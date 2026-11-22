// Package youtubemock provides a stateful local subset of YouTube Data API v3.
package youtubemock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxMetadataBytes             = 1 << 20
	maxUploadBytes               = int64(8 << 30)
	youtubeProcessingDetailsPart = "processingDetails"
	defaultVideoCategoryID       = "22"
	defaultProcessingPolls       = 2
	tokenLifetimeSeconds         = 3600
	defaultListMaxResults        = 5
	maxListResults               = 50
	longUploadsAllowed           = "allowed"
	privacyPublic                = "public"
	processingPending            = "processing"
	processingSucceeded          = "succeeded"
	processingFailed             = "failed"
	uploadPending                = "uploaded"
	uploadProcessed              = "processed"
	uploadRejected               = "rejected"
	videosPath                   = "/youtube/v3/videos"

	operationChannelsListValue        = "channels.list"
	operationPlaylistsListValue       = "playlists.list"
	operationPlaylistItemsListValue   = "playlistItems.list"
	operationVideosInsertValue        = "videos.insert"
	operationVideosListValue          = "videos.list"
	operationVideosUpdateValue        = "videos.update"
	operationVideosDeleteValue        = "videos.delete"
	operationPlaylistItemsInsertValue = "playlistItems.insert"
)

type operation struct{ value string }

func newOperationFromValue(value string) (operation, error) {
	switch value {
	case operationChannelsListValue, operationPlaylistsListValue,
		operationPlaylistItemsListValue, operationPlaylistItemsInsertValue,
		operationVideosInsertValue, operationVideosListValue, operationVideosUpdateValue,
		operationVideosDeleteValue:
		return operation{value: value}, nil
	default:
		return operation{}, fmt.Errorf("unsupported mock operation %q", value)
	}
}

// Config seeds a Server.
type Config struct {
	BearerToken                 string
	Channel                     ChannelSeed
	Playlists                   []PlaylistSeed
	Videos                      []VideoSeed
	PlaylistItems               []PlaylistItemSeed
	ProcessingPollsUntilSuccess int
}

// ChannelSeed describes the channel returned for mine=true.
type ChannelSeed struct {
	Description       string `json:"description"`
	ID                string `json:"id"`
	Handle            string `json:"handle"`
	LongUploadsStatus string `json:"longUploadsStatus"`
	Title             string `json:"title"`
	ThumbnailURL      string `json:"thumbnailUrl"`
	UploadsPlaylistID string `json:"uploadsPlaylistId"`
}

// PlaylistSeed describes an existing playlist. The mock never creates playlists.
type PlaylistSeed struct {
	Description    string
	ID             string
	OwnerChannelID string `json:"ownerChannelId"`
	ThumbnailURL   string
	Title          string
	PrivacyStatus  string `json:"privacyStatus"`
	Podcast        bool
	ManualSorting  bool
	Hidden         bool
}

// VideoSeed describes an existing channel video.
type VideoSeed struct {
	ID                   string   `json:"id"`
	Title                string   `json:"title"`
	OwnerChannelID       string   `json:"ownerChannelId"`
	Description          string   `json:"description"`
	Tags                 []string `json:"tags"`
	CategoryID           string   `json:"categoryId"`
	PublishedAt          string   `json:"publishedAt"`
	Duration             string   `json:"duration"`
	PrivacyStatus        string   `json:"privacyStatus"`
	ProcessingStatus     string   `json:"processingStatus"`
	UploadStatus         string   `json:"uploadStatus"`
	FailureReason        string   `json:"failureReason"`
	RejectionReason      string   `json:"rejectionReason"`
	LiveBroadcastContent string   `json:"liveBroadcastContent"`
	ActualEndTime        string   `json:"actualEndTime"`
	MediaBytes           int64    `json:"mediaBytes"`
	NotifySubscribers    *bool    `json:"notifySubscribers"`
}

// PlaylistItemSeed describes an existing playlist membership.
type PlaylistItemSeed struct {
	ID          string
	PlaylistID  string
	VideoID     string
	Position    int
	PublishedAt string
}

// DefaultConfig returns deterministic local fixtures.
func DefaultConfig() Config {
	return Config{
		BearerToken: "test-token",
		Channel: ChannelSeed{
			Description:       "The channel behind Listenbox publishing walkthroughs and product updates.",
			ID:                "UC_LISTENBOX_TEST_123456",
			Handle:            "listenbox_test",
			LongUploadsStatus: longUploadsAllowed,
			Title:             "Listenbox Test Channel",
			ThumbnailURL:      "https://example.test/listenbox-channel.png",
			UploadsPlaylistID: "UU_LISTENBOX_TEST",
		},
		Playlists: []PlaylistSeed{
			{
				Description:   "Weekly publishing notes, release walkthroughs, and product updates from Listenbox.",
				ID:            "PL_LISTENBOX_PODCAST",
				ThumbnailURL:  "https://example.test/listenbox-playlist.jpg",
				Title:         "Listenbox Test Podcast",
				Podcast:       true,
				ManualSorting: true,
			},
		},
		ProcessingPollsUntilSuccess: defaultProcessingPolls,
	}
}

// State is the assertion surface exposed by GET /__mock__/state.
type State struct {
	Channel         ChannelSeed     `json:"channel"`
	Playlists       []PlaylistState `json:"playlists"`
	UploadsPlaylist *PlaylistState  `json:"uploadsPlaylist,omitempty"`
	Videos          []VideoState    `json:"videos"`
	Requests        map[string]int  `json:"requests"`
	Quota           QuotaState      `json:"quota"`
	Delays          map[string]int  `json:"delays"`
}

// QuotaState describes deterministic quota controls and observed API traffic.
type QuotaState struct {
	Exhausted     bool   `json:"exhausted"`
	Reason        string `json:"reason"`
	AfterRequests *int   `json:"afterRequests,omitempty"`
	Requests      int    `json:"requests"`
}

// PlaylistState contains current top-to-bottom item order.
type PlaylistState struct {
	Description    string              `json:"description"`
	ID             string              `json:"id"`
	ThumbnailURL   string              `json:"thumbnailUrl"`
	Title          string              `json:"title"`
	PrivacyStatus  string              `json:"privacyStatus"`
	Podcast        bool                `json:"podcast"`
	ManualSorting  bool                `json:"manualSorting"`
	Hidden         bool                `json:"hidden"`
	OwnerChannelID string              `json:"ownerChannelId"`
	Items          []PlaylistItemState `json:"items"`
}

// PlaylistItemState is one playlist membership.
type PlaylistItemState struct {
	ID          string `json:"id"`
	VideoID     string `json:"videoId"`
	Position    int    `json:"position"`
	PublishedAt string `json:"publishedAt"`
}

// VideoState records metadata and the number of uploaded media bytes.
type VideoState struct {
	ID                           string   `json:"id"`
	Title                        string   `json:"title"`
	OwnerChannelID               string   `json:"ownerChannelId"`
	UploadsMember                bool     `json:"uploadsMember"`
	Description                  string   `json:"description"`
	Tags                         []string `json:"tags"`
	CategoryID                   string   `json:"categoryId"`
	Duration                     string   `json:"duration"`
	PrivacyStatus                string   `json:"privacyStatus"`
	MediaBytes                   int64    `json:"mediaBytes"`
	PublishedAt                  string   `json:"publishedAt"`
	ProcessingStatus             string   `json:"processingStatus"`
	ProcessingPolls              int      `json:"processingPolls"`
	ProcessingPollsUntilTerminal int      `json:"processingPollsUntilTerminal"`
	TerminalProcessingStatus     string   `json:"terminalProcessingStatus"`
	UploadStatus                 string   `json:"uploadStatus"`
	TerminalUploadStatus         string   `json:"terminalUploadStatus"`
	FailureReason                string   `json:"failureReason,omitempty"`
	RejectionReason              string   `json:"rejectionReason,omitempty"`
	LiveBroadcastContent         string   `json:"liveBroadcastContent,omitempty"`
	ActualEndTime                string   `json:"actualEndTime,omitempty"`
	NotifySubscribers            bool     `json:"notifySubscribers"`
}

type pageInfo struct {
	TotalResults   int `json:"totalResults"`
	ResultsPerPage int `json:"resultsPerPage"`
}

type listResponse struct {
	Kind          string   `json:"kind"`
	Items         []any    `json:"items"`
	PageInfo      pageInfo `json:"pageInfo"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
	PrevPageToken string   `json:"prevPageToken,omitempty"`
}

func paginatedList(
	kind string,
	items []any,
	total, offset, maxResults int,
	generation int64,
) listResponse {
	payload := listResponse{
		Kind:     kind,
		Items:    items,
		PageInfo: pageInfo{TotalResults: total, ResultsPerPage: len(items)},
	}
	end := offset + len(items)
	if maxResults > 0 && end < total {
		payload.NextPageToken = pageToken(generation, end)
	}
	if offset > 0 {
		payload.PrevPageToken = pageToken(generation, max(0, offset-maxResults))
	}
	return payload
}

type videoMetadata struct {
	Snippet struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		CategoryID  string   `json:"categoryId"`
	} `json:"snippet"`
	Status struct {
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
}

type videoUpdateRequest struct {
	ID      string `json:"id"`
	Snippet *struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		CategoryID  string   `json:"categoryId"`
	} `json:"snippet"`
	Status *struct {
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
}

type playlistItemRequest struct {
	Snippet struct {
		PlaylistID string `json:"playlistId"`
		Position   *int   `json:"position"`
		ResourceID struct {
			Kind    string `json:"kind"`
			VideoID string `json:"videoId"`
		} `json:"resourceId"`
	} `json:"snippet"`
}

type injectedFailure struct {
	Status  int    `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type failNextRequest struct {
	Operation string `json:"operation"`
	ChannelID string `json:"channelId"`
	Status    int    `json:"status"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

type credentialRefreshFailureRequest struct {
	RefreshToken string `json:"refreshToken"`
	Status       int    `json:"status"`
	Error        string `json:"error"`
}

type quotaRequest struct {
	Exhausted     bool   `json:"exhausted"`
	Reason        string `json:"reason"`
	AfterRequests *int   `json:"afterRequests"`
}

type delayRequest struct {
	Operation    string `json:"operation"`
	Milliseconds int    `json:"milliseconds"`
}

type gateRequest struct {
	Operation string `json:"operation"`
	ChannelID string `json:"channelId"`
	Enabled   bool   `json:"enabled"`
}

type processingOutcome struct {
	PollsUntilTerminal int    `json:"pollsUntilTerminal"`
	ProcessingStatus   string `json:"processingStatus"`
	UploadStatus       string `json:"uploadStatus"`
	FailureReason      string `json:"failureReason"`
	RejectionReason    string `json:"rejectionReason"`
}

// Server is an in-memory, concurrency-safe HTTP mock.
type Server struct {
	mu                 sync.RWMutex
	config             Config
	state              State
	failures           map[string][]injectedFailure
	delays             map[string]time.Duration
	gates              map[string]chan struct{}
	requests           map[string]int
	requestsByChannel  map[string]map[string]int
	quota              QuotaState
	processingOutcomes []processingOutcome
	nextID             int64
	generation         int64
	channels           map[string]ChannelSeed
	tokenChannels      map[string]string
	refreshTokens      map[string]string
	refreshFailures    map[string][]credentialRefreshFailureRequest
	mux                *http.ServeMux
}

// New validates config and creates a resettable mock server.
func New(config Config) (*Server, error) {
	config.Channel = normalizedChannelSeed(config.Channel)
	if strings.TrimSpace(config.BearerToken) == "" {
		return nil, errors.New("create YouTube mock: bearer token must not be empty")
	}
	if strings.TrimSpace(config.Channel.ID) == "" ||
		strings.TrimSpace(config.Channel.Title) == "" ||
		strings.TrimSpace(config.Channel.UploadsPlaylistID) == "" {
		return nil, fmt.Errorf(
			"create YouTube mock: channel identity is incomplete: ID=%q title=%q uploadsPlaylistID=%q",
			config.Channel.ID,
			config.Channel.Title,
			config.Channel.UploadsPlaylistID,
		)
	}
	if !validLongUploadsStatus(config.Channel.LongUploadsStatus) {
		return nil, fmt.Errorf(
			"create YouTube mock: channel long-upload status %q is invalid",
			config.Channel.LongUploadsStatus,
		)
	}
	if config.ProcessingPollsUntilSuccess < 1 {
		return nil, fmt.Errorf(
			"create YouTube mock: processing polls until success must be positive, got %d",
			config.ProcessingPollsUntilSuccess,
		)
	}
	if err := validateSeeds(config); err != nil {
		return nil, err
	}

	server := &Server{config: cloneConfig(config), mux: http.NewServeMux()}
	server.resetLocked()
	server.routes()
	return server, nil
}

func validateSeeds(config Config) error {
	playlists, err := validatePlaylistSeeds(config)
	if err != nil {
		return err
	}
	videos, err := validateVideoSeeds(config)
	if err != nil {
		return err
	}
	return validatePlaylistItemSeeds(config.PlaylistItems, playlists, videos)
}

func validatePlaylistSeeds(config Config) (map[string]bool, error) {
	seenPlaylists := make(map[string]bool, len(config.Playlists))
	for index, playlist := range config.Playlists {
		if strings.TrimSpace(playlist.ID) == "" || strings.TrimSpace(playlist.Title) == "" {
			return nil, fmt.Errorf(
				"create YouTube mock: playlist at index %d must have ID and title",
				index,
			)
		}
		if seenPlaylists[playlist.ID] {
			return nil, fmt.Errorf("create YouTube mock: duplicate playlist ID %q", playlist.ID)
		}
		seenPlaylists[playlist.ID] = true
	}
	seenPlaylists[config.Channel.UploadsPlaylistID] = true
	return seenPlaylists, nil
}

func validateVideoSeeds(config Config) (map[string]bool, error) {
	seenVideos := make(map[string]bool, len(config.Videos))
	for index, video := range config.Videos {
		if strings.TrimSpace(video.ID) == "" || strings.TrimSpace(video.Title) == "" {
			return nil, fmt.Errorf(
				"create YouTube mock: video at index %d must have ID and title",
				index,
			)
		}
		if seenVideos[video.ID] {
			return nil, fmt.Errorf("create YouTube mock: duplicate video ID %q", video.ID)
		}
		seenVideos[video.ID] = true
		if err := validateVideoState(seedVideoState(video, config.Channel.ID)); err != nil {
			return nil, fmt.Errorf("create YouTube mock: video %q: %w", video.ID, err)
		}
	}
	return seenVideos, nil
}

func validatePlaylistItemSeeds(
	items []PlaylistItemSeed,
	seenPlaylists, seenVideos map[string]bool,
) error {
	seenItems := make(map[string]bool, len(items))
	positions := make(map[string]map[int]bool)
	for index, item := range items {
		if item.ID == "" || !seenPlaylists[item.PlaylistID] || !seenVideos[item.VideoID] ||
			item.Position < 0 {
			return fmt.Errorf(
				"create YouTube mock: playlist item at index %d has invalid identity, reference, or position",
				index,
			)
		}
		if seenItems[item.ID] {
			return fmt.Errorf("create YouTube mock: duplicate playlist item ID %q", item.ID)
		}
		seenItems[item.ID] = true
		if positions[item.PlaylistID] == nil {
			positions[item.PlaylistID] = make(map[int]bool)
		}
		if positions[item.PlaylistID][item.Position] {
			return fmt.Errorf(
				"create YouTube mock: duplicate position %d in playlist %q",
				item.Position,
				item.PlaylistID,
			)
		}
		positions[item.PlaylistID][item.Position] = true
	}
	for playlistID, playlistPositions := range positions {
		for position := range len(playlistPositions) {
			if !playlistPositions[position] {
				return fmt.Errorf(
					"create YouTube mock: playlist %q positions must be contiguous from zero",
					playlistID,
				)
			}
		}
	}
	return nil
}

func normalizedChannelSeed(channel ChannelSeed) ChannelSeed {
	if strings.TrimSpace(channel.LongUploadsStatus) == "" {
		channel.LongUploadsStatus = longUploadsAllowed
	}
	return channel
}

func validLongUploadsStatus(status string) bool {
	return status == longUploadsAllowed || status == "eligible" || status == "disallowed"
}

func cloneConfig(config Config) Config {
	config.Playlists = slices.Clone(config.Playlists)
	config.Videos = slices.Clone(config.Videos)
	for index := range config.Videos {
		config.Videos[index].Tags = slices.Clone(config.Videos[index].Tags)
	}
	config.PlaylistItems = slices.Clone(config.PlaylistItems)
	return config
}

// ServeHTTP enforces bearer authentication on YouTube API and upload paths.
func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/youtube/") ||
		strings.HasPrefix(request.URL.Path, "/upload/") {
		server.mu.RLock()
		channel := server.channelForRequestLocked(request)
		server.mu.RUnlock()
		if channel.ID == "" {
			writeAPIError(
				writer,
				http.StatusUnauthorized,
				"youtubeSignupRequired",
				"A valid mock bearer token is required.",
			)
			return
		}
		if operation, ok := operationForRequest(
			request,
		); ok &&
			server.applyControls(writer, request, operation) {
			return
		}
	}
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) routes() {
	server.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	server.mux.HandleFunc("GET /youtube/v3/channels", server.channelsList)
	server.mux.HandleFunc("GET /youtube/v3/playlists", server.playlistsList)
	server.mux.HandleFunc("GET /youtube/v3/playlistItems", server.playlistItemsList)
	server.mux.HandleFunc("POST /youtube/v3/playlistItems", server.playlistItemsInsert)
	server.mux.HandleFunc("GET /youtube/v3/videos", server.videosList)
	server.mux.HandleFunc("PUT /youtube/v3/videos", server.videosUpdate)
	server.mux.HandleFunc("DELETE /youtube/v3/videos", server.videosDelete)
	server.mux.HandleFunc("POST /upload/youtube/v3/videos", server.videosInsert)
	server.mux.HandleFunc("GET /__mock__/state", server.mockState)
	server.mux.HandleFunc("POST /__mock__/reset", server.mockReset)
	server.mux.HandleFunc("POST /__mock__/fail-next", server.mockFailNext)
	server.mux.HandleFunc("POST /__mock__/quota", server.mockQuota)
	server.mux.HandleFunc("POST /__mock__/delay", server.mockDelay)
	server.mux.HandleFunc("POST /__mock__/gate", server.mockGate)
	server.mux.HandleFunc("POST /__mock__/processing-outcome", server.mockProcessingOutcome)
	server.mux.HandleFunc("POST /__mock__/videos", server.mockSeedVideo)
	server.mux.HandleFunc("POST /__mock__/playlist-items", server.mockSeedPlaylistItem)
	server.mux.HandleFunc("POST /__mock__/credential-refresh", server.mockCredentialRefresh)
	server.mux.HandleFunc(
		"POST /__mock__/credential-refresh-failure",
		server.mockCredentialRefreshFailure,
	)
	server.mux.HandleFunc("POST /__mock__/catalogues", server.mockSeedCatalogue)
	server.mux.HandleFunc("POST /__mock__/videos/{videoID}/disappear", server.mockDisappearVideo)
}

// mockCredentialRefresh is a deterministic post-callback credential fixture for API E2E.
// It is deliberately part of the Data API fixture, not the browser-only Google OAuth mock.
func (server *Server) mockCredentialRefresh(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil || request.Form.Get("refresh_token") == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalid_refresh",
			"A refresh token is required.",
		)
		return
	}
	server.mu.Lock()
	failures := server.refreshFailures[request.Form.Get("refresh_token")]
	if len(failures) > 0 {
		failure := failures[0]
		server.refreshFailures[request.Form.Get("refresh_token")] = failures[1:]
		server.mu.Unlock()
		writeJSON(writer, failure.Status, map[string]any{"error": failure.Error})
		return
	}
	accessToken := server.refreshTokens[request.Form.Get("refresh_token")]
	server.mu.Unlock()
	if accessToken == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalid_refresh",
			"The refresh token is unknown.",
		)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"expires_in":   tokenLifetimeSeconds,
		"token_type":   "Bearer",
	})
}

type catalogueSeed struct {
	AccessToken  string         `json:"accessToken"`
	RefreshToken string         `json:"refreshToken"`
	Channel      ChannelSeed    `json:"channel"`
	Playlists    []PlaylistSeed `json:"playlists"`
}

func (server *Server) mockCredentialRefreshFailure(
	writer http.ResponseWriter,
	request *http.Request,
) {
	var input credentialRefreshFailureRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	validRevocation := input.Status == http.StatusBadRequest &&
		(input.Error == "invalid_grant" || input.Error == "invalid_token")
	validTransient := input.Status >= http.StatusInternalServerError &&
		input.Status <= 599 && input.Error == "temporarily_unavailable"
	if input.RefreshToken == "" || (!validRevocation && !validTransient) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidRefreshFailure",
			"invalid credential refresh failure",
		)
		return
	}
	server.mu.Lock()
	server.refreshFailures[input.RefreshToken] = append(
		server.refreshFailures[input.RefreshToken],
		input,
	)
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

// mockSeedCatalogue creates a fully isolated owned-channel catalogue. API
// requests select it by bearer token, so concurrent imports cannot observe
// another test's channel, playlists, uploads, or videos.
func (server *Server) mockSeedCatalogue(writer http.ResponseWriter, request *http.Request) {
	var input catalogueSeed
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Channel = normalizedChannelSeed(input.Channel)
	if !validateCatalogue(writer, input) {
		return
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if existing, exists := server.channels[input.Channel.ID]; exists {
		if existing.UploadsPlaylistID != input.Channel.UploadsPlaylistID ||
			existing.Handle != input.Channel.Handle {
			writeAPIError(
				writer,
				http.StatusConflict,
				"duplicateCatalogue",
				"channel identity conflicts with an existing catalogue.",
			)
			return
		}
		server.channels[input.Channel.ID] = input.Channel
		server.tokenChannels[input.AccessToken] = input.Channel.ID
		server.refreshTokens[input.RefreshToken] = input.AccessToken
		writer.WriteHeader(http.StatusCreated)
		return
	}
	server.channels[input.Channel.ID] = input.Channel
	server.tokenChannels[input.AccessToken] = input.Channel.ID
	server.refreshTokens[input.RefreshToken] = input.AccessToken
	playlists := slices.Clone(input.Playlists)
	playlists = append(
		playlists,
		PlaylistSeed{ID: input.Channel.UploadsPlaylistID, Title: "Uploads", Hidden: true},
	)
	for _, playlist := range playlists {
		if server.playlistLocked(playlist.ID) != nil {
			writeAPIError(
				writer,
				http.StatusConflict,
				"duplicateCatalogue",
				"playlist already exists.",
			)
			return
		}
		ownerChannelID := playlist.OwnerChannelID
		if ownerChannelID == "" {
			ownerChannelID = input.Channel.ID
		}
		server.state.Playlists = append(
			server.state.Playlists,
			seededPlaylist(playlist, ownerChannelID),
		)
	}
	server.generation++
	writer.WriteHeader(http.StatusCreated)
}

func validateCatalogue(writer http.ResponseWriter, input catalogueSeed) bool {
	if input.AccessToken == "" || input.RefreshToken == "" || input.Channel.ID == "" ||
		input.Channel.UploadsPlaylistID == "" ||
		input.Channel.Title == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidCatalogue",
			"tokens and complete channel identity are required.",
		)
		return false
	}
	if !validLongUploadsStatus(input.Channel.LongUploadsStatus) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidCatalogue",
			"channel long-upload status is invalid.",
		)
		return false
	}
	return true
}

func (server *Server) videosDelete(writer http.ResponseWriter, request *http.Request) {
	videoID := strings.TrimSpace(request.URL.Query().Get("id"))
	if videoID == "" {
		writeAPIError(writer, http.StatusBadRequest, "required", "videos.delete requires id.")
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for index, video := range server.state.Videos {
		if video.ID != videoID {
			continue
		}
		server.state.Videos = append(server.state.Videos[:index], server.state.Videos[index+1:]...)
		for playlistIndex := range server.state.Playlists {
			items := server.state.Playlists[playlistIndex].Items[:0]
			for _, item := range server.state.Playlists[playlistIndex].Items {
				if item.VideoID != videoID {
					items = append(items, item)
				}
			}
			server.state.Playlists[playlistIndex].Items = items
		}
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	writeAPIError(
		writer,
		http.StatusNotFound,
		"videoNotFound",
		"The requested video does not exist.",
	)
}

func operationForRequest(request *http.Request) (operation, bool) {
	var value string
	switch request.Method + " " + request.URL.Path {
	case "GET /youtube/v3/channels":
		value = operationChannelsListValue
	case "GET /youtube/v3/playlists":
		value = operationPlaylistsListValue
	case "GET /youtube/v3/playlistItems":
		value = operationPlaylistItemsListValue
	case "POST /youtube/v3/playlistItems":
		value = operationPlaylistItemsInsertValue
	case "GET /youtube/v3/videos":
		value = operationVideosListValue
	case "PUT /youtube/v3/videos":
		value = operationVideosUpdateValue
	case "DELETE /youtube/v3/videos":
		value = operationVideosDeleteValue
	case "POST /upload/youtube/v3/videos":
		value = operationVideosInsertValue
	default:
		return operation{}, false
	}
	return operation{value: value}, true
}

func (server *Server) applyControls(
	writer http.ResponseWriter,
	request *http.Request,
	operation operation,
) bool {
	server.mu.Lock()
	server.recordRequestLocked(operation, request)
	channel := server.channelForRequestLocked(request)
	server.quota.Requests++
	delay := server.delays[operation.value]
	gate := server.gates[gateKey(operation.value, channel.ID)]
	exhausted := server.quota.Exhausted ||
		(server.quota.AfterRequests != nil && server.quota.Requests > *server.quota.AfterRequests)
	reason := server.quota.Reason
	server.mu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return true
		case <-timer.C:
		}
	}
	if gate != nil {
		select {
		case <-request.Context().Done():
			return true
		case <-gate:
		}
	}
	if exhausted {
		if reason == "" {
			reason = "quotaExceeded"
		}
		writeAPIError(writer, http.StatusForbidden, reason, "The mock YouTube quota is exhausted.")
		return true
	}
	return server.writeInjectedFailure(writer, operation, channel.ID)
}

func (server *Server) recordRequestLocked(operation operation, request *http.Request) {
	server.requests[operation.value]++
	channel := server.channelForRequestLocked(request)
	if channel.ID != "" {
		if server.requestsByChannel[channel.ID] == nil {
			server.requestsByChannel[channel.ID] = make(map[string]int)
		}
		server.requestsByChannel[channel.ID][operation.value]++
	}
}

//nolint:goconst // Keep YouTube resource field names visible beside their protocol values.
func (server *Server) channelsList(writer http.ResponseWriter, request *http.Request) {
	if !requirePart(writer, request.URL.Query()) {
		return
	}
	query := request.URL.Query()
	server.mu.RLock()
	channel := server.channelForRequestLocked(request)
	if channel.ID == "" {
		server.mu.RUnlock()
		writeAPIError(
			writer,
			http.StatusUnauthorized,
			"youtubeSignupRequired",
			"A scoped mock bearer token is required.",
		)
		return
	}
	videoCount := 0
	for _, video := range server.state.Videos {
		if video.OwnerChannelID == channel.ID {
			videoCount++
		}
	}
	owned := query.Get("mine") == "true"
	byID := slices.Contains(query["id"], channel.ID)
	byHandle := query.Get("forHandle") != "" && query.Get("forHandle") == channel.Handle
	server.mu.RUnlock()
	if !owned && !byID && !byHandle {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidCriteria",
			"channels.list requires mine=true, the owned channel ID, or its handle in this mock.",
		)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"kind":     "youtube#channelListResponse",
		"pageInfo": pageInfo{TotalResults: 1, ResultsPerPage: 1},
		"items": []any{map[string]any{
			"kind": "youtube#channel", "id": channel.ID,
			"snippet": map[string]any{
				"description": channel.Description,
				"title":       channel.Title,
				"thumbnails": map[string]any{
					"medium": map[string]any{"url": channel.ThumbnailURL},
				},
			},
			"contentDetails": map[string]any{
				"relatedPlaylists": map[string]any{"uploads": channel.UploadsPlaylistID},
			},
			"status": map[string]any{
				"longUploadsStatus": channel.LongUploadsStatus,
			},
			"statistics": map[string]any{
				"videoCount": strconv.Itoa(videoCount),
			},
		}},
	})
}

func (server *Server) channelForRequestLocked(request *http.Request) ChannelSeed {
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if channelID := server.tokenChannels[token]; channelID != "" {
		return server.channels[channelID]
	}
	if token == server.config.BearerToken {
		return server.config.Channel
	}
	return ChannelSeed{}
}

func (server *Server) playlistsList(writer http.ResponseWriter, request *http.Request) {
	if !requirePart(writer, request.URL.Query()) {
		return
	}
	query := request.URL.Query()
	if query.Get("mine") != "true" && query.Get("id") == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidCriteria",
			"playlists.list requires mine=true or id in this mock.",
		)
		return
	}
	server.mu.RLock()
	channel := server.channelForRequestLocked(request)
	filtered := make([]PlaylistState, 0, len(server.state.Playlists))
	requestedIDs := stringSet(query["id"])
	for _, playlist := range server.state.Playlists {
		if matchesPlaylist(playlist, query, requestedIDs, channel.ID) {
			filtered = append(filtered, playlist)
		}
	}
	maxResults, ok := listMaxResults(writer, query, defaultListMaxResults)
	if !ok {
		server.mu.RUnlock()
		return
	}
	offset, ok := server.pageOffset(writer, query.Get("pageToken"), len(filtered))
	if !ok {
		server.mu.RUnlock()
		return
	}
	end := min(offset+maxResults, len(filtered))
	items := make([]any, 0, end-offset)
	for _, playlist := range filtered[offset:end] {
		items = append(items, server.playlistResourceLocked(playlist))
	}
	generation := server.generation
	server.mu.RUnlock()
	payload := paginatedList(
		"youtube#playlistListResponse",
		items,
		len(filtered),
		offset,
		maxResults,
		generation,
	)
	writeJSON(writer, http.StatusOK, payload)
}

func matchesPlaylist(
	playlist PlaylistState,
	query url.Values,
	requestedIDs map[string]bool,
	ownerID string,
) bool {
	if playlist.Hidden || (len(requestedIDs) > 0 && !requestedIDs[playlist.ID]) {
		return false
	}
	if channelID := query.Get("channelId"); channelID != "" && channelID != ownerID {
		return false
	}
	return len(requestedIDs) != 0 || playlist.OwnerChannelID == "" ||
		playlist.OwnerChannelID == ownerID
}

//nolint:goconst // Keep YouTube resource field names visible beside their protocol values.
func (server *Server) playlistResourceLocked(playlist PlaylistState) map[string]any {
	status := map[string]any{"privacyStatus": playlist.PrivacyStatus}
	if playlist.Podcast {
		status["podcastStatus"] = "enabled"
	}
	return map[string]any{
		"kind": "youtube#playlist", "id": playlist.ID,
		"snippet": map[string]any{
			"channelId":   playlist.OwnerChannelID,
			"description": playlist.Description,
			"thumbnails":  map[string]any{"medium": map[string]any{"url": playlist.ThumbnailURL}},
			"title":       playlist.Title,
		},
		"contentDetails": map[string]any{"itemCount": len(playlist.Items)},
		"status":         status,
	}
}

func (server *Server) playlistItemsList(writer http.ResponseWriter, request *http.Request) {
	if !requirePart(writer, request.URL.Query()) {
		return
	}
	playlistID := request.URL.Query().Get("playlistId")
	if playlistID == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"missingRequiredParameter",
			"playlistId is required.",
		)
		return
	}
	server.mu.RLock()
	playlist := server.playlistLocked(playlistID)
	if playlist == nil {
		server.mu.RUnlock()
		writeAPIError(
			writer,
			http.StatusNotFound,
			"playlistNotFound",
			"The requested playlist does not exist.",
		)
		return
	}
	filtered := filterPlaylistItems(playlist.Items, request.URL.Query())
	maxResults, ok := listMaxResults(writer, request.URL.Query(), defaultListMaxResults)
	if !ok {
		server.mu.RUnlock()
		return
	}
	offset, ok := server.pageOffset(writer, request.URL.Query().Get("pageToken"), len(filtered))
	if !ok {
		server.mu.RUnlock()
		return
	}
	end := min(offset+maxResults, len(filtered))
	items := make([]any, 0, end-offset)
	for _, item := range filtered[offset:end] {
		items = append(items, server.playlistItemResourceLocked(*playlist, item))
	}
	generation := server.generation
	server.mu.RUnlock()
	payload := paginatedList(
		"youtube#playlistItemListResponse",
		items,
		len(filtered),
		offset,
		maxResults,
		generation,
	)
	writeJSON(writer, http.StatusOK, payload)
}

func filterPlaylistItems(items []PlaylistItemState, query url.Values) []PlaylistItemState {
	filtered := make([]PlaylistItemState, 0, len(items))
	requestedVideoID := query.Get("videoId")
	requestedIDs := stringSet(query["id"])
	for _, item := range items {
		if requestedVideoID != "" && item.VideoID != requestedVideoID {
			continue
		}
		if len(requestedIDs) > 0 && !requestedIDs[item.ID] {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func (server *Server) playlistItemsInsert(writer http.ResponseWriter, request *http.Request) {
	if server.writeInjectedFailure(
		writer,
		operation{value: operationPlaylistItemsInsertValue},
		"",
	) ||
		!requirePart(writer, request.URL.Query()) {
		return
	}
	var input playlistItemRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Snippet.PlaylistID == "" || input.Snippet.ResourceID.VideoID == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"missingRequiredParameter",
			"snippet.playlistId and snippet.resourceId.videoId are required.",
		)
		return
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	playlist := server.playlistLocked(input.Snippet.PlaylistID)
	if playlist == nil {
		writeAPIError(
			writer,
			http.StatusNotFound,
			"playlistNotFound",
			"The requested playlist does not exist.",
		)
		return
	}
	if server.videoLocked(input.Snippet.ResourceID.VideoID) == nil {
		writeAPIError(
			writer,
			http.StatusNotFound,
			"videoNotFound",
			"The requested video does not exist.",
		)
		return
	}
	position, ok := playlistInsertPosition(writer, playlist, input.Snippet.Position)
	if !ok {
		return
	}

	item := PlaylistItemState{
		ID: server.newIDLocked("PLI"), VideoID: input.Snippet.ResourceID.VideoID,
		PublishedAt: time.Now().UTC().Format(time.RFC3339),
	}
	playlist.Items = slices.Insert(playlist.Items, position, item)
	reindexPlaylist(playlist)
	writeJSON(
		writer,
		http.StatusOK,
		server.playlistItemResourceLocked(*playlist, playlist.Items[position]),
	)
}

func playlistInsertPosition(
	writer http.ResponseWriter,
	playlist *PlaylistState,
	requested *int,
) (int, bool) {
	if requested != nil && !playlist.ManualSorting {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"manualSortRequired",
			"Set the playlist order to creator-provided/manual before specifying a position.",
		)
		return 0, false
	}
	position := len(playlist.Items)
	if requested != nil {
		position = *requested
	}
	if position < 0 || position > len(playlist.Items) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidValue",
			"snippet.position is outside the playlist bounds.",
		)
		return 0, false
	}
	return position, true
}

func (server *Server) videosList(writer http.ResponseWriter, request *http.Request) {
	if server.writeInjectedFailure(writer, operation{value: operationVideosListValue}, "") ||
		!requirePart(writer, request.URL.Query()) {
		return
	}
	requestedIDs := stringSet(request.URL.Query()["id"])
	if len(requestedIDs) == 0 {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"missingRequiredParameter",
			"id is required in this mock.",
		)
		return
	}
	if len(requestedIDs) > maxListResults {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidValue",
			"videos.list accepts at most 50 IDs.",
		)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if stringSet(request.URL.Query()["part"])[youtubeProcessingDetailsPart] {
		channel := server.channelForRequestLocked(request)
		for _, video := range server.state.Videos {
			if requestedIDs[video.ID] && video.OwnerChannelID != channel.ID {
				writeAPIError(
					writer,
					http.StatusForbidden,
					"forbidden",
					"The request is not properly authorized to access processing information for a video owned by another channel.",
				)
				return
			}
		}
	}
	items := make([]any, 0, len(requestedIDs))
	for index := range server.state.Videos {
		stored := &server.state.Videos[index]
		if !requestedIDs[stored.ID] {
			continue
		}
		advanceProcessing(stored)
		items = append(items, server.videoResource(*stored))
	}
	writeList(writer, "youtube#videoListResponse", items)
}

func advanceProcessing(video *VideoState) {
	if video.ProcessingStatus != processingPending {
		return
	}
	video.ProcessingPolls++
	if video.ProcessingPolls < video.ProcessingPollsUntilTerminal {
		return
	}
	video.ProcessingStatus = video.TerminalProcessingStatus
	if video.UploadStatus == uploadPending {
		video.UploadStatus = video.TerminalUploadStatus
	}
}

func (server *Server) videosUpdate(writer http.ResponseWriter, request *http.Request) {
	if server.writeInjectedFailure(writer, operation{value: operationVideosUpdateValue}, "") ||
		!requirePart(writer, request.URL.Query()) {
		return
	}
	var input videoUpdateRequest
	if !decodeJSON(writer, request, &input) || input.ID == "" {
		return
	}
	requestedParts := stringSet(request.URL.Query()["part"])
	if requestedParts["snippet"] &&
		!input.validSnippet() {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidVideoMetadata",
			"Updating snippet requires snippet.title and snippet.categoryId.",
		)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	video := server.videoLocked(input.ID)
	if video == nil {
		writeAPIError(
			writer,
			http.StatusNotFound,
			"videoNotFound",
			"The requested video does not exist.",
		)
		return
	}
	if requestedParts["snippet"] {
		video.Title = input.Snippet.Title
		video.Description = input.Snippet.Description
		video.Tags = slices.Clone(input.Snippet.Tags)
		video.CategoryID = input.Snippet.CategoryID
	}
	if requestedParts["status"] && input.Status != nil && input.Status.PrivacyStatus != "" {
		video.PrivacyStatus = input.Status.PrivacyStatus
	}
	writeJSON(writer, http.StatusOK, server.videoResource(*video))
}

func (input videoUpdateRequest) validSnippet() bool {
	return input.Snippet != nil && input.Snippet.Title != "" && input.Snippet.CategoryID != ""
}

func (server *Server) videosInsert(writer http.ResponseWriter, request *http.Request) {
	if server.writeInjectedFailure(writer, operation{value: operationVideosInsertValue}, "") ||
		!requirePart(writer, request.URL.Query()) {
		return
	}
	for _, header := range []string{"Range", "Content-Range", "X-Upload-Content-Length", "X-Upload-Content-Type"} {
		if request.Header.Get(header) != "" {
			writeAPIError(
				writer,
				http.StatusBadRequest,
				"unexpectedUploadRecoveryHeader",
				header+" is not supported.",
			)
			return
		}
	}
	notifySubscribers, ok := parseNotifySubscribers(writer, request.URL.Query())
	if !ok {
		return
	}
	if request.URL.Query().Get("uploadType") != "multipart" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidUploadType",
			"uploadType must be multipart.",
		)
		return
	}

	metadata, mediaBytes, ok := readUpload(writer, request)
	if !ok {
		return
	}
	server.mu.Lock()
	channel := server.channelForRequestLocked(request)
	video := server.createVideoLocked(
		metadata,
		mediaBytes,
		notifySubscribers,
		channel.ID,
		channel.UploadsPlaylistID,
	)
	server.mu.Unlock()
	writeJSON(writer, http.StatusOK, server.videoResource(video))
}

func (server *Server) mockDisappearVideo(writer http.ResponseWriter, request *http.Request) {
	videoID := request.PathValue("videoID")
	server.mu.Lock()
	defer server.mu.Unlock()
	for index, video := range server.state.Videos {
		if video.ID == videoID {
			server.state.Videos = slices.Delete(server.state.Videos, index, index+1)
			writer.WriteHeader(http.StatusNoContent)
			return
		}
	}
	writeAPIError(
		writer,
		http.StatusNotFound,
		"videoNotFound",
		"The requested video does not exist.",
	)
}

func (server *Server) mockState(writer http.ResponseWriter, request *http.Request) {
	server.mu.RLock()
	defer server.mu.RUnlock()
	snapshot := server.state
	snapshot.Playlists = make([]PlaylistState, 0, len(server.state.Playlists))
	for _, playlist := range server.state.Playlists {
		cloned := playlist
		cloned.Items = slices.Clone(playlist.Items)
		if playlist.ID == server.config.Channel.UploadsPlaylistID {
			snapshot.UploadsPlaylist = &cloned
		}
		if !playlist.Hidden {
			snapshot.Playlists = append(snapshot.Playlists, cloned)
		}
	}
	snapshot.Videos = slices.Clone(server.state.Videos)
	for index := range snapshot.Videos {
		snapshot.Videos[index].Tags = slices.Clone(snapshot.Videos[index].Tags)
		snapshot.Videos[index].UploadsMember = false
		if snapshot.UploadsPlaylist != nil {
			for _, item := range snapshot.UploadsPlaylist.Items {
				if item.VideoID == snapshot.Videos[index].ID {
					snapshot.Videos[index].UploadsMember = true
					break
				}
			}
		}
	}
	requestCounts := server.requests
	if channelID := strings.TrimSpace(request.URL.Query().Get("channelId")); channelID != "" {
		requestCounts = server.requestsByChannel[channelID]
	}
	snapshot.Requests = make(map[string]int, len(requestCounts))
	maps.Copy(snapshot.Requests, requestCounts)
	snapshot.Quota = server.quota
	snapshot.Delays = make(map[string]int, len(server.delays))
	for operation, delay := range server.delays {
		snapshot.Delays[operation] = int(delay / time.Millisecond)
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) mockReset(writer http.ResponseWriter, _ *http.Request) {
	server.mu.Lock()
	server.resetLocked()
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) mockFailNext(writer http.ResponseWriter, request *http.Request) {
	var input failNextRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	operation, err := newOperationFromValue(input.Operation)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalidOperation", err.Error())
		return
	}
	if input.Status < 400 || input.Status > 599 || input.Reason == "" || input.Message == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidFailure",
			"status must be 400..599 and reason/message must not be empty.",
		)
		return
	}
	server.mu.Lock()
	failureKey := operation.value
	if input.ChannelID != "" {
		failureKey = gateKey(operation.value, input.ChannelID)
	}
	server.failures[failureKey] = append(server.failures[failureKey], injectedFailure{
		Status: input.Status, Reason: input.Reason, Message: input.Message,
	})
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) mockQuota(writer http.ResponseWriter, request *http.Request) {
	var input quotaRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Reason != "" && input.Reason != "quotaExceeded" &&
		input.Reason != "dailyLimitExceeded" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidQuota",
			"reason must be quotaExceeded or dailyLimitExceeded.",
		)
		return
	}
	if input.AfterRequests != nil && *input.AfterRequests < 0 {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidQuota",
			"afterRequests must not be negative.",
		)
		return
	}
	server.mu.Lock()
	server.quota = QuotaState{
		Exhausted: input.Exhausted, Reason: input.Reason, AfterRequests: input.AfterRequests,
	}
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) mockDelay(writer http.ResponseWriter, request *http.Request) {
	var input delayRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	operation, err := newOperationFromValue(input.Operation)
	if err != nil || input.Milliseconds < 0 || input.Milliseconds > 10_000 {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidDelay",
			"operation must be supported and milliseconds must be 0..10000.",
		)
		return
	}
	server.mu.Lock()
	if input.Milliseconds == 0 {
		delete(server.delays, operation.value)
	} else {
		server.delays[operation.value] = time.Duration(input.Milliseconds) * time.Millisecond
	}
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) mockGate(writer http.ResponseWriter, request *http.Request) {
	var input gateRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	operation, err := newOperationFromValue(input.Operation)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalidGate", "operation must be supported.")
		return
	}
	if strings.TrimSpace(input.ChannelID) == "" {
		writeAPIError(writer, http.StatusBadRequest, "invalidGate", "channelId is required.")
		return
	}
	key := gateKey(operation.value, input.ChannelID)
	server.mu.Lock()
	existing := server.gates[key]
	if existing != nil {
		close(existing)
		delete(server.gates, key)
	}
	if input.Enabled {
		server.gates[key] = make(chan struct{})
	}
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func gateKey(operation, channelID string) string {
	return operation + "\x00" + channelID
}

func (server *Server) mockProcessingOutcome(writer http.ResponseWriter, request *http.Request) {
	var input processingOutcome
	if !decodeJSON(writer, request, &input) {
		return
	}
	if err := validateProcessingOutcome(input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalidProcessingOutcome", err.Error())
		return
	}
	server.mu.Lock()
	server.processingOutcomes = append(server.processingOutcomes, input)
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) mockSeedVideo(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Video             VideoSeed `json:"video"`
		UploadsMembership bool      `json:"uploadsMembership"`
		ChannelID         string    `json:"channelId"`
		UploadsPlaylistID string    `json:"uploadsPlaylistId"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if strings.TrimSpace(input.Video.ID) == "" || strings.TrimSpace(input.Video.Title) == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidVideo",
			"video id and title must not be empty.",
		)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.videoLocked(input.Video.ID) != nil {
		writeAPIError(writer, http.StatusConflict, "duplicateVideo", "video id already exists.")
		return
	}
	channelID := input.ChannelID
	if channelID == "" {
		channelID = server.config.Channel.ID
	}
	video := seedVideoState(input.Video, channelID)
	if err := validateVideoState(video); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalidVideo", err.Error())
		return
	}
	server.state.Videos = append(server.state.Videos, video)
	if input.UploadsMembership {
		uploadsPlaylistID := input.UploadsPlaylistID
		if uploadsPlaylistID == "" {
			uploadsPlaylistID = server.config.Channel.UploadsPlaylistID
		}
		server.addUploadMembershipToLocked(uploadsPlaylistID, video.ID, video.PublishedAt)
	}
	writeJSON(writer, http.StatusCreated, server.videoResource(video))
}

func (server *Server) mockSeedPlaylistItem(writer http.ResponseWriter, request *http.Request) {
	var input PlaylistItemSeed
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.ID == "" || input.PlaylistID == "" || input.VideoID == "" || input.Position < 0 {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidPlaylistItem",
			"playlist item identity, video, playlist, and nonnegative position are required.",
		)
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	playlist := server.playlistLocked(input.PlaylistID)
	if playlist == nil || server.videoLocked(input.VideoID) == nil {
		writeAPIError(
			writer,
			http.StatusNotFound,
			"notFound",
			"playlist item references an unknown playlist or video.",
		)
		return
	}
	if input.Position > len(playlist.Items) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidPlaylistItem",
			"playlist item position must not leave a gap.",
		)
		return
	}
	if playlistItemExists(playlist.Items, input.ID) {
		writeAPIError(
			writer,
			http.StatusConflict,
			"duplicatePlaylistItem",
			"playlist item ID already exists.",
		)
		return
	}
	playlist.Items = append(
		playlist.Items,
		PlaylistItemState{
			ID:          input.ID,
			VideoID:     input.VideoID,
			Position:    input.Position,
			PublishedAt: input.PublishedAt,
		},
	)
	slices.SortFunc(
		playlist.Items,
		func(left PlaylistItemState, right PlaylistItemState) int { return left.Position - right.Position },
	)
	reindexPlaylist(playlist)
	resource := server.playlistItemResourceLocked(*playlist, playlist.Items[input.Position])
	writeJSON(writer, http.StatusCreated, resource)
}

func playlistItemExists(items []PlaylistItemState, id string) bool {
	return slices.ContainsFunc(items, func(item PlaylistItemState) bool { return item.ID == id })
}

func seededPlaylist(seed PlaylistSeed, ownerID string) PlaylistState {
	privacy := seed.PrivacyStatus
	if privacy == "" {
		privacy = privacyPublic
	}
	return PlaylistState{
		Description:    seed.Description,
		ID:             seed.ID,
		ThumbnailURL:   seed.ThumbnailURL,
		Title:          seed.Title,
		PrivacyStatus:  privacy,
		Podcast:        seed.Podcast,
		ManualSorting:  seed.ManualSorting,
		Hidden:         seed.Hidden,
		OwnerChannelID: ownerID,
		Items:          make([]PlaylistItemState, 0),
	}
}

func (server *Server) resetLocked() {
	server.generation++
	server.state = State{
		Channel: server.config.Channel,
		Playlists: make(
			[]PlaylistState,
			len(server.config.Playlists),
			len(server.config.Playlists)+1,
		),
		Videos: make([]VideoState, len(server.config.Videos)),
	}
	for index, playlist := range server.config.Playlists {
		server.state.Playlists[index] = seededPlaylist(playlist, server.config.Channel.ID)
	}
	if server.playlistLocked(server.config.Channel.UploadsPlaylistID) == nil {
		server.state.Playlists = append(server.state.Playlists, PlaylistState{
			ID:             server.config.Channel.UploadsPlaylistID,
			Title:          "Uploads",
			Hidden:         true,
			OwnerChannelID: server.config.Channel.ID,
			Items:          make([]PlaylistItemState, 0),
		})
	}
	for index, seed := range server.config.Videos {
		server.state.Videos[index] = seedVideoState(seed, server.config.Channel.ID)
	}
	for _, seed := range server.config.PlaylistItems {
		playlist := server.playlistLocked(seed.PlaylistID)
		publishedAt := seed.PublishedAt
		if publishedAt == "" {
			publishedAt = server.videoLocked(seed.VideoID).PublishedAt
		}
		playlist.Items = append(playlist.Items, PlaylistItemState{
			ID: seed.ID, VideoID: seed.VideoID, Position: seed.Position, PublishedAt: publishedAt,
		})
	}
	for index := range server.state.Playlists {
		slices.SortFunc(
			server.state.Playlists[index].Items,
			func(left PlaylistItemState, right PlaylistItemState) int {
				return left.Position - right.Position
			},
		)
		reindexPlaylist(&server.state.Playlists[index])
	}
	server.failures = make(map[string][]injectedFailure)
	server.delays = make(map[string]time.Duration)
	for _, gate := range server.gates {
		close(gate)
	}
	server.gates = make(map[string]chan struct{})
	server.requests = make(map[string]int)
	server.requestsByChannel = make(map[string]map[string]int)
	server.quota = QuotaState{}
	server.processingOutcomes = nil
	server.channels = map[string]ChannelSeed{server.config.Channel.ID: server.config.Channel}
	server.tokenChannels = map[string]string{server.config.BearerToken: server.config.Channel.ID}
	server.refreshTokens = map[string]string{"test-refresh-token": server.config.BearerToken}
	server.refreshFailures = make(map[string][]credentialRefreshFailureRequest)
}

func (server *Server) writeInjectedFailure(
	writer http.ResponseWriter,
	operation operation,
	channelID string,
) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	failureKey := operation.value
	queue := server.failures[failureKey]
	if channelID != "" {
		failureKey = gateKey(operation.value, channelID)
		if scopedQueue := server.failures[failureKey]; len(scopedQueue) > 0 {
			queue = scopedQueue
		} else {
			failureKey = operation.value
		}
	}
	if len(queue) == 0 {
		return false
	}
	failure := queue[0]
	server.failures[failureKey] = queue[1:]
	writeAPIError(writer, failure.Status, failure.Reason, failure.Message)
	return true
}

func (server *Server) playlistLocked(id string) *PlaylistState {
	for index := range server.state.Playlists {
		if server.state.Playlists[index].ID == id {
			return &server.state.Playlists[index]
		}
	}
	return nil
}

func (server *Server) videoLocked(id string) *VideoState {
	for index := range server.state.Videos {
		if server.state.Videos[index].ID == id {
			return &server.state.Videos[index]
		}
	}
	return nil
}

func (server *Server) createVideoLocked(
	metadata videoMetadata,
	mediaBytes int64,
	notifySubscribers bool,
	ownerChannelID string,
	uploadsPlaylistID string,
) VideoState {
	if ownerChannelID == "" {
		ownerChannelID = server.config.Channel.ID
	}
	if uploadsPlaylistID == "" {
		uploadsPlaylistID = server.config.Channel.UploadsPlaylistID
	}
	privacy := metadata.Status.PrivacyStatus
	if privacy == "" {
		privacy = "private"
	}
	categoryID := metadata.Snippet.CategoryID
	if categoryID == "" {
		categoryID = defaultVideoCategoryID
	}
	outcome := processingOutcome{
		PollsUntilTerminal: server.config.ProcessingPollsUntilSuccess,
		ProcessingStatus:   processingSucceeded, UploadStatus: uploadProcessed,
	}
	if len(server.processingOutcomes) > 0 {
		outcome = server.processingOutcomes[0]
		server.processingOutcomes = server.processingOutcomes[1:]
	}
	video := VideoState{
		ID: server.newIDLocked(
			"VIDEO",
		),
		Title:          metadata.Snippet.Title,
		Description:    metadata.Snippet.Description,
		OwnerChannelID: ownerChannelID,
		Tags: slices.Clone(
			metadata.Snippet.Tags,
		),
		CategoryID:                   categoryID,
		Duration:                     "PT1M",
		PrivacyStatus:                privacy,
		MediaBytes:                   mediaBytes,
		PublishedAt:                  time.Now().UTC().Format(time.RFC3339),
		ProcessingStatus:             processingPending,
		ProcessingPollsUntilTerminal: outcome.PollsUntilTerminal,
		TerminalProcessingStatus:     outcome.ProcessingStatus,
		UploadStatus:                 uploadPending,
		TerminalUploadStatus:         outcome.UploadStatus,
		FailureReason:                outcome.FailureReason,
		RejectionReason:              outcome.RejectionReason,
		NotifySubscribers:            notifySubscribers,
	}
	server.state.Videos = append(server.state.Videos, video)
	server.addUploadMembershipToLocked(uploadsPlaylistID, video.ID, video.PublishedAt)
	return video
}

func (server *Server) addUploadMembershipToLocked(
	uploadsPlaylistID string,
	videoID string,
	publishedAt string,
) {
	playlist := server.playlistLocked(uploadsPlaylistID)
	if playlist == nil {
		return
	}
	item := PlaylistItemState{
		ID: server.newIDLocked("PLI"), VideoID: videoID, PublishedAt: publishedAt,
	}
	playlist.Items = slices.Insert(playlist.Items, 0, item)
	reindexPlaylist(playlist)
}

func (server *Server) newIDLocked(prefix string) string {
	for {
		server.nextID++
		candidate := fmt.Sprintf("%s_%06d", prefix, server.nextID)
		if server.videoLocked(candidate) != nil || server.playlistItemIDExistsLocked(candidate) {
			continue
		}
		return candidate
	}
}

func (server *Server) playlistItemIDExistsLocked(id string) bool {
	for _, playlist := range server.state.Playlists {
		for _, item := range playlist.Items {
			if item.ID == id {
				return true
			}
		}
	}
	return false
}

func (server *Server) videoResource(video VideoState) map[string]any {
	status := map[string]any{
		"privacyStatus": video.PrivacyStatus, "uploadStatus": video.UploadStatus,
	}
	if video.FailureReason != "" {
		status["failureReason"] = video.FailureReason
	}
	if video.RejectionReason != "" {
		status["rejectionReason"] = video.RejectionReason
	}
	snippet := map[string]any{
		"channelId": video.OwnerChannelID, "title": video.Title,
		"description": video.Description, "tags": video.Tags, "categoryId": video.CategoryID,
		"publishedAt": video.PublishedAt,
	}
	if video.LiveBroadcastContent != "" {
		snippet["liveBroadcastContent"] = video.LiveBroadcastContent
	}
	resource := map[string]any{
		"kind": "youtube#video", "id": video.ID,
		"snippet":           snippet,
		"contentDetails":    map[string]any{"duration": video.Duration},
		"status":            status,
		"processingDetails": map[string]any{"processingStatus": video.ProcessingStatus},
	}
	if video.ActualEndTime != "" {
		resource["liveStreamingDetails"] = map[string]any{"actualEndTime": video.ActualEndTime}
	}
	return resource
}

func (server *Server) playlistItemResourceLocked(
	playlist PlaylistState,
	item PlaylistItemState,
) map[string]any {
	video := server.videoLocked(item.VideoID)
	title := ""
	videoOwnerChannelID := ""
	if video != nil {
		title = video.Title
		videoOwnerChannelID = video.OwnerChannelID
	}
	return map[string]any{
		"kind": "youtube#playlistItem",
		"id":   item.ID,
		"snippet": map[string]any{
			"channelId":           server.config.Channel.ID,
			"playlistId":          playlist.ID,
			"position":            item.Position,
			"title":               title,
			"publishedAt":         item.PublishedAt,
			"videoOwnerChannelId": videoOwnerChannelID,
			"resourceId":          map[string]any{"kind": "youtube#video", "videoId": item.VideoID},
		},
		"contentDetails": map[string]any{
			"videoId":          item.VideoID,
			"videoPublishedAt": item.PublishedAt,
		},
	}
}

func reindexPlaylist(playlist *PlaylistState) {
	for index := range playlist.Items {
		playlist.Items[index].Position = index
	}
}

func seedVideoState(seed VideoSeed, defaultOwnerChannelID string) VideoState {
	categoryID := seed.CategoryID
	if categoryID == "" {
		categoryID = defaultVideoCategoryID
	}
	publishedAt := seed.PublishedAt
	if publishedAt == "" {
		publishedAt = "2024-01-01T00:00:00Z"
	}
	duration := seed.Duration
	if duration == "" {
		duration = "PT1M"
	}
	privacy := seed.PrivacyStatus
	if privacy == "" {
		privacy = privacyPublic
	}
	processing := seed.ProcessingStatus
	if processing == "" {
		processing = processingSucceeded
	}
	upload := seed.UploadStatus
	if upload == "" {
		upload = uploadProcessed
	}
	notify := true
	if seed.NotifySubscribers != nil {
		notify = *seed.NotifySubscribers
	}
	ownerChannelID := seed.OwnerChannelID
	if ownerChannelID == "" {
		ownerChannelID = defaultOwnerChannelID
	}
	return VideoState{
		ID:                           seed.ID,
		Title:                        seed.Title,
		Description:                  seed.Description,
		Tags:                         slices.Clone(seed.Tags),
		OwnerChannelID:               ownerChannelID,
		CategoryID:                   categoryID,
		PublishedAt:                  publishedAt,
		Duration:                     duration,
		PrivacyStatus:                privacy,
		MediaBytes:                   seed.MediaBytes,
		ProcessingStatus:             processing,
		ProcessingPollsUntilTerminal: 1,
		TerminalProcessingStatus:     processing,
		UploadStatus:                 upload,
		TerminalUploadStatus:         upload,
		FailureReason:                seed.FailureReason,
		RejectionReason:              seed.RejectionReason,
		LiveBroadcastContent:         seed.LiveBroadcastContent,
		ActualEndTime:                seed.ActualEndTime,
		NotifySubscribers:            notify,
	}
}

func validateVideoState(video VideoState) error {
	if strings.TrimSpace(video.OwnerChannelID) == "" {
		return fmt.Errorf("owner channel ID %q must not be empty", video.OwnerChannelID)
	}
	switch video.PrivacyStatus {
	case privacyPublic, "private", "unlisted":
	default:
		return fmt.Errorf("privacy status %q is invalid", video.PrivacyStatus)
	}
	switch video.ProcessingStatus {
	case processingPending, processingSucceeded, processingFailed, "terminated":
	default:
		return fmt.Errorf("processing status %q is invalid", video.ProcessingStatus)
	}
	switch video.UploadStatus {
	case uploadPending, uploadProcessed, processingFailed, uploadRejected, "deleted":
	default:
		return fmt.Errorf("upload status %q is invalid", video.UploadStatus)
	}
	if video.UploadStatus == processingFailed && video.FailureReason == "" {
		return errors.New("failed upload requires failure reason")
	}
	if video.UploadStatus == uploadRejected && video.RejectionReason == "" {
		return errors.New("rejected upload requires rejection reason")
	}
	return nil
}

func validateProcessingOutcome(outcome processingOutcome) error {
	if outcome.PollsUntilTerminal < 1 {
		return fmt.Errorf("pollsUntilTerminal must be positive, got %d", outcome.PollsUntilTerminal)
	}
	switch outcome.ProcessingStatus {
	case processingSucceeded:
		if outcome.UploadStatus != uploadProcessed {
			return errors.New("succeeded processing requires processed upload status")
		}
	case processingFailed, "terminated":
		if outcome.UploadStatus != processingFailed && outcome.UploadStatus != uploadRejected {
			return errors.New(
				"failed or terminated processing requires failed or rejected upload status",
			)
		}
	default:
		return fmt.Errorf(
			"processingStatus %q must be succeeded, failed, or terminated",
			outcome.ProcessingStatus,
		)
	}
	if outcome.UploadStatus == processingFailed && outcome.FailureReason == "" {
		return errors.New("failed upload requires failureReason")
	}
	if outcome.UploadStatus == uploadRejected && outcome.RejectionReason == "" {
		return errors.New("rejected upload requires rejectionReason")
	}
	return nil
}

func listMaxResults(writer http.ResponseWriter, query url.Values, defaultValue int) (int, bool) {
	value := query.Get("maxResults")
	if value == "" {
		return defaultValue, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > maxListResults {
		writeAPIError(writer, http.StatusBadRequest, "invalidValue", "maxResults must be 0..50.")
		return 0, false
	}
	return parsed, true
}

func pageToken(generation int64, offset int) string {
	return fmt.Sprintf("page_%d_%d", generation, offset)
}

func (server *Server) pageOffset(writer http.ResponseWriter, token string, length int) (int, bool) {
	if token == "" {
		return 0, true
	}
	prefix := fmt.Sprintf("page_%d_", server.generation)
	if !strings.HasPrefix(token, prefix) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidPageToken",
			"pageToken is invalid or stale.",
		)
		return 0, false
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(token, prefix))
	if err != nil || offset < 0 || offset > length {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidPageToken",
			"pageToken offset is invalid.",
		)
		return 0, false
	}
	return offset, true
}

func parseNotifySubscribers(writer http.ResponseWriter, query url.Values) (bool, bool) {
	value := query.Get("notifySubscribers")
	if value == "" {
		return true, true
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidValue",
			"notifySubscribers must be boolean.",
		)
		return false, false
	}
	return parsed, true
}

func requirePart(writer http.ResponseWriter, query url.Values) bool {
	if len(query["part"]) == 0 || strings.TrimSpace(strings.Join(query["part"], ",")) == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"missingRequiredParameter",
			"part is required.",
		)
		return false
	}
	return true
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool)
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				result[item] = true
			}
		}
	}
	return result
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxMetadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidJSON",
			fmt.Sprintf("decode request JSON: %v", err),
		)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidJSON",
			"request body must contain exactly one JSON value.",
		)
		return false
	}
	return true
}

func readMetadata(writer http.ResponseWriter, body io.Reader) (videoMetadata, bool) {
	var metadata videoMetadata
	decoder := json.NewDecoder(io.LimitReader(body, maxMetadataBytes+1))
	if err := decoder.Decode(&metadata); err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMetadata",
			fmt.Sprintf("decode video metadata: %v", err),
		)
		return videoMetadata{}, false
	}
	return metadata, true
}

func readUpload(writer http.ResponseWriter, request *http.Request) (videoMetadata, int64, bool) {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidContentType",
			fmt.Sprintf("parse Content-Type: %v", err),
		)
		return videoMetadata{}, 0, false
	}
	if mediaType != "multipart/related" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidContentType",
			"Content-Type must be multipart/related.",
		)
		return videoMetadata{}, 0, false
	}
	return readMultipartUpload(writer, request.Body, parameters["boundary"])
}

func readMultipartUpload(
	writer http.ResponseWriter,
	body io.Reader,
	boundary string,
) (videoMetadata, int64, bool) {
	if boundary == "" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidContentType",
			"multipart boundary is required.",
		)
		return videoMetadata{}, 0, false
	}
	reader := multipart.NewReader(body, boundary)
	metadataPart, err := reader.NextPart()
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			fmt.Sprintf("read metadata part: %v", err),
		)
		return videoMetadata{}, 0, false
	}
	metadata, ok := readMetadataPart(writer, metadataPart)
	if !ok {
		return videoMetadata{}, 0, false
	}

	mediaPart, err := reader.NextPart()
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			fmt.Sprintf("read media part: %v", err),
		)
		return videoMetadata{}, 0, false
	}
	mediaBytes, ok := readMediaPart(writer, mediaPart)
	if !ok {
		return videoMetadata{}, 0, false
	}
	if extra, extraErr := reader.NextPart(); extraErr != io.EOF {
		if extra != nil {
			_ = extra.Close()
		}
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			"The upload must contain exactly two multipart parts.",
		)
		return videoMetadata{}, 0, false
	}
	return metadata, mediaBytes, true
}

func readMetadataPart(
	writer http.ResponseWriter,
	metadataPart *multipart.Part,
) (videoMetadata, bool) {
	metadataType, _, metadataTypeErr := mime.ParseMediaType(
		metadataPart.Header.Get("Content-Type"),
	)
	if metadataTypeErr != nil || metadataType != "application/json" {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			"The first multipart part must be application/json metadata.",
		)
		return videoMetadata{}, false
	}
	metadata, ok := readMetadata(writer, metadataPart)
	_ = metadataPart.Close()
	if !ok {
		return videoMetadata{}, false
	}
	return metadata, true
}

func readMediaPart(writer http.ResponseWriter, mediaPart *multipart.Part) (int64, bool) {
	mediaType, _, mediaTypeErr := mime.ParseMediaType(mediaPart.Header.Get("Content-Type"))
	if mediaTypeErr != nil ||
		(mediaType != "application/octet-stream" && !strings.HasPrefix(mediaType, "video/")) {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			"The second multipart part must contain supported video media.",
		)
		return 0, false
	}
	mediaBytes, ok := countBody(writer, mediaPart)
	_ = mediaPart.Close()
	if !ok {
		return 0, false
	}
	if mediaBytes == 0 {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"invalidMultipart",
			"The media part must not be empty.",
		)
		return 0, false
	}
	return mediaBytes, true
}

func countBody(writer http.ResponseWriter, body io.Reader) (int64, bool) {
	read, err := io.Copy(io.Discard, io.LimitReader(body, maxUploadBytes+1))
	if err != nil {
		writeAPIError(
			writer,
			http.StatusBadRequest,
			"uploadFailed",
			fmt.Sprintf("read upload body: %v", err),
		)
		return 0, false
	}
	if read > maxUploadBytes {
		writeAPIError(
			writer,
			http.StatusRequestEntityTooLarge,
			"uploadTooLarge",
			fmt.Sprintf("mock upload exceeds %d bytes", maxUploadBytes),
		)
		return 0, false
	}
	return read, true
}

func writeList(writer http.ResponseWriter, kind string, items []any) {
	writeJSON(
		writer,
		http.StatusOK,
		listResponse{
			Kind:     kind,
			Items:    items,
			PageInfo: pageInfo{TotalResults: len(items), ResultsPerPage: len(items)},
		},
	)
}

func writeAPIError(writer http.ResponseWriter, status int, reason string, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]any{
		"code":    status,
		"message": message,
		"errors": []any{
			map[string]any{"domain": "youtube.mock", "reason": reason, "message": message},
		},
	}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		slog.Error("write YouTube mock response", "error", err)
	}
}
