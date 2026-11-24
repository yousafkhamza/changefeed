// changefeed project site: interactive demo and integration tabs.
// The data and scoring mirror internal/demo and internal/rank in the Go code.
(() => {
  "use strict";
  const KINDS = { deploy: "Deploy", config: "Config", infra: "Infra", flag: "Flag", access: "Access", scale: "Scale" };
  const LANES = [["github", "GitHub"], ["argocd", "Argo CD"], ["kubernetes", "Kubernetes"], ["helm", "Helm"], ["terraform", "Terraform"], ["aws", "AWS"], ["flags", "Flags"]];
  const DEPS = { "checkout-api": ["payments-api", "cart", "orders-db"] };
  const PLATFORM = ["platform", "ci-deployer"];
  const W = { same: 40, dep: 25, platform: 10, recency: 25, high: 20, medium: 10, outside: 4, linked: 4 };
  const INC = { t: 14 * 60 + 32, service: "checkout-api", env: "prod" };
  const START = 12 * 60 + 30, END = 15 * 60;

  // [minutes before alert, source, kind, service, env, actor, summary, risk, revision, changeset, outsideCI, diff]
  const ROWS = [
    [111, "github", "deploy", "search", "prod", "d.ivanova", "PR #398 merged: tune search ranking", "low", "3f9a1c2d5e", "gh:shop/search#398", false],
    [105, "argocd", "deploy", "search", "prod", "argocd (auto-sync)", "Synced search to 3f9a1c2", "low", "3f9a1c2d5e", "", false],
    [94, "terraform", "infra", "platform", "prod", "terraform-ci", "Terraform apply: 0 to add, 1 to change, 0 to replace, 0 to destroy", "medium", "", "", false, ["~ module.eks.aws_eks_node_group.general"]],
    [87, "flags", "flag", "web", "prod", "m.silva", "Flag new-nav enabled for 25% of users", "low", "", "", false, ["~ rollout: 0% → 25%"]],
    [80, "kubernetes", "scale", "cart", "prod", "horizontal-pod-autoscaler", "Scaled cart 4 → 7 replicas", "low", "", "", false, ["~ replicas: 4 → 7"]],
    [72, "aws", "access", "ci-deployer", "prod", "PlatformAdmin/a.okafor", "iam AttachRolePolicy on ci-deployer (console)", "high", "", "", true, ["+ policyArn: arn:aws:iam::aws:policy/AmazonRDSFullAccess", "+ roleName: ci-deployer"]],
    [58, "github", "deploy", "payments-api", "prod", "j.mensah", "PR #407 merged: retry budget for PSP calls", "medium", "a17e0d4b9c", "gh:shop/payments#407", false],
    [51, "argocd", "deploy", "payments-api", "prod", "argocd (auto-sync)", "Synced payments-api to a17e0d4", "medium", "a17e0d4b9c", "", false],
    [49, "kubernetes", "deploy", "payments-api", "prod", "argocd-controller", "Rollout payments-api: payments:a17e0d4", "medium", "a17e0d4", "", false, ["~ image app: ghcr.io/shop/payments:5be21aa → ghcr.io/shop/payments:a17e0d4"]],
    [40, "flags", "flag", "checkout-api", "prod", "m.silva", "Flag fast-checkout ramped 10% → 50%", "medium", "", "", true, ["~ rollout: 10% → 50%"]],
    [30, "kubernetes", "config", "checkout-api", "prod", "kubectl-edit", "ConfigMap checkout-config changed (2 keys)", "high", "9c2b7e1f00", "", true, ["- PAYMENT_TIMEOUT_MS: 3000", "+ PAYMENT_TIMEOUT_MS: 800", "+ PSP_BREAKER_THRESHOLD: 5"]],
    [26, "github", "deploy", "checkout-api", "prod", "n.rahman", "PR #412 merged: lower PSP timeout, add circuit breaker", "high", "9c2b7e1f00", "gh:shop/checkout#412", false],
    [21, "argocd", "deploy", "checkout-api", "prod", "argocd (auto-sync)", "Synced checkout-api to 9c2b7e1", "high", "9c2b7e1f00", "", false, ["health: Progressing", "destination: https://kubernetes.default.svc checkout"]],
    [18, "kubernetes", "deploy", "checkout-api", "prod", "argocd-controller", "Rollout checkout-api: checkout:9c2b7e1", "medium", "9c2b7e1", "", false, ["~ image app: ghcr.io/shop/checkout:4d81f30 → ghcr.io/shop/checkout:9c2b7e1"]],
    [13, "aws", "infra", "orders-db", "prod", "PlatformAdmin/a.okafor", "rds ModifyDBParameterGroup on orders-db (console)", "high", "", "", true, ["+ dBParameterGroupName: orders-db", "+ parameters[0].parameterName: max_connections", "+ parameters[0].parameterValue: 400"]],
    [7, "terraform", "infra", "platform", "staging", "terraform-ci", "Terraform apply: 0 to add, 1 to change, 0 to replace, 0 to destroy", "medium", "", "", false, ["~ aws_elasticache_replication_group.cache"]],
    [4, "helm", "deploy", "web", "prod", "helm", "helm release web revision 31 deployed", "medium", "", "", false],
    [-4, "kubernetes", "scale", "checkout-api", "prod", "horizontal-pod-autoscaler", "Scaled checkout-api 8 → 14 replicas", "low", "", "", false, ["~ replicas: 8 → 14"]],
    [-6, "flags", "flag", "checkout-api", "prod", "m.silva", "Flag fast-checkout rolled back to 10%", "low", "", "", false, ["~ rollout: 50% → 10%"]],
  ];

  const short = (r) => (/^[0-9a-f]{7}/i.test(r) ? r.slice(0, 7).toLowerCase() : r);
  const fmt = (t) => String(Math.floor(t / 60)).padStart(2, "0") + ":" + String(t % 60).padStart(2, "0");
  const E = [];
  // Same linking rules as store.Insert.
  ROWS.forEach((r, i) => {
    const e = { id: "demo_" + String(i).padStart(2, "0"), t: INC.t - r[0], source: r[1], kind: r[2], service: r[3], env: r[4], actor: r[5], summary: r[6], risk: r[7], revision: r[8], changeset: r[9], outsideCI: r[10], diff: r[11] || [] };
    e.hhmm = fmt(e.t);
    const rev7 = short(e.revision);
    if (e.changeset && rev7) E.forEach((o) => { if (short(o.revision) === rev7 && o.changeset === "rev:" + rev7) o.changeset = e.changeset; });
    else if (!e.changeset && rev7) {
      const prev = E.filter((o) => short(o.revision) === rev7 && o.changeset).sort((a, b) => b.t - a.t)[0];
      e.changeset = prev ? prev.changeset : "rev:" + rev7;
    }
    E.push(e);
  });

  const state = { env: "prod", look: 60, kinds: new Set(Object.keys(KINDS)), tab: "sus", sel: "demo_12" };
  const $ = (id) => document.getElementById(id);
  const pct = (t) => ((t - START) / (END - START) * 100).toFixed(3) + "%";
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const visible = (e) => (state.env === "all" || e.env === state.env) && state.kinds.has(e.kind);
  const srcName = (s) => (LANES.find((l) => l[0] === s) || [s, s])[1];

  // Same weights and rules as internal/rank.
  function scoreEvent(e) {
    const dt = INC.t - e.t;
    if (dt <= 0 || dt > state.look || e.env !== INC.env) return null;
    let s = 0; const why = [];
    if (e.service === INC.service) { s += W.same; why.push("same service"); }
    else if ((DEPS[INC.service] || []).includes(e.service)) { s += W.dep; why.push("dependency of " + INC.service); }
    else if (PLATFORM.includes(e.service)) { s += W.platform; why.push("shared platform"); }
    else return null;
    if (dt < 60) s += Math.floor(W.recency * (1 - dt / 60));
    why.push(dt + " min before");
    if (e.risk === "high") { s += W.high; why.push("high-risk change"); }
    else if (e.risk === "medium") { s += W.medium; why.push("medium risk"); }
    if (e.outsideCI) { s += W.outside; why.push("made outside CI"); }
    return { s, why, dt };
  }
  function suspects() {
    const groups = new Map();
    for (const e of E) {
      if (!visible(e)) continue;
      const sc = scoreEvent(e); if (!sc) continue;
      const lone = e.changeset.startsWith("rev:") && !E.some((o) => o.id !== e.id && o.changeset === e.changeset);
      const key = !e.changeset || lone ? e.id : e.changeset;
      const g = groups.get(key) || { key, members: [], best: null };
      g.members.push(e);
      if (!g.best || sc.s > g.best.sc.s || (sc.s === g.best.sc.s && e.t > g.best.e.t)) g.best = { e, sc };
      groups.set(key, g);
    }
    const out = [...groups.values()].map((g) => {
      const gh = g.members.find((m) => m.source === "github");
      let score = g.best.sc.s; const why = [...g.best.sc.why];
      if (g.members.length >= 3) { score += W.linked; why.push(g.members.length + " linked events"); }
      return { ...g, title: gh ? gh.summary : g.best.e.summary, score: Math.min(score, 99), why };
    });
    return out.sort((a, b) => b.score - a.score || b.best.e.t - a.best.e.t);
  }

  function renderKinds() {
    $("kinds").innerHTML = Object.entries(KINDS).map(([k, label]) =>
      `<button type="button" class="kchip" style="--c:var(--k-${k})" data-k="${k}" aria-pressed="${state.kinds.has(k)}"><i></i>${label}</button>`).join("");
  }
  function renderStrip() {
    const ticks = []; for (let t = START; t <= END; t += 15) ticks.push(t);
    let h = "";
    LANES.forEach(([key, label], i) => {
      h += `<div class="lane-label" style="grid-row:${i + 1}">${label}</div><div class="track" style="grid-row:${i + 1}">`;
      h += ticks.map((t) => `<span class="gridline" style="left:${pct(t)}"></span>`).join("");
      for (const e of E.filter((x) => x.source === key)) {
        const vis = visible(e);
        h += `<button type="button" class="ev risk-${e.risk}${vis ? "" : " dim"}${e.id === state.sel ? " sel" : ""}" style="left:${pct(e.t)};--c:var(--k-${e.kind})" data-id="${e.id}" aria-label="${esc(e.hhmm + " " + e.summary)}" title="${esc(e.hhmm + " UTC · " + e.summary)}"${vis ? "" : ' tabindex="-1"'}></button>`;
      }
      h += "</div>";
    });
    const n = LANES.length;
    h += `<div style="grid-row:${n + 1}"></div><div class="axis" style="grid-row:${n + 1}">${ticks.map((t) => `<span class="tick" style="left:${pct(t)}">${fmt(t)}</span>`).join("")}</div>`;
    const from = Math.max(START, INC.t - state.look);
    h += `<div class="overlay" style="grid-row:1 / span ${n}"><div class="shade" style="left:${pct(from)};width:calc(${pct(INC.t)} - ${pct(from)})"></div><div class="incline" style="left:${pct(INC.t)}"><span>14:32 alert</span></div></div>`;
    $("strip").innerHTML = h;
  }
  function renderList() {
    const sus = suspects();
    const all = E.filter(visible).slice().reverse();
    $("n-sus").textContent = sus.length; $("n-all").textContent = all.length;
    $("t-sus").setAttribute("aria-selected", state.tab === "sus");
    $("t-all").setAttribute("aria-selected", state.tab === "all");
    let h;
    if (state.tab === "sus") {
      h = !sus.length ? `<p class="empty">No changes to checkout-api or its dependencies in the last ${state.look} minutes for this filter. Widen the look-back window or switch env to prod.</p>`
        : `<ol class="list">` + sus.map((g, i) => {
          const sel = g.members.some((e) => e.id === state.sel);
          return `<li><button type="button" class="sus${sel ? " sel" : ""}" data-id="${g.best.e.id}">
            <span class="rank">${i + 1}</span>
            <span><span class="sus-title">${esc(g.title)}</span>
              <span class="sus-meta">${g.best.e.hhmm} UTC · T-${g.best.sc.dt}m · ${esc(g.best.e.service)}${g.members.length > 1 ? " · " + g.members.length + " events" : ""}</span>
              <span class="reasons">${g.why.map((w) => `<span>${esc(w)}</span>`).join("")}</span></span>
            <span class="score"><b>${g.score}</b><span class="bar"><i style="width:${g.score}%"></i></span></span>
          </button></li>`;
        }).join("") + "</ol>";
    } else {
      h = '<ul class="list">';
      let marked = false;
      for (const e of all) {
        if (!marked && e.t < INC.t) { h += '<li class="divider-inc">14:32 checkout-api 5xx alert</li>'; marked = true; }
        h += `<li><button type="button" class="row${e.id === state.sel ? " sel" : ""}${e.t > INC.t ? " after" : ""}" data-id="${e.id}" style="--c:var(--k-${e.kind})"><span class="t">${e.hhmm}</span><span class="k"></span><span class="s">${esc(e.summary)}<small>${srcName(e.source)} · ${esc(e.service)} · ${e.env}</small></span></button></li>`;
      }
      h += "</ul>";
    }
    $("p-list").innerHTML = h;
  }
  function renderDetail() {
    const e = E.find((x) => x.id === state.sel); if (!e) { $("detail").innerHTML = ""; return; }
    const dt = INC.t - e.t;
    const setEvents = e.changeset && !e.changeset.startsWith("rev:") || E.filter((x) => x.changeset === e.changeset).length > 1 ? E.filter((x) => x.changeset === e.changeset) : [];
    const diff = e.diff.length ? `<div class="diff">${e.diff.map((l) => `<span class="${l[0] === "-" ? "m" : l[0] === "+" ? "p2" : ""}">${esc(l)}</span>`).join("\n")}</div>` : "";
    const chain = setEvents.length > 1 ? `<div><p class="eyebrow" style="margin-bottom:6px">Change set · ${esc(e.changeset)}</p><div class="chain">${setEvents.map((x, i) => `${i ? "<b>→</b>" : ""}<span class="${x.id === e.id ? "cur" : ""}">${x.hhmm} ${srcName(x.source)}</span>`).join("")}</div></div>` : "";
    const ce = { specversion: "1.0", id: e.id, type: "changefeed." + e.kind, source: e.source + "/demo", subject: e.service, time: `2026-11-24T${e.hhmm}:00Z`,
      data: { env: e.env, summary: e.summary, actor: e.actor, risk: e.risk, ...(e.revision ? { revision: e.revision } : {}), ...(e.changeset ? { changeset: e.changeset } : {}), ...(e.outsideCI ? { outsideCI: true } : {}), ...(e.diff.length ? { diff: e.diff } : {}) } };
    $("detail").innerHTML = `
      <div class="d-head"><span class="pill" style="--c:var(--k-${e.kind})">${KINDS[e.kind]}</span><span class="risk ${e.risk}">${e.risk} risk</span>${e.outsideCI ? '<span class="risk">outside CI</span>' : ""}<span class="eyebrow">${dt > 0 ? `T-${dt}m before alert` : `${-dt}m after alert`}</span></div>
      <p class="d-title">${esc(e.summary)}</p>
      <dl class="kv"><dt>Time</dt><dd>${e.hhmm}:00 UTC</dd><dt>Source</dt><dd>${srcName(e.source)}</dd><dt>Service</dt><dd>${esc(e.service)} · ${e.env}</dd><dt>Actor</dt><dd>${esc(e.actor)}</dd>${e.revision ? `<dt>Revision</dt><dd>${e.revision}</dd>` : ""}</dl>
      ${diff}${chain}
      <details class="json"><summary>Show as CloudEvent JSON</summary><pre class="code">${esc(JSON.stringify(ce, null, 2))}</pre></details>`;
  }
  function render() { renderStrip(); renderList(); renderDetail(); }

  document.addEventListener("click", (ev) => {
    const k = ev.target.closest(".kchip");
    if (k) { const key = k.dataset.k; state.kinds.has(key) ? state.kinds.delete(key) : state.kinds.add(key); renderKinds(); render(); return; }
    const pick = ev.target.closest("[data-id]");
    if (pick && pick.closest("#demo")) { state.sel = pick.dataset.id; render(); return; }
    if (ev.target.closest("#t-sus")) { state.tab = "sus"; renderList(); }
    if (ev.target.closest("#t-all")) { state.tab = "all"; renderList(); }
  });
  $("f-env").addEventListener("change", (e) => { state.env = e.target.value; render(); });
  $("f-look").addEventListener("change", (e) => { state.look = +e.target.value; render(); });

  // ---------- integrations ----------
  const c = (s) => `<span class="c">${s}</span>`, k = (s) => `<span class="k">${s}</span>`, v = (s) => `<span class="v">${s}</span>`;
  const I = [
    { id: "helm", name: "Kubernetes", hint: "Helm chart, 2 minutes",
      text: "Installs the server with the Kubernetes collector on. It watches Deployments, StatefulSets, DaemonSets and ConfigMaps, and optionally Helm releases. List namespaces to get a namespaced Role instead of a ClusterRole.",
      code: `${c("# values.yaml")}
${k("config")}:
  ${k("collectors")}:
    ${k("kubernetes")}:
      ${k("env")}: ${v("prod")}
      ${k("cluster")}: ${v("prod-eu-1")}
      ${k("watch")}: [deployments, statefulsets, configmaps, helm]
  ${k("auth")}:
    ${k("tokens")}:
      - { ${k("name")}: ${v("oncall")}, ${k("sha256")}: ${v('"&lt;from changefeed token&gt;"')}, ${k("scopes")}: [read] }
  ${k("services")}:
    ${k("checkout-api")}: { ${k("dependsOn")}: [payments-api, cart, orders-db] }

${c("$")} helm install changefeed oci://ghcr.io/yousafkhamza/charts/changefeed \\
    -n changefeed --create-namespace -f values.yaml`,
      steps: ["Create tokens with `changefeed token`. Only the SHA-256 hash goes in the config.", "Each service is named from its app.kubernetes.io/name label, or the changefeed/service annotation."] },
    { id: "gha", name: "GitHub Actions", hint: "one step, OIDC",
      text: "Add one step after your deploy. It signs in with the workflow's OIDC token, so no secret is stored. The PR number, commit SHA and run link are added for you.",
      code: `${k("permissions")}:
  ${k("id-token")}: write    ${c("# lets changefeed verify this workflow")}
  ${k("contents")}: read

${k("steps")}:
  - ${k("uses")}: ${v("yousafkhamza/changefeed@v0.1.0")}
    ${k("with")}:
      ${k("endpoint")}: ${v("https://changefeed.example.com")}
      ${k("service")}: ${v("checkout-api")}
      ${k("env")}: ${v("prod")}
      ${k("risk")}: ${v("medium")}`,
      steps: ["On the server, enable auth.github and list allowed repositories, e.g. [my-org/*].", "If changefeed is unreachable the step warns and your deploy continues. Set fail-on-error: true to block instead."] },
    { id: "argo", name: "Argo CD", hint: "notifications webhook",
      text: "Argo CD notifications sends each sync to changefeed. The Git revision links the sync to its PR and to the rollout that follows.",
      code: `${c("# merge into argocd-notifications-cm (full file: deploy/argocd/)")}
${k("service.webhook.changefeed")}: |
  ${k("url")}: ${v("http://changefeed.changefeed.svc.cluster.local/hooks/argocd")}
  ${k("headers")}:
    - ${k("name")}: ${v("Authorization")}
      ${k("value")}: ${v("Bearer $changefeed-token")}
${k("template.changefeed-sync")}: |
  ${c("# app, revision, phase, health, initiatedBy, labels …")}
${k("trigger.changefeed-on-sync-succeeded")}: |
  - ${k("when")}: app.status.operationState.phase in ['Succeeded']
    ${k("send")}: [changefeed-sync]`,
      steps: ["Store a token limited to sources: [argocd] in argocd-notifications-secret as changefeed-token.", "Subscribe apps: notifications.argoproj.io/subscribe.changefeed-on-sync-succeeded.changefeed: \"\""] },
    { id: "aws", name: "AWS CloudTrail", hint: "EventBridge, no AWS keys",
      text: "A Terraform module forwards successful write calls from CloudTrail to changefeed. Read-only and failed calls are dropped inside AWS. Undelivered events go to a dead-letter queue with an optional alarm.",
      code: `${k("module")} ${v('"changefeed_cloudtrail"')} {
  ${k("source")}   = ${v('"github.com/yousafkhamza/changefeed//deploy/terraform/aws-cloudtrail?ref=v0.1.0"')}
  ${k("endpoint")} = ${v('"https://changefeed.example.com"')}
  ${k("token")}    = var.changefeed_aws_token  ${c("# scopes [write], sources [aws]")}
  ${k("services")} = [${v('"rds"')}, ${v('"iam"')}, ${v('"ec2"')}, ${v('"eks"')}, ${v('"elasticloadbalancing"')}]
}`,
      steps: ["Console changes are marked outside CI, which raises their score.", "Map account IDs to envs with hooks.aws.accounts in the server config."] },
    { id: "tf", name: "Terraform", hint: "plan JSON or planlens",
      text: "Send each terraform apply with the resources it changed, destructive changes first. If you pass a planlens report, its risk level and findings are used.",
      code: `${c("$")} terraform show -json tf.plan > plan.json
${c("$")} terraform apply tf.plan
${c("$")} changefeed emit --service platform --env prod --from-plan plan.json
${c("created chg_4b1e… (change set -)")}
${c("# Terraform apply: 2 to add, 1 to change, 1 to replace, 0 to destroy")}`,
      steps: ["Works in any CI: set CHANGEFEED_ENDPOINT and CHANGEFEED_TOKEN.", "Add --revision $GIT_SHA to link the apply to its PR."] },
    { id: "any", name: "Anything else", hint: "CLI or CloudEvents",
      text: "Feature flags, runbooks and scripts can send a CloudEvent over HTTP, or call the CLI.",
      code: `${c("$")} changefeed emit --service checkout-api --env prod --kind flag \\
    --source flags --outside-ci "Flag fast-checkout ramped 10% → 50%"

${c("$")} curl -X POST https://changefeed.example.com/api/v1/events \\
    -H "Authorization: Bearer $CHANGEFEED_TOKEN" \\
    -H "Content-Type: application/cloudevents+json" \\
    -d '{"specversion":"1.0","type":"changefeed.config","source":"runbook/prod",
         "subject":"cart","data":{"env":"prod","summary":"Raised redis maxmemory"}}'`,
      steps: ["Events with the same id are stored once, so retries are safe.", "Up to 100 events per request."] },
  ];
  let itab = "helm";
  function renderInteg() {
    $("itabs").innerHTML = I.map((x) => `<button type="button" role="tab" class="itab" data-i="${x.id}" aria-selected="${x.id === itab}"><b>${x.name}</b><small>${x.hint}</small></button>`).join("");
    const x = I.find((y) => y.id === itab);
    $("ipanel").innerHTML = `<h3>${x.name}</h3><p>${x.text}</p><div class="codebox"><pre class="code" id="code-${x.id}">${x.code}</pre><button class="copy" type="button" data-copy="code-${x.id}">Copy</button></div><ul class="steps">${x.steps.map((s) => `<li>${esc(s)}</li>`).join("")}</ul>`;
  }
  $("itabs").addEventListener("click", (ev) => { const b = ev.target.closest("[data-i]"); if (b) { itab = b.dataset.i; renderInteg(); } });

  document.addEventListener("click", async (ev) => {
    const b = ev.target.closest("[data-copy]"); if (!b) return;
    const el = $(b.dataset.copy);
    try { await navigator.clipboard.writeText(el.textContent); b.textContent = "Copied"; }
    catch { const r = document.createRange(); r.selectNodeContents(el); const s = getSelection(); s.removeAllRanges(); s.addRange(r); b.textContent = "Selected"; }
    setTimeout(() => { b.textContent = "Copy"; }, 1600);
  });

  renderKinds(); render(); renderInteg();
})();
