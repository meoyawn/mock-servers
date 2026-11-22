package youtubewebmock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (server *Server) serveEmbedPage(writer io.Writer, request *http.Request) {
	video := strings.TrimPrefix(request.URL.Path, "/embed/")
	server.recordFor(video, request)
	flags := ""
	if request.Header.Get("Referer") == "https://www.google.com/" {
		flags = "fixture-embed:" + video
	}
	config := map[string]any{
		"INNERTUBE_CONTEXT": map[string]any{
			"client": map[string]any{
				"clientName":    "WEB_EMBEDDED_PLAYER",
				"clientVersion": "2.20261002.01.00",
				"visitorData":   anonymousVisitor,
				"userAgent":     request.Header.Get("User-Agent"),
			},
			"user": map[string]any{"lockedSafetyMode": false},
			"thirdParty": map[string]any{
				"embeddedPlayerContext": map[string]any{
					"embeddedPlayerEncryptedContext": "fixture-context:" + video,
				},
			},
		},
		"WEB_PLAYER_CONTEXT_CONFIGS": map[string]any{
			"WEB_PLAYER_CONTEXT_CONFIG_ID_EMBEDDED_PLAYER": map[string]any{
				"encryptedHostFlags": flags,
			},
		},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		panic(fmt.Errorf("encode embedded player fixture: %w", err))
	}
	writeProxyResponse(
		writer,
		http.StatusOK,
		`<!doctype html><script>ytcfg.set(`+string(
			encoded,
		)+`);</script><script src="/s/player/deadbeef/player_ias.vflset/en_US/base.js"></script>`,
	)
}
