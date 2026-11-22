package youtubewebmock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FixtureManifest is the checked-in result of the explicit maintainer FFmpeg
// generation task. Runtime code only verifies it; it never regenerates it.
type FixtureManifest struct {
	FFmpegVersion string          `json:"ffmpeg_version"`
	Fixtures      []FixtureRecord `json:"fixtures"`
}

type FixtureRecord struct {
	Topology    string          `json:"topology"`
	Path        string          `json:"path"`
	Arguments   []string        `json:"arguments"`
	SHA256      string          `json:"sha256"`
	ByteLength  int64           `json:"byte_length"`
	ContentType string          `json:"content_type"`
	Probe       json.RawMessage `json:"probe,omitempty"`
}

// VerifyFixtures proves the committed corpus was neither omitted nor modified
// and that its playable containers still satisfy FFprobe. The E2E supervisor
// calls this before it launches the non-forwarding CONNECT proxy.
func VerifyFixtures(ctx context.Context, directory string) error {
	manifestPath := filepath.Join(directory, "manifest.json")
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read YouTube fixture manifest: %w", err)
	}
	var manifest FixtureManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return fmt.Errorf("decode YouTube fixture manifest: %w", err)
	}
	if strings.TrimSpace(manifest.FFmpegVersion) == "" || len(manifest.Fixtures) == 0 {
		return errors.New("YouTube fixture manifest is incomplete")
	}
	for _, record := range manifest.Fixtures {
		if err := verifyFixtureRecord(ctx, directory, record); err != nil {
			return err
		}
	}
	return nil
}

func verifyFixtureRecord(ctx context.Context, directory string, record FixtureRecord) error {
	if record.Path == "" || !filepath.IsLocal(filepath.FromSlash(record.Path)) ||
		record.ByteLength <= 0 ||
		record.SHA256 == "" ||
		record.ContentType == "" {
		return errors.New("YouTube fixture manifest contains an invalid record")
	}
	path := filepath.Join(directory, filepath.FromSlash(record.Path))
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read YouTube fixture %q: %w", record.Path, err)
	}
	if int64(len(content)) != record.ByteLength {
		return fmt.Errorf("YouTube fixture %q length differs from manifest", record.Path)
	}
	digest := sha256.Sum256(content)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), record.SHA256) {
		return fmt.Errorf("YouTube fixture %q checksum differs from manifest", record.Path)
	}
	if len(record.Probe) == 0 {
		return nil
	}
	return verifyFixtureProbe(ctx, path, record)
}

func verifyFixtureProbe(ctx context.Context, path string, record FixtureRecord) error {
	output, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_format", "-show_streams", "-of", "json", path).
		Output()
	if err != nil {
		return fmt.Errorf("probe YouTube fixture %q: %w", record.Path, err)
	}
	if !json.Valid(output) || !json.Valid(record.Probe) {
		return fmt.Errorf("YouTube fixture %q has invalid probe data", record.Path)
	}
	var actual, expected probeIdentity
	if err := json.Unmarshal(output, &actual); err != nil {
		return fmt.Errorf("decode actual YouTube fixture probe %q: %w", record.Path, err)
	}
	if err := json.Unmarshal(record.Probe, &expected); err != nil {
		return fmt.Errorf("decode recorded YouTube fixture probe %q: %w", record.Path, err)
	}
	if !actual.Playable() || !expected.Playable() ||
		actual.StreamIdentity() != expected.StreamIdentity() {
		return fmt.Errorf("YouTube fixture %q probe facts differ from manifest", record.Path)
	}
	return nil
}

type probeIdentity struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Profile   string `json:"profile"`
		Width     int64  `json:"width"`
		Height    int64  `json:"height"`
	} `json:"streams"`
}

func (probe probeIdentity) Playable() bool { return len(probe.Streams) > 0 }

func (probe probeIdentity) StreamIdentity() string {
	parts := make([]string, 0, len(probe.Streams))
	for _, stream := range probe.Streams {
		parts = append(
			parts,
			fmt.Sprintf(
				"%s:%s:%s:%dx%d",
				stream.CodecType,
				stream.CodecName,
				stream.Profile,
				stream.Width,
				stream.Height,
			),
		)
	}
	return strings.Join(parts, ",")
}
