import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../internal/webui/assets/app.js", import.meta.url), "utf8");
const elements = new Map();
const listeners = new Map();
const requests = [];

function getElement(selector) {
  if (elements.has(selector)) return elements.get(selector);
  const element = {
    selector,
    type: "text",
    value: "",
    checked: false,
    hidden: false,
    disabled: false,
    open: false,
    textContent: "",
    innerHTML: "",
    dataset: {},
    options: [],
    classList: {
      add() {},
      remove() {},
      toggle() {},
      contains() { return false; }
    },
    addEventListener(type, handler) {
      if (!listeners.has(selector)) listeners.set(selector, new Map());
      listeners.get(selector).set(type, handler);
    },
    querySelector(selector) { return getElement(this.selector + " " + selector); },
    querySelectorAll() { return []; },
    showModal() { this.open = true; },
    close() { this.open = false; },
    pause() {},
    load() {},
    removeAttribute() {},
    insertAdjacentHTML() {},
    scrollIntoView() {}
  };
  elements.set(selector, element);
  return element;
}

const window = {
  crypto: { randomUUID: () => "startup-smoke" },
  innerHeight: 800,
  scrollY: 0,
  setTimeout() { return 1; },
  clearTimeout() {},
  setInterval() { return 2; },
  clearInterval() {},
  requestAnimationFrame(callback) { callback(); },
  scrollTo() {},
  addEventListener() {}
};
const document = {
  querySelector: getElement,
  querySelectorAll() { return []; },
  documentElement: { scrollHeight: 1200 },
  addEventListener() {}
};
const localStorage = {
  getItem() { return null; },
  setItem() {}
};
async function fetch(path) {
  const url = String(path);
  requests.push(url);
  let payload;
  if (url === "/api/v1/settings") {
    payload = { settings: {}, cache: {}, preheat: {} };
  } else if (/^\/api\/v1\/series\/[^/]+\/detail$/.test(url)) {
    payload = {
      series: { id: "7100000000000101", title: "Startup Smoke Series" },
      episodes: [{ id: "7100000000000201", number: 1, playable: true }]
    };
  } else {
    payload = { items: [], cursor: "", has_more: false };
  }
  return {
    ok: true,
    status: 200,
    headers: { get() { return null; } },
    json: async () => payload
  };
}

vm.runInNewContext(source, {
  document,
  window,
  localStorage,
  fetch,
  AbortController,
  URLSearchParams
});

await new Promise((resolve) => setImmediate(resolve));
assert.ok(
  requests.some((url) => url.startsWith("/api/v1/discover?channel=")),
  "Opening the page must request the initial Discover list without a manual refresh"
);

const settingsClick = listeners.get("#settings-button")?.get("click");
assert.equal(typeof settingsClick, "function", "The settings button must have a click handler");
await settingsClick();
assert.equal(getElement("#settings-dialog").open, true, "The settings dialog must open");
assert.ok(requests.includes("/api/v1/settings"), "Opening settings must request the runtime settings");

const beforeDetail = requests.length;
const gridClick = listeners.get("#series-grid")?.get("click");
assert.equal(typeof gridClick, "function", "Series grid must have a detail click handler");
gridClick({
  target: {
    closest(selector) {
      return selector === ".series-card"
        ? { dataset: { id: "7100000000000101" } }
        : null;
    }
  }
});
await new Promise((resolve) => setImmediate(resolve));
await new Promise((resolve) => setImmediate(resolve));
const detailRequests = requests.slice(beforeDetail).filter((url) => url.includes("/api/v1/series/7100000000000101"));
assert.equal(detailRequests.length, 1, "Opening a series must make a single combined detail request");
assert.ok(detailRequests[0].endsWith("/detail"), "Series detail must use the combined endpoint");
assert.ok(!detailRequests.some((url) => url.endsWith("/episodes")), "Series detail must not duplicate the upstream request for episodes");
console.log("UI smoke test passed: initial list loads, settings opens, and detail uses one combined request.");
