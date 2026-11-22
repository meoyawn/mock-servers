# youtube-web-mock

A non-forwarding HTTPS `CONNECT` proxy that answers YouTube web, InnerTube, and
`googlevideo.com` media traffic from local state. Clients trust the CA it
writes to `CAFile`; every other host is refused.

## Media corpus

All media is synthetic FFmpeg `lavfi` output, generated rather than committed:

```sh
moon run youtube-web-mock:generate-fixtures
```

The task writes `fixtures/` (Git-ignored): single-file source videos at its top
level and YouTube stream topologies (native HLS, DASH byte ranges, progressive
fallbacks, Opus audio, `manifest.json`) under `fixtures/youtube`. Each source
keeps the geometry, cadence, codec profile, and keyframe layout that consumers
assert; the pixels and tones are arbitrary. Moon caches the output, shared
across Git worktrees, so it re-encodes only when the generator changes. `test`
depends on it.

At startup the mock checks every manifest entry's SHA-256 and probes it with
`ffprobe`, so a stale or partial corpus fails fast.

## Running

In-process from Go:

```go
mock, err := youtubewebmock.Start(ctx, youtubewebmock.Config{
	Address:          "127.0.0.1:0",
	CAFile:           caPath,
	FixtureDirectory: "path/to/youtube-web-mock/fixtures/youtube",
})
```

Or as a container. The image carries only the binary and `ffprobe`; mount the
generated corpus read-only and a writable directory for the CA:

```sh
docker build -t youtube-web-mock .
docker run --rm -p 3128:3128 \
  -v "$PWD/fixtures:/fixtures:ro" -v "$PWD/.cache/ca:/ca" youtube-web-mock
curl --proxy http://127.0.0.1:3128 --cacert .cache/ca/youtube-web-mock-ca.pem \
  'https://r1.googlevideo.com/videoplayback?fixture=demo--native-hls%2Fmaster.m3u8'
```

| Variable                             | Image default                      |
| ------------------------------------ | ---------------------------------- |
| `YOUTUBE_WEB_MOCK_ADDRESS`           | `0.0.0.0:3128`                     |
| `YOUTUBE_WEB_MOCK_CA_FILE`           | `/ca/youtube-web-mock-ca.pem`      |
| `YOUTUBE_WEB_MOCK_FIXTURE_DIRECTORY` | `/fixtures/youtube`                |

Test controls (`/__mock__/playlist`, `channel`, `gate`, `range-failure`,
`media-window`, `requests`) are plain HTTP on the same port.
