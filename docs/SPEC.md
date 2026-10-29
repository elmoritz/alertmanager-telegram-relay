# alertmanager-telegram-relay: specification

Status: **v0.1 and v0.2 implemented** (see §13); later milestones are design. This
document is the source of truth for what the relay does. Change it in the same PR as the behaviour it
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
`log/slog`, `encoding/json`. Third-party dependencies:

- `go.etcd.io/bbolt`: state store (needs only `golang.org/x/sys`)
- `github.com/prometheus/client_golang`: `/metrics` (brings
  `prometheus/client_model`, `prometheus/common`, `prometheus/procfs`,
  `google.golang.org/protobuf` and two small helpers)

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

Other answers: `405` for anything but `POST`, `401` without the right bearer
secret, `415` unless `Content-Type: application/json`, `413` over 1 MiB, `400`
for a payload that is not version `"4"`, has no `groupKey`, no alerts or an
unknown status. Responses never echo request content.

The queue lives in the state file, one queue per chat, so a notification
accepted with `200` survives a restart and is delivered on the next start.

## 4. Message lifecycle

The unit is the **incident**: one Alertmanager group (`groupKey`) from its first
firing notification until all of its alerts are resolved.

| Notification | Telegram action | Phone notifies? |
|---|---|---|
| Group fires, no open incident for this `groupKey` | `sendMessage`: new message with buttons; store `groupKey → message_id`, open incident | yes (see §7 for quiet severities) |
| Same incident, alert set changed (alert added) | `editMessageText` on the original, AND a short reply to it ("+1 firing: …") so the change is noticed | yes, the reply |
| Same incident, alert set changed (some resolved, some still firing) | `editMessageText` only | no |
| Same incident, nothing changed (Alertmanager `repeat_interval` reminder) | short reply to the original: "still firing, 4 h" | configurable: `REMINDERS=reply\|silent\|off` (default `reply`) |
| Group resolved | `editMessageText` to the resolved layout; the keyboard becomes one disabled button "✅ Resolved after 2h 5m"; close incident | no; optional silent reply (`RESOLVE_REPLY=true`) |
| Group resolved, but no open incident (state lost or expired) | `sendMessage` with the resolved layout | no (silent) |
| Original message can no longer be edited or was deleted | fall back to a new message; store the new `message_id` | yes (no, for a resolve) |

- Alerts are told apart by Alertmanager's `fingerprint` (by their sorted label
  set if a sender omits it). "Alert added" means a fingerprint is firing that
  was not firing in the previous notification.
- The message is edited only when its text actually changes. The fallback
  text inside `<tg-time>` entities is ignored for this comparison: it drifts
  with the clock while clients render the entity live.
- The reminder reply reads "⏰ still firing, 4h" (time since the incident
  opened); the added reply "➕ 1 more firing: <summaries>"; the resolve reply
  "✅ resolved after 2h 5m".
- Replies are best-effort: if one cannot be delivered, the incident is still
  updated.
- An incident whose alerts fire again after it was closed is a **new** incident
  (new message), so the history stays truthful.
- Incidents are expired from the store `INCIDENT_TTL` (default 30 days) after
  their **last** notification even if never resolved (Alertmanager can lose a
  resolve across restarts). An incident that keeps receiving reminders is not
  expired.
- Editing old messages: the Bot API documents a 48-hour edit limit only for
  business messages, not for a bot's own messages in groups and channels.
  Whatever the reason an edit fails ("message to edit not found", "message
  can't be edited"), the relay falls back to a new message.

### Layout

Same structure as a well-made Alertmanager template, so the relay is an upgrade
and not a different look:

```
⚠️ HostDown · server-1                      ← severity emoji, alertname, shared instance
                                              (· N firing · M resolved for groups)
server-1 has not answered for 5 minutes      ← summary, one line per alert (max 6, then "…and N more")
since Mon 04:56 (2h 23m ago)                 ← <tg-time> "wt" and "r", so it shows in the reader's zone

▸ description, collapsed                     ← <blockquote expandable>, first alert only, clipped
[🔕 1h] [🔕 24h] [👀 I'm on it]              ← inline keyboard (§5)
[📈 Grafana] [📖 Runbook]
[📋 Copy command]
```

The summary line is the alert's `summary` annotation, else its `instance`
label, else its `alertname`. The description is the `description` annotation,
else `message`. The text inside `<tg-time>` is a fallback for clients without
date-time entities, formatted in `TIMEZONE`.

Resolved: `✅ HostDown · server-1 resolved`, then one struck-through line per
alert with how long it lasted ("after 2h 5m"), and a single disabled button
[✅ Resolved after 2h 5m] (from the first alert's start to the last alert's
end).

- Templates are Go `html/template` files. A default ships in the binary
  (`internal/render/default.tmpl`); a user-supplied file (`TEMPLATE_FILE`) can
  redefine any of its blocks: `message`, `firing`, `resolved`, `title`,
  `severity_emoji`, `added`, `reminder`, `escalation`, `resolve_reply`,
  `board`. The template data
  mirrors Alertmanager's (`.Status`, `.Alerts.Firing`, `.CommonLabels`,
  `.GroupLabels`, `.CommonAnnotations`, `.ExternalURL`, `.TruncatedAlerts`) so
  existing Alertmanager templates port with little change, plus `.Added`,
  `.OpenedAt`, `.Now`, `.Limits`, `.Silence` and `.Ack`. Helper functions: `clip`, `oneline`,
  `first`, `more`, `summary`, `description`, `alertname`, `severity`,
  `earliest`, `duration`, `httpURL`, `tgtime`, `tgago`. The templates are tried
  on a sample notification at startup; a broken one stops the relay from
  starting.
- Severity emoji from the `severity` label (common label, else the most severe
  firing alert): `critical` 🔥, `warning` ⚠️, anything else ℹ️. Configurable by
  redefining `severity_emoji`.
- Length: Telegram counts 4096 Unicode code points after entity parsing (an
  emoji outside the BMP counts once; checked against the live API); the
  relay measures the same way. If a message is too long, it re-renders with
  stepwise smaller `.Limits` (alerts listed 6 → 3 → 1, description 1500 → 500
  → 200 → hidden, single values 200 → 60 characters) until it fits. If even
  that does not fit, or the template fails at run time, or Telegram rejects the
  text anyway, a built-in minimal message (emoji, clipped alertname, count) is
  sent instead. A notification never turns into no message.

## 5. Buttons

Inline keyboard under every firing message:

| Button | Kind | Effect |
|---|---|---|
| 🔕 1h, 🔕 24h | callback | Create a silence for this incident's group labels (see SECURITY.md §3). Message is edited to show "🔕 silenced until 18:00 by Alice" and the button row becomes [↩️ Unsilence]. |
| 🔕 Silenced until 18:00 by Alice | disabled | Shown above ↩️ Unsilence while a silence made from Telegram is active. Time in `TIMEZONE` ("Tue 18:00" on another day), name clipped to 32 characters. Cannot be pressed. |
| ↩️ Unsilence | callback, highlighted | Expire that silence (`DELETE /api/v2/silence/{id}`). Row returns to the silence buttons. |
| ⏱ Custom… | callback | Ask for a custom silence: the relay replies "reply with a duration and a reason" and opens the presser's reply field (see Custom silence below). |
| 👀 I'm on it | callback | Mark the incident as handled by the presser (see Acknowledgement below). Becomes ↩️ Release. In its own row below the silence buttons. |
| ↩️ Release | callback | Undo "I'm on it". Any admin may release, not only the one who acknowledged. |
| 📈 Grafana | URL | Opens a dashboard link from the alert's `dashboard_url` annotation, else `GRAFANA_URL` if set. Hidden if neither exists. |
| 📖 Runbook | URL | From the `runbook_url` annotation. Hidden if absent. |
| 🔗 Source | URL | `generatorURL` of the first alert, if `SHOW_SOURCE_BUTTON=true` (default off: it is usually an internal Prometheus URL). |
| 📋 Copy command | copy | Copies the `copy_command` annotation to the clipboard, in the client, with no callback to the relay. Hidden if absent or over 256 characters (Telegram's limit; a clipped command would be a different command). The text is whatever the alert rule's author wrote: the button runs nothing, but it is not a trusted command source. |

- Layout: the disabled silence status (if any), the callback buttons, the
  link buttons, the copy button, each in its own row. Resolved messages keep
  only the disabled [✅ Resolved after …] button. Disabled buttons are sent
  without any action: Telegram silently drops `disabled` from a button that
  also has one, which would make it pressable. ↩️ Unsilence uses the
  `primary` style; the other buttons keep the client's default look.
- Silence buttons are offered only when `ALERTMANAGER_URL` is set and the
  group has group labels. The silence's matchers are all of the group labels,
  exact equality, in sorted order; `createdBy` is `telegram:<user id>` and the
  comment "Silenced from Telegram via alertmanager-telegram-relay".
- Durations are configurable (`SILENCE_DURATIONS=1h,24h`): 1 to 4 of them,
  each between 1 minute and 7 days.
- Updates arrive by long polling (`getUpdates`, 25 s, only `callback_query`,
  `message_reaction` and `message`).
  The relay never sets a webhook. A `409` (another consumer of the same token,
  or a webhook set on the bot) is logged as an error and retried with backoff.
- **Every press is checked, in this order** (SECURITY.md §2 and §3):
  1. the message's chat is in `TELEGRAM_CHAT_IDS` **and** the user is in
     `TELEGRAM_ADMIN_USER_IDS`; otherwise an empty answer, counted in
     `relay_telegram_updates_rejected_total`, nothing else;
  2. `callback_data` is the ID of a stored, unexpired action (clients can send
     any data, so nothing else in it is ever used);
  3. the action belongs to an open incident, the press came from that
     incident's chat and current message, and the action is on that message's
     current keyboard.
  Anything failing 2 or 3 gets the toast "This button has expired."
- Action IDs are 128 random bits. Actions expire after 7 days; while an
  incident stays open, its buttons are re-issued with new IDs on the first
  notification after half of that. Unsilence is single-use: once pressed, its
  action and the replaced silence buttons stop working.
- Toasts for allowed users: "Silenced for 1h.", "Silence removed.",
  "It's yours.", "Released.", "Alertmanager is unreachable. Nothing
  changed.", "This button has expired." The answer comes before the message
  is updated.
- If Alertmanager is unreachable, the toast says so and nothing changes (the
  buttons keep working). Unsilencing a silence Alertmanager no longer knows
  counts as success.
- **Silenced groups are checked against Alertmanager.** Alertmanager sends no
  notification when silenced alerts resolve, and none right away when a
  silence runs out while the alerts keep firing (the next one may be a
  `repeat_interval` away). So every minute, for each incident silenced from
  Telegram, the relay asks for the group's unresolved alerts
  (`GET /api/v2/alerts`, exact matchers on the group labels, silenced and
  inhibited ones included):
  - none for 5 minutes in a row: the relay queues the resolve notification
    Alertmanager did not send (the last notification, all alerts resolved
    now), and the message is resolved as usual. The 5 minutes cover an
    Alertmanager restart, after which the list stays empty until Prometheus
    sends the alerts again (every minute by default). The silence itself is
    left to run out in Alertmanager.
  - still firing after the silence ran out: the message is updated and the
    silence buttons come back.
  - Alertmanager unreachable: nothing changes; the next check tries again.
  A silence that runs out is also noticed at the next notification for the
  incident.
- A silence created outside Telegram is not tracked; the message only reflects
  silences made through its own buttons.
- Link buttons use only `http(s)` URLs. `dashboard_url` and `runbook_url` are
  taken from the first firing alert that has them, else the common
  annotations; `copy_command` likewise. Telegram rejects some URLs (e.g. host
  names without a dot); the message is then sent without the link and copy
  rows, and if even that fails, as the minimal message.
- Notifications and button presses for the same group are processed one at a
  time, so the message is never edited from two sides at once.

### Custom silence by reply

- An admin can reply to an incident's message (or to one of its escalation
  replies, or to the prompt "⏱ Custom…" sends) with a duration and an
  optional reason: `3h kernel update`, `45m`, `2d disk swap` (#7). Strictly:
  a number with `m`, `h` or `d`, 1 minute to 7 days, then a reason of at most
  200 characters. The silence is created like a button's (matchers from the
  stored group labels only, `createdBy: telegram:<user id>`), with the reason
  before the relay's comment. An active silence made from Telegram is
  replaced (the new one is created, then the old one expired).
- Feedback is a reaction on the reply: 👌 done, 🤔 not understood, 🤷
  Alertmanager unreachable (nothing changed). The message is updated as for
  a button press.
- "⏱ Custom…" replies under the incident with a prompt that mentions the
  presser and opens their reply field (`force_reply`, selective). Telegram
  has `force_reply` only for a whole message, not for one inline button
  (Bot API 10.3 changelog), hence the prompt.
- Replies from anyone else, replies to other messages, and every other
  message are dropped unlogged (SECURITY.md §2). The relay asks for
  `message` updates for this.

### Acknowledgement

- "I'm on it" (#13) is offered on every firing message, with or without
  `ALERTMANAGER_URL`, in the callback row after the silence buttons. It
  changes only the relay's state, never Alertmanager. The message shows
  "👀 Alice is on it since 18:02" until the incident resolves or someone
  presses ↩️ Release.
- An admin's 👀 or 👍 reaction on an incident's current message counts as
  "I'm on it" too (the relay asks for `message_reaction` updates; Telegram
  sends them only to bots that are chat administrators). The same checks as
  for buttons apply (SECURITY.md §2); anonymous reactions are ignored, and
  removing a reaction changes nothing.
- The bot sets one reaction on the message to show its state while
  scrolling: 👌 resolved, else 😴 silenced, else 👀 acknowledged, else none.
  Bots can set one reaction at a time from Telegram's fixed list, which has
  no ✅ or 🔕 (checked against the live API). Best-effort: a failure is
  logged at debug level.

### Escalation

- With `ESCALATE_AFTER` set (e.g. `15m`, at least `1m`; default off), a
  **critical** incident that is still firing, not acknowledged and not
  silenced gets a reply with sound, "🚨 still unacknowledged after 15m",
  `ESCALATE_AFTER` after it opened and again `ESCALATE_AFTER` after each
  escalation, at most 3 times (#5). Counting from the last escalation keeps
  an incident that is already old (relay downtime, `ESCALATE_AFTER` newly
  set) from escalating three times in a row.
- A 👀 or 👍 from an admin on an escalation reply acknowledges the incident,
  like one on the incident's message. Quiet severities and quiet hours do not apply: critical always
  rings. Checked every minute; counted in `relay_escalations_total`.
- Direct messages to admins (also proposed in #5) are not implemented: they
  would carry alert text outside the allowed chats and need a change to
  SECURITY.md first.

### Status board

- With `STATUS_BOARD=true`, the relay keeps one message per chat in
  `TELEGRAM_CHAT_IDS`, pinned without notification (#6): "🚦 2 firing" and
  one line per firing incident (severity, title, 😴 silenced, 👀
  acknowledged), each linking to its message, or "✅ All clear since 14:02".
  In a forum it is in the General topic.
- Every 30 seconds the board is rendered and edited only if its text changed,
  so it is edited at most twice a minute. If it was deleted, a new one is
  sent and pinned. Pinning needs the bot's right to pin messages; without it
  the board stays unpinned (warning in the log).
- Links (`https://t.me/c/…`) exist only in supergroups; in other chats the
  board lists incidents without links. At most 30 incidents are listed, then
  "…and N more"; the template block is `board`.

## 6. Commands (later, optional)

Only for allowed users in allowed chats; anything else ignored silently.

- `/alerts`: currently firing alerts, grouped, using the same layout.
- `/silences`: active silences with an [↩️ Unsilence] button each.

## 7. Routing within Telegram

- **Several chats/topics:** `ROUTES` maps label matchers to a target chat and
  optional forum topic (`message_thread_id`). One rule per line (or separated
  by `;`), first match wins; lines starting with `#` are comments:

  ```
  team="media" => -1001234567890/42
  severity="critical", instance=~"db-.*" => -1001234567891
  ```

  Matchers use Alertmanager's syntax (`=`, `!=`, `=~`, `!~`; regexes are
  anchored; values quoted, with `\"`, `\\` and `\n` escapes, or bare words).
  They are matched against the labels all alerts of the group share
  (`commonLabels`); a label that is missing matches as `""`. The target is a
  chat from `TELEGRAM_CHAT_IDS`, optionally `/<topic id>`. Without a matching
  rule, notifications go to the first chat in `TELEGRAM_CHAT_IDS`.
- **An incident stays where it started.** Later notifications of the group,
  replies and the fallback after a lost message go to the incident's chat and
  topic, even if the group's common labels change.
- **A topic that does not exist** (deleted, or a wrong ID in `ROUTES`):
  Telegram answers "message thread not found"; the message is sent to the
  same chat without a topic, logged as a warning, and the incident stays
  there. A notification in the wrong place beats none.
- **A group upgraded to a supergroup** (turning on topics does this) gets a
  new chat ID; Telegram rejects sends to the old one and names the new one.
  The relay does not follow it, since chats are an explicit allowlist
  (SECURITY.md §2): the notification is dropped (counted in
  `relay_deliveries_dropped_total`) with an error saying to put the new ID
  into `TELEGRAM_CHAT_IDS` and `ROUTES`; the new ID itself is logged only at
  debug level. Note that `getChat` on the old ID may still describe the old
  group.
- **Quiet severities:** `QUIET_SEVERITIES=warning,info` sends those with
  `disable_notification: true`: the new message, the "added" reply and
  reminders. Critical always rings; `critical` in the list is a startup
  error.
- **Quiet hours:** `QUIET_HOURS=22:00-07:00` (in `TIMEZONE`, may span
  midnight) sends everything except critical without sound in that window.
- The severity is the one the template shows: the common `severity` label,
  else that of the most severe firing alert.

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
- Network errors and `5xx`: retried with exponential backoff (1 s doubling to
  1 min). Other `4xx` errors are permanent and not retried.
- Shutdown on SIGTERM: stop accepting webhooks, flush the queue for up to 10 s,
  close the store. Whatever is still queued stays in the state file and is
  delivered on the next start.
- One relay instance per bot token: Telegram allows only one `getUpdates`
  consumer.

## 10. Configuration

All configuration by environment; every secret also as `*_FILE` (setting both
is an error). Invalid values stop the relay at startup with an error that
names the variable but never echoes its value.

| Variable | Since | Required | Default | Meaning |
|---|---|---|---|---|
| `TELEGRAM_BOT_TOKEN` | v0.1 | yes | | Bot token from @BotFather. Checked with `getMe` at startup: a rejected token stops the relay |
| `TELEGRAM_CHAT_IDS` | v0.1 | yes | | Comma-separated chats the relay posts to and accepts updates from. Notifications go to the first unless `ROUTES` says otherwise |
| `TELEGRAM_ADMIN_USER_IDS` | v0.1 | yes | | Users whose button presses are acted on (required from v0.1, used from v0.2) |
| `WEBHOOK_SECRET` | v0.1 | yes | | Bearer secret Alertmanager sends |
| `LISTEN_ADDR` | v0.1 | | `127.0.0.1:9095` | Webhook, `/healthz`, `/metrics` |
| `ALERTMANAGER_URL` | v0.2 | for silence buttons | | e.g. `http://alertmanager:9093`, may include a route prefix. No credentials, query or fragment |
| `ALERTMANAGER_USERNAME` + `_PASSWORD`, or `_BEARER_TOKEN` | v0.2 | | | API auth, if any; basic or bearer, not both |
| `STATE_DIR` | v0.1 | | `/data` | bbolt file location (`relay.db`) |
| `TEMPLATE_FILE` | v0.1 | | built-in | Custom message template |
| `TIMEZONE` | v0.1 | | `UTC` | Fallback text inside `<tg-time>`, button labels, `QUIET_HOURS` |
| `SILENCE_DURATIONS` | v0.2 | | `1h,24h` | Silence buttons |
| `ROUTES` | v0.3 | | empty | Chat and forum topic by labels (§7) |
| `QUIET_SEVERITIES` | v0.3 | | empty | Comma-separated severities sent without sound; not `critical` (§7) |
| `QUIET_HOURS` | v0.3 | | empty | e.g. `22:00-07:00` in `TIMEZONE`: everything but critical without sound (§7) |
| `ESCALATE_AFTER` | v0.3 | | off | e.g. `15m`: loud reply to unacknowledged critical incidents, up to 3 times (§5) |
| `STATUS_BOARD` | v0.3 | | `false` | Pinned status board per chat (§5) |
| `REMINDERS` | v0.1 | | `reply` | `reply`, `silent` or `off` |
| `RESOLVE_REPLY` | v0.1 | | `false` | Silent reply on resolve in addition to the edit |
| `GRAFANA_URL` | v0.2 | | | Dashboard button fallback, graph rendering |
| `SHOW_SOURCE_BUTTON` | v0.2 | | `false` | 🔗 Source button with the first alert's `generatorURL` |
| `PROTECT_CONTENT` | v0.2 | | `false` | Send every message with `protect_content`, so alert messages cannot be forwarded or saved (this also blocks forwarding to yourself). Link previews are always off. |
| `INCIDENT_TTL` | v0.1 | | `720h` | Forget unresolved incidents this long after their last notification |
| `LOG_LEVEL` | v0.1 | | `info` | `debug`, `info`, `warn`, `error`; `debug` logs IDs, never secrets |

## 11. Observability

- `GET /healthz`: 200 when the store is open and the last `getUpdates` poll
  succeeded within 2 minutes (counted from startup until the first poll). `relay healthcheck` calls it
  and exits 0 or 1, for container health checks (the image has no shell or
  curl).
- `GET /metrics` (Prometheus). No alert names, label values or IDs as metric
  labels:

  | Metric | Labels |
  |---|---|
  | `relay_notifications_received_total` | `status` (`firing`, `resolved`) |
  | `relay_webhook_rejected_total` | `reason` (`method`, `unauthorized`, `media_type`, `too_large`, `bad_json`, `invalid`, `store`) |
  | `relay_telegram_requests_total` | `method`, `result` (`ok`, `error`, `ratelimited`, `network`) |
  | `relay_delivery_retries_total` | |
  | `relay_deliveries_dropped_total` | |
  | `relay_template_errors_total` | |
  | `relay_queue_length` | |
  | `relay_open_incidents` | |
  | `relay_telegram_updates_rejected_total` | |
  | `relay_button_presses_total` | `action` (`silence`, `unsilence`, `ack`, `release`, `unknown`), `result` (`ok`, `error`, `stale`) |
  | `relay_escalations_total` | |
  | `relay_alertmanager_requests_total` | `op` (`create_silence`, `expire_silence`, `list_alerts`), `result` (`ok`, `error`) |

- Logs: `log/slog` JSON to stdout. Incidents are identified in logs by a short
  hash of the group key, since group keys contain label values. Creating and
  expiring a silence is logged at info with the silence ID and the user ID
  (SECURITY.md §3); otherwise IDs appear only at debug level.

## 12. Delivery

- `Dockerfile`: multi-stage, `CGO_ENABLED=0`, distroless `static:nonroot`
  pinned by digest.
- CI (GitHub Actions): `go vet`, `staticcheck`, `go test -race`, `govulncheck`,
  gitleaks plus the infrastructure-name check from SECURITY.md §7.
- Release: on a `v*` tag, run the CI checks, then build multi-arch (`amd64`,
  `arm64`), push to `ghcr.io/<owner>/alertmanager-telegram-relay:<version>`
  (and `:<major>.<minor>`) with provenance and SBOM. `latest` follows the
  newest non-prerelease.
- `examples/`: `compose.yml`, an Alertmanager snippet (`alertmanager.yml`) with
  the webhook receiver and the built-in Telegram fallback, and the
  fallback-alert rules from §9 (`prometheus-rules.yml`), all with placeholder
  values.

## 13. Milestones

1. **v0.1, parity plus edit-on-resolve (done):** webhook with auth,
   allowlists, default template with length control, incidents in bbolt,
   send/edit/reply lifecycle (§4), healthz/metrics, Dockerfile, CI, release
   workflow, examples.
2. **v0.2, buttons (done):** long polling, callback handling,
   silence/unsilence, Grafana/runbook URL buttons (released as v0.2.0, live
   check passed); `PROTECT_CONTENT`, coloured and disabled buttons, copy
   button (v0.2.1, issues #1–#4); resolving groups silenced from Telegram,
   which Alertmanager does not report (v0.2.1). Live check passed.
3. **v0.3, routing (in progress):** quiet severities, multiple chats and
   forum topics, quiet hours (#8) in v0.3.0; acknowledgement (#13) and
   escalation (#5, without direct messages) in v0.3.1 (live checks passed);
   custom silence by
   reply (#7) and the status board (#6) for v0.3.2. Topics in private chats
   (#9) are deferred: they need BotFather's threaded mode, which may cost a
   fee, and are reported to fail.
4. **v0.4, extras:** `/alerts` and `/silences` commands, graph images.

A milestone's scope is the list above **plus the open issues in the GitHub
milestone of the same name**; it is done when both are, and the live check
has passed.

Each milestone ships with tests: unit tests for rendering (golden files built
from hand-written fixtures), the incident state machine and authorisation;
integration tests against fake Telegram and Alertmanager HTTP servers
(`httptest`), never the real APIs. A milestone is marked done only after it
has been checked once against a real bot and Alertmanager
([LIVE-TESTING.md](LIVE-TESTING.md): opt-in `-tags=live` tests and a manual
checklist, never run in CI).
