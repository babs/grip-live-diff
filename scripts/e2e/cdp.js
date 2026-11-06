// Harness of the e2e scripts: drives the page target of headless Chrome over CDP and counts the
// checks.
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let failures = 0;
let completed = false;
const consoleErrors = [];
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

// connect attaches to the page target, records its console errors and exceptions, fails a
// browser dialog, and enables Page and Runtime.
async function connect(cdpPort) {
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
}

// run runs main, then prints the verdict and exits: 1 on a failed check, 2 on an error.
function run(main) {
  main()
    .then(() => {
      console.log(failures ? failures + " FAILURE(S)" : "ALL PASS");
      completed = true;
      ws.close();
      process.exit(failures ? 1 : 0);
    })
    .catch((e) => {
      console.error("ERROR", e);
      process.exit(2);
    });
}

module.exports = { check, connect, consoleErrors, ev, run, send, sleep, waitFor };
