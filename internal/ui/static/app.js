// changefeed UI. Plain DOM, no framework, no inline styles (strict CSP).
"use strict";

const LANE_ORDER = ["github", "argocd", "kubernetes", "helm", "terraform", "aws", "flags", "webhook"];
const LANE_NAMES = { github: "GitHub", argocd: "Argo CD", kubernetes: "Kubernetes", helm: "Helm", terraform: "Terraform", aws: "AWS", flags: "Flags", webhook: "Webhook" };
const KIND_NAMES = { deploy: "Deploy", config: "Config", infra: "Infra", flag: "Flag", access: "Access", scale: "Scale" };
const REFRESH_MS = 30000;

const $ = (id) => document.getElementById(id);
const state = {
  env: "", range: "6h", live: true, kinds: new Set(Object.keys(KIND_NAMES)),
  service: "", at: null, window: "60m", tab: "all", sel: "",
  events: [], suspects: [], timer: 0,
};

// ---------- helpers ----------
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "vars") for (const [p, val] of Object.entries(v)) el.style.setProperty(p, val);
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) el.append(c instanceof Node ? c : String(c));
  return el;
}
const pad = (n) => String(n).padStart(2, "0");
const hhmm = (d) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
const durMs = (s) => { const m = /^(\d+)(m|h)$/.exec(s); return m ? +m[1] * (m[2] === "h" ? 3600e3 : 60e3) : 3600e3; };
const laneName = (s) => LANE_NAMES[s] || s;
const kindVar = (k) => `var(--k-${KIND_NAMES[k] ? k : "scale"})`;

function token() { try { return sessionStorage.getItem("cf-token") || ""; } catch { return ""; } }
function setToken(t) { try { sessionStorage.setItem("cf-token", t); } catch { /* private mode: keep in memory only */ } state.memToken = t; }

async function api(path) {
  const headers = { Accept: "application/json" };
  const t = token() || state.memToken;
  if (t) headers.Authorization = "Bearer " + t;
  const res = await fetch(path, { headers, cache: "no-store" });
  if (res.status === 401) { showLogin(); throw new Error("sign-in required"); }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
  return body;
}

function setStatus(text) { $("status").textContent = text; }

// ---------- URL state ----------
function readURL() {
  const q = new URLSearchParams(location.search);
  state.env = q.get("env") || "";
  state.range = q.get("range") || state.range;
  state.service = q.get("service") || "";
  state.window = q.get("window") || state.window;
  const at = q.get("at");
  state.at = at && !isNaN(Date.parse(at)) ? new Date(at) : null;
  state.sel = q.get("event") || "";
  if (state.service) state.tab = "sus";
}
function writeURL() {
  const q = new URLSearchParams();
  if (state.env) q.set("env", state.env);
  if (state.range !== "6h") q.set("range", state.range);
  if (state.service) { q.set("service", state.service); q.set("window", state.window); }
  if (state.at) q.set("at", state.at.toISOString().slice(0, 16) + "Z");
  if (state.sel) q.set("event", state.sel);
  const s = q.toString();
  history.replaceState(null, "", s ? "?" + s : location.pathname);
}

// ---------- data ----------
async function loadMeta() {
  const m = await api("api/v1/meta");
  const envSel = $("f-env");
  envSel.replaceChildren(...(m.envs.length ? m.envs : ["prod"]).map((e) => h("option", { value: e }, e)));
  if (!state.env) state.env = m.envs.includes("prod") ? "prod" : (m.envs[0] || "prod");
  envSel.value = state.env;
  const svcSel = $("i-service");
  svcSel.replaceChildren(h("option", { value: "" }, "Pick a service…"), ...m.services.map((s) => h("option", { value: s }, s)));
  if (state.service && !m.services.includes(state.service)) svcSel.append(h("option", { value: state.service }, state.service));
  svcSel.value = state.service;
  $("i-window").value = state.window;
  $("f-range").value = state.range;
  if (state.at) $("i-at").value = state.at.toISOString().slice(0, 16);
}

async function refresh() {
  setStatus("Loading…");
  try {
    const until = state.service ? new Date((state.at || new Date()).getTime() + 15 * 60e3) : new Date();
    const since = new Date(until.getTime() - Math.max(durMs(state.range), state.service ? durMs(state.window) + 15 * 60e3 : 0));
    const q = new URLSearchParams({ env: state.env, since: since.toISOString().replace(/\.\d+Z$/, "Z"), until: until.toISOString().replace(/\.\d+Z$/, "Z"), limit: "2000" });
    const [list, sus] = await Promise.all([
      api("api/v1/events?" + q),
      state.service ? api("api/v1/suspects?" + new URLSearchParams({ service: state.service, env: state.env, window: state.window, ...(state.at ? { at: state.at.toISOString().replace(/\.\d+Z$/, "Z") } : {}) })) : null,
    ]);
    state.events = list.events;
    state.domain = [since, until];
    state.suspects = sus ? sus.suspects : [];
    state.incident = sus ? sus.incident : null;
    render();
    setStatus(`${list.events.length} changes · updated ${hhmm(new Date())} UTC`);
  } catch (err) {
    setStatus(err.message);
  }
}

function schedule() {
  clearInterval(state.timer);
  if (state.live && !state.at) state.timer = setInterval(() => { if (!document.hidden) refresh(); }, REFRESH_MS);
}

// ---------- render ----------
const visible = (e) => state.kinds.has(e.kind);

function render() {
  renderKinds();
  renderStrip();
  renderList();
  renderDetail();
  $("i-copy").hidden = !state.service;
}

function renderKinds() {
  $("kinds").replaceChildren(...Object.entries(KIND_NAMES).map(([k, label]) =>
    h("button", { type: "button", class: "kchip", "aria-pressed": String(state.kinds.has(k)), vars: { "--c": kindVar(k) },
      onclick: () => { state.kinds.has(k) ? state.kinds.delete(k) : state.kinds.add(k); render(); } }, h("i"), label)));
}

function renderStrip() {
  const strip = $("strip");
  const evs = state.events.filter(visible);
  if (!state.domain) return;
  const [from, to] = state.domain.map((d) => d.getTime());
  const pct = (t) => `${(((t - from) / (to - from)) * 100).toFixed(3)}%`;
  const lanes = [...new Set(evs.map((e) => e.source))].sort((a, b) => (LANE_ORDER.indexOf(a) + 99) % 99 - (LANE_ORDER.indexOf(b) + 99) % 99 || a.localeCompare(b));
  if (!lanes.length) {
    strip.replaceChildren(h("p", { class: "empty" }, `No changes in ${state.env} for this window. Send one with `,
      h("code", {}, `changefeed emit --service cart --env ${state.env} --kind config "Raised maxmemory"`), "."));
    return;
  }
  const span = to - from;
  const step = [5, 15, 30, 60, 120, 360, 720, 1440].map((m) => m * 60e3).find((s) => span / s <= 10) || 1440 * 60e3;
  const ticks = [];
  for (let t = Math.ceil(from / step) * step; t <= to; t += step) ticks.push(t);
  const nodes = [];
  lanes.forEach((lane, i) => {
    const row = String(i + 1);
    const label = h("div", { class: "lane-label" }, laneName(lane));
    label.style.gridRow = row;
    const track = h("div", { class: "track" }, ticks.map((t) => h("span", { class: "gridline", vars: { left: pct(t) } })));
    track.style.gridRow = row;
    for (const e of evs.filter((x) => x.source === lane)) {
      const t = Date.parse(e.time);
      const dot = h("button", {
        type: "button", class: `ev ${e.risk}${e.id === state.sel ? " sel" : ""}`, "aria-label": `${hhmm(new Date(t))} ${e.summary}`,
        title: `${hhmm(new Date(t))} UTC · ${e.service} · ${e.summary}`, onclick: () => select(e.id),
      });
      dot.style.left = pct(t);
      dot.style.setProperty("--c", kindVar(e.kind));
      track.append(dot);
    }
    nodes.push(label, track);
  });
  const axis = h("div", { class: "axis" }, ticks.map((t) => { const s = h("span", { class: "tick" }, span > 86400e3 ? new Date(t).toISOString().slice(5, 10) + " " + hhmm(new Date(t)) : hhmm(new Date(t))); s.style.left = pct(t); return s; }));
  axis.style.gridRow = String(lanes.length + 1);
  nodes.push(h("div", {}), axis);
  if (state.incident) {
    const at = Date.parse(state.incident.at);
    const start = Math.max(from, at - durMs(state.window));
    const overlay = h("div", { class: "overlay" });
    overlay.style.gridRow = `1 / span ${lanes.length}`;
    const shade = h("div", { class: "shade" }); shade.style.left = pct(start); shade.style.width = `calc(${pct(at)} - ${pct(start)})`;
    const line = h("div", { class: "incline" }, h("span", {}, `${hhmm(new Date(at))} ${state.incident.service}`)); line.style.left = pct(at);
    overlay.append(shade, line);
    nodes.push(overlay);
  }
  strip.replaceChildren(...nodes);
}

function renderList() {
  const box = $("list");
  const evs = state.events.filter(visible);
  $("n-all").textContent = evs.length;
  $("n-sus").textContent = state.service ? state.suspects.length : "";
  $("t-sus").setAttribute("aria-selected", String(state.tab === "sus"));
  $("t-all").setAttribute("aria-selected", String(state.tab === "all"));
  if (state.tab === "sus") {
    if (!state.service) { box.replaceChildren(h("p", { class: "empty" }, "Pick the alerting service and the alert time above. changefeed ranks the changes made to it and its dependencies before the alert.")); return; }
    const sus = state.suspects.filter((s) => visible(s.lead));
    if (!sus.length) { box.replaceChildren(h("p", { class: "empty" }, `No changes to ${state.service} or its dependencies in the ${state.window} before the alert. Widen the look-back window, or add dependencies under services: in the server config.`)); return; }
    box.replaceChildren(h("ol", { class: "list" }, sus.map((s, i) => {
      const sel = s.events.some((e) => e.id === state.sel);
      const fill = h("i", { class: "bar-fill" }); fill.style.width = `${s.score}%`;
      return h("li", {}, h("button", { type: "button", class: `sus${sel ? " sel" : ""}`, onclick: () => select(s.lead.id) },
        h("span", { class: "rank" }, String(i + 1)),
        h("span", {},
          h("span", { class: "title" }, s.title),
          h("span", { class: "meta" }, `${hhmm(new Date(s.lead.time))} UTC · T-${s.minutesBefore}m · ${s.lead.service}${s.events.length > 1 ? ` · ${s.events.length} events` : ""}`),
          h("span", { class: "reasons" }, s.reasons.map((r) => h("span", {}, r)))),
        h("span", { class: "score" }, h("b", {}, String(s.score)), h("span", { class: "bar-track" }, fill))));
    })));
    return;
  }
  if (!evs.length) { box.replaceChildren(h("p", { class: "empty" }, "No changes match these filters.")); return; }
  const at = state.incident ? Date.parse(state.incident.at) : null;
  const items = [];
  let marked = false;
  for (const e of evs) {
    const t = Date.parse(e.time);
    if (at && !marked && t < at) { items.push(h("li", { class: "divider" }, `${hhmm(new Date(at))} alert · ${state.incident.service}`)); marked = true; }
    items.push(h("li", {}, h("button", { type: "button", class: `row${e.id === state.sel ? " sel" : ""}${at && t > at ? " after" : ""}`, vars: { "--c": kindVar(e.kind) }, onclick: () => select(e.id) },
      h("span", { class: "t" }, hhmm(new Date(t))), h("span", { class: "dot" }),
      h("span", {}, h("span", { class: "title" }, e.summary), h("span", { class: "meta" }, `${laneName(e.source)} · ${e.service} · ${e.env}${e.actor ? " · " + e.actor : ""}`)))));
  }
  box.replaceChildren(h("ul", { class: "list" }, items));
}

async function renderDetail() {
  const box = $("detail");
  if (!state.sel) {
    box.replaceChildren(h("p", { class: "empty" }, "Select a change to see its diff, the events linked to it, and the raw CloudEvent."));
    return;
  }
  const id = state.sel;
  let data;
  try { data = await api("api/v1/events/" + encodeURIComponent(id)); } catch (err) { box.replaceChildren(h("p", { class: "empty" }, err.message)); return; }
  if (id !== state.sel) return; // a newer selection won the race
  const e = data.event;
  const t = new Date(e.time);
  const rel = state.incident ? Math.round((Date.parse(state.incident.at) - t) / 60e3) : null;
  const links = Object.entries(e.links || {});
  box.replaceChildren(
    h("div", { class: "d-head" },
      h("span", { class: "pill", vars: { "--c": kindVar(e.kind) } }, KIND_NAMES[e.kind] || e.kind),
      h("span", { class: `risk ${e.risk}` }, `${e.risk} risk`),
      e.outsideCI ? h("span", { class: "risk" }, "outside CI") : null,
      rel !== null ? h("span", { class: "eyebrow" }, rel >= 0 ? `T-${rel}m before alert` : `${-rel}m after alert`) : null),
    h("p", { class: "d-title" }, e.summary),
    h("dl", { class: "kv" },
      h("dt", {}, "Time"), h("dd", { title: t.toString() }, t.toISOString().replace("T", " ").slice(0, 19) + " UTC"),
      h("dt", {}, "Source"), h("dd", {}, laneName(e.source) + (e.origin ? ` · ${e.origin}` : "")),
      h("dt", {}, "Service"), h("dd", {}, `${e.service} · ${e.env}`),
      e.actor ? [h("dt", {}, "Actor"), h("dd", {}, e.actor)] : null,
      e.revision ? [h("dt", {}, "Revision"), h("dd", {}, e.revision)] : null,
      links.length ? [h("dt", {}, "Links"), h("dd", {}, links.flatMap(([name, url], i) => [i ? " · " : "", h("a", { href: url, target: "_blank", rel: "noopener noreferrer" }, name)]))] : null),
    e.diff && e.diff.length ? h("pre", { class: "diff", "aria-label": "Change diff" }, e.diff.flatMap((l, i) => [i ? "\n" : "", h("span", { class: l.startsWith("-") ? "m" : l.startsWith("+") ? "p" : "" }, l)])) : null,
    data.changeset.length > 1 ? h("div", {}, h("p", { class: "eyebrow" }, `Change set · ${e.changeset}`),
      h("div", { class: "chain" }, data.changeset.slice().reverse().flatMap((x, i) => [i ? h("b", {}, "→") : null,
        h("button", { type: "button", class: x.id === e.id ? "cur" : "", onclick: () => select(x.id) }, `${hhmm(new Date(x.time))} ${laneName(x.source)}`)]))) : null,
    h("details", {}, h("summary", {}, "Show as CloudEvent JSON"), h("pre", { class: "json" }, JSON.stringify(data.cloudevent, null, 2))),
  );
}

function select(id) {
  state.sel = id;
  writeURL();
  renderStrip();
  renderList();
  renderDetail();
}

// ---------- login ----------
function showLogin() {
  const d = $("login");
  if (!d.open) d.showModal();
}
$("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const t = $("login-token").value.trim();
  setToken(t);
  try {
    await api("api/v1/whoami");
    $("login").close();
    $("login-err").hidden = true;
    await start();
  } catch (err) {
    $("login-err").textContent = "That token was not accepted. Check it has the read scope.";
    $("login-err").hidden = false;
  }
});

// ---------- wiring ----------
$("filters").addEventListener("change", () => {
  state.env = $("f-env").value; state.range = $("f-range").value; state.live = $("f-live").checked;
  writeURL(); schedule(); refresh();
});
$("filters").addEventListener("submit", (ev) => ev.preventDefault());
$("inv").addEventListener("submit", (ev) => {
  ev.preventDefault();
  state.service = $("i-service").value;
  state.window = $("i-window").value;
  const at = $("i-at").value;
  state.at = at ? new Date(at + "Z") : null;
  state.tab = state.service ? "sus" : "all";
  writeURL(); schedule(); refresh();
});
$("i-clear").addEventListener("click", () => {
  Object.assign(state, { service: "", at: null, incident: null, suspects: [], tab: "all" });
  $("i-service").value = ""; $("i-at").value = "";
  writeURL(); schedule(); refresh();
});
$("i-copy").addEventListener("click", async () => {
  const btn = $("i-copy");
  if (!state.at && state.incident) { state.at = new Date(state.incident.at); writeURL(); }
  try { await navigator.clipboard.writeText(location.href); btn.textContent = "Link copied"; }
  catch { btn.textContent = "Copy from the address bar"; }
  setTimeout(() => { btn.textContent = "Copy link"; }, 1800);
});
$("t-sus").addEventListener("click", () => { state.tab = "sus"; renderList(); });
$("t-all").addEventListener("click", () => { state.tab = "all"; renderList(); });
document.addEventListener("visibilitychange", () => { if (!document.hidden && state.live && !state.at) refresh(); });

async function start() {
  readURL();
  try {
    const who = await api("api/v1/whoami");
    $("ver").textContent = `changefeed ${who.version}`;
    await loadMeta();
  } catch (err) {
    setStatus(err.message);
    return;
  }
  render();
  await refresh();
  schedule();
}
start();
