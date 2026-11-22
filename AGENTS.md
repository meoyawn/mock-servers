# Mock servers

- Each directory is a Go module at `github.com/meoyawn/mock-servers/<name>`
  listed in `go.work` and `.moon/workspace.yml`. Keep a new mock self-contained
  in its own directory with the same `moon.yml` task shape.
- Model only the provider contract a consumer depends on. Unknown operations
  fail explicitly; never forward to the real service.
- Keep state per server instance so parallel tests stay isolated. Expose test
  controls (state, faults, gates) as explicit endpoints or Go methods; never
  rely on sleeps or polling.
- Never commit credentials, captured traffic, or real account data. Fixtures
  and identifiers stay synthetic.
- `.golangci.yaml` and `.moon/tasks/go-lint.yml` are synced from the consuming
  monorepo's shared policy; change them there, not here.
- Before handing off, run `moon run <project>:check` for every affected module,
  or `moon run :check` after shared config changes.
