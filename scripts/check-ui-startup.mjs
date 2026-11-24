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
    querySelector() { return null; },
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
  const payload = url === "/api/v1/settings"
    ? { settings: {}, cache: {}, preheat: {} }
    : { items: [], cursor: "", has_more: false };
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
  AbortController
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
console.log("UI startup smoke test passed: initial list loads and settings dialog opens.");
