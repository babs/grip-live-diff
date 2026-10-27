// e2e for spec 010 (010-AC8, 010-AC11): drives headless Chrome over CDP against a running gld. Args: <server port> <cdp port> <work dir>
const fs = require("fs");
const path = require("path");
const { spawnSync, spawn } = require("child_process");

const [port, cdpPort, work] = process.argv.slice(2);
if (!work || !process.env.XDG_CONFIG_HOME) {
  console.error("usage: manual-trigger.js <server port> <cdp port> <work dir>, with XDG_CONFIG_HOME set");
  process.exit(2);
}
const docs = path.join(work, "docs");
const md = path.join(docs, "test.md");
const sidecar = path.join(docs, "test.annotations.json5");
const ready = path.join(docs, "test.annotations.ready");
const sleepPidFile = path.join(work, "sleep.pid");
// os.UserConfigDir ignores XDG_CONFIG_HOME on macOS; run.sh points gld's HOME at <work>/home.
const settingsFile = process.platform === "darwin"
  ? path.join(work, "home", "Library", "Application Support", "grip-live-diff", "settings.json")
  : path.join(process.env.XDG_CONFIG_HOME, "grip-live-diff", "settings.json");

let ws, seq = 0;
const waiting = new Map();
function send(method, params = {}) {
  // A send on a closed socket is dropped silently and would never settle.
  if (ws.readyState !== WebSocket.OPEN) return Promise.reject(new Error("CDP socket closed"));
  const id = ++seq;
  ws.send(JSON.stringify({ id, method, params }));
  // A reply that never comes (dialog, never-settling promise) would hang the run with the socket open.
  return new Promise((res, rej) => {
    const t = setTimeout(() => {
      waiting.delete(id);
      rej(new Error("CDP " + method + " got no reply within 10 s"));
    }, 10000);
    waiting.set(id, { res: (v) => (clearTimeout(t), res(v)), rej: (e) => (clearTimeout(t), rej(e)) });
  });
}
async function ev(expr) {
  const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitFor(expr, ms = 4000) {
  const t0 = Date.now();
  let lastErr;
  for (;;) {
    try {
      if (await ev(expr)) return Date.now() - t0;
    } catch (e) {
      lastErr = e;
    }
    if (Date.now() - t0 > ms) throw new Error("timeout: " + expr + (lastErr ? " (last error: " + lastErr.message + ")" : ""));
    await sleep(100);
  }
}
const ui = () =>
  ev(`(() => { const m = document.getElementById("annotation-mode"), g = document.getElementById("annotation-go"), c = document.getElementById("annotation-cancel");
    return { mode: m.hidden ? "(hidden)" : m.textContent + " pressed=" + m.getAttribute("aria-pressed"), go: g.hidden ? "(hidden)" : g.textContent + (g.disabled ? " [disabled: " + g.title + "]" : " [enabled]"),
      cancel: c.hidden ? "(hidden)" : "visible", err: document.getElementById("annotation-trigger-error").textContent, marker: window.__marker || null } })()`);
const sidecarJSON = () => JSON.parse(fs.readFileSync(sidecar, "utf8").replace(/^\/\/.*$/gm, ""));
let failures = 0;
const consoleErrors = [];
let completed = false;
// A pending promise nobody settles lets node drain and exit 0 without a verdict.
process.on("beforeExit", () => {
  if (completed) return;
  console.error("ERROR run ended before completion");
  process.exit(3);
});
function check(label, ok, extra) {
  if (!ok) failures++;
  console.log((ok ? "PASS " : "FAIL ") + label + (extra !== undefined ? "  " + JSON.stringify(extra) : ""));
}
// Waits until no annotations GET has started for 1 s: pending sidecar events have landed.
async function settle() {
  let last = await ev(`window.__gets`), since = Date.now();
  while (Date.now() - since < 1000) {
    await sleep(100);
    const now = await ev(`window.__gets`);
    if (now !== last) (last = now), (since = Date.now());
  }
}
const click = (id) => ev(`document.getElementById(${JSON.stringify(id)}).click()`);

(async () => {
  const list = await (await fetch(`http://127.0.0.1:${cdpPort}/json/list`)).json();
  ws = new WebSocket(list.find((t) => t.type === "page").webSocketDebuggerUrl);
  ws.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.method === "Runtime.consoleAPICalled" && d.params.type === "error") consoleErrors.push("console: " + d.params.args.map((x) => x.description || x.value).join(" ").slice(0, 200));
    if (d.method === "Runtime.exceptionThrown") consoleErrors.push("exception: " + (d.params.exceptionDetails.exception?.description || d.params.exceptionDetails.text).slice(0, 200));
    if (d.method === "Page.javascriptDialogOpening") {
      check("no browser dialog", false, d.params.message);
      send("Page.handleJavaScriptDialog", { accept: false }).catch(() => {});
    }
    if (d.id && waiting.has(d.id)) {
      const w = waiting.get(d.id);
      waiting.delete(d.id);
      if (d.error) w.rej(new Error("CDP " + d.error.message));
      else w.res(d.result);
    }
  };
  ws.onclose = () => {
    for (const w of waiting.values()) w.rej(new Error("CDP socket closed"));
    waiting.clear();
  };
  await new Promise((r) => (ws.onopen = r));
  await send("Page.enable");
  await send("Runtime.enable");
  // Page hooks: GET counter, one-shot GET delay, forced PUT failure, live-region mutation log.
  await send("Page.addScriptToEvaluateOnNewDocument", { source: `
    window.__liveLog = []; window.__gets = 0; window.__delayNextGet = 0; window.__failPut = false;
    const f = window.fetch;
    window.fetch = function (u, o) {
      const url = String(u), m = (o && o.method) || "GET";
      if (url.startsWith("/__annotations?") && m === "GET") {
        window.__gets++;
        if (window.__delayNextGet) {
          const d = window.__delayNextGet; window.__delayNextGet = 0;
          return f.apply(this, arguments).then((r) => new Promise((res) => setTimeout(() => res(r), d)));
        }
      }
      if (url.startsWith("/__annotations?") && m === "PUT" && window.__failPut) return Promise.reject(new Error("forced"));
      return f.apply(this, arguments);
    };
    // Registered before annotate.js: swallows the websocket's sidecar event while __blockAnn.
    window.__blockAnn = false;
    window.__blocked = 0;
    window.addEventListener("gld:annotations", (e) => { if (window.__blockAnn) window.__blocked++, e.stopImmediatePropagation(); }, true);
    document.addEventListener("DOMContentLoaded", () => {
      const s = document.getElementById("annotation-trigger-state");
      if (!s) return;
      window.__liveLog.push("initial aria-live=" + s.getAttribute("aria-live"));
      new MutationObserver((ms) => ms.forEach((m) => window.__liveLog.push(m.type === "attributes" ? "attr:" + m.attributeName : "text:" + s.textContent)))
        .observe(s, { attributes: true, childList: true, characterData: true, subtree: true });
    });` });
  await send("Page.navigate", { url: `http://127.0.0.1:${port}/test.md` });
  await waitFor(`document.readyState === "complete" && !!document.getElementById("annotate-toggle")`);
  await click("annotate-toggle");
  await waitFor(`!document.getElementById("annotation-mode").hidden`);
  await ev(`window.__marker = "m1"`);

  // An entry, typed in the page.
  await click("annotation-document");
  await ev(`document.querySelector(".annot-box textarea").value = "rework the intro"`);
  await ev(`document.querySelector(".annot-save").click()`);
  await waitFor(`!document.querySelector(".annot-box").matches(":popover-open")`);
  const before = sidecarJSON();
  console.log("start ui", await ui());
  check("auto shown, Go hidden", (await ui()).mode.startsWith("auto") && (await ui()).go === "(hidden)");

  // Mode toggle.
  await click("annotation-mode");
  await waitFor(`document.getElementById("annotation-mode").textContent === "manual"`);
  const after = sidecarJSON();
  check("sidecar trigger manual on disk", after.trigger === "manual", after.trigger);
  check("entries intact", JSON.stringify(after.annotations) === JSON.stringify(before.annotations), after.annotations.map((a) => [a.id, a.thread]));
  console.log("manual ui", await ui());

  // Go.
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  check("ready file written", fs.existsSync(ready), fs.readFileSync(ready, "utf8").trim());
  console.log("sent ui", await ui());

  // Agent claims with a live PID.
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: process.pid, claimed_at: new Date().toISOString() }));
  const tw = await waitFor(`document.getElementById("annotation-go").textContent === "Agent working · pid ${process.pid}"`);
  console.log("working ui", await ui(), "after", tw, "ms");
  check("working without reload", (await ui()).marker === "m1");

  // Dead PID.
  const dead = spawnSync("sh", ["-c", "echo $$"]).stdout.toString().trim();
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: Number(dead) }));
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent gone · pid ${dead}"`);
  const gone = await ui();
  console.log("gone ui", gone);
  check("gone: Go enabled again, ✕ shown", gone.go.endsWith("[enabled]") && gone.cancel === "visible");
  check("gone label readable (opacity 1)", (await ev(`getComputedStyle(document.getElementById("annotation-go")).opacity`)) === "1");

  // Ready file removed by hand (AC8).
  fs.unlinkSync(ready);
  const tg = await waitFor(`document.getElementById("annotation-go").textContent === "Go"`, 3000);
  check("Go back after manual rm, ms=" + tg + ", no reload", (await ui()).marker === "m1", await ui());

  // A GET sent before ✕ but answered after it must not bring "working" back.
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: process.pid, claimed_at: new Date().toISOString() }));
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent working · pid ${process.pid}"`);
  await sleep(300);
  await ev(`window.__delayNextGet = 1500; window.dispatchEvent(new Event("gld:annotations"))`);
  await sleep(100);
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);
  await sleep(2000);
  check("late GET dropped: still Go after ✕", (await ui()).go.startsWith("Go ") && !fs.existsSync(ready), await ui());

  // Cancel, with focus handed between Go and ✕ and the state announced.
  await ev(`document.getElementById("annotation-go").focus()`);
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  check("focus Go -> Go disabled -> focus on ✕", (await ev(`document.activeElement.id`)) === "annotation-cancel", await ev(`document.activeElement.id`));
  check("live region says Sent", (await ev(`(() => { const s = document.getElementById("annotation-trigger-state"); return s.getAttribute("aria-live") + ":" + s.textContent })()`)) === "polite:Sent");
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);
  check("cancel removed ready file", !fs.existsSync(ready), await ui());
  check("✕ hidden -> focus on Go", (await ev(`document.activeElement.id`)) === "annotation-go", await ev(`document.activeElement.id`));
  check("mode aria-label fixed, aria-pressed carries state", (await ev(`(() => { const m = document.getElementById("annotation-mode"); return m.getAttribute("aria-label") + "|" + m.getAttribute("aria-pressed") })()`)) === "Manual trigger|true");

  // Hold on.
  await click("settings-toggle");
  await ev(`document.querySelector("input[name=settings-hold_reload]").click()`);
  await waitFor(`document.documentElement.getAttribute("data-hold-reload") === "true"`);
  await sleep(300);
  check("settings.json hold_reload true", JSON.parse(fs.readFileSync(settingsFile, "utf8")).hold_reload === true, fs.readFileSync(settingsFile, "utf8").trim());
  await ev(`document.getElementById("settings-menu").hidePopover()`);
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  fs.appendFileSync(md, "\nheld line\n");
  await sleep(2000);
  check("hold on + sent: md write did not reload", (await ui()).marker === "m1");
  fs.unlinkSync(ready);
  const tr = await waitFor(`!window.__marker && document.readyState === "complete"`, 4000);
  check("ready rm -> page reloaded (" + tr + " ms), new text in page", await ev(`document.body.innerText.includes("held line")`));
  await waitFor(`!document.getElementById("annotation-mode").hidden`);
  await ev(`window.__marker = "m2"`);
  await sleep(1500);
  check("reloaded once (marker m2 survives)", (await ui()).marker === "m2");

  // Agent crashes without touching disk; poll turns it gone and the held reload runs.
  const child = spawn("sleep", ["60"], { stdio: "ignore" });
  fs.writeFileSync(sleepPidFile, String(child.pid));
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: child.pid, claimed_at: new Date().toISOString() }));
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent working · pid ${child.pid}"`);
  // The liveness poll leaves a selection in the list alone.
  await settle();
  await ev(`(() => { const t = document.querySelector(".annotations-list .annot-msg span"); const r = document.createRange(); r.selectNodeContents(t); getSelection().removeAllRanges(); getSelection().addRange(r); })()`);
  const gets0 = await ev(`window.__gets`);
  await sleep(6500);
  const sel = await ev(`getSelection().toString()`), gets1 = await ev(`window.__gets`);
  check("poll ran (" + (gets1 - gets0) + " GET) and list selection kept", gets1 > gets0 && sel !== "", sel);
  await ev(`getSelection().removeAllRanges()`);
  fs.appendFileSync(md, "\ncrash line\n");
  await sleep(1500);
  check("working + hold on: md write held", (await ui()).marker === "m2");
  const tk = Date.now();
  process.kill(child.pid);
  await new Promise((r) => child.once("exit", r));
  fs.unlinkSync(sleepPidFile);
  await waitFor(`!window.__marker && document.readyState === "complete" && document.body.innerText.includes("crash line")`, 7000);
  check("child killed -> held reload ran after " + (Date.now() - tk) + " ms", true);
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent gone · pid ${child.pid}"`);
  await ev(`window.__marker = "m2b"`);
  await sleep(1500);
  check("reloaded page shows gone, reloaded once", (await ui()).marker === "m2b", await ui());
  const log = await ev(`window.__liveLog`);
  check("live region: no aria-live in HTML, label written before aria-live", JSON.stringify(log) === JSON.stringify(["initial aria-live=null", "text:Agent gone · pid " + child.pid, "attr:aria-live"]) && (await ev(`document.getElementById("annotation-trigger-state").getAttribute("aria-live")`)) === "polite", log);
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);
  await ev(`window.__marker = "m2"`);

  // Hold off: same situation, reload at once.
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  await ev(`document.querySelector("input[name=settings-hold_reload]").click()`);
  await waitFor(`document.documentElement.getAttribute("data-hold-reload") === "false"`);
  await sleep(300);
  check("settings.json hold_reload false", JSON.parse(fs.readFileSync(settingsFile, "utf8")).hold_reload === false);
  const t0 = Date.now();
  fs.appendFileSync(md, "\nfree line\n");
  await waitFor(`!window.__marker && document.body.innerText.includes("free line")`, 6000).catch(async (e) => {
    console.log("DIAG", await ev(`({ marker: window.__marker, text: document.body.innerText.includes("free line"), ready: document.readyState, bubble: !document.querySelector(".annot-bubble").hidden, box: document.querySelector(".annot-box").matches(":popover-open"), hold: document.documentElement.getAttribute("data-hold-reload"), go: document.getElementById("annotation-go").textContent, sel: getSelection().toString() })`), "disk:", fs.readFileSync(md, "utf8").includes("free line"), fs.existsSync(ready) && fs.readFileSync(ready, "utf8"));
    await sleep(3000);
    console.log("DIAG+3s", await ev(`({ marker: window.__marker, text: document.body.innerText.includes("free line") })`));
    throw e;
  });
  check("hold off + sent: md write reloaded in " + (Date.now() - t0) + " ms", true);
  await waitFor(`!document.getElementById("annotation-mode").hidden`);
  await ev(`window.__marker = "m3"`);
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);

  // Agent answers: nothing open.
  const sc = sidecarJSON();
  sc.annotations[0].thread.push({ by: "agent", at: new Date().toISOString(), text: "done" });
  sc.annotations[0].status = "done";
  fs.writeFileSync(sidecar, JSON.stringify(sc, null, 2));
  await waitFor(`document.getElementById("annotation-go").disabled`);
  check("no open entry: Go disabled, no reload", (await ui()).marker === "m3", await ui());

  // Gone with nothing open still offers ✕.
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: Number(dead) }));
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent gone · pid ${dead}"`);
  const g2 = await ui();
  check("gone + nothing open: Go disabled, ✕ visible", g2.go.includes("[disabled") && g2.cancel === "visible", g2);
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);
  check("✕ cleared the gone batch", !fs.existsSync(ready));

  // Forced click on the disabled Go: the server's 409 shows inline and refetches.
  await ev(`(() => { const g = document.getElementById("annotation-go"); g.disabled = false; g.click(); })()`);
  await waitFor(`document.getElementById("annotation-trigger-error").textContent !== "" && document.getElementById("annotation-go").disabled`);
  const e = await ui();
  check("409 inline message, Go disabled again", e.err.startsWith("Not sent") && e.go.includes("[disabled"), e.err);
  check("no ready file after 409", !fs.existsSync(ready));
  // A new open entry makes the message stale.
  await click("annotation-document");
  await ev(`document.querySelector(".annot-box textarea").value = "one more"`);
  await ev(`document.querySelector(".annot-save").click()`);
  await waitFor(`!document.getElementById("annotation-go").disabled`);
  check("entry added -> error cleared, Go enabled", (await ui()).err === "", await ui());

  // A failed save survives refreshes until the box closes.
  await ev(`document.querySelector(".annotations-item").click()`);
  await waitFor(`document.querySelector(".annot-box").matches(":popover-open")`);
  await ev(`window.__failPut = true; document.querySelector(".annot-box textarea").value = "edited text"; document.querySelector(".annot-save").click()`);
  await waitFor(`document.querySelector(".annot-meta").textContent.includes("Save failed: forced")`);
  const getsA = await ev(`window.__gets`);
  // The event the websocket fires on a sidecar change. No unlocked disk rewrite here: a GET
  // racing its truncation reads a half-written sidecar (500), which an agent avoids by flock.
  await ev(`window.dispatchEvent(new Event("gld:annotations"))`);
  await waitFor(`window.__gets > ${getsA}`);
  await sleep(800);
  await ev(`window.dispatchEvent(new Event("gld:annotations"))`);
  await sleep(800);
  const m2 = await ev(`({ meta: document.querySelector(".annot-meta").textContent, text: document.querySelector(".annot-box textarea").value, open: document.querySelector(".annot-box").matches(":popover-open"), gets: window.__gets })`);
  check("save error kept across refresh (" + (m2.gets - getsA) + " GETs), box open, text kept", m2.meta.includes("Save failed: forced") && m2.text === "edited text" && m2.open && m2.gets > getsA, Object.assign(m2, { getsA, errs: consoleErrors }));
  await ev(`window.__failPut = false; document.querySelector(".annot-cancel").click()`);
  await ev(`document.querySelector(".annotations-item").click()`);
  await waitFor(`document.querySelector(".annot-box").matches(":popover-open")`);
  check("reopened box: error gone", !(await ev(`document.querySelector(".annot-meta").textContent`)).includes("Save failed"), await ev(`document.querySelector(".annot-meta").textContent`));
  await ev(`document.querySelector(".annot-cancel").click()`);

  // The agent deletes the entry of the open box; only the poll sees it.
  await click("annotation-go");
  await waitFor(`document.getElementById("annotation-go").textContent === "Sent"`);
  fs.writeFileSync(ready, JSON.stringify({ sent_at: new Date().toISOString(), pid: process.pid, claimed_at: new Date().toISOString() }));
  await waitFor(`document.getElementById("annotation-go").textContent === "Agent working · pid ${process.pid}"`);
  await settle();
  const victim = sidecarJSON().annotations.find((a) => a.status !== "done");
  await ev(`document.querySelectorAll(".annotations-item")[1].click()`);
  await waitFor(`document.querySelector(".annot-box").matches(":popover-open")`);
  check("box open on the entry to delete", (await ev(`document.querySelector(".annot-box textarea").value`)) === victim.thread[victim.thread.length - 1].text, victim.thread.map((m) => m.text));
  await ev(`document.querySelector(".annot-box textarea").value = "typed for A"; window.__blockAnn = true`);
  const sc3 = sidecarJSON();
  sc3.annotations = sc3.annotations.filter((a) => a.id !== victim.id);
  const b0 = await ev(`window.__blocked`);
  fs.writeFileSync(sidecar + ".tmp", JSON.stringify(sc3, null, 2));
  const tR = Date.now();
  fs.renameSync(sidecar + ".tmp", sidecar);
  const tb = await waitFor(`window.__blocked > ${b0}`, 3000).catch(() => -1);
  check("sidecar event swallowed while box open", tb >= 0 && (await ev(`document.querySelector(".annot-box").matches(":popover-open")`)), { tb, blocked: await ev(`window.__blocked`) });
  const closed = await waitFor(`!document.querySelector(".annot-box").matches(":popover-open")`, 7000).catch(() => -1);
  const tc = closed < 0 ? -1 : Date.now() - tR;
  check("entry deleted on disk -> poll closed its box (" + tc + " ms, >= 500)", tc >= 500);
  await ev(`document.querySelector(".annot-save").click()`);
  await sleep(1000);
  const after3 = sidecarJSON().annotations;
  check("later Save created nothing", after3.length === 1 && !JSON.stringify(after3).includes("typed for A"), after3.map((a) => a.thread.map((m) => m.text)));
  await ev(`window.__blockAnn = false`);
  await click("annotation-cancel");
  await waitFor(`document.getElementById("annotation-go").textContent === "Go"`);

  // Back to auto.
  await click("annotation-mode");
  await waitFor(`document.getElementById("annotation-mode").textContent === "auto"`);
  check("auto hides Go and cancel", (await ui()).go === "(hidden)" && (await ui()).cancel === "(hidden)", await ui());
  check("sidecar trigger auto on disk", sidecarJSON().trigger === "auto");

  // The forced PUT failure logs one console.error by design; any exception is a bug.
  const forced = consoleErrors.filter((m) => /^console: Error: forced\b/.test(m));
  check("no page exceptions / console errors", forced.length <= 1 && consoleErrors.length === forced.length, consoleErrors);

  console.log(failures ? failures + " FAILURE(S)" : "ALL PASS");
  completed = true;
  ws.close();
  process.exit(failures ? 1 : 0);
})().catch((e) => {
  console.error("ERROR", e);
  process.exit(2);
});
