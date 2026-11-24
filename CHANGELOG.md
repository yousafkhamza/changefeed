# Changelog

All notable changes are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org).

## [0.3.0] - 2026-11-24

### Added
- Change freeze windows (weekly or one-off, per env and service, IANA time zones) with `GET /api/v1/freeze`
  and the action's `mode: check-freeze`; overrides need a reason, which is recorded on the event.
  Changes made during a freeze are labeled and badged in the UI.
- Delivery metrics: deployments, deploys per day and change failure rate per service (`GET /api/v1/dora`,
  Prometheus gauges, UI table). A deployment fails when it was an incident's top-ranked cause with score 50 or more.
- Single sign-on for the UI with OpenID Connect (authorization code + PKCE, verified ID tokens, signed sessions).
- Teams map identity-provider groups to envs and services; tokens can be scoped the same way. Scoping
  applies to every read path.

### Changed
- GitHub OIDC credentials stay write-only; freeze checks accept read or write credentials.

## [0.2.0] - 2026-11-24

### Added
- Alertmanager webhook: firing alerts open an incident with ranked likely causes; resolved alerts close it.
- Slack: one post per incident with the top 3 likely causes, and the `/changefeed` slash command
  (signed requests, replay protection). App manifest in `deploy/slack/`.
- `changefeed mcp`: a read-only Model Context Protocol server for AI assistants
  (`changes_since`, `suspects`, `get_change`, `incidents`, `freeze_status`).
- Native LaunchDarkly (signed), Flagsmith (signed) and Unleash webhooks with flag-to-service mapping.
- Postgres backend (`CHANGEFEED_DATABASE_URL`) for several replicas: migration lock, per-revision
  linking lock, rolling updates and a PodDisruptionBudget in the chart.
- Incidents list in the UI and `GET /api/v1/incidents`.
- One `existingSecret` in the chart for all secrets; secrets are read only from the environment.

### Security
- The database DSN is never logged.

## [0.1.0] - 2026-11-24

### Added
- Unified change timeline with a CloudEvents-compatible `ChangeEvent` schema.
- Kubernetes collector: Deployments, StatefulSets, DaemonSets, ConfigMaps and Helm releases, with image, env, resource, replica and restart diffs and the field manager as actor.
- Argo CD notifications webhook and AWS CloudTrail (EventBridge) webhook, plus a Terraform module for the AWS side.
- GitHub Action with OIDC authentication; the server trusts signed claims for repository and actor.
- `changefeed emit --from-plan` for Terraform plan JSON and planlens reports.
- Change sets linked by PR number and commit SHA across sources.
- Ranked likely causes with visible reasons, in the API, CLI and web UI.
- Grafana annotation endpoint, Prometheus metrics, health and readiness probes.
- Redaction of credentials in summaries, diffs, env vars and ConfigMap values.
- Hashed, scoped bearer tokens with per-source limits and rate limiting.
- Helm chart with schema validation, namespaced RBAC option, ServiceMonitor, NetworkPolicy and helm test.
- `serve --demo` with a realistic sample incident.
- Signed multi-arch images with SBOM and provenance; attested binaries.
