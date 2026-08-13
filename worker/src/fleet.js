// Fleet: the v1 Go broker's mutex-guarded memory made durable. One named
// instance holds every agent socket, every browser session, token hashes
// and one-time session keys. It relays protocol v1e frames verbatim — the
// data/cmd ciphertext is never decrypted here (the fleet key never reaches
// Cloudflare).
//
// WebSocket Hibernation is load-bearing, not optional: agent sockets must
// survive DO eviction, and the byte-exact `{"t":"hb"}` heartbeat is absorbed
// by a WebSocket auto-response pair so an idle fleet costs ~nothing. The v1
// broker's 35 s read deadline and 25 s offline rule are re-implemented with
// an alarm sweep (DEVIATION: last_seen granularity is "real traffic +
// auto-response timestamp + alarm", not a 10 s file write; the panel
// contract — green/red within ~25 s — is unchanged).

const KEY_TTL_MS = 60_000; // one-time session key lifetime (v1 semantics)
const OFFLINE_MS = 25_000; // two missed 10 s heartbeats
const DEAD_MS = 35_000; // v1 broker's agent read deadline
const HELLO_MS = 10_000; // first frame must be hello within this
const SWEEP_MS = 15_000; // alarm cadence while anything is alive

function randHex(nBytes) {
  const b = crypto.getRandomValues(new Uint8Array(nBytes));
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}

async function sha256hex(s) {
  const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return [...new Uint8Array(d)].map((x) => x.toString(16).padStart(2, "0")).join("");
}

function json(v, status = 200) {
  return new Response(JSON.stringify(v), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function wsSend(ws, frame) {
  try {
    ws.send(JSON.stringify(frame));
  } catch (e) {
    /* socket already gone; close handlers do the cleanup */
  }
}

function attach(ws) {
  try {
    return ws.deserializeAttachment() || {};
  } catch (e) {
    return {};
  }
}

export class Fleet {
  constructor(ctx, env) {
    this.ctx = ctx;
    this.env = env;
    // Absorb heartbeats without waking the DO. The match is byte-exact,
    // which is why every agent sends the literal string {"t":"hb"}.
    this.ctx.setWebSocketAutoResponse(
      new WebSocketRequestResponsePair('{"t":"hb"}', '{"t":"hb_ok"}')
    );
  }

  // ---- socket lookups (hibernation-safe: state lives in tags/attachments) ----

  agentSocket(mid) {
    for (const ws of this.ctx.getWebSockets("agent")) {
      if (attach(ws).mid === mid) return ws;
    }
    return null;
  }

  termSockets(mid) {
    return this.ctx.getWebSockets("term").filter((ws) => attach(ws).mid === mid);
  }

  termBySid(sid) {
    const l = this.ctx.getWebSockets("sid:" + sid);
    return l.length ? l[0] : null;
  }

  // last activity for an agent socket: hello time or the newest absorbed hb
  lastSeenOf(ws) {
    const a = attach(ws);
    const auto = this.ctx.getWebSocketAutoResponseTimestamp(ws);
    return Math.max(a.seen || 0, auto ? auto.getTime() : 0);
  }

  async armAlarm() {
    if ((await this.ctx.storage.getAlarm()) === null) {
      await this.ctx.storage.setAlarm(Date.now() + SWEEP_MS);
    }
  }

  // ---- HTTP (already admin-authenticated by the Worker) + WS upgrades ----

  async fetch(request) {
    const url = new URL(request.url);
    const path = url.pathname;

    if (path === "/ws/agent") return this.upgradeAgent();
    if (path.startsWith("/ws/term/")) {
      return this.upgradeTerm(path.slice("/ws/term/".length), url.searchParams.get("k") || "");
    }

    if (path === "/api/tokens" && request.method === "POST") {
      const body = await request.json().catch(() => ({}));
      const m = { id: randHex(8), name: body.name || "", tokenSum: "", lastSeen: 0, agentVer: "" };
      const token = randHex(24);
      m.tokenSum = await sha256hex(token);
      await this.ctx.storage.put({ ["machine:" + m.id]: m, ["tok:" + m.tokenSum]: m.id });
      return json({ token });
    }

    if (path === "/api/machines" && request.method === "GET") {
      const rows = await this.ctx.storage.list({ prefix: "machine:" });
      const sessions = {};
      for (const ws of this.ctx.getWebSockets("term")) {
        const mid = attach(ws).mid;
        sessions[mid] = (sessions[mid] || 0) + 1;
      }
      const now = Date.now();
      const out = [];
      for (const m of rows.values()) {
        const ws = this.agentSocket(m.id);
        const last = Math.max(m.lastSeen || 0, ws ? this.lastSeenOf(ws) : 0);
        out.push({
          id: m.id,
          name: m.name,
          online: !!ws && now - last < OFFLINE_MS,
          last_seen: last ? new Date(last).toISOString() : "",
          agent_ver: m.agentVer || "",
          sessions: sessions[m.id] || 0,
        });
      }
      out.sort((a, b) => (a.id < b.id ? -1 : 1));
      return json(out);
    }

    let mm = path.match(/^\/api\/machines\/([^/]+)\/term$/);
    if (mm && request.method === "POST") {
      const mid = mm[1];
      if (!(await this.ctx.storage.get("machine:" + mid))) {
        return new Response("no such machine", { status: 404 });
      }
      const body = await request.json().catch(() => ({}));
      // cmd, when present, must already be v1e ciphertext: the panel and any
      // API integration hold the fleet key, this relay does not and cannot
      // tell the difference. Plaintext cmd simply fails GCM on the agent.
      //
      // Sealing needs the sid, which this call mints — so a caller that
      // wants a cmd calls twice: mint (learn sid), seal cmd against it, then
      // repeat with {"sid","cmd"} to attach it. The old key is retired and a
      // fresh one returned; one-time/60 s semantics restart (INTEGRATION.md).
      let sid;
      if (body.sid) {
        const prev = await this.ctx.storage.get("pend:" + body.sid);
        if (!prev || prev.mid !== mid || Date.now() > prev.exp) {
          return new Response("no such pending session", { status: 404 });
        }
        sid = body.sid;
      } else {
        sid = randHex(8);
      }
      const key = randHex(24);
      await this.ctx.storage.put("pend:" + sid, {
        keySum: await sha256hex(key),
        mid,
        cmd: body.cmd || "",
        exp: Date.now() + KEY_TTL_MS,
      });
      await this.armAlarm(); // expired-key sweep
      return json({ url: "/term/" + sid + "?k=" + key, sid });
    }

    mm = path.match(/^\/api\/machines\/([^/]+)$/);
    if (mm && request.method === "DELETE") {
      const mid = mm[1];
      const m = await this.ctx.storage.get("machine:" + mid);
      if (!m) return new Response("no such machine", { status: 404 });
      await this.ctx.storage.delete(["machine:" + mid, "tok:" + m.tokenSum]);
      const ws = this.agentSocket(mid);
      if (ws) this.dropAgent(ws, "machine revoked");
      return new Response(null, { status: 204 });
    }

    return new Response("not found", { status: 404 });
  }

  upgradeAgent() {
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server, ["agent"]);
    // Not enrolled until hello; the alarm reaps sockets that never say it.
    server.serializeAttachment({ kind: "pending", since: Date.now() });
    this.armAlarm();
    return new Response(null, { status: 101, webSocket: client });
  }

  async upgradeTerm(sid, key) {
    // Consume the one-time key: first valid upgrade wins, later ones lose.
    const pend = await this.ctx.storage.get("pend:" + sid);
    const keySum = await sha256hex(key);
    const valid = pend && Date.now() < pend.exp && keySum === pend.keySum;
    if (!valid) {
      return new Response("invalid, expired or already-used session key", { status: 403 });
    }
    await this.ctx.storage.delete("pend:" + sid);

    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    // Register before open, or the first PTY bytes race the socket list.
    this.ctx.acceptWebSocket(server, ["term", "sid:" + sid, "mid:" + pend.mid]);
    server.serializeAttachment({ kind: "term", sid, mid: pend.mid });

    // v1e negotiation with the page: everything after this frame is
    // ciphertext, so it must arrive before any output does.
    wsSend(server, { t: "e2e" });

    const agent = this.agentSocket(pend.mid);
    if (!agent) {
      wsSend(server, { t: "err", msg: "agent is offline" });
      server.close(1000, "agent is offline");
    } else {
      wsSend(agent, { t: "open", sid, cmd: pend.cmd });
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  // ---- frames ----

  async webSocketMessage(ws, raw) {
    if (typeof raw !== "string") return; // protocol is JSON text frames only
    let m;
    try {
      m = JSON.parse(raw);
    } catch (e) {
      return;
    }
    const a = attach(ws);

    if (a.kind === "pending") return this.hello(ws, m);

    if (a.kind === "agent") {
      switch (m.t) {
        case "hb": {
          // Fallback for a heartbeat that missed the auto-response match
          // (e.g. re-serialized JSON). Real traffic also proves liveness.
          ws.serializeAttachment({ ...a, seen: Date.now() });
          break;
        }
        case "out": {
          const t = this.termBySid(m.sid);
          // mid check: an enrolled agent must not write into another
          // machine's session, even though sids are 64 random bits.
          if (t && attach(t).mid === a.mid) wsSend(t, { t: "out", data: m.data });
          break;
        }
        case "exit": {
          const t = this.termBySid(m.sid);
          if (t && attach(t).mid === a.mid) {
            wsSend(t, { t: "exit", code: m.code });
            t.close(1000, "shell exited");
          }
          break;
        }
        case "err": {
          // v1e: the agent killed the session (GCM auth failure).
          const t = this.termBySid(m.sid);
          if (t && attach(t).mid === a.mid) {
            wsSend(t, { t: "err", msg: m.msg || "session error" });
            t.close(1000, "agent error");
          }
          break;
        }
      }
      return;
    }

    if (a.kind === "term") {
      const agent = this.agentSocket(a.mid);
      if (!agent) return;
      if (m.t === "in") wsSend(agent, { t: "in", sid: a.sid, data: m.data });
      else if (m.t === "resize") wsSend(agent, { t: "resize", sid: a.sid, c: m.c, r: m.r });
    }
  }

  async hello(ws, m) {
    if (m.t !== "hello") {
      ws.close(1002, "first frame must be hello");
      return;
    }
    const mid = await this.ctx.storage.get("tok:" + (await sha256hex(m.token || "")));
    if (!mid) {
      ws.close(1008, "unknown or revoked token");
      return;
    }
    if (!m.e2e) {
      // Plaintext through shared infrastructure is not a supported mode.
      wsSend(ws, {
        t: "err",
        msg:
          "this relay requires end-to-end encryption: upgrade the agent to v2 " +
          "and set POPFLEET_E2E_KEY (or run the v1 Go broker on your LAN)",
      });
      ws.close(1008, "e2e required");
      return;
    }

    // Duplicate token connect replaces the zombie (v1 rule): its sessions
    // die with a banner, the new socket keeps the machine id.
    const old = this.agentSocket(mid);
    if (old) {
      old.serializeAttachment({ kind: "replaced" }); // its close event must not reap the successor's sessions
      for (const t of this.termSockets(mid)) {
        wsSend(t, { t: "err", msg: "agent reconnected" });
        t.close(1000, "agent reconnected");
      }
      old.close(1000, "replaced by a newer connection");
    }

    ws.serializeAttachment({ kind: "agent", mid, seen: Date.now() });
    const machine = await this.ctx.storage.get("machine:" + mid);
    if (machine) {
      if (m.name) machine.name = m.name; // agent-sent name wins, v1 Touch semantics
      if (m.ver) machine.agentVer = m.ver;
      machine.lastSeen = Date.now();
      await this.ctx.storage.put("machine:" + mid, machine);
    }
    wsSend(ws, { t: "hello_ok", id: mid, e2e: true });
    await this.armAlarm();
  }

  // ---- lifecycle ----

  webSocketClose(ws) {
    return this.reap(ws);
  }

  webSocketError(ws) {
    return this.reap(ws);
  }

  async reap(ws) {
    const a = attach(ws);
    if (a.kind === "term") {
      // Browser went away: tell the agent to kill the PTY.
      const agent = this.agentSocket(a.mid);
      if (agent) wsSend(agent, { t: "close", sid: a.sid });
    } else if (a.kind === "agent") {
      // A replaced zombie is re-tagged before close and never gets here,
      // so these sessions really are orphans.
      this.dropAgent(ws, "agent went offline");
      const machine = await this.ctx.storage.get("machine:" + a.mid);
      if (machine) {
        machine.lastSeen = Math.max(machine.lastSeen || 0, this.lastSeenOf(ws));
        await this.ctx.storage.put("machine:" + a.mid, machine);
      }
    }
  }

  dropAgent(ws, why) {
    const mid = attach(ws).mid;
    for (const t of this.termSockets(mid)) {
      wsSend(t, { t: "err", msg: why });
      t.close(1000, why);
    }
    ws.serializeAttachment({ kind: "replaced" }); // idempotent teardown
    try {
      ws.close(1000, why);
    } catch (e) {
      /* already closed */
    }
  }

  async alarm() {
    const now = Date.now();

    // Expired never-used session keys must not leak.
    const pend = await this.ctx.storage.list({ prefix: "pend:" });
    for (const [k, p] of pend) {
      if (now > p.exp) await this.ctx.storage.delete(k);
    }

    let live = pend.size > 0;
    for (const ws of this.ctx.getWebSockets()) {
      const a = attach(ws);
      if (a.kind === "pending") {
        if (now - (a.since || 0) > HELLO_MS) ws.close(1002, "no hello");
        else live = true;
      } else if (a.kind === "agent") {
        // v1's 35 s read deadline: a silently dead TCP conn is reaped and
        // its sessions get err banners instead of a dead black box.
        if (now - this.lastSeenOf(ws) > DEAD_MS) {
          this.dropAgent(ws, "agent went offline");
          const machine = await this.ctx.storage.get("machine:" + a.mid);
          if (machine) {
            machine.lastSeen = Math.max(machine.lastSeen || 0, this.lastSeenOf(ws));
            await this.ctx.storage.put("machine:" + a.mid, machine);
          }
        } else live = true;
      } else if (a.kind === "term") {
        live = true;
      }
    }

    // Re-arm only while something is alive: an idle fleet (agents connected
    // but silent) still counts — their liveness deadline is what we sweep.
    if (live) await this.ctx.storage.setAlarm(now + SWEEP_MS);
  }
}
