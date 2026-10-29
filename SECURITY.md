# Security

This relay holds a Telegram bot token, can create and expire silences in
Alertmanager, and reads every alert a monitoring stack produces. A mistake here
means a stranger can mute someone's alerts, read their infrastructure's internals
from alert text, or take over their bot. These rules are therefore not
suggestions: code that breaks one does not get merged, whoever (or whatever)
wrote it.

## Reporting a vulnerability

Please do **not** open a public issue. Use GitHub's private vulnerability
reporting: the repository's **Security** tab → **Report a vulnerability**.
Expect a first answer within a week. Fixes for confirmed issues are released
before the details are published.

## Threat model

| Actor | Can reach | What they must not achieve |
|---|---|---|
| Anyone on Telegram | the bot (bots are public and searchable) | read alerts, press buttons, learn which chats the bot serves, make the bot send anything |
| A member of an allowed group chat who is not an allowed user | button presses in that chat | silence or unsilence anything |
| Anything on the network that can reach the webhook port | `POST /webhook` | inject fake alerts or fake resolutions without the shared secret |
| Whoever controls alert content (label values, annotations: often derived from scraped targets) | text that ends up in messages | inject HTML, links or buttons; break the message; hit the length limit to suppress an alert |
| Whoever reads logs, metrics or crash output | log lines, `/metrics`, panics | obtain the bot token, the webhook secret, Alertmanager credentials or chat IDs |
| A compromised dependency or CI step | build and release | ship a modified image under this project's name |

Out of scope: an attacker who already controls the host the relay runs on, or
the Telegram account that owns the bot.

## Rules

### 1. Secrets

- Secrets come only from environment variables or files (`*_FILE` variants,
  e.g. `TELEGRAM_BOT_TOKEN_FILE`), never from flags (visible in `ps`) and never
  from the repository.
- **The bot token is part of every Telegram API URL** (`/bot<token>/method`). Go's
  `net/http` puts the full URL into `*url.Error`, so a plain `log.Println(err)`
  after a failed request leaks the token. All Telegram calls go through one
  client that strips the token from every error before returning it, with a
  test that forces a network error and asserts the token is absent from the
  error string.
- Never log: request bodies or headers from Telegram or Alertmanager, the full
  config, or environment. Log chat IDs and user IDs only when debug logging is
  explicitly on, never at the default level.
- Secrets are compared with `crypto/subtle.ConstantTimeCompare`.
- Config structs that hold secrets implement `String()`/`LogValue()` returning a
  redacted form, so an accidental `%v` cannot print them.

### 2. Telegram: who may talk to the bot

- An explicit allowlist of chat IDs (`TELEGRAM_CHAT_IDS`) and of user IDs allowed
  to act (`TELEGRAM_ADMIN_USER_IDS`) is **required** at startup. Empty means the
  relay refuses to start; there is no "allow everyone" mode.
- Every update is checked before anything else happens: for a callback query,
  both the chat the message lives in AND the user who pressed the button must be
  allowed. A group chat being allowed does not make all its members admins.
  The same holds for a reaction that the relay acts on: allowed chat AND
  allowed user, on a message of the relay's own; anonymous reactions (made on
  behalf of a chat, with no user) are ignored.
- Updates from anywhere else are dropped silently: no reply, no error message,
  no `answerCallbackQuery` text that reveals anything. They are counted in a
  metric (`relay_telegram_updates_rejected_total`) without the IDs.
- The relay only acts on messages it sent itself: a callback must reference a
  message ID the relay has in its store.
- Updates are fetched by long polling (`getUpdates`). The relay never exposes an
  inbound endpoint to the internet and never calls `setWebhook`.

### 3. Buttons and silences

- `callback_data` carries only an opaque random ID (≥ 64 bits from
  `crypto/rand`) that maps to an action stored server-side. It never carries
  label values, matchers, durations or anything else the server would trust.
- Silence matchers are built server-side from the stored group labels of the
  notification the button belongs to. All matchers are exact equality
  (`isRegex: false`, `isEqual: true`).
- **Never create a silence with zero matchers**, and never one built only from
  `alertname` when the group had more labels. If a group has no group labels,
  the silence buttons are not offered.
- Durations come from a fixed server-side list (`SILENCE_DURATIONS`, max 7 days).
- Every silence records who made it: `createdBy: "telegram:<user id>"` and a
  comment naming the relay.
- Action IDs expire (default 7 days) and are single-use where that makes sense
  (unsilence).
- Every state-changing action is logged at info level with the action, the
  silence ID and the user ID.

### 4. Webhook endpoint (Alertmanager → relay)

- Requires a shared secret: `Authorization: Bearer <secret>` (Alertmanager's
  `http_config.authorization.credentials_file`). No secret configured → refuse
  to start.
- Listens on `127.0.0.1` by default. Binding elsewhere is an explicit choice
  (e.g. a container network), never `0.0.0.0` by default.
- `POST` only, `Content-Type: application/json` only, body capped (default 1 MiB)
  with `http.MaxBytesReader`, server read/write/idle timeouts set, unknown JSON
  fields tolerated but nothing beyond the documented payload is used.
- `/healthz` and `/metrics` carry no secrets, chat IDs, label values or alert
  text. Metrics labels are bounded (no alert names or label values as metric
  labels).

### 5. Rendering alert content

- Telegram's HTML mode is a parser: every value from an alert (labels,
  annotations, URLs) is escaped with `html/template` contextual escaping. The
  only HTML in a message comes from the relay's own templates.
- Links (Grafana, runbook, generator URLs) are rendered only if their scheme is
  `http` or `https`; anything else is dropped.
- Messages are kept under Telegram's 4096-character limit by construction
  (truncate alert lists and descriptions before sending, then measure the
  result). An over-long message must degrade to a shorter message, never to no
  message.

### 6. Alertmanager API (relay → Alertmanager)

- Credentials for the Alertmanager API (basic or bearer) are optional and
  handled like every other secret.
- Use HTTPS with certificate verification wherever Alertmanager is reached over
  an untrusted network. There is no `insecure_skip_verify` option.

### 7. Runtime and supply chain

- Image: static Go binary on a distroless `nonroot` base, pinned by digest. Runs
  as a non-root user, read-only root filesystem, no shell. Only the state
  directory is writable.
- Dependencies: as few as possible; every new one needs a reason in the PR.
  `go.sum` is committed; `govulncheck` runs in CI.
- GitHub Actions: third-party actions pinned by commit SHA, workflow
  `permissions:` set to the minimum per job, no `pull_request_target` with
  checkout of PR code.
- Releases: images published to GHCR from CI only, tagged by semver, with
  provenance/SBOM attestations. Never push images from a workstation.
- Secret scanning (gitleaks) runs on every push and PR, plus a check that fails
  on private-looking infrastructure: Tailscale names (`*.ts.net`), private IP
  ranges in non-test code, and real-looking bot tokens
  (`[0-9]{8,10}:[A-Za-z0-9_-]{35}`).

### 8. This is a public repository

- No real hostnames, domains, IPs, chat IDs, user IDs, alert texts or
  screenshots from anyone's real setup, in code, tests, docs, issues or commit
  messages. Use `example.com`, `alertmanager:9093`, `-1001234567890`, made-up
  alerts.
- Test fixtures are written by hand, not copied from real payloads.
- Deployment configuration for any specific installation lives in that
  installation's own (private) repository, not here.
