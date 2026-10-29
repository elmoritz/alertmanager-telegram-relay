# alertmanager-telegram-relay

A small Go service between Prometheus Alertmanager and Telegram: it receives
Alertmanager webhooks, posts readable alert messages, edits them in place when
alerts resolve, and offers inline buttons to silence alerts.

**Read before changing anything:**

1. `SECURITY.md`: the threat model and the rules. They are binding. If a task
   seems to need breaking one, stop and ask instead.
2. `docs/SPEC.md`: what the relay does. It is the source of truth; update it in
   the same change as the behaviour. Pick up work at the first unfinished
   milestone in SPEC §13, including the open issues in the GitHub milestone
   of the same name.

## This repository is public

Never write real infrastructure details into code, tests, docs, commit
messages, issues or PRs: no real hostnames, domains, IPs, Tailscale names, chat
or user IDs, alert texts. Use `example.com`, `alertmanager:9093`,
`-1001234567890`. Fixtures are hand-written. Deployment config for a specific
installation belongs in that installation's own repository.

## Conventions

- Go, current stable. Standard library first; a new dependency needs a reason
  (SPEC §2 lists the planned ones).
- Layout: `cmd/relay/` (main), `internal/…` per concern (`config`, `webhook`,
  `telegram`, `alertmanager`, `render`, `store`, `incident`).
- `log/slog`, JSON. Errors wrapped with context; never log secrets or full
  request bodies (SECURITY.md §1).
- All Telegram calls go through `internal/telegram`; it is the only place that
  knows the token and it redacts it from every error.
- Tests use `httptest` fakes for Telegram and Alertmanager, never the real APIs.
  Rendering is tested with golden files (`go test ./... -update` rewrites them).

## Commands

```bash
go build ./...
go vet ./... && staticcheck ./...
go test -race ./...
govulncheck ./...
docker build -t alertmanager-telegram-relay:dev .
```

A change is done when all of the above pass and SPEC.md matches the behaviour.
A milestone is done only after the live check in `docs/LIVE-TESTING.md`
(`go test -tags=live ./...` against a throwaway bot, plus the manual
checklist), which the maintainer runs; also `go vet -tags=live ./...` and
`staticcheck -tags=live ./...` so the live tests keep compiling.
