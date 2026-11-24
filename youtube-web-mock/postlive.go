package youtubewebmock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

// A stream that ended moments ago is in YouTube's post-live DVR state until
// its VOD is processed. Its adaptive formats carry no content length and
// serve the recording as numbered, self-initializing fMP4 segments: `sq=N`
// returns the stream's init boxes followed by fragment N, a plain byte-range
// probe answers `bytes 0-0/*`, and every response names the last segment in
// X-Head-Seqnum. A sequence past the head is 204 No Content.
const (
	postLiveSource = "yt_live_broadcast"
	mp4BoxHeader   = 8
)

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func postLivePlayerResponse(videoID string) map[string]any {
	formats := dashFormats(videoID)
	for _, format := range formats {
		format["url"] = fmt.Sprint(format["url"]) + "&source=" + postLiveSource
		format["targetDurationSec"] = 5
		format["maxDvrDurationSec"] = 43200
	}
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]any{
			"videoId":       videoID,
			"title":         "Listenbox post-live fixture",
			"lengthSeconds": "2",
			"author":        "Listenbox",
			"isLive":        false,
			"isLiveContent": true,
			"isPostLiveDvr": true,
		},
		"streamingData": map[string]any{"adaptiveFormats": formats},
	}
}

// configureLiveState records a video's live lifecycle answer: scheduled but
// offline, or ended but not yet processed. The caller holds server.mu.
func (server *Server) configureLiveState(video PlaylistVideo) {
	server.liveOfflineVideos[video.ID] = video.LiveOffline
	server.postLiveVideos[video.ID] = video.PostLive
}

func isPostLiveMedia(request *http.Request) bool {
	return request.URL.Query().Get("source") == postLiveSource
}

func (server *Server) servePostLiveMedia(writer io.Writer, request *http.Request, file *os.File) {
	init, fragments, err := liveSegments(file)
	if err != nil {
		writeProxyResponse(writer, http.StatusInternalServerError, err.Error())
		return
	}
	head := len(fragments) - 1
	sequence := request.URL.Query().Get("sq")
	if sequence == "" {
		// YouTube answers a byte-range probe of an unfinished recording with
		// one byte of an unknown total.
		writeLiveResponse(writer, http.StatusPartialContent, head, "Content-Range: bytes 0-0/*\r\n", init[:1])
		return
	}
	number, err := strconv.Atoi(sequence)
	if err != nil || number < 0 {
		writeProxyResponse(writer, http.StatusBadRequest, "invalid sequence number")
		return
	}
	if number > head {
		writeLiveResponse(writer, http.StatusNoContent, head, "", nil)
		return
	}
	writeLiveResponse(writer, http.StatusOK, head, "", append(append([]byte{}, init...), fragments[number]...))
}

func writeLiveResponse(writer io.Writer, status, head int, extra string, body []byte) {
	_, _ = fmt.Fprintf(
		writer,
		"HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Type: video/mp4\r\nContent-Length: %d\r\nX-Head-Seqnum: %d\r\n%s\r\n",
		status,
		http.StatusText(status),
		len(body),
		head,
		extra,
	)
	_, _ = writer.Write(body)
}

// liveSegments splits a fragmented MP4 fixture into its init boxes and one
// moof+mdat payload per fragment, dropping the VOD-only sidx index.
func liveSegments(file *os.File) ([]byte, [][]byte, error) {
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	var init []byte
	var fragments [][]byte
	for offset := 0; offset < len(data); {
		if len(data)-offset < mp4BoxHeader {
			return nil, nil, errors.New("truncated MP4 box")
		}
		size := int(binary.BigEndian.Uint32(data[offset:]))
		if size < mp4BoxHeader || size > len(data)-offset {
			return nil, nil, errors.New("invalid MP4 box size")
		}
		box := data[offset : offset+size]
		switch string(box[4:mp4BoxHeader]) {
		case "ftyp", "moov":
			init = append(init, box...)
		case "moof":
			fragments = append(fragments, append([]byte{}, box...))
		case "mdat":
			if len(fragments) == 0 {
				return nil, nil, errors.New("mdat precedes moof")
			}
			fragments[len(fragments)-1] = append(fragments[len(fragments)-1], box...)
		}
		offset += size
	}
	if len(init) == 0 || len(fragments) == 0 {
		return nil, nil, errors.New("fixture is not a fragmented MP4")
	}
	return init, fragments, nil
}
