# changefeed: questions and answers (private prep notes)

> Kept in `notes/`, which is git-ignored. Never commit this file.
> Answers are written in first person, so you can say them as-is.

---

## 1. The pitch

**30 seconds**
> In most incidents, the first question is "what changed?". The answer is spread across GitHub, Argo CD, Kubernetes, Terraform, CloudTrail and the feature-flag tool, so someone checks each one while the alert is firing. changefeed puts every production change on one timeline. When an alert fires, it ranks which changes most likely caused it, shows why, and posts that to Slack. It's open source, runs as one pod (or several with Postgres), only reads from your systems, and never stores secret values.

**One line:** "It answers *what changed before it broke?* in seconds instead of minutes."

**Links:** github.com/yousafkhamza/changefeed · yousafkhamza.github.io/changefeed

---

## 2. Why I built it

**Q: What problem does it solve?**
Most outages start with a change, but finding *which* change is manual. The hardest ones skip the pipeline entirely: a `kubectl edit` on a ConfigMap, an RDS parameter changed in the console, a feature flag ramped from 10% to 50%. Nobody mentions them in the incident channel because nobody thinks of them as a deploy.

**Q: Why not use existing tools?**
Each tool covers one source. Grafana deploy annotations show deploys only. Argo CD shows syncs. CloudTrail shows AWS calls. None of them links "this PR → this CI run → this Argo sync → this rollout" or ranks the results against the alerting service. That joining and ranking is the product.

**Q: Who is it for?**
- On-call SREs and platform teams, during incidents.
- Engineering managers, for deploy frequency and change failure rate.
- Security and audit (ISO 27001 / PCI) as change evidence: who changed what, when, through which path, and whether it bypassed CI.

**Q: Why does it matter to you personally?**
I've worked in fintech under ISO 27001 and PCI. There, "what changed?" is both an incident question and an audit question. I wanted one tool that answers both.

---

## 3. How it works

**Pipeline:** collect → normalize → redact → correlate → store → rank.

1. **Collect.**
   - Kubernetes: an in-cluster informer watch, get/list/watch only.
   - Pushed to changefeed: GitHub Actions (OIDC), Argo CD notifications, AWS CloudTrail through EventBridge, Terraform plan JSON or planlens, LaunchDarkly, Unleash, Flagsmith, Alertmanager, and anything else through the CLI or HTTP API.
2. **Normalize.** Every change becomes one CloudEvents-compatible `ChangeEvent`: source, kind, service, env, actor, summary, risk, revision, change set, diff, links.
3. **Redact.** Secrets are masked before storage (see section 7).
4. **Correlate.** Events are grouped into **change sets** by PR number and commit SHA. I compare the first 7 hex characters, so a full Git SHA, an Argo CD revision and an image tag like `checkout:9c2b7e1` all match.
5. **Store.** SQLite by default, Postgres for high availability.
6. **Rank.** Given an alerting service, env and time, every change set in the look-back window gets a score with reasons.

**Q: Walk me through a real incident.**
1. At 14:32, `CheckoutHigh5xx` fires and Alertmanager calls `/hooks/alertmanager`.
2. changefeed reads the `service` label (`checkout-api`) and opens an incident.
3. It ranks the last 60 minutes:
   - **#1 (score 80):** PR #412, "lower PSP timeout". That's 4 linked events: a ConfigMap edit, the merge, the Argo CD sync and the rollout. Reasons: same service, 21 minutes before, high risk, 4 linked events.
   - **#2 (68):** RDS `ModifyDBParameterGroup` on `orders-db`. Reasons: a dependency, made in the console, high risk.
   - **#3 (62):** a flag ramp from 10% to 50%.
4. It posts the top 3 to `#incidents` once, with a link to the incident view.
5. On-call clicks the link, sees the diff `PAYMENT_TIMEOUT_MS 3000 → 800`, and rolls back.
6. When the alert resolves, the incident closes, and PR #412 counts as a failed deployment in the change failure rate.

---

## 4. Design decisions (the "why" questions)

| Question | My answer |
|---|---|
| Why Go? | It fits the Kubernetes ecosystem (client-go informers), builds one static binary with no CGO, and the distroless image is about 20 MB. |
| Why SQLite by default? | Real platforms make hundreds of changes per hour, not millions. One pod and one volume means anyone can adopt it in 2 minutes. I used a pure-Go driver with WAL mode and one writer connection. |
| Then why add Postgres? | For HA and several replicas. I kept one code path with two SQL dialects, and the same test suite runs against both in CI. |
| Why read-only collectors? | Security teams approve them quickly. The Kubernetes RBAC is get/list/watch only, and AWS pushes through EventBridge, so changefeed holds **no cloud credentials**. |
| Why OIDC for GitHub Actions? | There's no static secret to leak or rotate. I take the repository and actor from the **signed token**, never the request body, so a workflow can't post as someone else. |
| Why a transparent score, not ML? | On-call engineers must trust the order. A score with readable reasons beats a black box, works on day one, and needs no training data. |
| Why CloudEvents? | It's an open, vendor-neutral standard, so any tool can send events without a custom SDK. |
| Why "never fail a deploy"? | If changefeed is down, the GitHub Action warns and continues. The observability tool must never become the incident. Freeze checks are the deliberate exception, because those are policy. |
| Why deterministic event IDs? | Retries and duplicate deliveries are safe. They also let several replicas each watch the cluster without storing anything twice. |
| Why an embedded UI with no framework? | No build step and no supply-chain surface, and it allows a strict CSP: no inline script or style, no third-party assets. |

---

## 5. The ranking model

| Signal | Points |
|---|---|
| Same service as the alert | 40 |
| A dependency (`services.<name>.dependsOn`) | 25 |
| A shared platform service | 10 |
| Recency: 25 at the alert, falling linearly to 0 at 60 minutes before | 0–25 |
| High / medium risk | 20 / 10 |
| Made outside CI (console, `kubectl edit`, flag UI) | 4 |
| Change set spans 3 or more events | 4 |

- A change set takes the score of its strongest event; ties go to the later event.
- Unrelated services, other envs, and changes after the alert are excluded.
- The highest possible score is 93, and it's capped at 99.

**Q: Isn't that too simple?**
On purpose. It narrows 40 changes to 3 that a human checks, and it never claims root cause. The weights live in one file with tests. The next step is learning the dependency graph from OpenTelemetry traces instead of configuring it.

**Q: How do you set the risk level?**
Each source does it. Examples:
- **AWS:** `Delete*`, `Terminate*`, IAM writes, security group ingress and RDS `Modify*` are high risk.
- **Kubernetes:** scaling to zero, or from many replicas down to 1, is high.
- **Terraform:** any delete or replace is high, or planlens's level is used when available.
- **Flags:** a delete or archive is high.

---

## 6. Feature questions

**Alertmanager**
- **Which label names the service?** It's configurable, by default `service`, `app_kubernetes_io_name`, `app`, `deployment`, then `job`.
- **What about alerts without a service label?** They're skipped and logged, not guessed.
- **Alertmanager repeats notifications. Do you post twice?** No. The incident ID is `am:<fingerprint>`, so a repeat is detected and Slack is posted only on creation. If an alert fires again after resolving, the incident reopens.

**Slack**
- **Authentication:** every slash command is checked against Slack's signing secret (HMAC-SHA256 over `v0:timestamp:body`), and requests older than 5 minutes are rejected to stop replays.
- **Posting:** uses `chat.postMessage` with a bot token that only has `chat:write`.
- **Failures:** they're logged and counted, and never make the Alertmanager webhook fail, so Alertmanager won't retry and open duplicate incidents.
- **Weak matches:** `minScore` hides them, so the bot says "no likely change found" instead of guessing.

**MCP (AI assistants)**
- **What is it?** `changefeed mcp` speaks the Model Context Protocol (JSON-RPC 2.0 over stdio). I implemented the protocol myself, with no SDK.
- **Tools:** five read-only tools, each with `readOnlyHint`.
- **Access:** tools call the REST API with the user's own token, so the assistant can never see more than the person can. A tool error comes back as a result the model can read (`isError`), not as a protocol failure.

**Feature flags**
- LaunchDarkly and Flagsmith sign their webhooks with HMAC, and I verify with a constant-time compare. Unleash uses a scoped bearer token.
- Flags are mapped to services through config, so a flag ramp is ranked against the right service.

**Change freezes**
- Freezes are weekly windows (for example Friday 16:00 to Monday 08:00, Asia/Dubai, wrapping over the weekend) or one-off dates, per env and service.
- The CI action's `mode: check-freeze` fails the job during a freeze.
- An override needs a written reason, which is recorded on the deploy event as `freeze-override`.
- Every change during a freeze, from any source, gets a `freeze` label.
- I embed the IANA time zone database in the binary, so it works in distroless images.

**DORA**
- **Deployment:** a distinct change set containing a deploy. A PR's sync and rollout count once.
- **Change failure rate:** the share of deployments that were an incident's **top-ranked cause with a score of 50 or more.**
- **Where it's shown:** the UI, the API and Prometheus gauges, refreshed every 5 minutes.
- **Caveat I say up front:** it measures changefeed's ranking, not confirmed root cause. Use it as a trend and confirm in postmortems.

**SSO and teams**
- **Sign-in:** OIDC authorization code flow with PKCE (S256), plus state and nonce checks.
- **ID tokens:** RS256 verified against the provider's JWKS; I check issuer, audience and expiry.
- **Sessions:** HMAC-signed cookies (`HttpOnly`, `Secure`, `SameSite=Lax`).
- **Teams:** IdP groups map to teams, and a user sees the union of their teams' envs and services. A user in no team is refused.
- **Tokens** can be scoped the same way.
- **Coverage:** scoping applies to *every* read path: events, single event, change set members, likely causes, incidents, DORA, Grafana and the service lists.

**Postgres and HA**
- Schema migration runs under a Postgres advisory lock, so replicas starting together don't race.
- Change-set linking is serialized per revision with `pg_advisory_xact_lock(hashtext(rev))`.
- The chart switches to RollingUpdate, adds a PodDisruptionBudget, and drops the volume.

---

## 7. Security questions

**Q: How do you make sure secrets never get stored?**
Several layers:
1. Values under keys that look sensitive (`password`, `token`, `secret`, `dsn`, `api_key`…) are masked.
2. Env vars sourced from Secrets are shown as `secret:name/key` and never resolved.
3. Every summary and diff is scanned for AWS keys, GitHub, GitLab, Slack and Stripe tokens, JWTs, private keys, URL credentials and Bearer headers before writing.
4. Helm releases are Secrets, so I use a **metadata-only** informer that reads labels, never data.

Honest caveat: Kubernetes RBAC can't grant "metadata only", so Helm tracking needs list/watch on Secrets. That's why it's opt-in and documented.

**Q: How are tokens stored?**
As SHA-256 hashes only, so the config can live in a ConfigMap. Every hash is compared in constant time, so timing doesn't reveal which token matched. Each token can be limited to:
- read and/or write
- specific source lanes (for example, the Argo CD token can only write `argocd`)
- specific envs and services
- a rate limit

**Q: Supply chain?**
- Every GitHub Action is pinned to a commit SHA, with Dependabot to update them.
- govulncheck runs in CI.
- Images are distroless and non-root, multi-arch, and cosign keyless-signed, with SBOM and SLSA provenance.
- Binaries ship with checksums and build attestations.
- The Helm chart is pushed to GHCR as an OCI artifact and signed.

**Q: How did you handle the HTTP layer?**
- Strict CSP: no inline script or style and same-origin only. The UI builds the DOM with `textContent`, never `innerHTML` with data.
- Links must be http(s), which blocks `javascript:` URLs.
- A 1 MiB body limit, header and read timeouts, and no stack traces in responses.
- The database DSN is never logged.

---

## 8. DevOps and engineering questions

**Q: How is it tested?**
- About 9,000 lines of Go, about 2,600 of them tests, with 86 test functions plus subtests.
- **Unit tests:** ranking math, redaction, every webhook mapper with recorded payloads, freeze time math including the weekend wrap, DORA, Slack signing, the MCP protocol.
- **Fake services:** the OIDC flows run against a fake identity provider, and the Kubernetes collector against client-go's fake clientset.
- **Both databases:** store tests run against SQLite and against a Postgres service container in CI.
- **CI also runs:** the race detector, golangci-lint (16 linters including gosec), govulncheck, kubeconform on the rendered chart, `check-config` on the rendered config, actionlint and shellcheck, and `terraform validate`.
- **End-to-end:** a kind cluster where I build the image, install the chart, edit a ConfigMap and roll out a deployment, then assert both appear in the API. The demo image is also smoke-tested, including an MCP tool call.

**Q: Tell me about a problem you hit while building it.** (Good behavioral answer.)
> My work laptop has a Defender ASR policy that blocks any newly compiled executable, so I couldn't run `go test` locally. I didn't try to bypass a company security control. I verified locally with `go vet`, which compiles the tests, plus lint and cross-compiles, and made CI the place where tests run. I worked through test expectations by hand before pushing. On the first push, govulncheck failed: CI was pinned to Go 1.26.0, and `golang.org/x/net` had HTTP/2 CVEs. I added a `toolchain` directive and upgraded `x/net`, then confirmed the fix against the OSV database before pushing again.

**Q: How do you release?**
I push a semver tag, and the release workflow then:
1. runs the tests again
2. builds binaries for 5 platforms with checksums and attestations
3. builds the multi-arch image with SBOM and provenance, then signs it
4. packages the Helm chart, pushes it to GHCR as OCI, and signs it

Release notes come from `CHANGELOG.md`.

**Q: How would you run it in production?**
Install with Helm using Postgres from a managed service (RDS or Cloud SQL) and 2 replicas. Then:
- put all secrets in one Secret (ideally synced by External Secrets)
- enable the ServiceMonitor, plus a NetworkPolicy that allows only the ingress controller and senders
- turn on SSO for the UI
- give each source its own scoped token
- set alerts on `changefeed_events_rejected_total`, `changefeed_slack_posts_total{result="error"}` and the EventBridge dead-letter queue

**Q: What happens if changefeed is down?**
Push sources retry: the GitHub Action 3 times, and EventBridge for an hour, then into a dead-letter queue. Deploys are never blocked except by a freeze check. Kubernetes changes during the downtime aren't backfilled. That's a known limitation and it's on the roadmap.

---

## 9. Trade-offs and limits (say these before they ask)

- Ranking is a heuristic: it narrows the search, it doesn't prove root cause.
- The dependency graph is configured by hand today.
- No backfill of Kubernetes changes after downtime.
- Rate limits are per replica, not global.
- The change failure rate depends on the ranking, so it's a trend, not an audit number.
- Helm tracking needs list/watch on Secrets (metadata only in code, not in RBAC).
- Slack users in the workspace see all services through `/changefeed`. Team scoping applies to the UI, the API and MCP.

---

## 10. What I'd build next

- A **dependency map built from OpenTelemetry traces**, replacing `dependsOn`.
- **Per-cluster agents** pushing to a central server, for multi-cluster and multi-region setups.
- **PagerDuty and Opsgenie** incidents, and **Microsoft Teams** messages.
- **Backfill** from ReplicaSet history and Argo CD history after downtime.
- Learning weights from postmortem feedback: someone marks the real cause, and the ranking improves.

---

## 11. Rapid-fire facts

- Stack: Go 1.26+, client-go, SQLite (modernc, no CGO) or Postgres (pgx), Prometheus client, plain JavaScript UI.
- Packaging: distroless image, Helm chart on GHCR (OCI), a composite GitHub Action, and a Terraform module for EventBridge with a DLQ and an alarm.
- Endpoints: `/api/v1/events`, `/events/{id}`, `/suspects`, `/incidents`, `/dora`, `/freeze`, `/meta`, `/grafana/annotations`; hooks `/hooks/argocd`, `aws`, `alertmanager`, `unleash`, `launchdarkly`, `flagsmith`, `slack/command`; plus `/healthz`, `/readyz`, `/metrics`.
- CLI: `serve`, `emit` (including `--from-plan`), `since`, `suspects`, `token`, `check-config`, `mcp`.
- Demo: `docker run --rm -p 8080:8080 ghcr.io/yousafkhamza/changefeed:v0.3.0 serve --demo`.
- Pairs with my other projects: **planlens** (Terraform risk goes into changefeed through `--from-plan`) and **K8s Incident Copilot** (it can ask changefeed "what changed?" over MCP).

## 12. Two-minute demo script

1. Run `serve --demo` and open the UI.
2. "An alert fired at 14:32 on checkout-api." The incident is already listed under *Recent incidents*. Click it.
3. Show the timeline with the red line, the shaded window and the ranked list.
4. Open #1 and show the ConfigMap diff and the 4-event change set chain.
5. "#2 is a console change on a dependency. People forget these."
6. Scroll to *Delivery*: checkout-api's change failure rate.
7. Close with: "the same answer is in Slack automatically and in Claude through MCP."
