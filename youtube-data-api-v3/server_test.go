package youtubemock_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	youtubemock "github.com/meoyawn/mock-servers/youtube-data-api-v3"
)

const (
	publicVisibility  = "public"
	privateVisibility = "private"
)

func TestAuthenticationPlaylistDiscoveryAndNoPlaylistCreation(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())

	unauthorized := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=snippet&mine=true",
		"", nil, "",
	)
	defer unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized ||
		errorReason(t, unauthorized) != "youtubeSignupRequired" {
		t.Fatalf("unauthorized response status = %d", unauthorized.StatusCode)
	}

	playlists := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/playlists?part=snippet&part=status&mine=true",
		"test-token", nil, "",
	)
	defer playlists.Body.Close()
	if playlists.StatusCode != http.StatusOK {
		t.Fatalf("playlists.list status = %d", playlists.StatusCode)
	}
	var list struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Description string `json:"description"`
				Thumbnails  struct {
					Medium struct {
						URL string `json:"url"`
					} `json:"medium"`
				} `json:"thumbnails"`
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	decodeResponse(t, playlists, &list)
	if len(list.Items) != 1 || list.Items[0].ID != "PL_LISTENBOX_PODCAST" ||
		list.Items[0].Snippet.Title == "" || list.Items[0].Snippet.Description == "" ||
		list.Items[0].Snippet.Thumbnails.Medium.URL != "https://example.test/listenbox-playlist.jpg" {
		t.Fatalf("playlists.list response = %#v", list)
	}

	create := doRequest(
		t, server, http.MethodPost,
		"/youtube/v3/playlists?part=snippet",
		"test-token", strings.NewReader("{}"), "application/json",
	)
	defer create.Body.Close()
	if create.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("playlists.insert status = %d, want 405", create.StatusCode)
	}
}

func TestPlaylistPositionZeroKeepsNewestEpisodeOnTop(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	firstVideoID := uploadMultipartVideo(t, server, "Episode 1", []byte("first-media"))
	secondVideoID := uploadMultipartVideo(t, server, "Episode 2", []byte("second-media"))

	firstInsert := insertPlaylistItem(t, server, firstVideoID, 0, http.StatusOK)
	_ = firstInsert.Body.Close()
	secondInsert := insertPlaylistItem(t, server, secondVideoID, 0, http.StatusOK)
	_ = secondInsert.Body.Close()

	state := getState(t, server)
	if len(state.Playlists) != 1 || len(state.Playlists[0].Items) != 2 {
		t.Fatalf("playlist state = %#v", state.Playlists)
	}
	items := state.Playlists[0].Items
	if items[0].VideoID != secondVideoID || items[0].Position != 0 ||
		items[1].VideoID != firstVideoID ||
		items[1].Position != 1 {
		t.Fatalf("top-to-bottom playlist items = %#v", items)
	}
	if len(state.Videos) != 2 || state.Videos[0].MediaBytes != int64(len("first-media")) {
		t.Fatalf("video state = %#v", state.Videos)
	}
}

func TestPositionRequiresCreatorProvidedManualOrdering(t *testing.T) {
	t.Parallel()
	config := youtubemock.DefaultConfig()
	config.Playlists[0].ManualSorting = false
	server := newTestServer(t, config)
	videoID := uploadMultipartVideo(t, server, "Episode", []byte("media"))

	response := insertPlaylistItem(t, server, videoID, 0, http.StatusBadRequest)
	defer response.Body.Close()
	if reason := errorReason(t, response); reason != "manualSortRequired" {
		t.Fatalf("playlist insertion error reason = %q", reason)
	}
}

func TestInjectedUploadFailureIsOneShot(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	failure := strings.NewReader(
		`{"operation":"videos.insert","status":503,"reason":"backendError","message":"try again"}`,
	)
	configure := doRequest(
		t, server, http.MethodPost,
		"/__mock__/fail-next",
		"", failure, "application/json",
	)
	defer configure.Body.Close()
	if configure.StatusCode != http.StatusNoContent {
		t.Fatalf("fail-next status = %d", configure.StatusCode)
	}

	failedBody, failedContentType := multipartVideoRequest(t, "Failed episode", []byte("media"))
	failed := doRequest(
		t, server, http.MethodPost,
		"/upload/youtube/v3/videos?part=snippet,status&uploadType=multipart",
		"test-token", failedBody, failedContentType,
	)
	defer failed.Body.Close()
	if failed.StatusCode != http.StatusServiceUnavailable ||
		errorReason(t, failed) != "backendError" {
		t.Fatalf("injected failure status = %d", failed.StatusCode)
	}

	if videoID := uploadMultipartVideo(
		t,
		server,
		"Successful episode",
		[]byte("media"),
	); videoID == "" {
		t.Fatal("retry returned an empty video ID")
	}
}

func TestProcessingPollAndPublishUpdateKeepOneVideo(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	videoID := uploadMultipartVideo(t, server, "Draft episode", []byte("media"))
	for range 2 {
		response := doRequest(
			t, server, http.MethodGet,
			"/youtube/v3/videos?part=status,processingDetails&id="+videoID,
			"test-token", nil, "",
		)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("videos.list status = %d", response.StatusCode)
		}
	}
	update := strings.NewReader(`{"id":"` + videoID + `","status":{"privacyStatus":"public"}}`)
	response := doRequest(
		t, server, http.MethodPut,
		"/youtube/v3/videos?part=status",
		"test-token", update, "application/json",
	)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("videos.update status = %d", response.StatusCode)
	}
	state := getState(t, server)
	if len(state.Videos) != 1 || state.Videos[0].ID != videoID ||
		state.Videos[0].PrivacyStatus != publicVisibility ||
		state.Videos[0].ProcessingStatus != "succeeded" {
		t.Fatalf("video state = %#v", state.Videos)
	}
}

func TestSeededUploadsCatalogPaginatesAndBatchesMetadata(t *testing.T) {
	t.Parallel()
	config := seededUploadsConfig()
	server := newTestServer(t, config)

	assertUploadsChannel(t, server)

	videoIDs, firstPageToken := collectUploadPages(t, server)

	filtered := doRequest(
		t,
		server,
		http.MethodGet,
		"/youtube/v3/playlistItems?part=contentDetails&playlistId=UU_LISTENBOX_TEST&videoId=SEEDED_3",
		"test-token",
		nil,
		"",
	)
	defer filtered.Body.Close()
	var filteredPage struct {
		Items []json.RawMessage `json:"items"`
	}
	decodeResponse(t, filtered, &filteredPage)
	if len(filteredPage.Items) != 1 {
		t.Fatalf("filtered playlist items = %d", len(filteredPage.Items))
	}

	assertSeededVideoMetadata(t, server, videoIDs)

	reset := doRequest(t, server, http.MethodPost, "/__mock__/reset", "", nil, "")
	_ = reset.Body.Close()
	state := getState(t, server)
	if len(state.Videos) != 5 || state.UploadsPlaylist == nil ||
		len(state.UploadsPlaylist.Items) != 5 {
		t.Fatalf("reset seeded state = %#v", state)
	}
	stale := doRequest(
		t,
		server,
		http.MethodGet,
		"/youtube/v3/playlistItems?part=contentDetails&playlistId=UU_LISTENBOX_TEST&pageToken="+firstPageToken,
		"test-token",
		nil,
		"",
	)
	defer stale.Body.Close()
	if stale.StatusCode != http.StatusBadRequest || errorReason(t, stale) != "invalidPageToken" {
		t.Fatalf("stale page token status = %d", stale.StatusCode)
	}
}

func assertSeededVideoMetadata(t *testing.T, server *httptest.Server, videoIDs []string) {
	t.Helper()
	videos := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/videos?part=snippet,contentDetails,status&id="+strings.Join(videoIDs, ","),
		"test-token", nil, "",
	)
	defer videos.Body.Close()
	var videoList struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title       string   `json:"title"`
				PublishedAt string   `json:"publishedAt"`
				Tags        []string `json:"tags"`
			} `json:"snippet"`
			ContentDetails struct {
				Duration string `json:"duration"`
			} `json:"contentDetails"`
			Status struct {
				PrivacyStatus string `json:"privacyStatus"`
			} `json:"status"`
		} `json:"items"`
	}
	decodeResponse(t, videos, &videoList)
	if len(videoList.Items) != 5 ||
		videoList.Items[1].Status.PrivacyStatus != "unlisted" ||
		videoList.Items[2].Status.PrivacyStatus != privateVisibility ||
		videoList.Items[4].ContentDetails.Duration != "PT5M" ||
		videoList.Items[0].Snippet.PublishedAt == "" || len(videoList.Items[0].Snippet.Tags) != 2 {
		t.Fatalf("videos.list seeded metadata = %#v", videoList.Items)
	}
}

func collectUploadPages(t *testing.T, server *httptest.Server) ([]string, string) {
	t.Helper()
	nextToken := ""
	firstPageToken := ""
	videoIDs := make([]string, 0, 5)
	pageCount := 0
	for {
		path := "/youtube/v3/playlistItems?part=contentDetails&playlistId=UU_LISTENBOX_TEST&maxResults=2"
		if nextToken != "" {
			path += "&pageToken=" + nextToken
		}
		response := doRequest(t, server, http.MethodGet, path, "test-token", nil, "")
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ContentDetails struct {
					VideoID string `json:"videoId"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		decodeResponse(t, response, &page)
		_ = response.Body.Close()
		pageCount++
		for _, item := range page.Items {
			videoIDs = append(videoIDs, item.ContentDetails.VideoID)
		}
		if page.NextPageToken == "" {
			break
		}
		if firstPageToken == "" {
			firstPageToken = page.NextPageToken
		}
		nextToken = page.NextPageToken
	}
	if pageCount != 3 ||
		strings.Join(videoIDs, ",") != "SEEDED_0,SEEDED_1,SEEDED_2,SEEDED_3,SEEDED_4" {
		t.Fatalf("paginated upload IDs = %v across %d pages", videoIDs, pageCount)
	}

	return videoIDs, firstPageToken
}

func assertUploadsChannel(t *testing.T, server *httptest.Server) {
	t.Helper()
	channels := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=contentDetails&mine=true",
		"test-token", nil, "",
	)
	defer channels.Body.Close()
	var channelList struct {
		Items []struct {
			ContentDetails struct {
				RelatedPlaylists struct {
					Uploads string `json:"uploads"`
				} `json:"relatedPlaylists"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	decodeResponse(t, channels, &channelList)
	if len(channelList.Items) != 1 ||
		channelList.Items[0].ContentDetails.RelatedPlaylists.Uploads != "UU_LISTENBOX_TEST" {
		t.Fatalf("channels.list contentDetails = %#v", channelList)
	}
}

func seededUploadsConfig() youtubemock.Config {
	config := youtubemock.DefaultConfig()
	privacy := []string{
		publicVisibility,
		"unlisted",
		privateVisibility,
		publicVisibility,
		publicVisibility,
	}
	for index := range 5 {
		id := "SEEDED_" + strconv.Itoa(index)
		config.Videos = append(config.Videos, youtubemock.VideoSeed{
			ID: id, Title: "Episode " + strconv.Itoa(index), Description: "listenbox:episode:" + id,
			Tags: []string{"podcast", id}, CategoryID: "22",
			PublishedAt: "2024-01-0" + strconv.Itoa(index+1) + "T12:00:00Z",
			Duration:    "PT" + strconv.Itoa(index+1) + "M", PrivacyStatus: privacy[index],
		})
		config.PlaylistItems = append(config.PlaylistItems, youtubemock.PlaylistItemSeed{
			ID: "UPLOAD_ITEM_" + strconv.Itoa(index), PlaylistID: config.Channel.UploadsPlaylistID,
			VideoID: id, Position: index,
		})
	}
	return config
}

func TestRuntimeVideoSeedExposesOwnershipDurationUploadsAndRequestCounters(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	body := strings.NewReader(`{
		"video": {
			"id": "SEEDED_RUNTIME",
			"title": " Existing episode ",
			"ownerChannelId": "FOREIGN_CHANNEL",
			"duration": "PT2M1S",
			"privacyStatus": "unlisted"
		},
		"uploadsMembership": true
	}`)
	seeded := doRequest(
		t, server, http.MethodPost,
		"/__mock__/videos",
		"", body, "application/json",
	)
	defer seeded.Body.Close()
	if seeded.StatusCode != http.StatusCreated {
		t.Fatalf("seed video status = %d", seeded.StatusCode)
	}

	listed := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/playlistItems?part=contentDetails&playlistId=UU_LISTENBOX_TEST&maxResults=50",
		"test-token", nil, "",
	)
	_ = listed.Body.Close()
	metadata := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/videos?part=snippet,contentDetails&id=SEEDED_RUNTIME",
		"test-token", nil, "",
	)
	_ = metadata.Body.Close()

	state := getState(t, server)
	if len(state.Videos) != 1 || state.Videos[0].OwnerChannelID != "FOREIGN_CHANNEL" ||
		state.Videos[0].Duration != "PT2M1S" || !state.Videos[0].UploadsMember {
		t.Fatalf("seeded video state = %#v", state.Videos)
	}
	if state.UploadsPlaylist == nil || len(state.UploadsPlaylist.Items) != 1 ||
		state.Requests["playlistItems.list"] != 1 ||
		state.Requests["videos.list"] != 1 {
		t.Fatalf("seeded uploads/request state = %#v", state)
	}
}

func TestQuotaDelayAndResetControls(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	delay := strings.NewReader(`{"operation":"channels.list","milliseconds":30}`)
	response := doRequest(
		t, server, http.MethodPost,
		"/__mock__/delay",
		"", delay, "application/json",
	)
	_ = response.Body.Close()
	started := time.Now()
	response = doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=snippet&mine=true",
		"test-token", nil, "",
	)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || time.Since(started) < 20*time.Millisecond {
		t.Fatalf(
			"delayed channels.list status = %d, elapsed = %s",
			response.StatusCode,
			time.Since(started),
		)
	}

	assertQuotaAndReset(t, server)
}

func assertQuotaAndReset(t *testing.T, server *httptest.Server) {
	t.Helper()
	var response *http.Response
	quota := strings.NewReader(`{"exhausted":false,"afterRequests":1}`)
	response = doRequest(
		t, server, http.MethodPost,
		"/__mock__/quota",
		"", quota, "application/json",
	)
	_ = response.Body.Close()
	response = doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=snippet&mine=true",
		"test-token", nil, "",
	)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("first quota-limited request status = %d", response.StatusCode)
	}
	response = doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=snippet&mine=true",
		"test-token", nil, "",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden || errorReason(t, response) != "quotaExceeded" {
		t.Fatalf("exhausted request status = %d", response.StatusCode)
	}

	reset := strings.NewReader(`{"exhausted":false}`)
	response = doRequest(
		t, server, http.MethodPost,
		"/__mock__/quota",
		"", reset, "application/json",
	)
	_ = response.Body.Close()
	response = doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/channels?part=snippet&mine=true",
		"test-token", nil, "",
	)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("post-quota-reset request status = %d", response.StatusCode)
	}
	state := getState(t, server)
	if state.Requests["channels.list"] != 4 || state.Quota.Requests != 1 {
		t.Fatalf("request/quota state = %#v / %#v", state.Requests, state.Quota)
	}
}

func TestPlaylistsPaginationAndFilters(t *testing.T) {
	t.Parallel()
	config := youtubemock.DefaultConfig()
	config.Playlists = append(config.Playlists,
		youtubemock.PlaylistSeed{ID: "PL_SECOND", Title: "Second playlist"},
		youtubemock.PlaylistSeed{ID: "PL_THIRD", Title: "Third playlist"},
	)
	server := newTestServer(t, config)
	first := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/playlists?part=snippet&mine=true&maxResults=2",
		"test-token", nil, "",
	)
	defer first.Body.Close()
	var page struct {
		NextPageToken string `json:"nextPageToken"`
		Items         []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	decodeResponse(t, first, &page)
	if len(page.Items) != 2 || page.NextPageToken == "" {
		t.Fatalf("first playlists page = %#v", page)
	}
	second := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/playlists?part=snippet&mine=true&maxResults=2&pageToken="+page.NextPageToken,
		"test-token", nil, "",
	)
	defer second.Body.Close()
	page = struct {
		NextPageToken string `json:"nextPageToken"`
		Items         []struct {
			ID string `json:"id"`
		} `json:"items"`
	}{}
	decodeResponse(t, second, &page)
	if len(page.Items) != 1 || page.Items[0].ID != "PL_THIRD" || page.NextPageToken != "" {
		t.Fatalf("second playlists page = %#v", page)
	}
	filtered := doRequest(
		t, server, http.MethodGet,
		"/youtube/v3/playlists?part=snippet&id=PL_SECOND",
		"test-token", nil, "",
	)
	defer filtered.Body.Close()
	decodeResponse(t, filtered, &page)
	if len(page.Items) != 1 || page.Items[0].ID != "PL_SECOND" {
		t.Fatalf("filtered playlists = %#v", page)
	}
}

func TestProcessingFailureRejectionAndNotifySubscribers(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	for _, outcome := range []string{
		`{"pollsUntilTerminal":1,"processingStatus":"failed","uploadStatus":"failed","failureReason":"codec"}`,
		`{"pollsUntilTerminal":1,"processingStatus":"terminated","uploadStatus":"rejected","rejectionReason":"copyright"}`,
	} {
		response := doRequest(
			t, server, http.MethodPost,
			"/__mock__/processing-outcome",
			"", strings.NewReader(outcome), "application/json",
		)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("processing outcome status = %d", response.StatusCode)
		}
	}

	firstVideoID := uploadMultipartVideoWithNotify(t, server, "First episode", []byte("media"), false)
	secondVideoID := uploadMultipartVideoWithNotify(t, server, "Second episode", []byte("media"), true)
	for _, videoID := range []string{firstVideoID, secondVideoID} {
		response := doRequest(
			t, server, http.MethodGet,
			"/youtube/v3/videos?part=status,processingDetails&id="+videoID,
			"test-token", nil, "",
		)
		_ = response.Body.Close()
	}
	state := getState(t, server)
	if len(state.Videos) != 2 || state.UploadsPlaylist == nil ||
		len(state.UploadsPlaylist.Items) != 2 {
		t.Fatalf("processing/notification state = %#v", state)
	}
	assertTerminalVideo(
		t,
		state.Videos[0],
		youtubemock.VideoState{
			ProcessingStatus: "failed",
			UploadStatus:     "failed",
			FailureReason:    "codec",
		},
	)
	assertTerminalVideo(
		t,
		state.Videos[1],
		youtubemock.VideoState{
			ProcessingStatus:  "terminated",
			UploadStatus:      "rejected",
			RejectionReason:   "copyright",
			NotifySubscribers: true,
		},
	)
}

func assertTerminalVideo(t *testing.T, got, want youtubemock.VideoState) {
	t.Helper()
	if got.ProcessingStatus != want.ProcessingStatus || got.UploadStatus != want.UploadStatus ||
		got.FailureReason != want.FailureReason || got.RejectionReason != want.RejectionReason ||
		got.NotifySubscribers != want.NotifySubscribers {
		t.Fatalf("terminal video = %#v, want outcome %#v", got, want)
	}
}

func TestVideosUpdateSnippetRequiresCategoryID(t *testing.T) {
	t.Parallel()
	server := newTestServer(t, youtubemock.DefaultConfig())
	videoID := uploadMultipartVideo(t, server, "Original title", []byte("media"))

	invalid := strings.NewReader(
		`{"id":"` + videoID + `","snippet":{"title":"Updated title","description":"Updated description"}}`,
	)
	response := doRequest(
		t, server, http.MethodPut,
		"/youtube/v3/videos?part=snippet",
		"test-token", invalid, "application/json",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest ||
		errorReason(t, response) != "invalidVideoMetadata" {
		t.Fatalf("videos.update without category status = %d", response.StatusCode)
	}
	state := getState(t, server)
	if len(state.Videos) != 1 || state.Videos[0].Title != "Original title" {
		t.Fatalf("video state after rejected update = %#v", state.Videos)
	}

	valid := strings.NewReader(
		`{"id":"` + videoID + `","snippet":{"title":"Updated title","description":"Updated description","categoryId":"22"}}`,
	)
	response = doRequest(
		t, server, http.MethodPut,
		"/youtube/v3/videos?part=snippet",
		"test-token", valid, "application/json",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("videos.update with category status = %d", response.StatusCode)
	}
	state = getState(t, server)
	if len(state.Videos) != 1 || state.Videos[0].Title != "Updated title" ||
		state.Videos[0].Description != "Updated description" || state.Videos[0].CategoryID != "22" {
		t.Fatalf("video state after valid update = %#v", state.Videos)
	}
}

func newTestServer(t *testing.T, config youtubemock.Config) *httptest.Server {
	t.Helper()
	handler, err := youtubemock.New(config)
	if err != nil {
		t.Fatalf("create mock: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func uploadMultipartVideo(
	t *testing.T,
	server *httptest.Server,
	title string,
	media []byte,
) string {
	t.Helper()
	return uploadMultipartVideoWithNotify(t, server, title, media, true)
}

func uploadMultipartVideoWithNotify(
	t *testing.T,
	server *httptest.Server,
	title string,
	media []byte,
	notifySubscribers bool,
) string {
	t.Helper()
	body, contentType := multipartVideoRequest(t, title, media)
	path := "/upload/youtube/v3/videos?part=snippet,status&uploadType=multipart&notifySubscribers=" + strconv.FormatBool(
		notifySubscribers,
	)
	response := doRequest(t, server, http.MethodPost, path, "test-token", body, contentType)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("videos.insert status = %d: %s", response.StatusCode, raw)
	}
	var video struct {
		ID string `json:"id"`
	}
	decodeResponse(t, response, &video)
	if video.ID == "" {
		t.Fatal("videos.insert returned empty ID")
	}
	return video.ID
}

func multipartVideoRequest(t *testing.T, title string, media []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataHeader := makeMultipartHeader("application/json")
	metadataPart, err := writer.CreatePart(metadataHeader)
	if err != nil {
		t.Fatalf("create metadata part: %v", err)
	}
	if err := json.NewEncoder(metadataPart).Encode(map[string]any{
		"snippet": map[string]string{"title": title},
		"status":  map[string]string{"privacyStatus": privateVisibility},
	}); err != nil {
		t.Fatalf("encode metadata: %v", err)
	}
	mediaPart, err := writer.CreatePart(makeMultipartHeader("video/mp4"))
	if err != nil {
		t.Fatalf("create media part: %v", err)
	}
	if _, err := mediaPart.Write(media); err != nil {
		t.Fatalf("write media: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	return &body, "multipart/related; boundary=" + writer.Boundary()
}

func makeMultipartHeader(contentType string) map[string][]string {
	return map[string][]string{"Content-Type": {contentType}}
}

func insertPlaylistItem(
	t *testing.T,
	server *httptest.Server,
	videoID string,
	position int,
	wantStatus int,
) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"snippet": map[string]any{
		"playlistId": "PL_LISTENBOX_PODCAST", "position": position,
		"resourceId": map[string]string{"kind": "youtube#video", "videoId": videoID},
	}})
	if err != nil {
		t.Fatalf("encode playlist item: %v", err)
	}
	response := doRequest(
		t, server, http.MethodPost,
		"/youtube/v3/playlistItems?part=snippet",
		"test-token", bytes.NewReader(body), "application/json",
	)
	if response.StatusCode != wantStatus {
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf(
			"playlistItems.insert status = %d, want %d: %s",
			response.StatusCode,
			wantStatus,
			raw,
		)
	}
	return response
}

func getState(t *testing.T, server *httptest.Server) youtubemock.State {
	t.Helper()
	response := doRequest(t, server, http.MethodGet, "/__mock__/state", "", nil, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("mock state status = %d", response.StatusCode)
	}
	var state youtubemock.State
	decodeResponse(t, response, &state)
	return state
}

func doRequest(
	t *testing.T,
	server *httptest.Server,
	method string,
	path string,
	token string,
	body io.Reader,
	contentType string,
) *http.Response {
	t.Helper()
	return doAbsoluteRequest(t, server.Client(), method, server.URL+path, token, body, contentType)
}

func doAbsoluteRequest(
	t *testing.T,
	client *http.Client,
	method string,
	requestURL string,
	token string,
	body io.Reader,
	contentType string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, requestURL, body)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func errorReason(t *testing.T, response *http.Response) string {
	t.Helper()
	var payload struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	decodeResponse(t, response, &payload)
	if len(payload.Error.Errors) == 0 {
		t.Fatalf("error response has no reasons: %#v", payload)
	}
	return payload.Error.Errors[0].Reason
}
