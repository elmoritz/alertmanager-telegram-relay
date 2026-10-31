# Changelog

All notable changes. Versions follow [semantic versioning](https://semver.org);
what counts as a breaking change is defined in
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md). The release workflow publishes
each version's section as its GitHub release notes, so every release needs one.

## [Unreleased]

### Added
- Silences made outside Telegram (Alertmanager UI, amtool) are shown on the
  incident's message ("silenced until … by … in Alertmanager"), with the 😴
  reaction and no escalation; incidents silenced that way also resolve when
  their alerts are gone.
- Alerts with the same summary are told apart by the labels that differ
  (`server-1 is down · device=sdb`); new template helper `apart`.
- Fuzz tests for all parsers of outside input.
- CHANGELOG and release notes; docs/COMPATIBILITY.md; example alert rules for
  the newer metrics.

### Fixed
- An alert starting before 1970 (a broken sender) rendered a negative
  date-time entity, which Telegram rejects; such times are plain text now.

## [0.4.1] - 2026-10-31

### Added
- Grafana panel images: with `GRAFANA_URL` and `GRAFANA_TOKEN`, a new
  incident's `panel_url` annotation is rendered and sent as a silent photo
  reply, in the background. `GRAFANA_RENDER_URL` renders through an internal
  address when `GRAFANA_URL` is public. The token goes only to the render
  address; redirects are never followed.

### Changed
- Decided to stay with Telegram HTML rather than rich messages (#11, SPEC §4).

## [0.4.0] - 2026-10-30

### Added
- `/alerts` and `/silences` as ephemeral commands: only the admin who asks
  sees the answer (#10). `/silences` can end silences made from Telegram, and
  its answer updates after a press.
- Daily digest with `DIGEST_AT` (#12); closed incidents are kept for 7 days.

## [0.3.2] - 2026-10-30

### Added
- Custom silence by replying `3h kernel update` to an alert, and a
  "⏱ Custom…" button that opens the reply field (#7).
- Pinned status board per chat with `STATUS_BOARD` (#6).

### Changed
- "👀 I'm on it" moved to its own row.

## [0.3.1] - 2026-10-29

### Added
- "👀 I'm on it" button and 👀/👍 reactions as acknowledgement; status
  reactions by the bot (👀, 😴, 👌) (#13).
- Escalation of unacknowledged critical incidents with `ESCALATE_AFTER` (#5,
  without direct messages).

## [0.3.0] - 2026-10-29

### Added
- `ROUTES` to chats and forum topics by labels; an incident stays where it
  started.
- `QUIET_SEVERITIES` and `QUIET_HOURS` (#8).

### Fixed
- A missing forum topic no longer loses the notification; a group upgraded to
  a supergroup is reported clearly.

## [0.2.1] - 2026-10-29

### Added
- `PROTECT_CONTENT` (#1), coloured buttons (#2), disabled status buttons
  (#3), copy-command button (#4).

### Fixed
- Incidents silenced from Telegram are resolved when their alerts are gone
  (Alertmanager sends no resolve for silenced alerts).

## [0.2.0] - 2026-10-29

### Added
- Silence and unsilence buttons, dashboard/runbook/source link buttons, long
  polling for button presses.
- Live tests and a manual checklist against a real bot and Alertmanager.

### Fixed
- Message length counted in code points as Telegram does; quoted values
  stripped from Telegram errors.

[Unreleased]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/elmoritz/alertmanager-telegram-relay/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/elmoritz/alertmanager-telegram-relay/releases/tag/v0.2.0
