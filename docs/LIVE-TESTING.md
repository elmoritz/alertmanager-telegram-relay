# Live testing

The test suite runs against `httptest` fakes, which only prove the relay
agrees with our idea of the Bot API and Alertmanager. Before a milestone is
marked done in SPEC §13, it is checked once against the real services: the
automated live tests below, then the manual checklist.

Nothing here runs in CI. Use a **throwaway bot** and a **private test chat**,
never a production bot or chat: the tests send messages and create silences.

When something fails or looks wrong, report what happened with IDs, tokens,
host names and alert texts removed (the repository is public). Real API
responses become hand-written fixtures so the fakes stay faithful.

## Setup

1. **Test bot.** In Telegram, ask @BotFather for `/newbot`. Keep the token in a
   file outside the repository, e.g. `~/relay-live/secrets/telegram_bot_token`
   (`chmod 600`). Revoke it with `/revoke` when you no longer need it.
2. **Test chat.** Create a private group with just you and the bot. Send
   `/start@yourbot` in it (with privacy mode on, which is the default, a bot
   does not see plain messages or mentions), then read the chat ID (negative,
   `-100…`) and your user ID from:

   ```bash
   curl -s "https://api.telegram.org/bot$(cat ~/relay-live/secrets/telegram_bot_token)/getUpdates"
   ```

   (This puts the token on a command line briefly; acceptable for a
   throwaway bot on your own machine.)
3. **Alertmanager.** Run a local one with
   [`examples/dev/alertmanager.yml`](../examples/dev/alertmanager.yml), copied
   to `~/relay-live/`, next to a webhook secret:

   ```bash
   cd ~/relay-live
   openssl rand -hex 32 > secrets/relay_webhook_secret
   alertmanager --config.file=alertmanager.yml --storage.path=data
   # or: docker run --rm --network host -v "$PWD:/etc/am" prom/alertmanager \
   #       --config.file=/etc/am/alertmanager.yml --storage.path=/tmp/am
   ```

   It listens on `http://127.0.0.1:9093`.

## Automated live tests

```bash
export RELAY_LIVE_TELEGRAM_BOT_TOKEN_FILE=~/relay-live/secrets/telegram_bot_token
export RELAY_LIVE_CHAT_ID=-1001234567890       # your test chat
export RELAY_LIVE_ALERTMANAGER_URL=http://127.0.0.1:9093
go test -tags=live -count=1 -run Live -v ./...
```

The relay must **not** be running with the same token: two consumers of
`getUpdates` make both fail with 409. Each group of tests skips unless its
variables are set.

| Test | Checks |
|---|---|
| `TestLiveGetMe`, `TestLiveWrongTokenIsRedacted` | the token works; a rejected token does not appear in the error |
| `TestLiveMessageLifecycle` | send with both button kinds, edit to identical content (must not fail), remove the keyboard, silent reply, reply to a missing message |
| `TestLiveEditMissingMessageIsGone` | the error text `IsMessageGone` relies on |
| `TestLiveFormatErrors`, `TestLiveLengthLimit` | the error texts `IsFormatError` relies on; the limit is 4096 UTF-16 units after entity parsing, as `render.Len` counts |
| `TestLiveButtonErrors` | rejected URLs and oversized callback data are recognised by `IsButtonError` (the log shows which ones Telegram accepts) |
| `TestLiveGetUpdates` | long polling works: no webhook set, no other consumer |
| `TestLiveGoldenFilesAreAccepted`, `TestLiveLongestMessagesAreAccepted` | Telegram accepts every golden file (`tg-time`, expandable blockquotes) and the longest messages the length control produces |
| `TestLiveSilenceLifecycle`, `TestLiveExpireUnknownSilence` | create and expire a silence; expiring an expired or unknown silence counts as success for the relay |
| `TestLiveCallbackQuery` | **interactive**, needs `RELAY_LIVE_INTERACTIVE=1`: press the button within 90 s; a real callback query has the fields the relay checks |

The interactive test prints your user and chat ID; don't paste its output
into issues.

## Manual checklist (v0.2)

Run the relay from the repository against the local Alertmanager:

```bash
export TELEGRAM_BOT_TOKEN_FILE=~/relay-live/secrets/telegram_bot_token
export WEBHOOK_SECRET_FILE=~/relay-live/secrets/relay_webhook_secret
export TELEGRAM_CHAT_IDS=-1001234567890        # your test chat
export TELEGRAM_ADMIN_USER_IDS=123456789       # your user ID
export ALERTMANAGER_URL=http://127.0.0.1:9093
export GRAFANA_URL=https://grafana.example.com
export SILENCE_DURATIONS=2m,1h
export STATE_DIR=~/relay-live/state LOG_LEVEL=debug
mkdir -p "$STATE_DIR" && go run ./cmd/relay
```

Fire and resolve alerts by posting them to Alertmanager:

```bash
fire() {  # fire <alertname> <instance> [device]; ENDS overrides endsAt
  local ends=${ENDS:-$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)}
  curl -s -XPOST -H 'Content-Type: application/json' http://127.0.0.1:9093/api/v2/alerts -d "[{
    \"labels\": {\"alertname\": \"$1\", \"instance\": \"$2\", \"device\": \"${3:-sda}\", \"severity\": \"critical\"},
    \"annotations\": {\"summary\": \"$2 is down\", \"description\": \"Test alert from the live checklist.\",
                    \"runbook_url\": \"https://example.com/runbooks/$1\"},
    \"endsAt\": \"$ends\"}]"
}
resolve() { ENDS=$(date -u +%Y-%m-%dT%H:%M:%SZ) fire "$@"; }
```

Tick each item, and note anything that differs:

- [ ] **Startup:** the log shows `telegram bot ready`; `curl -s 127.0.0.1:9095/healthz` is OK after the first poll.
- [ ] **Fire:** `fire HostDown server-1`. One message within ~10 s: readable, times shown, buttons `🔕 2m`, `🔕 1h` in the first row and `📊`/`📖` links in the second; the links open.
- [ ] **Added alert:** `fire HostDown server-1 sdb` (same group, new alert). Within ~30 s the original message is edited and a reply mentions the new alert. `fire HostDown server-2` gives a second, separate message.
- [ ] **Silence:** press `🔕 1h`. Toast "Silenced for 1h."; the message shows the silence and an Unsilence button; the silence appears in `http://127.0.0.1:9093/#/silences` with matchers exactly `alertname`, `instance`, created by `telegram:<your id>`.
- [ ] **Unsilence:** press Unsilence. Toast "Silence removed."; the silence is expired in Alertmanager; the silence buttons are back. Pressing the old Unsilence again (scroll up, or press twice quickly) gives "This button has expired."
- [ ] **Silence runs out:** press `🔕 2m`, wait over 2 minutes, then press Unsilence. It must not say "Alertmanager is unreachable". At the next reminder the silence buttons are back.
- [ ] **Alertmanager down:** stop Alertmanager, press `🔕 1h`. Toast "Alertmanager is unreachable. Nothing changed."; the message is unchanged. Start it again and the button works.
- [ ] **Partly resolved:** `resolve HostDown server-1 sdb`. The message is edited; the buttons stay.
- [ ] **Resolve:** `resolve HostDown server-1`. The message is edited to resolved and has no buttons.
- [ ] **Not an admin:** ask a second Telegram account to join the group (or restart the relay with another `TELEGRAM_ADMIN_USER_IDS`) and press a button. Nothing happens apart from the spinner ending, no toast text; `relay_telegram_updates_rejected_total` in `curl -s 127.0.0.1:9095/metrics` goes up.
- [ ] **Restart mid-incident:** fire an alert, stop the relay (Ctrl-C), start it again, press a button on the existing message, then resolve. Everything still works on the same message.
- [ ] **Long content:** fire an alert whose description is a few thousand characters (edit the description in `fire`). The message arrives shortened, not missing.
- [ ] **Logs:** nothing at `info` contains the token, the webhook secret or alert text; button presses log the action, silence ID and user ID.

When everything is ticked, the milestone can be marked done in SPEC §13.
