# popfleet

> A registry of every machine you can reach. Enroll a box with one pasted line —
> laptop, lab server, or a podman container that lives for ten minutes — and it appears
> in your panel with a green dot. Click Terminal, you're at a prompt in under a second.

## What it is

A broker and as many agents as you have machines. The broker is one static Go binary
(`popfleet serve`) that hosts the panel, a small HTTP API and two WebSocket endpoints.
An agent is the same binary (`popfleet agent`), a container image, or a single Python
file you can embed in an app you already run — all three speak the same protocol and
are equal citizens on the panel.

Agents dial **out** and hold the socket open. That is the whole trick: a box behind
NAT, a VPN or a lab firewall needs no inbound port, no tunnel and no port forwarding,
and opening a terminal is one message down a connection that already exists. A
container running the agent self-registers on start and drops off the panel on exit,
so ephemeral machines are live inventory instead of stale rows.

```
browser ──WSS /ws/term/{sid}──▶  broker  ──WSS /ws/agent──▶  agent ──▶ PTY
                                 relays frames, owns no PTY of its own
```

State is a JSON file plus a mutex, sitting next to the binary. The wire contract is
frozen in [docs/PROTOCOL.md](docs/PROTOCOL.md) — that file, not this one, is what the
three agent implementations follow.

## Quick start

Two minutes, one machine, no TLS and no other boxes involved. Enroll the laptop you
are sitting at first — it proves the whole loop works before NAT and certificates get
a vote.

**1. Build and start the broker.**

```sh
go build -o popfleet .

export POPFLEET_ADMIN_TOKEN=$(openssl rand -hex 32)
echo "$POPFLEET_ADMIN_TOKEN"        # you paste this into the panel, once per browser tab
./popfleet serve
```

You should see:

```
popfleet broker listening on http://127.0.0.1:7333 (state: popfleet.json)
open the panel and paste your POPFLEET_ADMIN_TOKEN when it asks
```

It refuses to start without `POPFLEET_ADMIN_TOKEN`, and it will not bind a public
address without TLS in front of it — see [Putting TLS in front of it](#putting-tls-in-front-of-it).
`--addr` and `--state` move the port and the state file.

**2. Open the panel** at <http://127.0.0.1:7333> and paste the token when it asks. It
lives in `sessionStorage` — never in the page, never in a cookie, gone when you close
the tab. An empty fleet is the expected first sight.

**3. Add your first machine.** Click **Add machine**, name it anything, and you get
three enrollment blocks with a real token already filled in. To enroll *this* box, run
the one-liner in another terminal — the broker hands out a copy of its own binary, so
this works with nothing published anywhere:

```sh
curl -sSf http://127.0.0.1:7333/agent.sh | POPFLEET_URL=http://127.0.0.1:7333 sh -s -- <TOKEN>
```

**4. Watch it land.** Within a few seconds the machine appears with a green dot, a
last-seen time, and a session count. Click **Terminal** and you have a prompt in a new
tab — typically under a second from the click.

That is the whole product. Everything else is more machines.

### If something does not go that way

| Symptom | Cause |
|---|---|
| `cannot listen on 127.0.0.1:7333: address already in use` | Another broker is already running. `--addr 127.0.0.1:7334`. |
| Panel keeps re-asking for the admin token | The token is wrong. A 401 clears it and re-prompts, so a typo loops until you paste the value `echo "$POPFLEET_ADMIN_TOKEN"` printed. |
| `curl … agent.sh` prints `no popfleet-<os>-<arch> binary available` | The target box is a different OS/arch than the broker and no release is published. Cross-compile — see [Enrolling a machine](#enrolling-a-machine). |
| Machine stays red | The agent cannot reach `POPFLEET_URL` from *that box*. `127.0.0.1` only works when the agent runs on the broker's own machine. |
| Terminal opens then immediately shows a banner | The session URL is one-time and expires in 60 s. Click **Terminal** again for a fresh one. |

`popfleet.json` holds live enrollment tokens in clear. It is written `0600` and is in
`.gitignore` — back it up like a key file, not like a config. Losing
`POPFLEET_ADMIN_TOKEN` costs you nothing permanent: set a new one and restart, your
enrolled machines keep working, because a machine's identity is its own enrollment
token and not the admin key.

## Enrolling a machine

Mint a token in the panel (**Add machine**) or by hand:

```sh
curl -s -X POST -H "Authorization: Bearer $POPFLEET_ADMIN_TOKEN" \
     -d '{"name":"lab1"}' http://127.0.0.1:7333/api/tokens
# {"token":"…"}
```

Three ways to spend it. All three speak the same protocol and are equal citizens on
the panel.

**1. The one-liner** — installs the static binary and a service (systemd on Linux as
root, launchd on macOS as your user), writes the token to a `0600` env file, starts it:

```sh
curl -sSf https://fleet.example/agent.sh | POPFLEET_URL=https://fleet.example sh -s -- <TOKEN>
```

The broker serves `/agent.sh` itself, so that URL is your own broker.

For the binary, the script asks your broker first: `GET /bin/popfleet-<os>-<arch>` hands
back a copy of the running broker binary, because the broker and the agent are the same
program. That means enrollment works with no GitHub release published at all — as long as
the new box runs the **same OS and architecture as the broker**.

A different platform (darwin broker, linux box) needs a real binary from somewhere else.
The script falls back to the GitHub release URL and, if that 404s too, exits with build
instructions rather than half-installing. Cross-compile and point it at your own copy:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o popfleet-linux-arm64 .
# serve it anywhere, then on the target box:
curl -sSf https://fleet.example/agent.sh | POPFLEET_URL=https://fleet.example \
  POPFLEET_BIN_URL=https://your-host/popfleet-linux-arm64 sh -s -- <TOKEN>
```

**2. Podman** — the ephemeral path. Self-registers on start, drops off the panel on exit:

```sh
podman run -d --name popfleet-agent \
  -e POPFLEET_URL=https://fleet.example \
  -e POPFLEET_TOKEN=<TOKEN> \
  ghcr.io/ejoliet/popfleet-agent:latest
```

That image is published by the release CI (see [Cutting a release](#cutting-a-release));
before the first tag exists, build it locally with
`podman build -f Containerfile -t ghcr.io/ejoliet/popfleet-agent:latest .`. It is a
static binary on `busybox:musl` (~9 MB), so a terminal into the container gets a real
`/bin/sh` prompt — swap the final stage to `scratch` if you only want
enroll/heartbeat/drop-off with no shell inside.

**3. The Python agent** — [contrib/agent.py](contrib/agent.py), the embed-in-your-own-app
path. Copy the one file to the box, `pip install websockets`, run:

```sh
POPFLEET_URL=https://fleet.example POPFLEET_TOKEN=<TOKEN> python3 agent.py
```

It is ~280 lines with one dependency and no build step, so you can read it before you
trust it. To hook it into an app you already run, import it instead of shelling out:

```python
import os, threading, agent
os.environ["POPFLEET_URL"] = "https://fleet.example"
os.environ["POPFLEET_TOKEN"] = "<TOKEN>"
os.environ["POPFLEET_NAME"] = "my-app"
threading.Thread(target=agent.main, daemon=True).start()
```

On `SIGTERM`/`SIGINT` it kills its PTYs and closes the socket cleanly, so the panel
sees it leave immediately rather than after a heartbeat timeout.

`POPFLEET_URL` is the broker's base URL in all three cases — the agents append
`/ws/agent` themselves, and `http`/`https` are upgraded to `ws`/`wss`.

## Putting TLS in front of it

The broker speaks plain HTTP and binds loopback. TLS is the reverse proxy's job.
Caddy, the whole config:

```
fleet.example.com {
	reverse_proxy 127.0.0.1:7333
}
```

Caddy gets the certificate itself and proxies WebSockets without further ceremony.
Point DNS at the box, `caddy run`, and `https://fleet.example.com` is your broker.

If you would rather not expose anything to the public internet, `tailscale serve` puts
it on your tailnet with a valid certificate and no open port:

```sh
tailscale serve --bg 7333
# https://<machine>.<tailnet>.ts.net  — reachable only by devices on your tailnet
```

Either way the broker itself keeps listening on `127.0.0.1:7333`. Leave it there.

`popfleet serve --addr 0.0.0.0:7333` is refused outright: a non-loopback bind requires
`--insecure`, and the error message prints the Caddyfile above. `--insecure` exists for
the case where something else already terminates TLS in front of the broker — a proxy on
the same host, a service mesh sidecar. Using it with nothing in front means every
enrollment token, the admin bearer token and every keystroke of every session cross the
network in clear. There is no third option; there is no built-in TLS.

## Trust model

Stated plainly, because the pleasant version would be a lie:

- **TLS terminates at a broker you own.** The broker decrypts the browser's frames and
  re-encrypts them to the agent. Between those two points the session is plaintext in
  the broker's memory.
- **The broker can see session bytes.** Everything you type and everything the shell
  prints passes through it. That is acceptable exactly as long as the broker runs on
  hardware you control. It is not acceptable on shared infrastructure.
- **End-to-end crypto past the broker is v2 debt**, not an oversight — fragment-key
  AES-GCM, the poptail pattern. It buys nothing while you own the broker, and it is
  required the day you do not. If you move the broker to someone else's machine, that
  debt comes due first.
- **The enrollment token is the machine's identity.** Anything holding a valid token is
  that machine as far as the broker is concerned. Tokens live in `0600` env files on the
  agent side and in `popfleet.json` on the broker side.
- **`DELETE /api/machines/{id}` is the kill switch** (the **Revoke** button). It deletes
  the token and drops the live socket in the same call; the agent's reconnect attempts
  are then rejected at `hello`. Revoke first, investigate second.
- **Session URLs are not access.** One use, 60 s to claim it, consumed at the WebSocket
  upgrade. A screenshot of the panel or a URL in someone's history buys nothing.
- **The agent opens no listening sockets.** It only dials out. A compromised broker can
  run commands on enrolled machines; nothing else on the network can reach the agent
  at all.
- **One operator, one admin token.** No users, no roles, no audit log. If you need to
  know who did what, this is not the tool yet.

## Environment variables

Secrets are read from the environment and nowhere else. They are never flags, because
flags are visible to every user on the box in `ps`. `--name` is a flag precisely
because a display name is not a secret.

| Variable | Used by | Required | Meaning |
|---|---|---|---|
| `POPFLEET_ADMIN_TOKEN` | `popfleet serve` | yes | Bearer token for every `/api/*` route and the panel. Broker refuses to start without it. Generate with `openssl rand -hex 32`. |
| `POPFLEET_URL` | every agent | yes | Broker base URL, e.g. `https://fleet.example`. The agent appends `/ws/agent` and upgrades `http(s)` to `ws(s)`. |
| `POPFLEET_TOKEN` | every agent | yes | Enrollment token from `POST /api/tokens`. This is the machine's identity. |
| `POPFLEET_NAME` | every agent | no | Display name in the panel. Defaults to the hostname. `popfleet agent --name` overrides it. |
| `POPFLEET_BIN_URL` | `agent.sh` | no | Where the installer fetches the binary; defaults to the GitHub latest release. |

The Python agent strips `POPFLEET_TOKEN` and `POPFLEET_URL` from the environment before
`exec`ing your shell, so the token does not leak into every session you open.

## Cutting a release

One command:

```sh
git tag v1.2.0 && git push origin v1.2.0
```

CI runs the full suite first (`gofmt`, `go vet`, `go test -race`) and publishes
nothing if it fails — a broken tag is worse than no tag. On success the tag produces:

- **Raw binaries** on the GitHub Release, named exactly `popfleet-<os>-<arch>` for
  linux/amd64, linux/arm64, darwin/amd64 and darwin/arm64, plus `checksums.txt`.
  These are what `agent.sh` fetches from `releases/latest/download/`. They are bare
  executables on purpose, never tarballs — the installer curls the URL straight to
  disk and `chmod`s it, so an archive there would install a tarball as your binary.
- **A multi-arch image** (amd64 + arm64) at `ghcr.io/ejoliet/popfleet-agent`, tagged
  with the version and `latest` — the image the panel's podman block names.

Released builds report the tag without the `v` as their agent version in the panel;
a local `go build` reports `dev`.

Until you cut that first tag, cross-platform enrollment needs a hand-built binary
(see [Enrolling a machine](#enrolling-a-machine)). Same-platform boxes work regardless,
because the broker serves a copy of itself.

> **One-time, after the first release:** GHCR packages start out private. Make
> `popfleet-agent` public (GitHub → your profile → Packages → popfleet-agent →
> Package settings → Change visibility) or `podman run` will demand a registry login.
> Nothing else needs setting up: CI uses the built-in `GITHUB_TOKEN`, so there are no
> secrets to create.

## Integrating with your other apps

The one-time session URL is the whole integration surface: `POST` to
`/api/machines/{id}/term`, open the URL you get back in an iframe or a tab. See
[docs/INTEGRATION.md](docs/INTEGRATION.md).

## Design record

- [docs/RDD.md](docs/RDD.md) — the original v1 spec: purpose, architecture, interface
  contract, security invariants, the gate table, and what was deferred to v2.
- [docs/GATES.md](docs/GATES.md) — the gate table turned into a hand-run acceptance
  checklist, with the thresholds each gate has to hit.
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the frozen wire contract. Every agent
  implements exactly this.
- [docs/INTEGRATION.md](docs/INTEGRATION.md) — embedding a terminal in your own app.
- [implementation-notes.md](implementation-notes.md) — one line per non-obvious build
  decision, `DEVIATION:` marking where the code knowingly departs from the RDD.
