package youtubewebmock

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	playlistControlBodyLimit = 1 << 20
	continuationFieldCount   = 3
)

// CookieSession contains only synthetic test credentials. Never use browser exports here.
type CookieSession struct {
	Expected string `json:"expected"`
	Rotate   string `json:"rotate,omitempty"`
}

// PlaylistVideo models visible and unavailable entries independently of absence.
type PlaylistVideo struct {
	ID                        string         `json:"id"`
	MediaFixture              string         `json:"media_fixture,omitempty"`
	Title                     string         `json:"title,omitempty"`
	ThumbnailURL              string         `json:"thumbnail_url,omitempty"`
	PublishedAt               string         `json:"published_at,omitempty"`
	UploadedAt                string         `json:"uploaded_at,omitempty"`
	CookieSession             *CookieSession `json:"cookie_session,omitempty"`
	DurationSeconds           *int64         `json:"duration_seconds,omitempty"`
	Unavailable               bool           `json:"unavailable,omitempty"`
	Malformed                 bool           `json:"malformed,omitempty"`
	Lockup                    bool           `json:"lockup,omitempty"`
	ToggleableMenu            bool           `json:"toggleable_menu,omitempty"`
	UnknownMenu               bool           `json:"unknown_menu,omitempty"`
	MalformedMenu             bool           `json:"malformed_menu,omitempty"`
	UnknownItem               bool           `json:"unknown_item,omitempty"`
	EmptyNode                 bool           `json:"empty_node,omitempty"`
	MissingTitle              bool           `json:"missing_title,omitempty"`
	Hidden                    bool           `json:"hidden,omitempty"`
	PlaybackUnavailable       bool           `json:"playback_unavailable,omitempty"`
	PlaybackSubreason         string         `json:"playback_subreason,omitempty"`
	MediaForbidden            bool           `json:"media_forbidden,omitempty"`
	RefusedAudioVariant       bool           `json:"refused_audio_variant,omitempty"`
	MobileMediaProbeForbidden bool           `json:"mobile_media_probe_forbidden,omitempty"`
	EmbeddedMediaForbidden    bool           `json:"embedded_media_forbidden,omitempty"`
	NativeMediaForbidden      bool           `json:"native_media_forbidden,omitempty"`
	NativePlaybackUnavailable bool           `json:"native_playback_unavailable,omitempty"`
	LoginRequired             bool           `json:"login_required,omitempty"`
	Private                   bool           `json:"private,omitempty"`
	SABR                      bool           `json:"sabr,omitempty"`
	MobileSABR                bool           `json:"mobile_sabr,omitempty"`
	EmbeddingDisabled         bool           `json:"embedding_disabled,omitempty"`
	PlayerDisconnect          bool           `json:"player_disconnect,omitempty"`
	// RateLimited answers every player client the way YouTube answers a
	// session it has rate-limited; NativeRateLimited limits only VisionOS.
	RateLimited       bool `json:"rate_limited,omitempty"`
	NativeRateLimited bool `json:"native_rate_limited,omitempty"`
	LiveOffline       bool `json:"live_offline,omitempty"`
}

type videoDates struct {
	published, uploaded string
}

// PlaylistFixture is scoped to a unique test playlist; changing it cannot affect sibling tests.
type PlaylistFixture struct {
	ID          string            `json:"id"`
	Title       string            `json:"title,omitempty"`
	ArtworkURL  string            `json:"artwork_url,omitempty"`
	TitlePrefix string            `json:"title_prefix,omitempty"`
	Pages       [][]PlaylistVideo `json:"pages"`
	FailPage    *int              `json:"fail_page,omitempty"`
	// HiddenVideos are omitted by YouTube, so their IDs cannot prove absence.
	HiddenVideos   int  `json:"hidden_videos,omitempty"`
	PreferenceForm bool `json:"preference_form,omitempty"`
	ItemSection    bool `json:"item_section,omitempty"`
	UnknownAlert   bool `json:"unknown_alert,omitempty"`
}

func (server *Server) configurePlaylist(writer http.ResponseWriter, request *http.Request) {
	var fixture PlaylistFixture
	if err := json.NewDecoder(io.LimitReader(request.Body, playlistControlBodyLimit)).
		Decode(&fixture); err != nil || !validPlaylistID(fixture.ID) ||
		len(fixture.Pages) == 0 {
		http.Error(writer, "invalid playlist fixture", http.StatusBadRequest)
		return
	}
	media := make(map[string]map[string]any)
	for _, page := range fixture.Pages {
		for _, video := range page {
			if video.MediaFixture != "" {
				response, err := server.fixturePlayerResponse(
					request.Context(),
					video.ID,
					video.MediaFixture,
				)
				if err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				media[video.ID] = response
			}
		}
	}
	server.mu.Lock()
	server.playlists[fixture.ID] = fixture
	for _, page := range fixture.Pages {
		for _, video := range page {
			server.videoTitles[video.ID] = video.Title
			server.fixturePlayers[video.ID] = media[video.ID]
			server.videoArtwork[video.ID] = video.ThumbnailURL
			server.videoDates[video.ID] = videoDates{
				published: video.PublishedAt,
				uploaded:  video.UploadedAt,
			}
			server.unavailableVideos[video.ID] = video.PlaybackUnavailable
			server.playbackSubreasons[video.ID] = video.PlaybackSubreason
			server.sabrVideos[video.ID] = video.SABR
			server.mobileSABR[video.ID] = video.MobileSABR
			server.embeddingDisabled[video.ID] = video.EmbeddingDisabled
			server.loginRequiredVideos[video.ID] = video.loginReason()
			server.forbiddenMedia[video.ID] = video.MediaForbidden
			server.refusedAudioVariants[video.ID] = video.RefusedAudioVariant
			server.mobileForbiddenProbes[video.ID] = video.MobileMediaProbeForbidden
			server.embeddedForbiddenMedia[video.ID] = video.EmbeddedMediaForbidden
			server.nativeForbiddenMedia[video.ID] = video.NativeMediaForbidden
			server.nativeUnavailableVideos[video.ID] = video.NativePlaybackUnavailable
			server.cookieSessions[video.ID] = video.CookieSession
			server.playerDisconnects[video.ID] = video.PlayerDisconnect
			server.rateLimitedVideos[video.ID] = video.RateLimited
			server.nativeRateLimited[video.ID] = video.NativeRateLimited
			server.liveOfflineVideos[video.ID] = video.LiveOffline
		}
	}
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (video PlaylistVideo) loginReason() string {
	if video.Private {
		return "Private video"
	}
	if video.LoginRequired {
		return "Sign in to confirm you’re not a bot"
	}
	return ""
}

func (server *Server) servePlaylist(writer io.Writer, request *http.Request) {
	id, page, showUnavailable, err := parsePlaylistRequest(request)
	if err != nil {
		writeProxyResponse(writer, http.StatusBadRequest, err.Error())
		return
	}
	server.recordFor(id, request)
	server.waitForGate(fmt.Sprintf("%s:%d", id, page))
	server.mu.Lock()
	fixture, found := server.playlists[id]
	server.mu.Unlock()
	if !found {
		writeJSON(writer, playlistResponse(strings.TrimPrefix(id, "PL")))
		return
	}
	if fixture.FailPage != nil && *fixture.FailPage == page {
		writeProxyResponse(writer, http.StatusServiceUnavailable, "injected playlist failure")
		return
	}
	if page < 0 || page >= len(fixture.Pages) {
		writeProxyResponse(writer, http.StatusBadRequest, "page outside fixture")
		return
	}
	entries := playlistEntries(fixture.Pages[page], fixture.TitlePrefix, showUnavailable)
	writeJSON(writer, playlistPageResponse(fixture, entries, id, page, showUnavailable))
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistPageResponse(fixture PlaylistFixture, entries []map[string]any,
	id string, page int, showUnavailable bool,
) map[string]any {
	if page+1 < len(fixture.Pages) {
		// Continuation tokens retain the initial browse mode, independently of
		// the stable id:page request gates used by integrated tests.
		token := fmt.Sprintf("%s:%d:%t", id, page+1, showUnavailable)
		if fixture.ItemSection {
			entries = append(entries, playlistContinuationView(token))
		} else {
			entries = append(entries, map[string]any{"continuationItemRenderer": map[string]any{
				"trigger": "CONTINUATION_TRIGGER_ON_ITEM_SHOWN",
				"continuationEndpoint": map[string]any{
					"continuationCommand": map[string]string{
						"token":   token,
						"request": "CONTINUATION_REQUEST_TYPE_BROWSE",
					},
				},
			}})
		}
	}
	if page > 0 {
		response := map[string]any{
			"onResponseReceivedActions": []map[string]any{
				{"appendContinuationItemsAction": map[string]any{"continuationItems": entries}},
			},
		}
		if fixture.ItemSection {
			// Structural fixture based on the 2026-10-22 capture: continuation
			// responses retain bodyless tab chrome alongside the append action.
			response["contents"] = map[string]any{
				"twoColumnBrowseResultsRenderer": map[string]any{"tabs": []any{
					map[string]any{
						"tabRenderer": map[string]any{"selected": true, "title": "Playlist"},
					},
				}},
			}
		}
		return response
	}
	return playlistInitialResponse(fixture, entries, id, showUnavailable)
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistInitialResponse(fixture PlaylistFixture, entries []map[string]any,
	id string, showUnavailable bool,
) map[string]any {
	title := fixture.Title
	if title == "" {
		title = "Local sync playlist " + fixture.ID
	}
	response := map[string]any{
		"metadata": map[string]any{"playlistMetadataRenderer": map[string]string{"title": title}},
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
	if fixture.ArtworkURL != "" {
		response["sidebar"] = playlistSidebar(fixture, title)
	}
	if fixture.ItemSection {
		response["contents"] = playlistSectionContents(entries, id)
	}
	if fixture.PreferenceForm {
		response["actions"] = playlistPreferences()
	}
	if fixture.HiddenVideos > 0 {
		message := fmt.Sprintf("%d unavailable videos are hidden", fixture.HiddenVideos)
		if showUnavailable {
			message = "Unavailable videos will be hidden during playback"
		}
		response["alerts"] = []map[string]any{{"alertWithButtonRenderer": map[string]any{
			"type": "INFO",
			"text": map[string]string{"simpleText": message},
		}}}
	}
	if fixture.UnknownAlert {
		response["alerts"] = []any{map[string]any{"unrecognizedAlertRenderer": map[string]any{
			"type": "WARNING",
			"text": map[string]string{"simpleText": "Some playlist items are omitted"},
		}}}
	}
	return response
}

func playlistContinuationView(token string) map[string]any {
	return map[string]any{"continuationItemViewModel": map[string]any{
		"trigger": "CONTINUATION_TRIGGER_ON_ITEM_SHOWN",
		"continuationCommand": map[string]any{"innertubeCommand": map[string]any{
			"commandMetadata": map[string]any{"webCommandMetadata": map[string]any{
				"sendPost": true, "apiUrl": "/youtubei/v1/browse",
			}},
			"continuationCommand": map[string]string{
				"token": token, "request": "CONTINUATION_REQUEST_TYPE_BROWSE",
			},
		}},
	}}
}

func validPlaylistID(id string) bool {
	return strings.HasPrefix(id, "PL") || strings.HasPrefix(id, "UU")
}

func parsePlaylistRequest(request *http.Request) (string, int, bool, error) {
	var body struct {
		BrowseID     string `json:"browseId"`
		Continuation string `json:"continuation"`
		Params       string `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(request.Body, controlBodyLimit)).Decode(&body); err != nil {
		return "", 0, false, errors.New("invalid playlist request")
	}
	id, page := strings.TrimPrefix(body.BrowseID, "VL"), 0
	showUnavailable := body.Params == "wgYCCAA="
	if body.Continuation != "" {
		parts := strings.Split(body.Continuation, ":")
		if len(parts) != continuationFieldCount {
			return "", 0, false, errors.New("invalid continuation")
		}
		id = parts[0]
		var err error
		page, err = strconv.Atoi(parts[1])
		if err != nil {
			return "", 0, false, errors.New("invalid page")
		}
		showUnavailable, err = strconv.ParseBool(parts[2])
		if err != nil {
			return "", 0, false, errors.New("invalid availability mode")
		}
	}
	if !validPlaylistID(id) {
		return "", 0, false, errors.New("invalid playlist")
	}
	return id, page, showUnavailable, nil
}

func playlistEntries(videos []PlaylistVideo, titlePrefix string, showUnavailable bool) []map[string]any {
	entries := make([]map[string]any, 0, len(videos))
	for _, video := range videos {
		if video.Hidden && !showUnavailable {
			continue
		}
		entries = append(entries, playlistEntry(video, titlePrefix))
	}
	return entries
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistEntry(video PlaylistVideo, titlePrefix string) map[string]any {
	if video.EmptyNode {
		return map[string]any{}
	}
	if video.UnknownItem {
		return map[string]any{"unrecognizedPlaylistItemRenderer": map[string]any{"videoId": video.ID}}
	}
	title := "Local video " + video.ID
	if titlePrefix != "" {
		title = titlePrefix + video.ID
	}
	if video.Title != "" {
		title = video.Title
	}
	if video.Unavailable {
		title = "[Private video]"
	}
	duration := int64(2)
	if video.DurationSeconds != nil {
		duration = *video.DurationSeconds
	}
	durationText := fmt.Sprintf("%d:%02d:%02d", duration/int64(time.Hour/time.Second),
		duration/int64(time.Minute/time.Second)%int64(time.Minute/time.Second),
		duration%int64(time.Minute/time.Second))
	if duration == 0 {
		durationText = ""
	}
	if video.Lockup {
		return playlistLockup(video, title, durationText)
	}
	renderer := map[string]any{
		"videoId":    video.ID,
		"isPlayable": !video.Unavailable,
		"title": map[string]any{
			"runs": []map[string]string{{"text": title}},
			"accessibility": map[string]any{
				"accessibilityData": map[string]string{"label": title},
			},
		},
		"lengthSeconds": strconv.FormatInt(
			duration,
			10,
		),
		"lengthText": map[string]string{"simpleText": durationText},
		"thumbnail":  map[string]any{"thumbnails": []any{}},
		"navigationEndpoint": map[string]any{
			"watchEndpoint": map[string]string{"videoId": video.ID},
		},
	}
	if duration == 0 {
		delete(
			renderer,
			"lengthSeconds",
		)
		delete(renderer, "lengthText")
	}
	if video.Malformed {
		delete(renderer, "title")
	}
	return map[string]any{"playlistVideoRenderer": renderer}
}

func playlistLockup(video PlaylistVideo, title, durationText string) map[string]any {
	metadata := map[string]any{"title": map[string]string{"content": title}}
	if video.MissingTitle {
		delete(metadata, "title")
	}
	if video.ToggleableMenu {
		metadata["menuButton"] = playlistMenu(video)
	}
	return map[string]any{"lockupViewModel": map[string]any{
		"contentId":   video.ID,
		"contentType": "LOCKUP_CONTENT_TYPE_VIDEO",
		"contentImage": map[string]any{
			"thumbnailViewModel": map[string]any{
				"image": map[string]any{"sources": []any{}},
				"overlays": []any{
					map[string]any{
						"thumbnailOverlayBadgeViewModel": map[string]any{
							"thumbnailBadges": []any{
								map[string]any{
									"thumbnailBadgeViewModel": map[string]any{
										"text":       durationText,
										"badgeStyle": "THUMBNAIL_BADGE_STYLE_DEFAULT",
									},
								},
							},
						},
					},
				},
			},
		},
		"metadata": map[string]any{"lockupMetadataViewModel": metadata},
	}}
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistMenu(video PlaylistVideo) map[string]any {
	sheet := map[string]any{
		"content": map[string]any{
			"listViewModel": map[string]any{"listItems": []any{
				map[string]any{
					"toggleableListItemViewModel": map[string]any{
						"defaultListItem": map[string]any{
							"listItemViewModel": map[string]any{
								"title": map[string]string{
									"content": "Mark as watched",
								},
							},
						},
						"toggledListItem": map[string]any{
							"listItemViewModel": map[string]any{
								"title": map[string]string{
									"content": "Mark as unwatched",
								},
							},
						},
						"initialState": map[string]bool{
							"isToggled": false,
						},
						"entityKey":          video.ID,
						"entitySelectorType": "TOGGLEABLE_LIST_ITEM_ENTITY_SELECTOR_TYPE_TOGGLE_MENU_SERVICE_ITEM_ENTITY",
					},
				},
			}},
		},
	}
	// These faults exercise tolerance of unrelated UI; they are not
	// captured provider regressions.
	if video.UnknownMenu {
		sheet["content"] = map[string]any{
			"listViewModel": map[string]any{"listItems": []any{
				map[string]any{
					"unrecognizedMenuItemViewModel": map[string]any{
						"title": map[string]string{"content": "Synthetic menu action"},
					},
				},
			}},
		}
	}
	if video.MalformedMenu {
		sheet["content"] = map[string]any{
			"listViewModel": map[string]any{
				"listItems": map[string]string{"unexpected": "object instead of array"},
			},
		}
	}
	return map[string]any{"buttonViewModel": map[string]any{
		"iconName": "MORE_VERT",
		"onTap": map[string]any{
			"innertubeCommand": map[string]any{"showSheetCommand": map[string]any{
				"panelLoadingStrategy": map[string]any{
					"inlineContent": map[string]any{"sheetViewModel": sheet},
				},
			}},
		},
	}}
}

//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func playlistSidebar(fixture PlaylistFixture, title string) map[string]any {
	return map[string]any{
		"playlistSidebarRenderer": map[string]any{"items": []any{
			map[string]any{"playlistSidebarPrimaryInfoRenderer": map[string]any{
				"stats": []any{},
				"title": map[string]string{"simpleText": title},
				"thumbnailRenderer": map[string]any{
					"playlistCustomThumbnailRenderer": map[string]any{
						"thumbnail": map[string]any{
							"thumbnails": []any{
								map[string]any{
									"url":    fixture.ArtworkURL,
									"width":  1400,
									"height": 1400,
								},
							},
						},
					},
				},
			}},
		}},
	}
}

func playlistSectionContents(entries []map[string]any, id string) map[string]any {
	// Structural fixture based on the 2026-10-22 capture: members belong
	// to ItemSection, with a sibling continuation outside that listing.
	return map[string]any{
		"twoColumnBrowseResultsRenderer": map[string]any{"tabs": []any{
			map[string]any{
				"tabRenderer": map[string]any{
					"selected": true,
					"content": map[string]any{
						"sectionListRenderer": map[string]any{"contents": []any{
							map[string]any{
								"itemSectionRenderer": map[string]any{"contents": entries},
							},
							playlistContinuationView("sibling:" + id),
						}},
					},
				},
			},
		}},
	}
}

func playlistPreferences() []any {
	// Signed-in browse responses can include an unrelated preferences popup.
	// YouTube.js 18.1.0's ToggleFormField references an undefined Text class.
	return []any{map[string]any{"openPopupAction": map[string]any{
		"popupType": "DIALOG",
		"popup": map[string]any{"formPopupRenderer": map[string]any{
			"title": map[string]string{"simpleText": "Preferences"},
			"form": map[string]any{"formRenderer": map[string]any{"fields": []any{
				map[string]any{"toggleFormFieldRenderer": map[string]any{
					"label": map[string]string{
						"simpleText": "Synthetic preference",
					},
					"toggled": false,
				}},
			}}},
			"buttons": []any{},
		}},
	}}}
}
