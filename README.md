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
docker run --rm -p 8080:8080 ghcr.io/yousafkhamza/changefeed:v0.3.0 serve --demo
# open http://localhost:8080 and pick checkout-api
```

## Contents

- [How it works](#how-it-works)
- [Install on Kubernetes](#install-on-kubernetes)
- [Connect your sources](#connect-your-sources): GitHub Actions, Argo CD, AWS CloudTrail, Terraform, anything else
- [Read it](#read-it): web UI, CLI, Grafana, API, Prometheus
- [Incidents, Slack and AI assistants](#incidents-slack-and-ai-assistants): Alertmanager, Slack, MCP
- [Change freezes](#change-freezes)
- [Delivery metrics (DORA)](#delivery-metrics-dora)
- [Single sign-on and teams](#single-sign-on-and-teams)
- [Postgres and high availability](#postgres-and-high-availability)
- [How ranking works](#how-ranking-works)
- [Security](#security)
- [Configuration reference](#configuration-reference)
- [Operations](#operations)
- [Development](#development)

## How it works

```
 GitHub Actions ──OIDC──┐
 Argo CD ──webhook──────┤        ┌──────────── changefeed ─────────────┐
 AWS CloudTrail ──EB────┤        │ normalize → redact → correlate →    │──▶ UI (SSO) · CLI · Grafana
 Terraform / planlens ──┼──────▶ │ store (SQLite or Postgres) → rank   │    REST API · Prometheus
 LaunchDarkly/Unleash/  │        │                                     │    MCP (AI assistants)
   Flagsmith ───────────┤        └──────▲───────────────▲──────────────┘
 any tool ──CLI/HTTP────┘               │ watch          │ alerts
                             Kubernetes (read-only)   Alertmanager ──▶ incident ──▶ Slack
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
      - uses: yousafkhamza/changefeed@v0.3.0
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
  source   = "github.com/yousafkhamza/changefeed//deploy/terraform/aws-cloudtrail?ref=v0.3.0"
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

### Feature flags

| Provider | Webhook URL | Authentication |
|---|---|---|
| LaunchDarkly | `/hooks/launchdarkly` | Webhook signing secret → `CHANGEFEED_LAUNCHDARKLY_SECRET` (checks `X-LD-Signature`) |
| Flagsmith | `/hooks/flagsmith` | Webhook secret → `CHANGEFEED_FLAGSMITH_SECRET` (checks `X-Flagsmith-Signature`) |
| Unleash | `/hooks/unleash` | Webhook addon with header `Authorization: Bearer <token>`, token limited to `sources: [flags]` |

Map flags to services and provider environments to your envs, so flag changes rank against the right service:

```yaml
hooks:
  flags:
    services: { fast-checkout: checkout-api, new-nav: web }
    defaultService: flags
    envs: { production: prod, staging: staging }
```

Flag changes are marked *made outside CI*, and deleting or archiving a flag is high risk.

### Runbooks, scripts, anything else

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
| **REST API** | `GET /api/v1/events`, `/events/{id}`, `/suspects`, `/incidents`, `/dora`, `/freeze`, `/meta`; `POST /api/v1/events` |
| **Prometheus** | `changefeed_deployments_per_day` and `changefeed_change_failure_ratio` per service and env; `rate(changefeed_events_ingested_total{kind="deploy"}[7d])` |
| **Slack** | Incident posts and `/changefeed` (see below) |
| **AI assistants** | `changefeed mcp` (see below) |

Time parameters accept RFC 3339 (`2026-11-24T14:32:00Z`), a duration ago (`90m`) or a UTC clock time today (`14:32`).

## Incidents, Slack and AI assistants

### Alertmanager opens incidents

Add the receiver from [`deploy/alertmanager/receiver.yaml`](deploy/alertmanager/receiver.yaml). The token needs `sources: [alertmanager]`.

For each **firing** alert, changefeed:
1. finds the service from the alert labels (`service`, `app_kubernetes_io_name`, `app`, `deployment`, `job`, configurable)
2. ranks likely causes in the 60 minutes before the alert
3. stores an incident with the top change set, and posts to Slack **once**, even though Alertmanager repeats notifications

**Resolved** alerts close the incident. Alerts without a service label are skipped and logged.

### Slack

1. Create the app from [`deploy/slack/app-manifest.yaml`](deploy/slack/app-manifest.yaml).
2. Put `CHANGEFEED_SLACK_BOT_TOKEN` and `CHANGEFEED_SLACK_SIGNING_SECRET` in the Secret.
3. Set:
   ```yaml
   publicURL: https://changefeed.example.com   # links in messages
   slack: { enabled: true, channel: "#incidents", minScore: 40 }
   ```

You get:
- **Incident posts:** the top 3 likely causes, each with its score, reasons, actor and a link to the incident view. Changes scoring under `minScore` are left out, so a quiet "no likely change found" replaces guesswork.
- **`/changefeed checkout-api 60m`:** likely causes right now. **`/changefeed since 2h cart`:** recent changes. Replies are visible only to you.

Every request is checked against Slack's signing secret and rejected if it's older than 5 minutes. Slack failures are logged and counted (`changefeed_slack_posts_total`), and they never fail the Alertmanager webhook.

### AI assistants (MCP)

`changefeed mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server on stdio. Its tools are `changes_since`, `suspects`, `get_change`, `incidents` and `freeze_status`. All of them are **read-only**, and they call the API with your token, so the assistant sees exactly what your token can see.

```json
{
  "mcpServers": {
    "changefeed": {
      "command": "changefeed",
      "args": ["mcp"],
      "env": { "CHANGEFEED_ENDPOINT": "https://changefeed.example.com", "CHANGEFEED_TOKEN": "cf_…" }
    }
  }
}
```

This works in Claude Desktop, Claude Code (`claude mcp add changefeed -- changefeed mcp`), Cursor and other MCP clients. Ask: *"checkout-api started returning 5xx at 14:32. What changed?"*

## Change freezes

```yaml
freezes:
  - name: weekend
    reason: No prod deploys at the weekend
    envs: [prod]
    weekly: { from: "fri 16:00", to: "mon 08:00", timezone: Asia/Dubai }
  - name: year-end
    services: [payments-api]
    start: "2026-12-20T00:00:00Z"
    end: "2027-01-03T00:00:00Z"
```

Gate deploys with the action, **before** the deploy step:

```yaml
- uses: yousafkhamza/changefeed@v0.3.0
  with: { mode: check-freeze, endpoint: https://changefeed.example.com, service: checkout-api, env: prod }
  # override-reason: "Hotfix for INC-123"   ← deploys anyway and records the reason
```

During a freeze, the job fails with the freeze name, end time and reason. An override is logged as a warning and recorded on the deploy event as the label `freeze-override`. Every change made during a freeze, from any source, is labeled `freeze`, and the UI shows a badge for it.

`GET /api/v1/freeze?env=prod&service=checkout-api` accepts read or write credentials.

## Delivery metrics (DORA)

For each service and env over the last 30 days, changefeed reports:
- **Deployments:** distinct change sets containing a deploy. A PR whose sync and rollout both appear still counts once.
- **Deploys per day.**
- **Change failure rate:** the share of deployments that were the **top-ranked cause, with a score of 50 or more,** of an incident.

You can find these numbers in:
- the UI, under *Delivery, last 30 days*
- `GET /api/v1/dora?env=prod&window=30d`
- the Prometheus gauges `changefeed_deployments_per_day` and `changefeed_change_failure_ratio`, updated every 5 minutes

The change failure rate measures changefeed's ranking, not a confirmed root cause. Treat it as a trend, and confirm causes in postmortems.

## Single sign-on and teams

```yaml
publicURL: https://changefeed.example.com
auth:
  oidc:
    enabled: true
    issuer: https://login.example.com     # Okta, Entra ID, Google, Keycloak, Dex…
    clientID: changefeed
  teams:
    - { name: payments, groups: [payments-oncall], envs: [prod], services: [payments-api, checkout-api] }
    - { name: platform, groups: [platform] }   # no limits: sees everything
```

You also need these values in the Secret:
- `CHANGEFEED_SESSION_KEY`: 32 or more random characters (`openssl rand -base64 48`)
- `CHANGEFEED_OIDC_CLIENT_SECRET`: only for confidential clients

Register the redirect URL `https://changefeed.example.com/auth/callback` with your identity provider.

How it works:
- **Sign-in** uses the authorization code flow with PKCE, with state and nonce checks.
- **ID tokens** are verified against the provider's JWKS.
- **Sessions** are HMAC-signed cookies: `HttpOnly`, `Secure`, `SameSite=Lax`, 12 hours by default.
- **Teams:** a user sees the union of their teams' envs and services. With no teams configured, every user who signs in sees everything. A user in no team is refused.

**Tokens can be scoped the same way:** `envs: [prod]`, `services: [payments-api]`. Scoping applies to every read: events, change sets, likely causes, incidents, DORA, Grafana and the service lists.

## Postgres and high availability

```sh
kubectl -n changefeed create secret generic changefeed-secrets \
  --from-literal=CHANGEFEED_DATABASE_URL='postgres://changefeed:…@pg.example.com:5432/changefeed?sslmode=require'
helm upgrade changefeed oci://ghcr.io/yousafkhamza/charts/changefeed -n changefeed \
  --set existingSecret=changefeed-secrets --set postgres.enabled=true --set postgres.replicaCount=2
```

With Postgres, the chart:
- runs several replicas with rolling updates and a PodDisruptionBudget
- doesn't create a volume

What keeps replicas consistent:
- **Schema:** migrations run under an advisory lock, so replicas starting together don't race.
- **Change sets:** linking is serialized per revision.
- **Duplicates:** each replica's Kubernetes collector sees the same changes, but deterministic event IDs make sure each change is stored only once.

Rate limits apply per replica.

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
- **Tokens stored as SHA-256 hashes**, each limited to `read` or `write`, to specific source lanes, and optionally to envs and services, with a per-token rate limit.
- **Signed webhooks.** Slack, LaunchDarkly and Flagsmith requests are verified with HMAC-SHA256, and Slack requests older than 5 minutes are rejected.
- **SSO for people.** OIDC with PKCE, verified ID tokens, signed `HttpOnly`/`Secure`/`SameSite=Lax` sessions, and team scoping on every read path.
- **No secrets in config or logs.** Database URLs and Slack, OIDC and webhook secrets come only from environment variables (one Kubernetes Secret). The database DSN is never logged.
- **Hardened HTTP.** Strict CSP with no inline script or style, request size limits, timeouts, and no stack traces in responses.
- **Supply chain.** Images are distroless and non-root, signed with cosign (keyless), and ship an SBOM and SLSA provenance. Binaries come with checksums and build attestations. Every workflow action is pinned to a commit SHA.

Verify an image:

```sh
cosign verify ghcr.io/yousafkhamza/changefeed:v0.3.0 \
  --certificate-identity-regexp 'https://github.com/yousafkhamza/changefeed/.github/workflows/release.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Configuration reference

The server reads one YAML file (`--config` or `CHANGEFEED_CONFIG`). Unknown keys are rejected, so typos fail at startup. Check a file without starting the server:

```sh
changefeed check-config changefeed.yaml
```

A commented example is in [`examples/changefeed.yaml`](examples/changefeed.yaml). Environment overrides: `CHANGEFEED_LISTEN`, `CHANGEFEED_DB`. Secrets, read only from the environment: `CHANGEFEED_DATABASE_URL`, `CHANGEFEED_SLACK_BOT_TOKEN`, `CHANGEFEED_SLACK_SIGNING_SECRET`, `CHANGEFEED_LAUNCHDARKLY_SECRET`, `CHANGEFEED_FLAGSMITH_SECRET`, `CHANGEFEED_OIDC_CLIENT_SECRET`, `CHANGEFEED_SESSION_KEY`.

## Operations

| | |
|---|---|
| Probes | `/healthz` (process), `/readyz` (database) |
| Metrics | `/metrics`: ingested, rejected, duplicate and ignored events, incidents opened, Slack posts, deploys per day, change failure ratio, HTTP latency, build info |
| Logs | JSON (`--log-format text` for local use) |
| Storage | SQLite in WAL mode (back up `/data` with volume snapshots) or Postgres (use your normal Postgres backups). Events and incidents are purged hourly after `storage.retention` (default 30 days). |
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
- `internal/store`: SQLite/Postgres, correlation, incidents
- `internal/rank`: scoring
- `internal/collector/kube`: the cluster watcher
- `internal/hooks`: Argo CD, AWS, Alertmanager and feature-flag mappers
- `internal/slack`, `internal/mcp`, `internal/dora`: Slack, the MCP server, delivery metrics
- `internal/auth`: tokens, GitHub OIDC, SSO
- `internal/server`: the HTTP API
- `internal/ui`: the embedded web UI

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Roadmap

v0.2 (Alertmanager incidents, Slack, MCP, feature-flag webhooks, Postgres) and v0.3 (change failure rate, change freezes, SSO and teams) are done. Next:

- Dependency map built from OpenTelemetry traces instead of `services.dependsOn`
- Agents per cluster that push to a central server (multi-cluster)
- PagerDuty and Opsgenie incidents; Microsoft Teams messages
- Backfill of Kubernetes changes from rollout history after downtime

## License

[Apache-2.0](LICENSE) © Yousaf K Hamza
