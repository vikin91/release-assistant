const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const appSource = fs.readFileSync(path.join(__dirname, "static/app.js"), "utf8");
const storageKey = "release-assistant:sections";

function emitter(properties = {}) {
  const listeners = new Map();
  return Object.assign(properties, {
    addEventListener(type, callback) {
      const callbacks = listeners.get(type) || [];
      callbacks.push(callback);
      listeners.set(type, callbacks);
    },
    emit(type, event = {}) {
      for (const callback of listeners.get(type) || []) callback(event);
    },
  });
}

function makeDetails({ id = "", foldKey = "", open = false } = {}) {
  return emitter({ id, dataset: foldKey ? { foldKey } : {}, open, tagName: "DETAILS" });
}

function makePage({ sections = [], storage = new Map(), session = "session:1", hash = "", search, storageFails = false, activityLog, fetchActivity, livePage, fetchDashboard, fetchQueue, parseDashboard, clipboard, jumpToLatest } = {}) {
  const byId = new Map(sections.filter((section) => section.id).map((section) => [section.id, section]));
  if (activityLog) {
    byId.set("activity-log", activityLog);
    byId.set("activity-refresh-error", { hidden: true, textContent: "" });
    if (jumpToLatest) byId.set("activity-jump-latest", jumpToLatest);
  }
  if (livePage) {
    byId.set("live-dashboard", livePage.dashboard);
    byId.set("dashboard-refresh-status", livePage.status);
  }
  const links = [];
  const intervals = [];
  const timeouts = [];
  let reloads = 0;
  const sessionStorage = {
    getItem(key) {
      if (storageFails) throw new Error("storage unavailable");
      return storage.get(key) ?? null;
    },
    setItem(key, value) {
      if (storageFails) throw new Error("storage unavailable");
      storage.set(key, value);
    },
  };
  const document = emitter({
    body: { dataset: { loading: "false", session } },
    activeElement: null,
    querySelectorAll(selector) {
      if (selector === "details") return livePage ? livePage.dashboard.querySelectorAll("details") : sections;
      if (selector === "[data-open-section]") return links;
      return livePage ? livePage.dashboard.querySelectorAll(selector) : [];
    },
    querySelector(selector) {
      if (selector === "header" && livePage) return livePage.header;
      return null;
    },
    getElementById(id) {
      return byId.get(id) || (livePage ? livePage.dashboard.querySelectorAll("[id]").find((node) => node.id === id) : null) || null;
    },
  });
  for (const section of sections) section.addEventListener("toggle", () => document.emit("toggle", { target: section }));
  if (livePage) document.body.dataset = { ...livePage.body.dataset };
  const window = emitter({
    sessionStorage,
    navigator: { clipboard },
    location: { hash, search: search ?? (livePage?.body.dataset.activeTab ? `?tab=${livePage.body.dataset.activeTab}` : ""), reload() { reloads += 1; } },
    setInterval(callback) { intervals.push(callback); },
    setTimeout(callback, delay) { callback.delay = delay; timeouts.push(callback); return callback; },
    clearTimeout(callback) { const index = timeouts.indexOf(callback); if (index >= 0) timeouts.splice(index, 1); },
    scrollX: 0, scrollY: 0,
    scrollTo(x, y) { this.scrollX = x; this.scrollY = y; },
  });
  window.history = {
    pushState(_state, _title, target) { this.replaceState(_state, _title, target); },
    replaceState(_state, _title, target) { const parsed = new URL(target, "http://localhost"); window.location.search = parsed.search; window.location.hash = parsed.hash; },
  };
  return {
    document,
    window,
    links,
    intervals,
    timeouts,
    reloads: () => reloads,
    start() {
      vm.runInNewContext(appSource, {
        document, window, AbortSignal, URLSearchParams,
        FormData: class {
          constructor(form) { return form.querySelectorAll('input[type="hidden"]').map(({ name, value }) => [name, value]); }
        },
        fetch: (url, options) => ({
          "/activity": fetchActivity,
          "/action": fetchQueue,
        }[url] || fetchDashboard)(url, options),
        DOMParser: class { parseFromString(text) { return parseDashboard(text); } },
      });
      document.emit("DOMContentLoaded");
    },
    stored() {
      return JSON.parse(storage.get(storageKey))?.sections || {};
    },
  };
}

function savedState(sections, session = "session") {
  return new Map([[storageKey, JSON.stringify({ session, sections })]]);
}

test("copies the exact command text and reports clipboard failures", async () => {
  const command = "cdrox\ngit status\n";
  const writes = [];
  const clipboard = { writeText: async (text) => writes.push(text) };
  const page = makePage({ clipboard });
  const status = { textContent: "" };
  const block = { querySelector: (selector) => ({ "pre code": { textContent: command }, "[data-copy-status]": status })[selector] };
  const button = { dataset: { copyText: "" }, closest: () => block };
  const click = () => page.document.emit("click", {
    target: { closest: (selector) => selector === "[data-copy-text]" ? button : null },
  });
  page.start();
  click();
  await new Promise(setImmediate);
  assert.deepEqual(writes, [command]);
  assert.equal(status.textContent, "Copied");
  page.timeouts.shift()();
  assert.equal(status.textContent, "");

  clipboard.writeText = async () => { throw new Error("clipboard denied"); };
  click();
  await new Promise(setImmediate);
  assert.equal(status.textContent, "Select and copy the text manually.");
});

test("opens topic panels on a new app run even when the browser tab has saved folds", () => {
  const storage = savedState({ "previous-cycles": false, "release-images": false });
  const sections = ["previous-cycles", "release-images"].map((id) => makeDetails({ id, open: true }));
  const page = makePage({ storage, sections, session: "restarted:1" });
  page.start();
  assert.ok(sections.every(({ open }) => open));
});

test("starts a fresh tag choice collapsed even when revisiting the same selected tag", () => {
  const foldKey = "tag-switch:fact:acme/fact@1.0";
  const chooser = makeDetails({ foldKey, open: false });
  chooser.dataset.transient = "";
  const page = makePage({ storage: savedState({ [foldKey]: true }), sections: [chooser] });
  page.start();
  assert.equal(chooser.open, false);
});

test("restores exact open and closed choices across changed markup defaults", () => {
  const storage = savedState({ configuration: false, "repo:one": true });
  const first = makePage({
    storage,
    sections: [makeDetails({ id: "configuration", open: true }), makeDetails({ foldKey: "repo:one", open: false })],
  });
  first.start();
  assert.equal(first.document.querySelectorAll("details")[0].open, false);
  assert.equal(first.document.querySelectorAll("details")[1].open, true);

  const next = makePage({
    storage,
    session: "session:2",
    sections: [makeDetails({ foldKey: "repo:one", open: true }), makeDetails({ id: "configuration", open: true })],
  });
  next.start();
  assert.equal(next.document.querySelectorAll("details")[0].open, true);
  assert.equal(next.document.querySelectorAll("details")[1].open, false);
});

test("uses stable nested identities and leaves new panels at their markup defaults", () => {
  const storage = savedState({ "repo:alpha/pr:17": false, "repo:beta/tag:v2": true });
  const page = makePage({
    storage,
    sections: [
      makeDetails({ foldKey: "repo:beta/tag:v2", open: false }),
      makeDetails({ foldKey: "repo:gamma/pr:21", open: true }),
      makeDetails({ foldKey: "repo:alpha/pr:17", open: true }),
    ],
  });
  page.start();
  const sections = page.document.querySelectorAll("details");
  assert.deepEqual(sections.map(({ open }) => open), [true, true, false]);
  assert.deepEqual(page.stored(), { "repo:alpha/pr:17": false, "repo:beta/tag:v2": true });
});

test("captures section changes without reloading the page", () => {
  const section = makeDetails({ id: "component-images", open: true });
  const page = makePage({ storage: savedState({ "component-images": false }), sections: [section] });
  page.start();
  section.open = true;
  section.emit("toggle");
  assert.deepEqual(page.stored(), { "component-images": true });
  assert.equal(page.reloads(), 0);
  assert.equal(page.intervals.length, 0);
});

test("saves section state on form submission and pagehide", () => {
  const section = makeDetails({ id: "repo-panel", open: false });
  const page = makePage({ sections: [section] });
  page.start();
  section.open = true;
  page.document.emit("submit", { target: { matches: () => false } });
  assert.deepEqual(page.stored(), { "repo-panel": true });
  section.open = false;
  page.window.emit("pagehide");
  assert.deepEqual(page.stored(), { "repo-panel": false });
});

test("explicit navigation opens and stores a section; same-hash reload preserves later manual close", () => {
  const target = makeDetails({ id: "repo-target", open: false });
  const page = makePage({ sections: [target], storage: savedState({ "repo-target": false }) });
  page.links.push(emitter({ hash: "#repo-target" }));
  page.start();
  page.document.emit("click", { target: { closest: (selector) => selector === "[data-open-section]" ? page.links[0] : null }, preventDefault() {} });
  assert.equal(target.open, true);
  assert.deepEqual(page.stored(), { "repo-target": true });

  target.open = false;
  target.emit("toggle");
  page.window.location.hash = "#repo-target";
  page.window.emit("hashchange");
  assert.equal(target.open, true);
  assert.deepEqual(page.stored(), { "repo-target": true });

  target.open = false;
  target.emit("toggle");
  assert.deepEqual(page.stored(), { "repo-target": false });
  const reloaded = makePage({ storage: savedState({ "repo-target": false }), sections: [makeDetails({ id: "repo-target", open: true })], hash: "#repo-target" });
  reloaded.start();
  assert.equal(reloaded.document.querySelectorAll("details")[0].open, false);
});

test("section changes tolerate unavailable session storage", () => {
  const page = makePage({ sections: [makeDetails({ id: "repo-panel", open: true })], storageFails: true });
  page.start();
  page.document.emit("toggle");
  assert.equal(page.reloads(), 0);
});

function makeActivityLog() {
  let text = "entry\n".repeat(40);
  let top = 0;
  return emitter({
    hidden: false,
    get clientHeight() { return this.hidden ? 0 : 100; },
    get scrollHeight() { return this.hidden ? 0 : text.split("\n").length * 20; },
    get scrollTop() { return this.hidden ? 0 : top; },
    set scrollTop(value) { top = Math.max(0, Math.min(value, this.scrollHeight - this.clientHeight)); },
    get textContent() { return text; },
    set textContent(value) {
      text = value;
      top = 0;
    },
  });
}

function activityResponse(text) {
  return { ok: true, text: async () => text };
}

test("keeps a reader's scroll position on activity updates and page reloads", async () => {
  const storage = new Map();
  const activityLog = makeActivityLog();
  let text = activityLog.textContent;
  const fetchActivity = async () => activityResponse(text);
  const page = makePage({ storage, activityLog, fetchActivity });
  page.start();
  await new Promise(setImmediate);

  activityLog.scrollTop = 300;
  activityLog.emit("scroll");
  text += "new entry\n";
  await page.timeouts.shift()();
  assert.equal(activityLog.textContent, text);
  assert.equal(activityLog.scrollTop, 300);

  activityLog.scrollTop = 200;
  page.window.emit("pagehide");
  const reloadedLog = makeActivityLog();
  const reloaded = makePage({ storage, activityLog: reloadedLog, fetchActivity });
  reloaded.start();
  await new Promise(setImmediate);
  assert.equal(reloadedLog.scrollTop, 200);
});

test("jumps to the latest activity and resumes following", async () => {
  const storage = new Map();
  const activityLog = makeActivityLog();
  const jumpToLatest = emitter({ hidden: true });
  let logFocused = false;
  activityLog.focus = () => { logFocused = true; };
  let text = activityLog.textContent;
  const fetchActivity = async () => activityResponse(text);
  const page = makePage({ storage, activityLog, fetchActivity, jumpToLatest });
  page.start();
  await new Promise(setImmediate);
  assert.equal(activityLog.scrollTop, activityLog.scrollHeight - activityLog.clientHeight);
  assert.equal(jumpToLatest.hidden, true);

  activityLog.scrollTop -= 10;
  activityLog.emit("scroll");
  assert.equal(jumpToLatest.hidden, false);
  const pausedTop = activityLog.scrollTop;
  text += "new entry\n";
  await page.timeouts.shift()();
  assert.equal(activityLog.scrollTop, pausedTop);

  jumpToLatest.emit("click");
  assert.equal(jumpToLatest.hidden, true);
  assert.equal(logFocused, true);
  text += "another entry\n";
  await page.timeouts.shift()();
  assert.equal(activityLog.scrollTop, activityLog.scrollHeight - activityLog.clientHeight);

  page.window.emit("pagehide");
  const reloadedLog = makeActivityLog();
  const reloaded = makePage({ storage, activityLog: reloadedLog, fetchActivity });
  reloaded.start();
  await new Promise(setImmediate);
  assert.equal(reloadedLog.scrollTop, reloadedLog.scrollHeight - reloadedLog.clientHeight);
});

test("honors scrolling while an activity request is in flight", async () => {
  const activityLog = makeActivityLog();
  let respond;
  const page = makePage({ activityLog, fetchActivity: () => new Promise((resolve) => { respond = resolve; }) });
  page.start();
  activityLog.scrollTop = 200;
  activityLog.emit("scroll");
  respond(activityResponse(activityLog.textContent + "new entry\n"));
  await new Promise(setImmediate);
  assert.equal(activityLog.scrollTop, 200);
});

test("activity updates preserve scrolling when session storage is unavailable", async () => {
  const activityLog = makeActivityLog();
  let text = activityLog.textContent;
  const page = makePage({ activityLog, storageFails: true, fetchActivity: async () => activityResponse(text) });
  page.start();
  await new Promise(setImmediate);
  activityLog.scrollTop = 200;
  activityLog.emit("scroll");
  text += "new entry\n";
  await page.timeouts.shift()();
  assert.equal(activityLog.scrollTop, 200);
});

function matches(node, selector) {
  if (selector === "details" || selector === "button") return node.tagName.toLowerCase() === selector;
  if (selector.startsWith("#")) return node.id === selector.slice(1);
  if (selector === "[hidden]") return node.hidden;
  if (selector === "button:not(:disabled)") return node.tagName === "BUTTON" && !node.disabled;
  if (selector === 'input[name="queue_id"]') return node.tagName === "INPUT" && node.name === "queue_id";
  if (selector === 'input[type="hidden"]') return node.tagName === "INPUT" && node.type === "hidden";
  if (/^\[[^\]]+\]$/.test(selector)) return node.hasAttribute(selector.slice(1, -1));
  return false;
}

function domNode(tag, attrs = {}, children = []) {
  const attributes = new Map(Object.entries(attrs));
  const node = {
    nodeType: 1, nodeName: tag, tagName: tag, childNodes: [], parentNode: null,
    dataset: Object.fromEntries(Object.entries(attrs).filter(([key]) => key.startsWith("data-")).map(([key, value]) => [key.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase()), value])),
    id: attrs.id || "", type: attrs.type || "text", name: attrs.name || "", value: attrs.value || "",
    checked: "checked" in attrs, disabled: "disabled" in attrs, open: "open" in attrs, hidden: "hidden" in attrs, isConnected: true,
    focus() { this.focused = true; },
    matches(selector) { return matches(this, selector); },
    closest(selector) { for (let current = this; current; current = current.parentNode) if (matches(current, selector)) return current; return null; },
    get attributes() { return Array.from(attributes, ([name, value]) => ({ name, value })); },
    get firstChild() { return this.childNodes[0] || null; },
    get nextSibling() { return this.parentNode?.childNodes[this.parentNode.childNodes.indexOf(this) + 1] || null; },
    hasAttribute(name) { return attributes.has(name); },
    getAttribute(name) { return attributes.get(name) ?? null; },
    setAttribute(name, value) { attributes.set(name, value); },
    removeAttribute(name) { attributes.delete(name); },
    insertBefore(child, before) {
      child.remove();
      const index = before ? this.childNodes.indexOf(before) : this.childNodes.length;
      this.childNodes.splice(index, 0, child);
      child.parentNode = this;
    },
    remove() {
      if (this.parentNode) this.parentNode.childNodes.splice(this.parentNode.childNodes.indexOf(this), 1);
      this.parentNode = null;
    },
    querySelectorAll(selector) {
      return this.childNodes.flatMap((child) => [...(matches(child, selector) ? [child] : []), ...child.querySelectorAll(selector)]);
    },
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; },
    cloneNode() { return domNode(tag, Object.fromEntries(attributes), this.childNodes.map((child) => child.cloneNode(true))); },
    classList: { add() {}, remove() {} },
  };
  for (const child of children) node.insertBefore(child, null);
  return node;
}

function livePage({ loading = "false", session = "session:1", activeTab = "component-prs", children = [] } = {}) {
  const status = domNode("P", { id: "dashboard-refresh-status", "data-live-preserve": "" });
  const dashboard = domNode("MAIN", { id: "live-dashboard" }, [status, ...children]);
  const header = domNode("HEADER");
  return {
    status, dashboard, header, body: { dataset: { loading, session, activeTab } },
    getElementById(id) { return id === "live-dashboard" ? dashboard : null; },
    querySelector(selector) { return selector === "header" ? header : null; },
  };
}

function dashboardPage(initial, options = {}) {
  let next = initial;
  const page = makePage({
    livePage: initial,
    fetchDashboard: async () => activityResponse("dashboard"),
    parseDashboard: () => next,
    ...options,
  });
  return Object.assign(page, { updateTo(value) { next = value; } });
}

test("updates keyed rows in place, preserves drafts and focus, and applies saved choices to new details", async () => {
  const original = domNode("ARTICLE", { "data-live-key": "pr:17", class: "pending" });
  const draft = domNode("INPUT", { name: "version", value: "unsaved" });
  const config = domNode("SECTION", { id: "configuration-panel", "data-live-preserve": "" }, [draft]);
  const details = domNode("DETAILS", { id: "pr-selection" }, [original]);
  const initial = livePage({ loading: "true", children: [config, details] });
  const page = dashboardPage(initial, { storage: savedState({ "new-evidence": true }), storageFails: false });
  page.start();
  await new Promise(setImmediate);
  page.document.activeElement = draft;
  details.open = true;
  page.document.emit("toggle", { target: details });
  page.updateTo(livePage({ children: [
    domNode("SECTION", { id: "configuration-panel", "data-live-preserve": "" }),
    domNode("DETAILS", { id: "pr-selection" }, [
      domNode("ARTICLE", { "data-live-key": "pr:18" }),
      domNode("ARTICLE", { "data-live-key": "pr:17", class: "passed" }),
      domNode("DETAILS", { id: "new-evidence" }),
    ]),
  ] }));
  await page.timeouts.shift()();
  assert.equal(details.childNodes[1], original);
  assert.equal(original.getAttribute("class"), "passed");
  assert.equal(details.open, true);
  assert.equal(details.childNodes[2].open, true);
  assert.equal(config.firstChild, draft);
  assert.equal(draft.value, "unsaved");
  assert.equal(page.document.activeElement, draft);
  assert.equal(page.reloads(), 0);
  assert.equal(page.timeouts[0].delay, 60_000);
});

test("keeps the queue search and row identities across a live refresh", async () => {
  const row = (id, job) => domNode("TR", { "data-live-key": `retest:${id}`, "data-retest-row": `${id} ${job}` });
  const queue = (reverse = false) => {
    const search = emitter(domNode("INPUT", { id: "retest-search", type: "search" }));
    const integration = row(7, "integration");
    const upgrade = row(8, "Upgrade");
    return livePage({ activeTab: "retest-queue", children: [
      domNode("DIV", { id: "retest-filter" }, [search]),
      domNode("P", { id: "retest-no-matches", hidden: "" }),
      domNode("SECTION", { "data-retest-bucket": "", "data-live-key": "bucket" }, [
        domNode("TABLE", {}, [
          domNode("TBODY", { "data-retest-group": "PR #42 backport", "data-live-key": "target" },
            reverse ? [upgrade, integration] : [integration, upgrade]),
          domNode("TBODY", { "data-retest-empty": "", hidden: "" }),
        ]),
      ]),
      domNode("SECTION", { "data-retest-bucket": "", "data-live-key": "empty-bucket" }, [
        domNode("TABLE", {}, [domNode("TBODY", { "data-retest-empty": "" })]),
      ]),
    ] });
  };
  const storage = new Map([["release-assistant:retest-search:session:1", "uPgRaDe"]]);
  const initial = queue();
  const page = dashboardPage(initial, { storage });
  page.start();
  await new Promise(setImmediate);
  const search = page.document.getElementById("retest-search");
  const [integration, upgrade] = page.document.querySelectorAll("[data-retest-row]");
  assert.equal(search.value, "uPgRaDe");
  assert.equal(integration.hidden, true);
  assert.equal(upgrade.hidden, false);
  assert.equal(page.document.querySelectorAll("[data-retest-bucket]")[1].hidden, true);
  assert.equal(page.timeouts[0].delay, 5_000);

  page.updateTo(queue(true));
  await page.timeouts.shift()();
  assert.deepEqual(page.document.querySelectorAll("[data-retest-row]"), [upgrade, integration]);
  assert.equal(search.value, "uPgRaDe");
  assert.equal(integration.hidden, true);
  assert.equal(upgrade.hidden, false);

  search.value = "#42";
  search.emit("input");
  assert.equal(integration.hidden, false);
  assert.equal(upgrade.hidden, false);

  search.value = "missing";
  search.emit("input");
  assert.equal(page.document.getElementById("retest-no-matches").hidden, false);
  assert.equal(page.document.querySelectorAll("[data-retest-bucket]")[0].hidden, true);
  assert.equal(storage.get("release-assistant:retest-search:session:1"), "missing");

  search.value = "";
  search.emit("input");
  const [queued, empty] = page.document.querySelectorAll("[data-retest-bucket]");
  assert.equal(queued.querySelector("[data-retest-empty]").hidden, true);
  assert.equal(empty.hidden, false);
  assert.equal(empty.querySelector("[data-retest-empty]").hidden, false);
  assert.equal(page.document.getElementById("retest-no-matches").hidden, true);
});

test("Send all captures visible eligible rows and leaves independent sends running", async () => {
  const row = (id, title, disabled = false) => domNode("TR", {
    "data-live-key": `retest:${id}`, "data-retest-row": `${id} ${title}`,
  }, [
    domNode("SMALL", { "data-retest-sending": "", hidden: "" }),
    domNode("SMALL", { "data-retest-result": "", "data-live-preserve": "" }),
    domNode("FORM", { action: "/action", "data-retest-send": "" }, [
      domNode("INPUT", { type: "hidden", name: "queue_id", value: id }),
      domNode("BUTTON", disabled ? { disabled: "" } : {}),
    ]),
  ]);
  const search = emitter(domNode("INPUT", { id: "retest-search", type: "search" }));
  const button = domNode("BUTTON", { id: "retest-send-all", hidden: "" });
  const rows = [
    row("1", "included"), row("2", "included"), row("3", "included"),
    row("4", "filtered"), row("5", "included", true),
  ];
  const group = domNode("SECTION", { "data-retest-group": "PR #42" }, rows);
  const initial = livePage({ activeTab: "retest-queue", children: [
    domNode("DIV", { id: "retest-filter" }, [search, button]),
    domNode("SECTION", { "data-retest-bucket": "" }, [group]),
  ] });
  const requests = [];
  const page = dashboardPage(initial, {
    fetchQueue: (_url, { body }) => new Promise((resolve) => {
      requests.push({ ids: body.getAll("queue_id"), resolve });
    }),
  });
  page.start();
  await new Promise(setImmediate);
  search.value = "included";
  search.emit("input");
  const send = (item) => {
    let prevented = false;
    page.document.emit("submit", {
      target: item.querySelector("[data-retest-send]"),
      preventDefault() { prevented = true; },
    });
    assert.equal(prevented, true);
  };
  send(rows[0]);
  page.document.emit("click", { target: button });
  send(rows[1]);
  assert.deepEqual(requests.map(({ ids }) => ids), [["1"], ["2", "3"]]);
  assert.equal(button.disabled, true);
  assert.equal(rows[0].querySelector("[data-retest-sending]").hidden, false);

  search.value = "";
  search.emit("input");
  assert.equal(button.disabled, false);
  assert.deepEqual(requests.map(({ ids }) => ids), [["1"], ["2", "3"]]);
  requests[1].resolve({ ok: true, json: async () => ({ "2": "", "3": "PR no longer eligible" }) });
  await new Promise(setImmediate);
  assert.equal(rows[1].parentNode, null);
  assert.equal(rows[2].querySelector("[data-retest-result]").textContent, "PR no longer eligible");
  assert.equal(rows[0].querySelector("button").disabled, true);
  requests[0].resolve({ ok: true, json: async () => ({ "1": "" }) });
  await new Promise(setImmediate);
  assert.equal(rows[0].parentNode, null);
});

test("refreshes the dashboard promptly while Konflux health is unverified", async () => {
  const initial = livePage();
  initial.body.dataset.konfluxHealth = "pending";
  const page = dashboardPage(initial);
  page.start();
  await new Promise(setImmediate);
  assert.equal(page.timeouts[0].delay, 1_000);

  const reached = livePage();
  reached.body.dataset.konfluxHealth = "unverified";
  page.updateTo(reached);
  await page.timeouts.shift()();
  assert.equal(page.timeouts[0].delay, 30_000);

  const healthy = livePage();
  healthy.body.dataset.konfluxHealth = "healthy";
  page.updateTo(healthy);
  await page.timeouts.shift()();
  assert.equal(page.timeouts[0].delay, 60_000);
});

test("serializes requests, pauses while hidden, and discards a response after form submission", async () => {
  let respond;
  let requests = 0;
  const initial = livePage({ loading: "true" });
  const page = dashboardPage(initial, { fetchDashboard: () => { requests++; return new Promise((resolve) => { respond = resolve; }); } });
  page.start();
  page.document.emit("visibilitychange");
  assert.equal(requests, 1);
  page.document.hidden = true;
  respond(activityResponse("dashboard"));
  await new Promise(setImmediate);
  assert.equal(initial.status.textContent, undefined);
  assert.equal(page.timeouts[0].delay, 1_000);
  await page.timeouts.shift()();
  assert.equal(requests, 1);
  page.document.hidden = false;
  page.document.emit("visibilitychange");
  assert.equal(requests, 2);
  page.document.emit("submit", { target: { matches: () => false } });
  respond(activityResponse("dashboard"));
  await new Promise(setImmediate);
  assert.equal(initial.status.textContent, undefined);
  assert.equal(page.reloads(), 0);
});

test("retains the last results on failure, recovers, and rejects a different server session", async () => {
  const row = domNode("ARTICLE", { id: "result", class: "last-success" });
  const initial = livePage({ children: [row] });
  let fail = true;
  const page = dashboardPage(initial, { storageFails: true, fetchDashboard: async () => {
    if (fail) throw new Error("network unavailable");
    return activityResponse("dashboard");
  } });
  page.start();
  await new Promise(setImmediate);
  assert.match(initial.status.textContent, /Cannot refresh release data/);
  assert.equal(initial.dashboard.childNodes[1], row);
  fail = false;
  await page.timeouts.shift()();
  assert.match(initial.status.textContent, /Last updated/);
  page.updateTo(livePage({ session: "another-server:1" }));
  await page.timeouts.shift()();
  assert.match(initial.status.textContent, /Release configuration changed/);
  assert.equal(initial.dashboard.childNodes[1], row);
});

function workflowPage(activeTab = "component-prs") {
  const names = ["component-prs", "component-releases", "acs-prs", "acs-rcs"];
  const tabs = names.map((name) => domNode("A", { id: `tab-${name}`, "data-workflow-tab": name, "aria-selected": name === activeTab ? "true" : "false", tabindex: name === activeTab ? "0" : "-1" }));
  const panels = names.map((name) => domNode("SECTION", { id: `panel-${name}`, "data-workflow-panel": name, ...(name === activeTab ? {} : { hidden: "" }) }, [domNode("DETAILS", { id: `${name}-details` })]));
  return Object.assign(livePage({ activeTab, children: [...tabs, ...panels] }), { tabs, panels });
}

test("keeps independently selected tabs across live updates and form navigation", async () => {
  const initial = workflowPage();
  const page = dashboardPage(initial);
  page.start();
  await new Promise(setImmediate);
  let prevented = false;
  page.document.emit("click", { target: initial.tabs[2], preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(page.window.location.search, "?tab=acs-prs");
  assert.equal(initial.tabs[2].getAttribute("aria-selected"), "true");
  assert.deepEqual(initial.panels.map((panel) => panel.hidden), [true, true, false, true]);
  const refreshed = workflowPage();
  const statusLabel = "3. ACS PRs · Action needed · Decide whether to include 1 PR.";
  refreshed.tabs[2].setAttribute("aria-label", statusLabel);
  refreshed.tabs[2].insertBefore(domNode("SPAN", { class: "tab-status action-needed", "aria-hidden": "true" }), null);
  page.updateTo(refreshed);
  await page.timeouts.shift()();
  assert.equal(initial.tabs[2].getAttribute("aria-selected"), "true");
  assert.equal(initial.tabs[2].getAttribute("tabindex"), "0");
  assert.equal(initial.tabs[2].getAttribute("aria-label"), statusLabel);
  assert.equal(initial.tabs[2].firstChild.getAttribute("class"), "tab-status action-needed");
  assert.deepEqual(initial.panels.map((panel) => panel.hidden), [true, true, false, true]);
  page.document.emit("submit", { target: { matches: () => false } });
  assert.equal(page.window.location.search, "?tab=acs-prs");
});

test("tab keyboard navigation wraps, supports Home and End, and browser history restores selection", () => {
  const initial = workflowPage();
  const page = dashboardPage(initial);
  page.start();
  const key = (index, value) => page.document.emit("keydown", { target: initial.tabs[index], key: value, preventDefault() {} });
  key(0, "ArrowLeft");
  assert.equal(initial.tabs[3].getAttribute("aria-selected"), "true");
  assert.equal(initial.tabs[3].focused, true);
  key(3, "Home");
  assert.equal(initial.tabs[0].getAttribute("aria-selected"), "true");
  key(0, "End");
  assert.equal(initial.tabs[3].getAttribute("aria-selected"), "true");
  page.window.location.search = "?tab=component-releases";
  page.window.emit("popstate");
  assert.equal(initial.tabs[1].getAttribute("aria-selected"), "true");
});

test("a deep section link reveals its activity and retains section choices", () => {
  const initial = workflowPage();
  const page = dashboardPage(initial, { storage: savedState({ "acs-prs-details": false }) });
  page.start();
  const details = initial.panels[2].childNodes[0];
  const link = { hash: "#acs-prs-details", closest(selector) { return selector === "[data-open-section]" ? this : null; } };
  page.document.emit("click", { target: link, preventDefault() {} });
  assert.equal(details.open, true);
  assert.equal(initial.panels[2].hidden, false);
  assert.equal(page.window.location.search, "?tab=acs-prs");
  assert.equal(page.window.location.hash, "#acs-prs-details");
  assert.equal(page.stored()["acs-prs-details"], true);
});


test("restores the selected release's last screen and refreshes that profile", () => {
  const active = workflowPage();
  active.body.dataset.profile = "profile-a";
  const storage = new Map([
    ["release-assistant:tab:session:profile-a", "acs-rcs"],
    ["release-assistant:tab:session:profile-b", "settings"],
  ]);
  const requests = [];
  const page = makePage({
    storage, search: "?release=profile-a", livePage: active,
    fetchDashboard: (url) => { requests.push(url); return new Promise(() => {}); },
  });
  page.start();
  assert.equal(page.document.body.dataset.activeTab, "acs-rcs");
  assert.equal(page.window.location.search, "?release=profile-a&tab=acs-rcs");
  assert.deepEqual(requests, ["/?release=profile-a&tab=acs-rcs"]);
});

test("Remove opens the matching release confirmation dialog", () => {
  const first = emitter({ id: "remove-first", dataset: {}, showModal() { assert.fail("opened the wrong release"); } });
  const second = emitter({ id: "remove-second", dataset: {}, showModal() { this.open = true; } });
  const remove = { getAttribute: (name) => name === "aria-controls" ? second.id : null };
  const page = makePage({ sections: [first, second] });
  page.start();
  page.document.emit("click", { target: { closest: (selector) => selector === "[data-confirm-remove]" ? remove : null } });
  assert.equal(second.open, true);
});
