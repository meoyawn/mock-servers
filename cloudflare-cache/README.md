# cloudflare-cache

`cloudflare-cache` is a small local emulator of a Cloudflare-cached zone for
deterministic end-to-end tests and local development. It sits in front of an
origin, caches `GET`/`HEAD` responses like an edge would, and implements the
`POST /client/v4/zones/{zone_id}/purge_cache` endpoint so code that purges by
URL can be exercised without a Cloudflare account.

It has no external dependencies and never polls: concurrent misses collapse
into one origin fetch, and purge waiters wake on the exact purge they wait for.

## Behavior

- Requests under a zone's public prefix (default `/i/`) are proxied to
  `{origin}{origin_prefix}{suffix}` (origin prefix default `/i/`), keeping the
  query string. An origin prefix of `/` maps `{public_prefix}{suffix}` to
  `{origin}/{suffix}`.
- `2xx` and `301` origin responses are cached for 10 minutes per zone and path
  (the query string is not part of the cache key). Responses carry
  `Cf-Cache-Status: HIT|MISS` and `Age`.
- Redirects from the origin are returned, not followed. Origin bodies over
  16 MiB yield `502`.
- Purge requires `Authorization: Bearer {api_token}` and a JSON body
  `{"files": ["https://host/path", ...]}` with 1–100 URLs; it answers with the
  Cloudflare v4 envelope (`success`, `errors`, `messages`, `result`).

## Running

```sh
go run ./cmd/cloudflare-cache
```

| Variable                     | Default | Meaning                                   |
| ---------------------------- | ------- | ----------------------------------------- |
| `CLOUDFLARE_CACHE_ADDR`      | `:8084` | Listen address                            |
| `CLOUDFLARE_CACHE_ZONE_ID`   |         | Zone registered at startup                |
| `CLOUDFLARE_CACHE_API_TOKEN` |         | Purge token for that zone                 |
| `CLOUDFLARE_CACHE_ORIGIN`    |         | Origin base URL (`http` or `https`)       |
| `CLOUDFLARE_CACHE_PUBLIC_PREFIX` | `/i/` | Public path prefix for that zone        |
| `CLOUDFLARE_CACHE_ORIGIN_PREFIX` | `/i/` | Origin path prefix for that zone        |

The three zone variables are all-or-nothing (the prefixes are optional); leave them unset to register
zones through the control API instead. `cloudflare-cache healthcheck` probes
`/healthz` for container health checks. A `Dockerfile` builds a static
`scratch` image.

## Control API

Test-only endpoints for setting up zones and asserting on cache state:

| Endpoint                                                   | Purpose                                                                    |
| ---------------------------------------------------------- | -------------------------------------------------------------------------- |
| `POST /__test__/zones`                                     | Register `{zone_id, api_token, origin, public_prefix?, origin_prefix?}`; returns `public_base_url` |
| `GET /__test__/cache?zone_id=`                             | List cached entries with `stored_at`/`expires_at`                          |
| `DELETE /__test__/cache?zone_id=`                          | Clear a zone's cache                                                       |
| `POST /__test__/faults`                                    | Queue `{zone_id, operation: "purge"\|"origin", path?, status, body?, count?, retry_after?}` |
| `GET /__test__/requests?zone_id=&path=&wait_after=`        | Purge request count for a path; with `wait_after=N`, block until purge N+1 is acknowledged |

The same handler is available in-process for Go tests:

```go
handler := cloudflarecache.NewHandler()
err := handler.RegisterZone(cloudflarecache.ZoneRegistration{
	ZoneID:   "test-zone",
	APIToken: "test-token",
	Origin:   origin.URL,
})
```

## License

MIT
