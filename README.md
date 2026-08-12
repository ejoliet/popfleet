# popfleet — v1 Spec (Type A, RDD)

> A registry of every machine you can reach. Enroll a box with one pasted line —
> laptop, lab server, or a podman container that lives for ten minutes — and it appears
> in your panel with a green dot. Click Terminal, you're at a prompt in under a second.

**Author**: Emmanuel Joliet
**Date**: 2026-08-11
**Status**: v1 spec — agent implements after Gate 0
**Working name**: `popfleet` (placeholder — verify collisions before launch; `popdock`
is taken by an eOne SaaS product; System76 owns "Pop Shell" mindshare)

---

## Purpose

Personal, self-owned, account-free fleet terminal:

- **Broker**: standalone single Go binary you run anywhere (`popfleet serve`). Serves
  the panel UI, a tiny HTTP API, and two WebSocket endpoints.
- **Agent**: outbound-only. Same binary (`popfleet agent`) OR a ~200-line reference
  Python agent (one dep: `websockets`) for embedding as a hook in your own apps,
  OR the podman image. All speak the same tiny protocol.
- **Reverse connection is the core trick**: agents dial OUT and hold a WSS. Machines
  behind NAT/VPN/lab firewalls need no inbound ports, no tunnels, no port forwarding.
  Terminal open = one message down an already-connected socket.
- **Ephemeral workloads are a feature**: a container running the agent self-registers
  on start and drops off the panel on exit. Disposable machines, live inventory.

Not in scope, ever, for v1: multi-user, guests, escalation ceremonies, file transfer.
This is single-operator personal infrastructure.

## Architecture

```
BROKER  popfleet serve  (single static Go binary, embeds panel + xterm.js)
 ├─ GET  /                          panel UI (machine list, status dots, Terminal btns)
 ├─ HTTP API (admin bearer token)   see Interface Contract
 ├─ WSS /ws/agent                   agents connect here, authenticate, heartbeat
 ├─ WSS /ws/term/{sid}              browser terminal sessions
 ├─ relay: pipes term frames <-> agent frames per session (no PTY on broker)
 └─ state: SQLite (machines, tokens, last_seen)  — single file next to binary

AGENT   popfleet agent --name lab1        (or: python agent.py / podman run)
 ├─ dials POPFLEET_URL, authenticates with POPFLEET_TOKEN, heartbeats every 10 s
 ├─ on {t:"open",sid}  -> spawn PTY (login shell or --cmd), attach to session stream
 ├─ on {t:"close",sid} -> kill PTY
 └─ config via env only: POPFLEET_URL, POPFLEET_TOKEN, POPFLEET_NAME
```

- One PTY per session, spawned fresh on open — no screen reconstruction needed.
- Multiple concurrent sessions per agent allowed (map sid -> PTY).
- PTY geometry: browser sends {t:"resize"} on open and on window resize; single
  operator, so the viewer owns geometry (inverse of the support-tool rule).
- Panel is a single embedded HTML file: vanilla JS + **bundled** xterm.js. Zero
  third-party JS, strict CSP, no analytics, no fonts, no service worker.

## Interface contract (v1)

HTTP (all under admin bearer token `POPFLEET_ADMIN_TOKEN`):

```
POST   /api/tokens                -> {token}            mint enrollment token
GET    /api/machines              -> [{id,name,online,last_seen,agent_ver}]
POST   /api/machines/{id}/term    -> {url}              one-time session URL, 60 s TTL
DELETE /api/machines/{id}                               revoke token + drop agent
GET    /healthz
```

The one-time session URL is how *your other web apps* hook in: call the API, get a
URL, open it in an iframe or new tab. That is the entire integration surface.

WS messages (JSON, short tags): agent: `{t:"hello",token,name,ver}` `{t:"hb"}`
`{t:"out",sid,data}` `{t:"exit",sid,code}` · broker->agent: `{t:"open",sid,cmd?}`
`{t:"close",sid}` `{t:"in",sid,data}` `{t:"resize",sid,c,r}`.

Enrollment UX (the wow moment): panel "Add machine" button mints a token and shows
copy-paste blocks:

```
curl -sSf https://fleet.example/agent.sh | sh -s -- <TOKEN>        # any box
podman run -d -e POPFLEET_URL=... -e POPFLEET_TOKEN=... popfleet/agent
python3 agent.py     # POPFLEET_URL + POPFLEET_TOKEN in env — the app-hook path
```

Seconds later the machine is on the panel with a green dot.

## Security invariants

- Tokens and admin key via env only; never in argv (visible in `ps`), never committed.
  `.gitignore` for any local `.env` BEFORE first token is minted (cullroom rule).
- Agent is outbound-only; opens no listening sockets.
- Session URLs are one-time and expire in 60 s; a leaked panel screenshot is not access.
- Broker binds 127.0.0.1 by default; operator explicitly fronts it with TLS
  (caddy/traefik/tailscale serve) — README shows the caddy two-liner. No TLS = refuse
  non-localhost bind unless `--insecure`.
- Trust model v1: TLS to a broker YOU own. E2E crypto past the broker is v2 debt
  (fragment-key AES-GCM, poptail pattern) — needed only if the broker moves to
  shared infra. README states this plainly.
- Enrollment token is machine identity; `DELETE /machines/{id}` is the kill switch.

## Gates (spike-first)

| # | Spike | GO/NO-GO |
|---|---|---|
| 0 | Broker + Go agent on phone-hotspot NAT; click Terminal in browser | Prompt < 1 s from click; keystroke RTT < 200 ms p50 on LTE; two concurrent sessions to same agent both live |
| 1 | Podman ephemeral flow | Container start -> on panel < 5 s; `podman kill` -> dot red within 2 missed heartbeats (~25 s); container exit removes session cleanly |
| 2 | Reference Python agent (~200 LOC, websockets dep) | Same Gate 0 checks pass; runs on Python 3.9+ with `pip install websockets` only |
| 3 | Resilience: restart broker; drop agent network 60 s | Agents auto-reconnect with backoff; panel repopulates with NO manual re-enroll; in-flight session dies cleanly with banner, new session works immediately |
| 4 | Firehose + daily: `yes` 60 s in one session while typing in a second; then 7-day dogfood on 3 real machines (mac laptop, one EC2 box, one podman container) | Panel responsive, agent memory flat; after 7 days: zero manual restarts of agents |

Gate 4's dogfood is the real product gate: if you stop using the panel by day 4,
the product thesis fails regardless of the tech.

## v1 extras that are nearly free (state machine made visible)

- Status dots + last-seen + "3 sessions" count per machine (data already exists).
- `--cmd` on open: API accepts `{cmd:"htop"}` -> button variants per machine later.
- Panel toast when a machine joins/leaves (the ephemeral-container demo moment).
- Session-ended banner in the terminal page instead of a dead black box.

## v2 debt (explicitly deferred)

E2E fragment-key crypto past broker · multi-user/auth beyond one admin token ·
audit log & signed session receipts · file transfer · agent auto-update · mobile
accessory key row · session recording/replay · WebRTC direct path (latency) ·
Windows agent · tags/groups/search for large fleets.

## Open questions

- [x] JSON+mutex is fine at this scale.
- [ ] Podman image base: scratch + static binary (lean) vs python-slim carrying the
      reference agent. Lean scratch/Go for the image; Python agent stays a file, not an image.
- [x] IPAC outside-activity gate if this ever monetizes — it is squarely in the
      infra-tooling day-job domain. Personal-use v1 has no gate.

## Next steps

1. Gate 0 spike (1 day): broker relay + Go agent + embedded xterm page, hotspot NAT test.
2. Gate 1 same week: podman self-register/deregister — it is the demo AND the daily use.
3. Write agent.sh installer + README with the caddy TLS two-liner, then start the
   7-day dogfood before adding anything else.
