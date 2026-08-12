# popfleet wire protocol v1

Frozen for v1. Broker, Go agent, and the reference Python agent all implement exactly this.

## Transport

All WebSocket messages are JSON text frames, one JSON object per frame.
Terminal payloads (`data`) are **base64-encoded bytes** — PTY output is not valid UTF-8 in
general, so raw strings would corrupt it.

Field `t` is the tag. Unknown tags are ignored (forward compatibility).

## `/ws/agent` — agent <-> broker

Agent must send `hello` as its first frame. Broker closes the connection if the first
frame is anything else, or if the token is unknown/revoked.

Agent -> broker:

| frame | fields |
|---|---|
| `{"t":"hello","token":"…","name":"lab1","ver":"1.0.0"}` | `name` is a display hint; broker keeps the first name it saw for that token unless the agent sends a new one |
| `{"t":"hb"}` | every 10 s |
| `{"t":"out","sid":"…","data":"<base64>"}` | PTY stdout/stderr |
| `{"t":"exit","sid":"…","code":0}` | PTY exited |

Broker -> agent:

| frame | fields |
|---|---|
| `{"t":"hello_ok","id":"<machine id>"}` | sent once, after a successful `hello` |
| `{"t":"open","sid":"…","cmd":"htop"}` | `cmd` optional; absent/empty means login shell |
| `{"t":"in","sid":"…","data":"<base64>"}` | keystrokes |
| `{"t":"resize","sid":"…","c":120,"r":40}` | cols, rows |
| `{"t":"close","sid":"…"}` | kill the PTY |

`POPFLEET_URL` is a **base URL**, not an endpoint: `https://fleet.example`. Every agent appends
`/ws/agent` itself and upgrades the scheme (`http`->`ws`, `https`->`wss`). The panel's copy-paste
enrollment blocks emit the base form, so an agent that demands the full path breaks them.

`exit` carries the shell's wait status: the exit code for a normal exit, and **128+N when the
shell was killed by signal N** (137 for SIGKILL). Agents must not report a bare `-1`.

Agent rules:
- one PTY per `sid`, spawned fresh on `open`; multiple concurrent `sid`s allowed
- `open` for a `sid` that already exists is ignored
- `in`/`resize` for an unknown `sid` are ignored (never an error)
- on PTY exit: send `exit`, then forget the `sid`
- on socket loss: kill every PTY, reconnect with backoff (1 s, doubling, cap 30 s, ±20% jitter)

## `/ws/term/{sid}?k=<key>` — browser <-> broker

`k` is the one-time session key from `POST /api/machines/{id}/term`. It is consumed at
WebSocket upgrade: the first upgrade wins, every later attempt is rejected. Keys expire
60 s after minting if never used.

Browser -> broker:

| frame | fields |
|---|---|
| `{"t":"in","data":"<base64>"}` | keystrokes |
| `{"t":"resize","c":120,"r":40}` | sent once on open, then on every window resize |

Broker -> browser:

| frame | fields |
|---|---|
| `{"t":"out","data":"<base64>"}` | PTY output |
| `{"t":"exit","code":0}` | shell exited |
| `{"t":"err","msg":"agent went offline"}` | session died for a reason other than shell exit |

The browser owns geometry: single operator, so the viewer dictates PTY size.

The terminal page sets **no** `frame-ancestors` / `X-Frame-Options`, deliberately: the RDD's
whole integration surface is "open the session URL in an iframe" from the operator's own apps,
and locking framing to `'self'` would break it. The one-time 60 s key is what guards the session,
not the framing policy.

## HTTP API

Every `/api/*` route requires `Authorization: Bearer $POPFLEET_ADMIN_TOKEN`.
`/healthz`, `/` (panel), `/term/{sid}` and the embedded assets are unauthenticated —
they carry no fleet data on their own.

```
POST   /api/tokens                 {"name":"lab1"}?  -> {"token":"…"}
GET    /api/machines               -> [{"id","name","online","last_seen","agent_ver","sessions"}]
POST   /api/machines/{id}/term     {"cmd":"htop"}?   -> {"url":"/term/<sid>?k=<key>","sid":"…"}
DELETE /api/machines/{id}          -> 204   revokes the token and drops the live agent
GET    /healthz                    -> 200 "ok"
```

`last_seen` is RFC3339. `online` is false once two heartbeats are missed (25 s).
