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

function makePage({ sections = [], storage = new Map(), hash = "", configDraft = false, storageFails = false } = {}) {
  const byId = new Map(sections.filter((section) => section.id).map((section) => [section.id, section]));
  const links = [];
  const intervals = [];
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
  });
  return {
    document,
    window,
    links,
    intervals,
    reloads: () => reloads,
    start() {
      vm.runInNewContext(appSource, { document, window });
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
