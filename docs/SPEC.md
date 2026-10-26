# alertmanager-telegram-relay: specification

Status: **design, nothing implemented yet.** This document is the source of truth
for what the relay does. Change it in the same PR as the behaviour it
describes. Points marked **(verify)** are assumptions about Telegram or
Alertmanager that must be checked against the real API before relying on them.

## 1. Why

Alertmanager's built-in Telegram receiver can only send a new formatted text
message per notification. That leaves out the things that make alerts on a
phone pleasant:

- a resolve **updates the original message** (🔥 → ✅) instead of sending a
  second one, so the chat shows each incident once, with its current state;
- **buttons** under each alert: silence it for 1 h / 24 h right from Telegram,
  undo that, open the dashboard or runbook;
- optionally a **graph** of the metric that fired.

The relay sits between Alertmanager and Telegram to add exactly these, and
nothing that Alertmanager already does well (grouping, routing, inhibition,
deduplication stay in Alertmanager).

## 2. Architecture

```
Alertmanager ──POST /webhook (bearer secret)──▶ relay ──Bot API (HTTPS)──▶ Telegram
      ▲                                           │  ▲
      └──── /api/v2/silences, /api/v2/alerts ─────┘  └── getUpdates long poll (button presses)
                                                  │
                                            state store (bbolt file)
```

- Single static Go binary, one process, no inbound internet exposure.
- **Inbound:** only Alertmanager's webhook, on a local/internal address.
- **Outbound:** Telegram Bot API, Alertmanager API, optionally Grafana.
- **State:** one embedded key-value file (bbolt) in a volume, so a restart does
  not lose which Telegram message belongs to which alert group.

### Language and dependencies

Go (current stable). Standard library first: `net/http`, `html/template`,
`log/slog`, `encoding/json`. Planned third-party dependencies, each needing
nothing beyond itself:

- `go.etcd.io/bbolt`: state store
- `github.com/prometheus/client_golang`: `/metrics`

No Telegram SDK: the handful of Bot API methods used are called directly through
one small client (this is also where the token-redaction rule from SECURITY.md
lives).

## 3. Receiving notifications

Alertmanager is configured with a `webhook_configs` receiver pointing at
`POST /webhook`, authenticated with a bearer secret (see SECURITY.md §4).

The payload is Alertmanager's webhook format (`version: "4"`): `groupKey`,
`status`, `receiver`, `groupLabels`, `commonLabels`, `commonAnnotations`,
`externalURL`, `truncatedAlerts` and `alerts[]` (each with `status`, `labels`,
`annotations`, `startsAt`, `endsAt`, `generatorURL`, `fingerprint`).

The relay answers `200` as soon as the notification is stored and queued, then
delivers to Telegram asynchronously with retries. If storing fails it answers
`5xx` so Alertmanager retries.

## 4. Message lifecycle

The unit is the **incident**: one Alertmanager group (`groupKey`) from its first
firing notification until all of its alerts are resolved.

| Notification | Telegram action | Phone notifies? |
|---|---|---|
| Group fires, no open incident for this `groupKey` | `sendMessage`: new message with buttons; store `groupKey → message_id`, open incident | yes (see §7 for quiet severities) |
| Same incident, alert set changed (alert added) | `editMessageText` on the original, AND a short reply to it ("+1 firing: …") so the change is noticed | yes, the reply |
| Same incident, alert set changed (some resolved, some still firing) | `editMessageText` only | no |
| Same incident, nothing changed (Alertmanager `repeat_interval` reminder) | short reply to the original: "still firing, 4 h" | configurable: `REMINDERS=reply\|silent\|off` (default `reply`) |
| Group resolved | `editMessageText` to the resolved layout, remove buttons; close incident | no; optional silent reply (`RESOLVE_REPLY=true`) |
| Original message can no longer be edited or was deleted | fall back to a new message; store the new `message_id` | yes |

- An incident whose alerts fire again after it was closed is a **new** incident
  (new message), so the history stays truthful.
- Incidents are expired from the store after `INCIDENT_TTL` (default 30 days)
  even if never resolved (Alertmanager can lose a resolve across restarts).
- **(verify)** limits on editing old bot messages in groups and channels.

### Layout

Same structure as a well-made Alertmanager template, so the relay is an upgrade
and not a different look:

```
⚠️ HostDown · server-1                      ← severity emoji, alertname, shared instance
                                              (· N firing · M resolved for groups)
server-1 has not answered for 5 minutes      ← summary, one line per alert (max 6, then "…and N more")
since Mon 04:56 (2h 23m)                     ← <tg-time>, so it shows in the reader's zone

▸ description, collapsed                     ← <blockquote expandable>, first alert only, clipped
[🔕 1h] [🔕 24h] [📈 Grafana] [📖 Runbook]    ← inline keyboard
```

Resolved: `✅ HostDown · server-1 resolved`, then one struck-through line per
alert with how long it lasted ("after 2h 5m").

- Templates are Go `html/template` files. A default ships in the binary; a
  user-supplied template file can replace it (`TEMPLATE_FILE`). The template
  data mirrors Alertmanager's (`.Alerts.Firing`, `.CommonLabels`, …) so existing
  Alertmanager templates port with little change.
- Severity emoji map from the `severity` label: `critical` 🔥, `warning` ⚠️,
  anything else ℹ️. Configurable.
- Length: rendered output is measured; if over 4096 characters, the relay
  re-renders with fewer alerts and a shorter description until it fits.

## 5. Buttons

Inline keyboard under every firing message:

| Button | Kind | Effect |
|---|---|---|
| 🔕 1h, 🔕 24h | callback | Create a silence for this incident's group labels (see SECURITY.md §3). Message is edited to show "🔕 silenced until 18:00 by Alice" and the button row becomes [↩️ Unsilence]. |
| ↩️ Unsilence | callback | Expire that silence (`DELETE /api/v2/silence/{id}`). Row returns to the silence buttons. |
| 📈 Grafana | URL | Opens a dashboard link from the alert's `dashboard_url` annotation, else `GRAFANA_URL` if set. Hidden if neither exists. |
| 📖 Runbook | URL | From the `runbook_url` annotation. Hidden if absent. |
| 🔗 Source | URL | `generatorURL` of the first alert, if `SHOW_SOURCE_BUTTON=true` (default off: it is usually an internal Prometheus URL). |

- Durations are configurable (`SILENCE_DURATIONS=1h,24h`), max 7 days.
- Every allowed callback gets an `answerCallbackQuery` with a short toast
  ("Silenced for 1h"). Unauthorised presses get an empty answer, with no text
  that says why (SECURITY.md §2).
- If Alertmanager is unreachable, the toast says so and nothing changes.
- A silence created outside Telegram is not tracked; the message only reflects
  silences made through its own buttons.

## 6. Commands (later, optional)

Only for allowed users in allowed chats; anything else ignored silently.

- `/alerts`: currently firing alerts, grouped, using the same layout.
- `/silences`: active silences with an [↩️ Unsilence] button each.

## 7. Routing within Telegram

- **Several chats/topics:** `ROUTES` maps label matchers to a target chat and
  optional forum topic (`message_thread_id`), e.g. everything with
  `team="media"` to one topic. Default: the first allowed chat.
- **Quiet severities:** `QUIET_SEVERITIES=warning,info` sends those with
  `disable_notification: true`. Critical always rings.

## 8. Graph images (later, optional)

When `GRAFANA_URL` and a render token are configured and an alert has a
`panel_url` annotation pointing at a Grafana panel, render it via Grafana's
`/render` endpoint (requires the image-renderer plugin) and send it as a photo
reply to the incident message. Strictly best-effort: any failure is logged and
skipped, never delays or blocks the text message.

## 9. Reliability

- **The relay is in the alert path.** If it is down, alerts stop. The
  deployment docs must show the fallback: a Prometheus alert on the relay's own
  health (`up == 0`, and Alertmanager's
  `alertmanager_notifications_failed_total{integration="webhook"}`) routed to
  Alertmanager's **built-in** Telegram receiver, which does not depend on the
  relay.
- Telegram `429`: honour `retry_after`; per-chat send queue, so one busy chat
  does not delay others.
- Network errors: retry with backoff for up to 10 minutes, then drop with an
  error log and a metric. Alertmanager's own retry covers longer outages.
- Shutdown on SIGTERM: stop accepting webhooks, flush the queue for up to 10 s,
  close the store.
- One relay instance per bot token: Telegram allows only one `getUpdates`
  consumer.

## 10. Configuration

All configuration by environment; every secret also as `*_FILE`.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | yes | | Bot token from @BotFather |
| `TELEGRAM_CHAT_IDS` | yes | | Comma-separated chats the relay posts to and accepts updates from |
| `TELEGRAM_ADMIN_USER_IDS` | yes | | Users whose button presses are acted on |
| `WEBHOOK_SECRET` | yes | | Bearer secret Alertmanager sends |
| `LISTEN_ADDR` | | `127.0.0.1:9095` | Webhook, `/healthz`, `/metrics` |
| `ALERTMANAGER_URL` | for buttons | | e.g. `http://alertmanager:9093` |
| `ALERTMANAGER_USERNAME` / `_PASSWORD` / `_BEARER_TOKEN` | | | API auth, if any |
| `STATE_DIR` | | `/data` | bbolt file location |
| `TEMPLATE_FILE` | | built-in | Custom message template |
| `TIMEZONE` | | `UTC` | Fallback text inside `<tg-time>` |
| `SILENCE_DURATIONS` | | `1h,24h` | Silence buttons |
| `QUIET_SEVERITIES` | | empty | Sent without sound |
| `REMINDERS` | | `reply` | `reply`, `silent` or `off` |
| `RESOLVE_REPLY` | | `false` | Silent reply on resolve in addition to the edit |
| `GRAFANA_URL` | | | Dashboard button fallback, graph rendering |
| `INCIDENT_TTL` | | `720h` | Forget unresolved incidents after this |
| `LOG_LEVEL` | | `info` | `debug` logs IDs, never secrets |

## 11. Observability

- `GET /healthz`: 200 when the store is open and the last `getUpdates` poll
  succeeded within 2 minutes.
- `GET /metrics` (Prometheus): notifications received, Telegram calls by method
  and result, queue length, retries, rejected updates, silences
  created/expired, open incidents. No alert names or label values as metric
  labels.
- Logs: `log/slog` JSON to stdout.

## 12. Delivery

- `Dockerfile`: multi-stage, `CGO_ENABLED=0`, distroless `static:nonroot`
  pinned by digest.
- CI (GitHub Actions): `go vet`, `staticcheck`, `go test -race`, `govulncheck`,
  gitleaks plus the infrastructure-name check from SECURITY.md §7.
- Release: on a `v*` tag, build multi-arch (`amd64`, `arm64`), push to
  `ghcr.io/<owner>/alertmanager-telegram-relay:<version>` with provenance and
  SBOM. `latest` follows the newest release.
- `examples/`: a `compose.yml` and an Alertmanager `webhook_configs` snippet with
  placeholder values, plus the fallback-alert rule from §9.

## 13. Milestones

1. **v0.1, parity plus edit-on-resolve:** webhook with auth, allowlists, default
   template with length control, incidents in bbolt, send/edit/reply lifecycle
   (§4), healthz/metrics, Dockerfile, CI, release workflow, examples.
2. **v0.2, buttons:** long polling, callback handling, silence/unsilence,
   Grafana/runbook URL buttons.
3. **v0.3, routing:** quiet severities, multiple chats and forum topics.
4. **v0.4, extras:** `/alerts` and `/silences` commands, graph images.

Each milestone ships with tests: unit tests for rendering (golden files built
from hand-written fixtures), the incident state machine and authorisation;
integration tests against fake Telegram and Alertmanager HTTP servers
(`httptest`), never the real APIs.
