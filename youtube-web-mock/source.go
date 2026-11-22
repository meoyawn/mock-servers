package youtubewebmock

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Share the existing source corpus without copying it into the YouTube corpus.
// Only these committed media assets may escape the YouTube fixture directory.
func (server *Server) sourceFixturePath(name string) (string, error) {
	switch name {
	case "source-video-360p-short.mp4",
		"source-video-copy-720p.mp4",
		"source-video-keyframe-tail.mp4",
		"source-video-keyframe-tail-360p.mp4",
		"source-video-1080p-short.mp4",
		"source-video-4k-short.mp4",
		"source-video-1440p-short.mp4",
		"source-video-short.mp4",
		"source-video-square-short.mp4",
		"source-video-portrait-short.mp4",
		"screen-short.mov":
		return filepath.Join(filepath.Dir(server.fixtures), name), nil
	case "youtube/fallback/1080.mp4":
		return filepath.Join(server.fixtures, "fallback", "1080.mp4"), nil
	default:
		return "", fmt.Errorf("unknown source fixture %q", name)
	}
}

// Metadata is obtained from the same bytes served by the downloader. Test
// expectations remain independently authored and check the downloaded source.
//
//nolint:goconst,mnd // Independent provider response contracts keep their wire keys and fixture values literal.
func (server *Server) fixturePlayerResponse(
	ctx context.Context,
	videoID, name string,
) (map[string]any, error) {
	path, err := server.sourceFixturePath(name)
	if err != nil {
		return nil, err
	}
	// No shell; sourceFixturePath restricts path to the committed-media allowlist.
	body, err := exec.CommandContext(ctx, "ffprobe", //nolint:gosec // Only allowlisted local fixture paths reach FFprobe.
		"-v", "error", "-show_streams", "-show_format", "-of", "json", path).
		Output()
	if err != nil {
		return nil, fmt.Errorf("probe source fixture: %w", err)
	}
	var probe sourceProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, err
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid source duration %q: %w", probe.Format.Duration, err)
	}
	if duration <= 0 {
		return nil, fmt.Errorf("source duration must be positive: %q", probe.Format.Duration)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	format := map[string]any{
		"itag":             22,
		"url":              fixtureURL("source/"+name, videoID),
		"mimeType":         "video/mp4; codecs=\"avc1.640028, mp4a.40.2\"",
		"bitrate":          int64(float64(info.Size()) * 8 / duration),
		"contentLength":    strconv.FormatInt(info.Size(), 10),
		"approxDurationMs": strconv.FormatInt(int64(duration*float64(time.Second/time.Millisecond)), 10),
	}
	for _, stream := range probe.Streams {
		if err := applySourceStream(format, stream); err != nil {
			return nil, fmt.Errorf("source fixture %q: %w", name, err)
		}
	}
	if format["width"] == nil || format["audioChannels"] == nil {
		return nil, fmt.Errorf("source fixture %q needs video and audio", name)
	}
	return map[string]any{
		"playabilityStatus": map[string]string{"status": "OK"},
		"videoDetails": map[string]string{
			"videoId":       videoID,
			"title":         "Source fixture " + name,
			"author":        "Listenbox",
			"lengthSeconds": strconv.Itoa(int(math.Ceil(duration))),
		},
		"streamingData": map[string]any{"formats": []map[string]any{format}},
	}, nil
}

type sourceProbe struct {
	Streams []sourceStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type sourceStream struct {
	Type       string `json:"codec_type"`
	Codec      string `json:"codec_name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Rate       string `json:"avg_frame_rate"`
	Channels   int    `json:"channels"`
	SampleRate string `json:"sample_rate"`
}

//nolint:goconst // Independent provider response contracts keep their wire keys and fixture values literal.
func applySourceStream(format map[string]any, stream sourceStream) error {
	if stream.Type == "video" {
		return applySourceVideo(format, stream)
	}
	if stream.Type == "audio" {
		format["audioQuality"] = "AUDIO_QUALITY_MEDIUM"
		format["audioSampleRate"] = stream.SampleRate
		format["audioChannels"] = stream.Channels
	}
	return nil
}

func applySourceVideo(format map[string]any, stream sourceStream) error {
	if stream.Codec != "h264" {
		return fmt.Errorf("video codec %q must be AVC", stream.Codec)
	}
	numerator, denominator, found := strings.Cut(stream.Rate, "/")
	if !found {
		return fmt.Errorf("invalid source frame rate %q", stream.Rate)
	}
	n, err := strconv.ParseFloat(numerator, 64)
	if err != nil {
		return fmt.Errorf("invalid source frame rate numerator %q: %w", numerator, err)
	}
	d, err := strconv.ParseFloat(denominator, 64)
	if err != nil {
		return fmt.Errorf("invalid source frame rate denominator %q: %w", denominator, err)
	}
	if n <= 0 || d <= 0 {
		return fmt.Errorf("invalid source frame rate %q", stream.Rate)
	}
	format["width"], format["height"], format["fps"] = stream.Width, stream.Height, n/d
	format["qualityLabel"] = strconv.Itoa(stream.Height) + "p"
	return nil
}
