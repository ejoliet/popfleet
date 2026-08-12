"use strict";
// Terminal page: xterm.js + fit addon over /ws/term/{sid}?k=<one-time key>.
// data fields are base64 bytes (PTY output is not valid UTF-8 in general).

function b64encode(str) {
  const bytes = new TextEncoder().encode(str);
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
const ws = new WebSocket(
  (location.protocol === "https:" ? "wss://" : "ws://") +
    location.host + "/ws/term/" + sid + "?k=" + encodeURIComponent(key)
);

function sendResize() {
  fit.fit();
  if (ws.readyState === WebSocket.OPEN)
    ws.send(JSON.stringify({ t: "resize", c: term.cols, r: term.rows }));
}

ws.onopen = sendResize; // viewer owns geometry: resize on open...
window.addEventListener("resize", sendResize); // ...and on every window resize

term.onData((d) => {
  if (ws.readyState === WebSocket.OPEN)
    ws.send(JSON.stringify({ t: "in", data: b64encode(d) }));
});

ws.onmessage = (ev) => {
  const m = JSON.parse(ev.data);
  if (m.t === "out") term.write(b64decode(m.data));
  else if (m.t === "exit") banner("session ended (exit " + m.code + ")", true);
  else if (m.t === "err") banner(m.msg || "session error", false);
};
ws.onclose = () => banner("disconnected from broker", false);
