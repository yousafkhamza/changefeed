# Changelog

All notable changes are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses [Semantic Versioning](https://semver.org).

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
