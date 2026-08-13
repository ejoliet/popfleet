"use strict";
// Terminal page: xterm.js + fit addon over /ws/term/{sid}?k=<one-time key>.
// data fields are base64 bytes (PTY output is not valid UTF-8 in general).
//
// Protocol v1e (docs/RDD-v2.md): against the Worker relay every data value is
// base64( nonce(12) || AES-256-GCM ) under HKDF-SHA256(fleet key, salt=sid,
// info="popfleet-v2"), decrypted here with WebCrypto. Whether the relay is
// v1e is decided by GET /config.json — the same probe index.js uses — and
// e2e is enabled BEFORE the socket opens: the relay also announces
// {"t":"e2e"} as its first frame, but that ordering is not guaranteed to
// beat the first PTY output through Cloudflare's handshake, and one
// plaintext-processed frame poisons the whole session (rendered ciphertext,
// keystrokes the agent must reject). The frame stays as a fallback only.
// The fleet key lives in localStorage (not sessionStorage like the admin
// token: losing it per-tab would make every terminal unreadable) and is
// never sent on any request. The v1 Go broker serves no /config.json and
// never sends the e2e frame, so this same file serves both.

function b64encode(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}
function b64decode(s) {
  const bin = atob(s);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

let ended = false;
function banner(msg, clean) {
  if (ended) return; // first cause wins; the ws close that follows must not overwrite it
  ended = true;
  const el = document.getElementById("banner");
  el.textContent = msg + " — close this tab or reopen from the panel";
  el.className = clean ? "clean" : "";
  el.style.display = "block";
}

const term = new Terminal({ cursorBlink: true, fontSize: 14 });
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();
term.focus();

const sid = location.pathname.split("/").pop();
const key = new URLSearchParams(location.search).get("k") || "";
let ws = null; // created in init() after the e2e decision is made

// ---- v1e payload encryption ----

let aead = null; // per-session AES-GCM CryptoKey once e2e is enabled

async function enableE2E() {
  if (aead) return true; // idempotent: config probe and relay frame both call this
  const te = new TextEncoder();
  let b64 = localStorage.getItem("popfleet_e2e") || "";
  for (;;) {
    if (!b64) {
      b64 = (prompt("This fleet is end-to-end encrypted.\nPaste the fleet key (POPFLEET_E2E_KEY):") || "").trim();
      if (!b64) {
        banner("no fleet key: cannot decrypt this session", false);
        return false;
      }
    }
    try {
      const raw = b64decode(b64);
      if (raw.length !== 32) throw new Error("fleet key must be 32 bytes");
      const ikm = await crypto.subtle.importKey("raw", raw, "HKDF", false, ["deriveKey"]);
      aead = await crypto.subtle.deriveKey(
        { name: "HKDF", hash: "SHA-256", salt: te.encode(sid), info: te.encode("popfleet-v2") },
        ikm, { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
      localStorage.setItem("popfleet_e2e", b64);
      return true;
    } catch (e) {
      localStorage.removeItem("popfleet_e2e");
      b64 = "";
    }
  }
}

async function seal(bytes) {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, aead, bytes));
  const out = new Uint8Array(12 + ct.length);
  out.set(nonce); out.set(ct, 12);
  return b64encode(out);
}

async function unseal(wire) {
  const raw = b64decode(wire);
  return new Uint8Array(await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: raw.slice(0, 12) }, aead, raw.slice(12)));
}

// ---- frames ----

function sendResize() {
  fit.fit();
  if (ws && ws.readyState === WebSocket.OPEN)
    ws.send(JSON.stringify({ t: "resize", c: term.cols, r: term.rows }));
}

window.addEventListener("resize", sendResize); // viewer owns geometry

let sendChain = Promise.resolve(); // keystrokes must reach the wire in order
term.onData((d) => {
  const bytes = new TextEncoder().encode(d);
  sendChain = sendChain.then(async () => {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const data = aead ? await seal(bytes) : b64encode(bytes);
    ws.send(JSON.stringify({ t: "in", data }));
  });
});

async function handleFrame(raw) {
  const m = JSON.parse(raw);
  if (m.t === "e2e") await enableE2E(); // fallback path; normally a no-op
  else if (m.t === "out") {
    if (aead) {
      try {
        term.write(await unseal(m.data));
      } catch (e) {
        // GCM auth failure: tampered frame or wrong key. Kill, render nothing.
        // Drop the stored key so the next session re-prompts instead of
        // failing forever on a typo.
        localStorage.removeItem("popfleet_e2e");
        banner("e2e decrypt failed (wrong fleet key or tampered frame) — session killed", false);
        ws.close();
      }
    } else term.write(b64decode(m.data));
  } else if (m.t === "exit") banner("session ended (exit " + m.code + ")", true);
  else if (m.t === "err") banner(m.msg || "session error", false);
}

async function init() {
  // e2e decision first, socket second: nothing is ever processed in the
  // wrong mode. The Go broker 404s the probe -> plain v1.
  const cfg = await fetch("/config.json")
    .then((r) => (r.ok ? r.json() : { e2e: false }))
    .catch(() => ({ e2e: false }));
  if (cfg.e2e && !(await enableE2E())) return; // declined the key: no session

  ws = new WebSocket(
    (location.protocol === "https:" ? "wss://" : "ws://") +
      location.host + "/ws/term/" + sid + "?k=" + encodeURIComponent(key)
  );
  ws.onopen = sendResize;
  let recvChain = Promise.resolve(); // frame order survives the async decrypt
  ws.onmessage = (ev) => {
    recvChain = recvChain.then(() => handleFrame(ev.data)).catch(() => {});
  };
  ws.onclose = () => banner("disconnected from broker", false);
}

init();
