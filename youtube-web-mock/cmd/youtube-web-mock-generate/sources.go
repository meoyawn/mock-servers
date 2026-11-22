package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
)

// source describes one single-file video served for a playlist media_fixture.
// Geometry, cadence, profile, and keyframes are the contract consumers assert;
// the pixels and tones are arbitrary lavfi patterns.
type source struct {
	name   string
	video  string
	audio  string
	frames int
	x264   []string
}

const (
	mapFlag    = "-map"
	presetFlag = "-preset"
)

var (
	highProfile     = x264("high", "veryfast", "-g", "600")
	baselineProfile = x264("baseline", "ultrafast", "-g", "600")
	// Two GOPs whose second IDR lands six seconds in.
	keyframeTail = x264("high", "veryfast",
		"-g", "1000", "-keyint_min", "1000", "-sc_threshold", "0", "-force_key_frames", "0,6")
)

func x264(profile, preset string, extra ...string) []string {
	return append([]string{"-profile:v", profile, presetFlag, preset}, extra...)
}

func tone(seconds string) string {
	return "sine=frequency=1000:sample_rate=48000:duration=" + seconds
}

func silence(seconds string) string {
	return "anullsrc=r=44100:cl=stereo:d=" + seconds
}

var sources = []source{
	{"source-video-360p-short.mp4", "testsrc2=size=640x360:rate=30", tone("0.512"), 17, highProfile},
	{"source-video-copy-720p.mp4", "color=c=black:size=1280x720:rate=25", silence("0.5"), 12, highProfile},
	{"source-video-keyframe-tail.mp4", "color=c=black:size=1280x720:rate=25", silence("8"), 200, keyframeTail},
	{"source-video-keyframe-tail-360p.mp4", "color=c=black:size=640x360:rate=5", silence("8"), 40, keyframeTail},
	{"source-video-1080p-short.mp4", "testsrc2=size=1920x1080:rate=30", tone("0.5"), 15, highProfile},
	{"source-video-4k-short.mp4", "testsrc2=size=3840x2160:rate=15", tone("0.512"), 8, baselineProfile},
	{"source-video-1440p-short.mp4", "testsrc2=size=2560x1440:rate=15", tone("0.512"), 8, baselineProfile},
	{"source-video-short.mp4", "testsrc2=size=320x180:rate=24", tone("0.25"), 6, highProfile},
	{"source-video-square-short.mp4", "testsrc2=size=360x360:rate=30", tone("0.512"), 15, baselineProfile},
	{"source-video-portrait-short.mp4", "testsrc2=size=202x360:rate=30", tone("0.512"), 15, baselineProfile},
}

func generateSources(ctx context.Context, output string) error {
	for _, source := range sources {
		arguments := []string{
			// Ending the video inside its graph lets the audio keep its own length.
			"-f", lavfiFormat, "-i", source.video + ",trim=end_frame=" + strconv.Itoa(source.frames),
			"-f", lavfiFormat, "-i", source.audio,
			mapFlag, "0:v:0", mapFlag, "1:a:0",
			"-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p",
		}
		arguments = append(arguments, source.x264...)
		arguments = append(arguments,
			"-c:a", "aac", "-ac", "2", "-map_metadata", "-1", "-movflags", "+faststart",
			filepath.Join(output, source.name),
		)
		if err := runFFmpeg(ctx, arguments); err != nil {
			return fmt.Errorf("generate %s: %w", source.name, err)
		}
	}
	return generateScreen(ctx, output)
}

// generateScreen keeps 17 frames of a 120 fps pattern at irregular indices, so
// the file reports a nominal 120 fps with a fractional 408/7 fps average, like
// a variable-rate screen capture at a nonstandard 1112x720. setts gives every
// frame two ticks and the one before the gap three, so the last frame keeps
// its duration.
func generateScreen(ctx context.Context, output string) error {
	const kept = "eq(n,0)+eq(n,2)+eq(n,4)+eq(n,6)+eq(n,8)+eq(n,10)+eq(n,13)+eq(n,15)+" +
		"eq(n,17)+eq(n,19)+eq(n,21)+eq(n,23)+eq(n,25)+eq(n,27)+eq(n,29)+eq(n,31)+eq(n,34)"
	arguments := slices.Concat([]string{
		"-f", lavfiFormat, "-i", "testsrc2=size=1112x720:rate=120:duration=1",
		"-f", lavfiFormat, "-i", "sine=frequency=330:sample_rate=48000",
		"-filter_complex",
		"[0:v]trim=end_frame=35,select='" + kept + "'[v];" +
			"[1:a]aformat=channel_layouts=stereo,atrim=end_sample=7936[a]",
		mapFlag, "[v]", mapFlag, "[a]", "-map_metadata", "-1",
		"-fps_mode", "passthrough", "-enc_time_base", "1:120", "-video_track_timescale", "19200",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
	}, x264("high", "veryfast", "-crf", "23"), []string{
		"-bsf:v", `setts=pts=PTS:dts=DTS:duration=if(eq(PTS\,10)\,3\,2)`,
		"-c:a", "aac", "-ar", "48000", "-ac", "2",
		filepath.Join(output, "screen-short.mov"),
	})
	if err := runFFmpeg(ctx, arguments); err != nil {
		return fmt.Errorf("generate screen-short.mov: %w", err)
	}
	return nil
}
