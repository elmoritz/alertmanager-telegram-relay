# alertmanager-telegram-relay

Readable Prometheus Alertmanager alerts in Telegram, with the things the
built-in Telegram receiver cannot do:

- **One message per incident.** When the alert resolves, the original message
  turns from 🔥 to ✅ instead of a second message arriving.
- **Buttons.** Silence an alert for 1 h or 24 h straight from Telegram, undo it,
  open the dashboard or runbook, copy a suggested command. A resolved message
  keeps a greyed-out "✅ Resolved after 2h 5m".
- **Made for phones.** Host and age in the headline, long descriptions
  collapsed, never over Telegram's length limit.

> **Status:** v0.3: one message per incident, edited on resolve, with
> silence/unsilence (also for a custom time by replying), "I'm on it",
> Grafana/runbook and copy buttons; routing to chats and forum topics by
> labels, quiet severities and quiet hours; escalation of unacknowledged
> critical alerts; an optional pinned status board; `/alerts` and
> `/silences` answered only to whoever asks; an optional daily digest. See [docs/SPEC.md](docs/SPEC.md) for the full feature description
> and roadmap, and [examples/](examples/) for a Compose setup, the
> Alertmanager receiver and the fallback alerts.

## How it fits

```
Alertmanager ──webhook──▶ relay ──▶ Telegram
      ▲                     │
      └── silences ◀────────┘ (button presses, via long polling: no inbound exposure)
```

Alertmanager keeps doing grouping, routing and inhibition. The relay only
changes how notifications look and what you can do with them.

## Security

The relay holds your bot token and can silence your alerts, so it only accepts
updates from chats and users you list explicitly, requires a shared secret from
Alertmanager, and never needs an inbound port on the internet. Read
[SECURITY.md](SECURITY.md) for the threat model, and report vulnerabilities
privately through the Security tab.
