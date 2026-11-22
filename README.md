# Mock servers

Deterministic local stand-ins for third-party services, built for fast
integrated end-to-end tests. Each one implements only the narrow contract a
real product depends on, keeps per-instance state, and fails explicitly on
anything it does not model. None of them ever contacts the real provider.

| Module                                       | Stands in for                                                      |
| -------------------------------------------- | ------------------------------------------------------------------ |
| [`cloudflare-cache`](cloudflare-cache/)       | Cloudflare edge cache and the zone `purge_cache` API               |
| [`google-oauth-mock`](google-oauth-mock/)     | Google OAuth 2.0 authorize, token, revoke, and userinfo endpoints   |
| [`stripe-mock`](stripe-mock/)                 | Stripe Customers, Subscriptions, Checkout, Portal, and webhooks    |
| [`turnstile-mock`](turnstile-mock/)           | Cloudflare Turnstile widget script and `siteverify`                |
| [`youtube-data-api-v3`](youtube-data-api-v3/) | YouTube Data API v3 publishing surface, checked against its spec   |
| [`youtube-web-mock`](youtube-web-mock/)       | YouTube web/InnerTube traffic through a non-forwarding HTTPS proxy |

Each directory is an independent Go module at
`github.com/meoyawn/mock-servers/<name>`, connected through `go.work`. Use a
mock in-process from Go tests through its package constructor (`New` or
`NewHandler`), or run the ones with a `cmd/` entrypoint as a standalone server:

```sh
go run ./cloudflare-cache/cmd/cloudflare-cache
go run ./turnstile-mock/cmd/turnstile-mock
```

Module READMEs describe each contract and its test-control endpoints.

## Development

[Mise](https://mise.jdx.dev/) pins Go, [FFmpeg](https://ffmpeg.org/),
[ShellCheck](https://www.shellcheck.net/), and [Moon](https://moonrepo.dev/);
golangci-lint is a Go tool in each module.

```sh
mise install --locked
moon run :check             # test and lint every module
moon run turnstile-mock:check
```

All modules share [`.golangci.yaml`](.golangci.yaml). Fixtures are synthetic:
never commit real account data, credentials, or captured traffic. Media is
generated, never committed: see [`youtube-web-mock`](youtube-web-mock/).

## License

[MIT](LICENSE)
