import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const source = fs.readFileSync(new URL("../internal/webui/assets/app.js", import.meta.url), "utf8");
const cssSource = fs.readFileSync(new URL("../internal/webui/assets/app.css", import.meta.url), "utf8");
const manifestSource = fs.readFileSync(new URL("../internal/webui/manifest.webmanifest", import.meta.url), "utf8");
const elements = new Map();
const listeners = new Map();
const requests = [];

function getElement(selector) {
  if (elements.has(selector)) return elements.get(selector);
  const classes = new Set();
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
      add(name) { classes.add(name); },
      remove(name) { classes.delete(name); },
      toggle(name, force) {
        const enabled = force === undefined ? !classes.has(name) : Boolean(force);
        if (enabled) classes.add(name);
        else classes.delete(name);
        return enabled;
      },
      contains(name) { return classes.has(name); }
    },
    setAttribute(name, value) {
      if (!this.attributes) this.attributes = {};
      this.attributes[name] = String(value);
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

const webFullscreenClick = listeners.get("#player-web-fullscreen")?.get("click");
assert.equal(typeof webFullscreenClick, "function", "The player must expose a separate web fullscreen control");
webFullscreenClick();
assert.equal(getElement("#playback-panel").classList.contains("web-fullscreen"), true, "Web fullscreen must use the CSS layout instead of the Fullscreen API");
assert.equal(getElement("#detail-dialog").classList.contains("player-web-fullscreen"), true, "The detail dialog must expand to the viewport");
assert.equal(getElement("#player-web-fullscreen").textContent, "退出网页全屏", "The web fullscreen control must offer an exit action");
let cancelPrevented = false;
const cancelFullscreen = listeners.get("#detail-dialog")?.get("cancel");
assert.equal(typeof cancelFullscreen, "function", "Escape handling must be bound to the detail dialog");
cancelFullscreen({ preventDefault() { cancelPrevented = true; } });
assert.equal(cancelPrevented, true, "Escape should exit web fullscreen without closing the series dialog");
assert.equal(getElement("#playback-panel").classList.contains("web-fullscreen"), false, "Escape must remove the web fullscreen panel class");
assert.equal(getElement("#detail-dialog").classList.contains("player-web-fullscreen"), false, "Escape must restore the normal detail dialog");
webFullscreenClick();
assert.equal(getElement("#playback-panel").classList.contains("web-fullscreen"), true, "Web fullscreen can be entered repeatedly");
webFullscreenClick();
assert.equal(getElement("#detail-dialog").classList.contains("player-web-fullscreen"), false, "Web fullscreen can be exited with its button");
assert.match(cssSource, /#detail-dialog\.player-web-fullscreen/, "Styles must expand the detail dialog to viewport size");
assert.match(cssSource, /#playback-panel\.web-fullscreen/, "Styles must lay out the controls in web fullscreen");
const episodeGridClick = listeners.get("#detail-content .episode-grid")?.get("click");
assert.equal(typeof episodeGridClick, "function", "The series detail must expose episode selection");
episodeGridClick({
  target: {
    closest(selector) {
      return selector === ".episode-chip"
        ? { dataset: { episodeId: "7100000000000201" }, disabled: false }
        : null;
    }
  }
});
await new Promise((resolve) => setImmediate(resolve));
await new Promise((resolve) => setImmediate(resolve));
const holdDown = listeners.get("#player-hold-speed")?.get("pointerdown");
const holdUp = listeners.get("#player-hold-speed")?.get("pointerup");
assert.doesNotMatch(source, /id="player-hold-speed"/, "The player video toolbar should not add a visually intrusive hold-to-boost button");
assert.match(source, /stage\.addEventListener\("touchstart"/, "The video stage must listen for swipe gestures");
assert.match(source, /changeEpisode\(dy < 0 \? 1 : -1\)/, "Vertical swipes must change episodes");
assert.doesNotMatch(source, /if \(touchMoved \|\| Math\.abs\(dy\) < 55/, "Movement that cancels long-press must not also cancel swipe-to-change-episode");
assert.doesNotMatch(source, /上滑下一集|下滑上一集|临时 2× 倍速/, "The video should not show persistent gesture instructions or a temporary speed text overlay");

const installClick = listeners.get("#install-app-button")?.get("click");
assert.equal(typeof installClick, "function", "Home page must expose an install/add-to-home-screen action");
await installClick();
assert.equal(getElement("#install-dialog").open, true, "The install action must open platform-specific installation guidance");
assert.match(manifestSource, /"display": "standalone"/, "Manifest must request standalone app display");
assert.match(manifestSource, /192x192/, "Manifest must include a 192px icon");
assert.match(manifestSource, /512x512/, "Manifest must include a 512px icon");
console.log("UI smoke test passed: startup, settings, details, web fullscreen, swipe/hold gesture separation, unobtrusive playback UI, installation guide, and PWA manifest.");
