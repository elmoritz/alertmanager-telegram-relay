# Compatibility

What an installation can rely on across versions. From 1.0, changing
anything listed under "Stable" is a breaking change and needs a new major
version; everything under "Not stable" may change in any release and is noted
in the [CHANGELOG](../CHANGELOG.md). Before 1.0 (0.x), a minor version may
break the stable list too; the CHANGELOG says so when it does.

## Stable

- **Configuration** (SPEC §10): the names of environment variables and their
  `*_FILE` variants, their meaning, accepted values and defaults. A variable
  that is renamed keeps working, with a warning in the log, for at least one
  minor version.
- **Inputs**: Alertmanager's webhook payload version 4; the annotations the
  relay reads (`summary`, `description`, `message`, `dashboard_url`,
  `runbook_url`, `copy_command`, `panel_url`) and labels (`severity`,
  `instance`, `alertname`); the `ROUTES` syntax; the reply syntax for custom
  silences (`3h reason`); the commands `/alerts` and `/silences`.
- **Templates** (`TEMPLATE_FILE`, SPEC §4): the block names, the fields of the
  template data and the helper functions with their arguments. New blocks,
  fields and helpers may be added; the default template defines every block,
  so a custom file only needs the ones it changes.
- **Metrics** (SPEC §11): metric names, label names and the meaning of label
  values. New metrics and new label values may be added.
- **Silences**: matchers only from group labels, `createdBy:
  telegram:<user id>` and the relay's comment, so tooling can find the
  relay's silences.
- **State**: a newer version reads the state file of an older one. Going back
  to an older version after an upgrade is not guaranteed; keep a copy of
  `relay.db` before upgrading if you might.
- **Security rules** (SECURITY.md): they only get stricter.

## Not stable

- The default template's wording and layout, button labels and emoji, toast
  texts and the bot's reactions.
- Log messages and their fields (a field that carries an ID stays at debug
  level, per SECURITY.md).
- Timings: check intervals, the 5-minute window before a silenced incident
  resolves, retry and backoff.
- The Go packages: there is no public Go API; `internal/` is internal.
- Behaviour that depends on Telegram or Alertmanager themselves, e.g. which
  URLs Telegram accepts in buttons.
