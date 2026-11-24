<p align="center">
  <img src="docs/icon.svg" width="64" height="64" alt="">
</p>

<h1 align="center">changefeed</h1>

<p align="center"><b>What changed before it broke?</b><br>
One timeline of every production change, with ranked likely causes when an incident starts.</p>

<p align="center">
  <a href="https://github.com/yousafkhamza/changefeed/actions/workflows/ci.yml"><img src="https://github.com/yousafkhamza/changefeed/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/yousafkhamza/changefeed/releases"><img src="https://img.shields.io/github/v/release/yousafkhamza/changefeed?sort=semver" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue" alt="License"></a>
  <a href="https://yousafkhamza.github.io/changefeed/"><img src="https://img.shields.io/badge/demo-live-0A7470" alt="Live demo"></a>
</p>

---

Most incidents start with a change: a deploy, a ConfigMap edit, a flag ramp, a console click. During an incident, those changes are spread across GitHub, Argo CD, Kubernetes, Terraform runs, CloudTrail and a feature-flag tool, and someone has to check each one while the alert keeps firing.

changefeed collects all of them into one timeline. When you name the alerting service and the time, it links each PR to its CI run, Argo CD sync and rollout, then ranks those change sets by how likely they are to have caused it. Every score shows its reasons.

```console
$ changefeed suspects --service checkout-api --at 14:32 --window 60m
RANK  SCORE  WHEN   CHANGE                                                           WHY
1     80     T-21m  PR #412 merged: lower PSP timeout, add circuit breaker (4 events)  same service, 21 min before, high-risk change, 4 linked events
2     68     T-13m  rds ModifyDBParameterGroup on orders-db (console)                 dependency of checkout-api, 13 min before, high-risk change, made outside CI
3     62     T-40m  Flag fast-checkout ramped 10% → 50%                               same service, 40 min before, medium risk, made outside CI
```

**Try it in 10 seconds** (sample incident, no setup):

```sh
docker run --rm -p 8080:8080 ghcr.io/yousafkhamza/changefeed:v0.1.0 serve --demo
# open http://localhost:8080 and pick checkout-api
```

## Contents

- [How it works](#how-it-works)
- [Install on Kubernetes](#install-on-kubernetes)
- [Connect your sources](#connect-your-sources): GitHub Actions, Argo CD, AWS CloudTrail, Terraform, anything else
- [Read it](#read-it): web UI, CLI, Grafana, API, Prometheus
- [How ranking works](#how-ranking-works)
- [Security](#security)
- [Configuration reference](#configuration-reference)
- [Operations](#operations)
- [Development](#development)

## How it works

```
 GitHub Actions ──OIDC──┐
 Argo CD ──webhook──────┤        ┌──────────── changefeed ────────────┐
 AWS CloudTrail ──EB────┼──────▶ │ normalize → redact → correlate →   │──▶ UI · CLI · Grafana
 Terraform / planlens ──┤        │ store (SQLite) → rank              │    REST API · Prometheus
 any tool ──CLI/HTTP────┘        └─────────────▲──────────────────────┘
                                               │ watch (read-only)
                                    Kubernetes: Deployments, StatefulSets,
                                    DaemonSets, ConfigMaps, Helm releases
```

1. **Collect.** Kubernetes is watched from inside the cluster with get/list/watch only. Everything else pushes events to changefeed.
2. **Normalize.** Every change becomes one [CloudEvents](https://cloudevents.io) `ChangeEvent`: who, what, where, when, kind, risk, revision and links.
3. **Redact.** Secret values never reach storage. ConfigMap keys that look sensitive, env vars, tokens, keys, JWTs and URL credentials are masked.
4. **Correlate.** Events are grouped into change sets by PR number and commit SHA. A full SHA, an Argo CD revision and a short image tag all match.
5. **Rank.** For an incident, change sets are scored against the alerting service and its dependencies.

## Install on Kubernetes

```sh
helm install changefeed oci://ghcr.io/yousafkhamza/charts/changefeed \
  --namespace changefeed --create-namespace \
  --set config.collectors.kubernetes.env=prod \
  --set config.collectors.kubernetes.cluster=prod-eu-1
```

Then create a token and add its hash to your values:

```sh
kubectl -n changefeed exec deploy/changefeed -- /changefeed token
```

```yaml
# values.yaml
config:
  auth:
    tokens:
      - { name: oncall-ui, sha256: "<hash>", scopes: [read] }
      - { name: argocd,    sha256: "<hash>", scopes: [write], sources: [argocd] }
      - { name: aws,       sha256: "<hash>", scopes: [write], sources: [aws] }
    github:
      enabled: true
      repositories: [my-org/*]
  services:                       # used to rank likely causes
    checkout-api: { dependsOn: [payments-api, cart, orders-db] }
  platformServices: [platform]
```

What the chart sets up by default:
- One replica with the `Recreate` strategy and a 2 Gi PVC (SQLite).
- A non-root, read-only root filesystem with every capability dropped.
- A `ClusterRole` with only get/list/watch. List `config.collectors.kubernetes.namespaces` to use a namespaced `Role` instead.
- Optional Ingress, ServiceMonitor and NetworkPolicy.

Values are validated by [`values.schema.json`](charts/changefeed/values.schema.json).

## Connect your sources

### GitHub Actions (OIDC, no secret)

```yaml
jobs:
  deploy:
    permissions:
      id-token: write    # lets changefeed verify this workflow
      contents: read
    steps:
      # ... your deploy ...
      - uses: yousafkhamza/changefeed@v0.1.0
        with:
          endpoint: https://changefeed.example.com
          service: checkout-api
          env: prod
          risk: medium
```

The action fills these in for you:
- the PR number, used as change set `gh:org/repo#123`
- the commit SHA
- the summary, from the PR title or the commit message
- a link to the workflow run

The server takes the repository and actor from the **signed OIDC claims**, not from the request body, so a workflow can't post as someone else. If changefeed is down, the step logs a warning and **your deploy continues**. Set `fail-on-error: true` to change that.

On the server, enable `auth.github` and list the repositories allowed to write (`owner/repo` or `owner/*`).

### Argo CD

Use [`deploy/argocd/argocd-notifications-cm.yaml`](deploy/argocd/argocd-notifications-cm.yaml):
1. Merge its keys into `argocd-notifications-cm`.
2. Store a write token limited to `sources: [argocd]` in `argocd-notifications-secret` as `changefeed-token`.
3. Subscribe apps with the annotation `notifications.argoproj.io/subscribe.changefeed-on-sync-succeeded.changefeed: ""`.

Each sync carries its Git revision, so it joins the PR's change set automatically.

### AWS CloudTrail (no AWS credentials in changefeed)

```hcl
module "changefeed_cloudtrail" {
  source   = "github.com/yousafkhamza/changefeed//deploy/terraform/aws-cloudtrail?ref=v0.1.0"
  endpoint = "https://changefeed.example.com"
  token    = var.changefeed_aws_token   # scopes [write], sources [aws]
  services = ["rds", "iam", "ec2", "eks", "elasticloadbalancing"]
}
```

The module creates these resources:
- an EventBridge rule that selects only successful **write** calls (read-only, `Describe*` and failed calls are dropped inside AWS)
- an API destination with retries
- a dead-letter queue, with an optional alarm

Console changes are tagged *outside CI*, which raises their score. Map account IDs to envs with `config.hooks.aws.accounts`.

### Terraform and planlens

```sh
terraform show -json tf.plan > plan.json
terraform apply tf.plan
changefeed emit --service platform --env prod --from-plan plan.json
# Terraform apply: 2 to add, 1 to change, 1 to replace, 0 to destroy
```

If you pass a [planlens](https://github.com/yousafkhamza/planlens) JSON report instead, changefeed copies its risk level, score and findings.

### Feature flags, runbooks, anything else

Use the CLI or post a CloudEvent:

```sh
changefeed emit --service checkout-api --env prod --kind flag --source flags \
  --outside-ci "Flag fast-checkout ramped 10% → 50%"

curl -X POST https://changefeed.example.com/api/v1/events \
  -H "Authorization: Bearer $CHANGEFEED_TOKEN" -H "Content-Type: application/cloudevents+json" \
  -d '{"specversion":"1.0","type":"changefeed.config","source":"runbook/prod","subject":"cart",
       "data":{"env":"prod","summary":"Raised redis maxmemory 2gb → 4gb","risk":"medium"}}'
```

Kinds: `deploy`, `config`, `infra`, `flag`, `access`, `scale`. Risks: `low`, `medium`, `high`. The API takes up to 100 events per request, and events with the same `id` are stored once, so retries are safe.

## Read it

| Where | How |
|---|---|
| **Web UI** | `http://changefeed/`. Pick the alerting service and time, then share the link with the incident channel. |
| **CLI** | `changefeed since 1h --env prod`, `changefeed suspects --service checkout-api --at 14:32` |
| **Grafana** | Annotations from `GET /api/v1/grafana/annotations?env=prod&from=${__from}&to=${__to}` with the [Infinity data source](https://grafana.com/grafana/plugins/yesoreyeram-infinity-datasource/). Fields are `time`, `title`, `text` and `tags`. |
| **REST API** | `GET /api/v1/events`, `/api/v1/events/{id}`, `/api/v1/suspects`, `/api/v1/meta`; `POST /api/v1/events` |
| **Prometheus** | `rate(changefeed_events_ingested_total{kind="deploy"}[7d])` is deploy frequency per service and env. |

Time parameters accept RFC 3339 (`2026-11-24T14:32:00Z`), a duration ago (`90m`) or a UTC clock time today (`14:32`).

## How ranking works

Only changes in the same env and inside the look-back window (default 60 minutes before the alert) are scored. Each change set takes the score of its strongest event:

| Signal | Points |
|---|---|
| Same service as the alert | 40 |
| A dependency of that service (`services.<name>.dependsOn`) | 25 |
| A shared platform service (`platformServices`) | 10 |
| Recency: 25 points at the alert time, falling to 0 at 60 minutes before | 0–25 |
| High / medium risk | 20 / 10 |
| Made outside CI (console, `kubectl edit`, flag UI) | 4 |
| Change set spans 3 or more events (several layers changed) | 4 |

Changes to unrelated services are not ranked. The model is deliberately simple, so on-call engineers can see why something ranks first and check it.

## Security

- **Read-only collection.** The Kubernetes collector needs get/list/watch only. AWS pushes through EventBridge, so changefeed holds no cloud credentials.
- **Secrets never stored.** Values under sensitive-looking keys are masked. Env vars that come from Secrets are shown as `secret:name/key`, never resolved. Summaries and diffs are scanned for tokens, keys, JWTs and URL credentials before they are written. Helm release Secrets are read for their labels only.
- **No static CI secrets.** GitHub Actions authenticate with OIDC, and the token's repository and environment are checked.
- **Tokens stored as SHA-256 hashes**, each limited to `read` or `write` and to specific source lanes, with a per-token rate limit.
- **Hardened HTTP.** Strict CSP with no inline script or style, request size limits, timeouts, and no stack traces in responses.
- **Supply chain.** Images are distroless and non-root, signed with cosign (keyless), and ship an SBOM and SLSA provenance. Binaries come with checksums and build attestations. Every workflow action is pinned to a commit SHA.

Verify an image:

```sh
cosign verify ghcr.io/yousafkhamza/changefeed:v0.1.0 \
  --certificate-identity-regexp 'https://github.com/yousafkhamza/changefeed/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Configuration reference

The server reads one YAML file (`--config` or `CHANGEFEED_CONFIG`). Unknown keys are rejected, so typos fail at startup. Check a file without starting the server:

```sh
changefeed check-config changefeed.yaml
```

A commented example is in [`examples/changefeed.yaml`](examples/changefeed.yaml). Environment overrides: `CHANGEFEED_LISTEN`, `CHANGEFEED_DB`.

## Operations

| | |
|---|---|
| Probes | `/healthz` (process), `/readyz` (database) |
| Metrics | `/metrics`: ingested, rejected, duplicate and ignored events, HTTP latency, build info |
| Logs | JSON (`--log-format text` for local use) |
| Storage | SQLite in WAL mode, purged hourly after `storage.retention` (default 30 days). Back up `/data` with your volume snapshots. |
| Capacity | A single pod handles thousands of events per minute. Real platforms produce hundreds per hour. |
| Restarts | Changes made while changefeed is down are not backfilled for Kubernetes. Push sources retry: the GitHub Action retries 3 times, and EventBridge retries for an hour into a DLQ. |

## Development

You need Go 1.26 or later. Helm is needed for chart work.

```sh
make test      # go test -race ./...
make lint      # golangci-lint
make run       # serve --demo on :8080
make chart     # helm lint + template
```

The project layout:
- `cmd/changefeed`: the binary
- `internal/event`: the schema
- `internal/store`: SQLite and correlation
- `internal/rank`: scoring
- `internal/collector/kube`: the cluster watcher
- `internal/hooks`: Argo CD and AWS mappers
- `internal/server`: the HTTP API
- `internal/ui`: the embedded web UI

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap

- **v0.2:** Slack replies to alerts, an MCP server for AI assistants (`changes_since`, `suspects`), native LaunchDarkly/Unleash/Flagsmith webhooks, Postgres for HA
- **v0.3:** change failure rate (DORA), change freeze windows, OIDC SSO and per-team scoping in the UI

## License

[Apache-2.0](LICENSE) © Yousaf K Hamza
