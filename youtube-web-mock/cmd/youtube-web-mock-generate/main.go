// Command youtube-web-mock-generate explicitly regenerates the deliberately
// small YouTube-media corpus used by the integrated E2E environment.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	durationSeconds        = "2"
	fixtureDirectoryMode   = 0o755
	fixtureFileMode        = 0o644
	video720Size           = "1280x720"
	video1080Size          = "1920x1080"
	lavfiFormat            = "lavfi"
	topologyRenditionParts = 2
)

type manifest struct {
	FFmpegVersion string          `json:"ffmpeg_version"`
	Fixtures      []fixtureRecord `json:"fixtures"`
}

// fixtureRecord is intentionally complete enough for E2E to independently
// verify every checked-in byte without re-running the generator.
type fixtureRecord struct {
	Topology    string          `json:"topology"`
	Path        string          `json:"path"`
	Arguments   []string        `json:"arguments"`
	SHA256      string          `json:"sha256"`
	ByteLength  int64           `json:"byte_length"`
	ContentType string          `json:"content_type"`
	Probe       json.RawMessage `json:"probe,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	output := flag.String("output", "", "fixture output directory")
	flag.Parse()
	if *output == "" {
		fatal(errors.New("--output is required"))
	}
	if err := os.RemoveAll(*output); err != nil {
		fatal(fmt.Errorf("clear fixture output: %w", err))
	}
	if err := os.MkdirAll(*output, fixtureDirectoryMode); err != nil {
		fatal(err)
	}
	version, err := exec.CommandContext(ctx, "ffmpeg", "-version").Output()
	if err != nil {
		fatal(fmt.Errorf("read ffmpeg version: %w", err))
	}
	commands := make(map[string][]string)
	if err := generateNativeHLS(ctx, *output, commands); err != nil {
		fatal(err)
	}
	if err := generateDASHRanges(ctx, *output, commands); err != nil {
		fatal(err)
	}
	if err := generateFallback(ctx, *output, commands); err != nil {
		fatal(err)
	}
	if err := generateOpus(ctx, *output, commands); err != nil {
		fatal(err)
	}
	records, err := recordsFor(ctx, *output, commands)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(
		manifest{FFmpegVersion: strings.TrimSpace(string(version)), Fixtures: records},
		"",
		"  ",
	)
	if err != nil {
		fatal(err)
	}
	// Public provider media fixtures and their manifest contain no credentials.
	if err := os.WriteFile(
		filepath.Join(*output, "manifest.json"),
		append(encoded, '\n'),
		fixtureFileMode,
	); err != nil {
		fatal(err)
	}
}

func generateNativeHLS(ctx context.Context, output string, commands map[string][]string) error {
	for _, rendition := range []struct{ name, size string }{{"720", video720Size}, {"1080", video1080Size}} {
		directory := filepath.Join(output, "native-hls", rendition.name)
		if err := os.MkdirAll(directory, fixtureDirectoryMode); err != nil {
			return err
		}
		playlist := filepath.Join(directory, "index.m3u8")
		arguments := append(
			lavfiArguments(rendition.size),
			"-map",
			"0:v:0",
			"-map",
			"1:a:0",
			"-c:v",
			"libx264",
			"-threads",
			"1",
			"-g",
			"30",
			"-keyint_min",
			"30",
			"-sc_threshold",
			"0",
			"-pix_fmt",
			"yuv420p",
			"-c:a",
			"aac",
			"-profile:a",
			"aac_low",
			"-ar",
			"48000",
			"-ac",
			"2",
			"-f",
			"hls",
			"-hls_time",
			"1",
			"-hls_playlist_type",
			"vod",
			"-hls_segment_type",
			"fmp4",
			"-hls_fmp4_init_filename",
			"init.mp4",
			"-hls_segment_filename",
			filepath.Join(directory, "segment-%03d.m4s"),
			playlist,
		)
		if err := runFFmpeg(ctx, arguments); err != nil {
			return fmt.Errorf("generate native HLS %sp: %w", rendition.name, err)
		}
		commands["native-hls/"+rendition.name+"/index.m3u8"] = arguments
	}
	return generateHLSMaster(output, commands)
}

func generateHLSMaster(output string, commands map[string][]string) error {
	master := "#EXTM3U\n#EXT-X-VERSION:7\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=1400000,RESOLUTION=1280x720,CODECS=\"avc1.64001F,mp4a.40.2\"\n" +
		"720/index.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\"\n" +
		"1080/index.m3u8\n"
	// Public provider media fixtures and their manifest contain no credentials.
	if err := os.WriteFile(
		filepath.Join(output, "native-hls", "master.m3u8"),
		[]byte(master),
		fixtureFileMode,
	); err != nil {
		return err
	}
	commands["native-hls/master.m3u8"] = []string{
		"generated HLS master from two deterministic muxed variants",
	}
	return nil
}

func generateDASHRanges(ctx context.Context, output string, commands map[string][]string) error {
	for _, rendition := range []struct{ name, size string }{{"720", video720Size}, {"1080", video1080Size}} {
		directory := filepath.Join(output, "dash-range", rendition.name)
		if err := os.MkdirAll(directory, fixtureDirectoryMode); err != nil {
			return err
		}
		manifestPath := filepath.Join(directory, "manifest.mpd")
		arguments := append(
			lavfiArguments(rendition.size),
			"-map",
			"0:v:0",
			"-map",
			"1:a:0",
			"-c:v",
			"libx264",
			"-threads",
			"1",
			"-g",
			"30",
			"-keyint_min",
			"30",
			"-sc_threshold",
			"0",
			"-pix_fmt",
			"yuv420p",
			"-c:a",
			"aac",
			"-profile:a",
			"aac_low",
			"-ar",
			"48000",
			"-ac",
			"2",
			"-f",
			"dash",
			"-seg_duration",
			"1",
			"-use_template",
			"1",
			"-use_timeline",
			"1",
			"-single_file",
			"1",
			manifestPath,
		)
		if err := runFFmpeg(ctx, arguments); err != nil {
			return fmt.Errorf("generate DASH ranges %sp: %w", rendition.name, err)
		}
		commands["dash-range/"+rendition.name+"/manifest.mpd"] = arguments
	}
	return nil
}

func generateFallback(ctx context.Context, output string, commands map[string][]string) error {
	for _, rendition := range []struct{ name, size string }{{"720", video720Size}, {"1080", video1080Size}} {
		path := filepath.Join(output, "fallback", rendition.name+".mp4")
		if err := os.MkdirAll(filepath.Dir(path), fixtureDirectoryMode); err != nil {
			return err
		}
		arguments := append(
			lavfiArguments(rendition.size),
			"-map",
			"0:v:0",
			"-map",
			"1:a:0",
			"-c:v",
			"libx264",
			"-threads",
			"1",
			"-g",
			"30",
			"-keyint_min",
			"30",
			"-sc_threshold",
			"0",
			"-pix_fmt",
			"yuv420p",
			"-c:a",
			"aac",
			"-profile:a",
			"aac_low",
			"-ar",
			"48000",
			"-ac",
			"2",
			"-movflags",
			"+faststart",
			path,
		)
		if err := runFFmpeg(ctx, arguments); err != nil {
			return fmt.Errorf("generate fallback %sp MP4: %w", rendition.name, err)
		}
		commands["fallback/"+rendition.name+".mp4"] = arguments
	}
	return nil
}

func generateOpus(ctx context.Context, output string, commands map[string][]string) error {
	relative := "audio/opus.webm"
	path := filepath.Join(output, relative)
	if err := os.MkdirAll(filepath.Dir(path), fixtureDirectoryMode); err != nil {
		return err
	}
	arguments := []string{
		"-hide_banner", "-loglevel", "error", "-y", "-f", lavfiFormat,
		"-i", "sine=frequency=1000:sample_rate=48000", "-t", durationSeconds,
		"-c:a", "libopus", "-b:a", "128k", "-ac", "2", "-fflags", "+bitexact", path,
	}
	if err := runFFmpeg(ctx, arguments); err != nil {
		return fmt.Errorf("generate Opus audio: %w", err)
	}
	commands[relative] = arguments
	return nil
}

func lavfiArguments(size string) []string {
	return []string{
		"-hide_banner",
		"-loglevel",
		"error",
		"-y",
		"-f",
		lavfiFormat,
		"-i",
		"testsrc2=size=" + size + ":rate=30",
		"-f",
		lavfiFormat,
		"-i",
		"sine=frequency=1000:sample_rate=48000",
		"-t",
		durationSeconds,
	}
}

func runFFmpeg(ctx context.Context, arguments []string) error {
	output, err := exec.CommandContext(ctx, "ffmpeg", arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func recordsFor(ctx context.Context, directory string, commands map[string][]string) ([]fixtureRecord, error) {
	var paths []string
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(path) == "manifest.json" {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	records := make([]fixtureRecord, 0, len(paths))
	for _, relative := range paths {
		path := filepath.Join(directory, filepath.FromSlash(relative))
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		digest, err := shaFile(path)
		if err != nil {
			return nil, err
		}
		topology, _, _ := strings.Cut(relative, "/")
		if strings.HasPrefix(relative, "fallback/") {
			topology = "fallback"
		}
		record := fixtureRecord{
			Topology:    topology,
			Path:        relative,
			Arguments:   commands[commandKey(relative)],
			SHA256:      digest,
			ByteLength:  info.Size(),
			ContentType: contentType(relative),
		}
		if probesAsContainer(relative) {
			probe, err := probe(ctx, path)
			if err != nil {
				return nil, err
			}
			record.Probe = probe
		}
		records = append(records, record)
	}
	return records, nil
}

func commandKey(relative string) string {
	if strings.HasSuffix(relative, "/index.m3u8") || strings.HasSuffix(relative, "/manifest.mpd") ||
		relative == "native-hls/master.m3u8" ||
		strings.HasPrefix(relative, "fallback/") {
		return relative
	}
	parts := strings.Split(relative, "/")
	if len(parts) >= topologyRenditionParts {
		if parts[0] == "native-hls" {
			return strings.Join(parts[:topologyRenditionParts], "/") + "/index.m3u8"
		}
		if parts[0] == "dash-range" {
			return strings.Join(parts[:topologyRenditionParts], "/") + "/manifest.mpd"
		}
	}
	return relative
}

func contentType(path string) string {
	switch filepath.Ext(path) {
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

func probesAsContainer(path string) bool {
	return strings.HasPrefix(path, "audio/") || strings.HasPrefix(path, "fallback/") ||
		path == "native-hls/master.m3u8" ||
		strings.HasSuffix(path, "/manifest-stream0.mp4") ||
		strings.HasSuffix(path, "/manifest-stream1.mp4")
}

func probe(ctx context.Context, path string) (json.RawMessage, error) {
	output, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_format", "-show_streams", "-of", "json", path).
		Output()
	if err != nil {
		return nil, fmt.Errorf("probe %q: %w", path, err)
	}
	var value json.RawMessage
	if err := json.Unmarshal(output, &value); err != nil {
		return nil, fmt.Errorf("decode probe %q: %w", path, err)
	}
	return value, nil
}

func shaFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

func fatal(err error) { _, _ = fmt.Fprintln(os.Stderr, err); os.Exit(1) }
