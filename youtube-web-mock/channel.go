package youtubewebmock

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const channelBodyLimit = 16 << 20

// ChannelFixture scopes channel HTML and failures to one test-owned handle.
type ChannelFixture struct {
	Handle   string `json:"handle"`
	ID       string `json:"id"`
	HTML     string `json:"html,omitempty"`
	Status   int    `json:"status,omitempty"`
	Location string `json:"location,omitempty"`
}

func (server *Server) configureChannel(writer http.ResponseWriter, request *http.Request) {
	var fixture ChannelFixture
	if err := json.NewDecoder(io.LimitReader(request.Body, channelBodyLimit)).
		Decode(&fixture); err != nil ||
		fixture.Handle == "" {
		http.Error(writer, "invalid channel fixture", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	server.channels[fixture.Handle] = fixture
	server.mu.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func (server *Server) serveChannel(writer io.Writer, request *http.Request) {
	handle, _, _ := strings.Cut(strings.TrimPrefix(request.URL.Path, "/@"), "/")
	server.recordFor(handle, request)
	server.mu.Lock()
	fixture, found := server.channels[handle]
	server.mu.Unlock()
	if !found {
		writeProxyResponse(writer, http.StatusNotFound, "channel not found")
		return
	}
	status := fixture.Status
	if status == 0 {
		status = http.StatusOK
	}
	if fixture.Location != "" {
		_, _ = fmt.Fprintf(
			writer,
			"HTTP/1.1 %d %s\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			status,
			http.StatusText(status),
			fixture.Location,
		)
		return
	}
	body := fixture.HTML
	if body == "" {
		metadata, err := json.Marshal(map[string]any{
			"contents": map[string]any{"externalId": "UCabcdefghijklmnopqrstuv"},
			"metadata": map[string]any{
				"channelMetadataRenderer": map[string]any{"externalId": fixture.ID},
			},
		})
		if err != nil {
			writeProxyResponse(writer, http.StatusInternalServerError, "encode channel")
			return
		}
		body = "<!doctype html><script>var ytInitialData = " + string(metadata) + ";</script>"
	}
	writeProxyResponse(writer, status, body)
}
