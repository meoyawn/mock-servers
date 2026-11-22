# YouTube Data API v3 mock

Stateful local server for YouTube publishing tests. It implements the
small API surface used by the destination flow:

- `channels.list`
- `playlists.list`
- `videos.list` and `videos.insert`
- ordered `multipart/related` video uploads
- `playlistItems.list` and `playlistItems.insert`

`channels.list(part=contentDetails)` exposes the channel uploads playlist.
The uploads playlist supports `maxResults`, opaque `pageToken` pagination, and
`videoId` filtering. `videos.list` accepts batches of up to 50 IDs and returns
snippet, duration, privacy, upload, failure, and rejection metadata used by
back-catalog matching.

It intentionally does not implement `playlists.insert`; consumers are expected
to ask the creator to make a playlist in YouTube Studio.

Run it from the repository root:

```sh
go run ./youtube-data-api-v3/cmd/youtube-mock
```

The default base URL is `http://127.0.0.1:8090`, and API requests must include
`Authorization: Bearer test-token`. Use `-empty-playlists` to exercise the
setup state where the podcaster still needs to create a playlist.

Every new episode should be added with `snippet.position: 0`. The default
playlist uses creator-provided/manual ordering, so the most recently inserted
episode remains at the top. A non-manual playlist returns
`manualSortRequired`, matching the YouTube API behavior.

Test controls do not require bearer authentication:

- `GET /__mock__/state` returns videos, playlists, top-to-bottom item order,
  uploads-playlist membership, media byte counts, notification flags, request
  counts, quota state, and configured delays.
- `POST /__mock__/reset` restores the initial fixtures.
- `POST /__mock__/fail-next` queues a one-shot operation error.
- `POST /__mock__/quota` enables immediate quota exhaustion or exhaustion after
  a deterministic number of API requests. Posting `{ "exhausted": false }`
  resets the quota request counter.
- `POST /__mock__/delay` sets a deterministic per-operation delay in
  milliseconds. Posting zero removes the delay.
- `POST /__mock__/processing-outcome` queues the processing/upload outcome for
  the next created video, including failure or rejection reasons.

Example failure payload:

```json
{
  "operation": "videos.insert",
  "status": 503,
  "reason": "backendError",
  "message": "Injected upload failure"
}
```

`contract_test.go` validates the mock's responses against
[`youtube.yaml`](youtube.yaml), an OpenAPI rendering of Google's published
YouTube Data API v3 description (via APIs.guru), licensed under
[CC BY 3.0](http://creativecommons.org/licenses/by/3.0/).
