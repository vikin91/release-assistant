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

function makePage({ sections = [], storage = new Map(), hash = "", configDraft = false, storageFails = false, activityLog, fetchActivity } = {}) {
  const byId = new Map(sections.filter((section) => section.id).map((section) => [section.id, section]));
  if (activityLog) {
    byId.set("activity-log", activityLog);
    byId.set("activity-refresh-error", { hidden: true, textContent: "" });
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
    body: { dataset: { loading: "false" } },
    activeElement: null,
    querySelectorAll(selector) {
      if (selector === "details") return sections;
      if (selector === "[data-open-section]") return links;
      return [];
    },
    querySelector(selector) {
      if (selector === "[data-config-draft]" && configDraft) return {};
      return null;
    },
    getElementById(id) {
      return byId.get(id) || null;
    },
  });
  const window = emitter({
    sessionStorage,
    location: { hash, reload() { reloads += 1; } },
    setInterval(callback) { intervals.push(callback); },
    setTimeout(callback) { timeouts.push(callback); },
  });
  return {
    document,
    window,
    links,
    intervals,
    timeouts,
    reloads: () => reloads,
    start() {
      vm.runInNewContext(appSource, { document, window, fetch: fetchActivity, AbortSignal });
      document.emit("DOMContentLoaded");
    },
    stored() {
      return JSON.parse(storage.get(storageKey) || "{}");
    },
  };
}

function savedState(states) {
  return new Map([[storageKey, JSON.stringify(states)]]);
}

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

test("captures a pending details change before automatic reload", () => {
  const section = makeDetails({ id: "release-configuration", open: true });
  const page = makePage({ storage: savedState({ "release-configuration": false }), sections: [section] });
  page.start();
  section.open = true;
  page.intervals[0]();
  assert.deepEqual(page.stored(), { "release-configuration": true });
  assert.equal(page.reloads(), 1);
});

test("saves section state on non-configuration form submission and pagehide", () => {
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
  page.links[0].emit("click");
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

test("configuration submission clears only its saved choice while a rejected draft stays open", () => {
  const config = makeDetails({ id: "release-configuration", open: false });
  const other = makeDetails({ id: "repo-panel", open: false });
  const page = makePage({
    storage: savedState({ "release-configuration": false, "repo-panel": true, "unrendered-panel": false }),
    sections: [config, other],
  });
  page.start();
  page.document.emit("submit", { target: { matches: (selector) => selector === ".config-grid" } });
  assert.deepEqual(page.stored(), { "repo-panel": true, "unrendered-panel": false });
  page.window.emit("pagehide");
  assert.deepEqual(page.stored(), { "repo-panel": true, "unrendered-panel": false });

  const draftConfig = makeDetails({ id: "release-configuration", open: true });
  const draftPage = makePage({
    storage: savedState({ "release-configuration": false }),
    sections: [draftConfig],
    configDraft: true,
  });
  draftPage.start();
  assert.equal(draftConfig.open, true);
});

test("skips automatic reload when session storage cannot save state", () => {
  const page = makePage({ sections: [makeDetails({ id: "repo-panel", open: true })], storageFails: true });
  page.start();
  page.intervals[0]();
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

test("follows new entries initially and resumes following when the reader returns to the bottom", async () => {
  const storage = new Map();
  const activityLog = makeActivityLog();
  let text = activityLog.textContent;
  const fetchActivity = async () => activityResponse(text);
  const page = makePage({ storage, activityLog, fetchActivity });
  page.start();
  await new Promise(setImmediate);
  assert.equal(activityLog.scrollTop, activityLog.scrollHeight - activityLog.clientHeight);

  activityLog.scrollTop -= 10;
  activityLog.emit("scroll");
  const pausedTop = activityLog.scrollTop;
  text += "new entry\n";
  await page.timeouts.shift()();
  assert.equal(activityLog.scrollTop, pausedTop);

  activityLog.scrollTop = activityLog.scrollHeight;
  activityLog.emit("scroll");
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

test("preserves the reading position while the activity panel is collapsed, including across reloads", async () => {
  const storage = new Map();
  const activityLog = makeActivityLog();
  const details = makeDetails({ id: "activity-details", open: true });
  let text = activityLog.textContent;
  const fetchActivity = async () => activityResponse(text);
  const page = makePage({ storage, sections: [details], activityLog, fetchActivity });
  page.start();
  await new Promise(setImmediate);
  activityLog.scrollTop = 250;
  activityLog.emit("scroll");

  activityLog.hidden = true;
  details.open = false;
  details.emit("toggle");
  text += "new entry\n";
  await page.timeouts.shift()();
  page.window.emit("pagehide");

  const reloadedLog = makeActivityLog();
  reloadedLog.hidden = true;
  const reloadedDetails = makeDetails({ id: "activity-details", open: false });
  const reloaded = makePage({ storage, sections: [reloadedDetails], activityLog: reloadedLog, fetchActivity });
  reloaded.start();
  await new Promise(setImmediate);
  reloadedLog.hidden = false;
  reloadedDetails.open = true;
  reloadedDetails.emit("toggle");
  assert.equal(reloadedLog.scrollTop, 250);
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
