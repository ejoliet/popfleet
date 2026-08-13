"use strict";
// Panel logic. The admin token is never baked into the page: prompt once,
// keep it in sessionStorage, send it as the Authorization header.
//
// v2 (Worker relay): GET /config.json says {"e2e":true}; enrollment blocks
// then carry POPFLEET_E2E_KEY. The fleet key lives in localStorage (shared
// with the terminal pages) and is never sent on any request. The v1 Go
// broker has no /config.json, so this same file serves both.

let cfg = { e2e: false };
fetch("/config.json").then((r) => (r.ok ? r.json() : cfg)).then((c) => (cfg = c)).catch(() => {});

function e2eKey() {
  let k = localStorage.getItem("popfleet_e2e");
  if (!k) {
    k = (prompt("Fleet E2E key (POPFLEET_E2E_KEY)\nGenerate once with: openssl rand -base64 32") || "").trim();
    if (k) localStorage.setItem("popfleet_e2e", k);
  }
  return k || "$POPFLEET_E2E_KEY"; // declined: leave a placeholder to fill in
}

function adminToken(force) {
  let t = sessionStorage.getItem("popfleet_admin");
  if (!t || force) {
    t = prompt("Admin token (POPFLEET_ADMIN_TOKEN)");
    if (t) sessionStorage.setItem("popfleet_admin", t.trim());
  }
  return sessionStorage.getItem("popfleet_admin") || "";
}

async function api(method, path, body) {
  const r = await fetch(path, {
    method,
    headers: { Authorization: "Bearer " + adminToken() },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (r.status === 401) {
    sessionStorage.removeItem("popfleet_admin");
    adminToken(true);
    throw new Error("unauthorized");
  }
  if (!r.ok) throw new Error(method + " " + path + ": " + r.status);
  return r.status === 204 ? null : r.json();
}

function toast(msg, cls) {
  const d = document.createElement("div");
  d.className = "toast " + (cls || "");
  d.textContent = msg;
  document.getElementById("toasts").appendChild(d);
  setTimeout(() => d.remove(), 4200);
}

function ago(rfc3339) {
  if (!rfc3339) return "never";
  const s = Math.max(0, (Date.now() - Date.parse(rfc3339)) / 1000);
  if (s < 60) return Math.floor(s) + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

let known = null; // id -> {name, online}, for join/leave toasts

function render(machines) {
  const rows = document.getElementById("rows");
  document.getElementById("tbl").hidden = machines.length === 0;
  document.getElementById("empty").hidden = machines.length !== 0;
  rows.textContent = "";
  const seen = {};
  for (const m of machines) {
    seen[m.id] = { name: m.name, online: m.online };
    const tr = document.createElement("tr");

    const name = document.createElement("td");
    const dot = document.createElement("span");
    dot.className = "dot " + (m.online ? "on" : "off");
    name.append(dot, m.name || m.id);
    tr.appendChild(name);

    const last = document.createElement("td");
    last.className = "muted";
    last.textContent = ago(m.last_seen);
    tr.appendChild(last);

    const ver = document.createElement("td");
    ver.className = "muted";
    ver.textContent = m.agent_ver || "—";
    tr.appendChild(ver);

    const sess = document.createElement("td");
    sess.textContent = m.sessions ? m.sessions + " session" + (m.sessions > 1 ? "s" : "") : "";
    tr.appendChild(sess);

    const act = document.createElement("td");
    act.style.textAlign = "right";
    const term = document.createElement("button");
    term.textContent = "Terminal";
    term.disabled = !m.online;
    term.onclick = async () => {
      const r = await api("POST", "/api/machines/" + m.id + "/term");
      window.open(r.url, "_blank");
    };
    const del = document.createElement("button");
    del.textContent = "Revoke";
    del.className = "danger";
    del.style.marginLeft = "8px";
    del.onclick = async () => {
      if (!confirm("Revoke token and drop " + (m.name || m.id) + "?")) return;
      await api("DELETE", "/api/machines/" + m.id);
      toast((m.name || m.id) + " revoked", "leave");
      poll();
    };
    act.append(term, del);
    tr.appendChild(act);
    rows.appendChild(tr);
  }
  if (known) {
    for (const id in seen) {
      const label = seen[id].name || id;
      if (!(id in known)) toast(label + " enrolled", "join");
      else if (seen[id].online && !known[id].online) toast(label + " is online", "join");
      else if (!seen[id].online && known[id].online) toast(label + " went offline", "leave");
    }
    for (const id in known) {
      if (!(id in seen)) toast((known[id].name || id) + " removed", "leave");
    }
  }
  known = seen;
}

async function poll() {
  try {
    render(await api("GET", "/api/machines"));
  } catch (e) {
    /* transient: next poll retries */
  }
}

document.getElementById("add").onclick = async () => {
  const name = prompt("Machine name (optional)") || "";
  const r = await api("POST", "/api/tokens", name ? { name } : undefined);
  const o = location.origin;
  const ek = cfg.e2e ? e2eKey() : "";
  document.getElementById("blk-sh").textContent =
    "curl -sSf " + o + "/agent.sh | POPFLEET_URL=" + o +
    (ek ? " POPFLEET_E2E_KEY=" + ek : "") + " sh -s -- " + r.token;
  document.getElementById("blk-podman").textContent =
    "podman run -d --name popfleet-agent \\\n  -e POPFLEET_URL=" + o +
    " \\\n  -e POPFLEET_TOKEN=" + r.token +
    (ek ? " \\\n  -e POPFLEET_E2E_KEY=" + ek : "") +
    " \\\n  ghcr.io/ejoliet/popfleet-agent:latest";
  document.getElementById("blk-py").textContent =
    "curl -sSfO " + o + "/agent.py && pip install websockets" +
    (ek ? " cryptography" : "") + "\n" +
    "POPFLEET_URL=" + o + " POPFLEET_TOKEN=" + r.token +
    (ek ? " POPFLEET_E2E_KEY=" + ek : "") + " python3 agent.py";
  document.getElementById("enroll").showModal();
};

document.getElementById("enroll-close").onclick = () =>
  document.getElementById("enroll").close();

for (const b of document.querySelectorAll("[data-copy]")) {
  b.onclick = () =>
    navigator.clipboard.writeText(
      document.getElementById(b.dataset.copy).textContent
    );
}

adminToken();
poll();
setInterval(poll, 2000);
