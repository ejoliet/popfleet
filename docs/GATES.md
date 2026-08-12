# Running the gates

The gate table these check against is in the [design record](RDD.md#gates-spike-first).

The gate table above is the acceptance criteria, not a wish list. Gates 0–4 need real
hardware: a phone hotspot, an EC2 box, podman. Run them by hand, in order, and stop at
the first NO-GO.

Setup used below: broker on your laptop behind Caddy or `tailscale serve` at
`https://fleet.example`, and `$T` holding an enrollment token minted for the box under
test.

**Gate 0 — broker + Go agent across NAT.** Put the target machine on a phone hotspot
(no port forwarding, no VPN). Enroll with the one-liner, then from the panel:

- Click **Terminal**. A shell prompt must appear in **under 1 second**. Time it with
  DevTools → Network → the `/ws/term/…` socket: upgrade to first `out` frame.
- Keystroke round trip on LTE must be **under 200 ms p50**. Same DevTools socket view:
  hold a key for a few seconds, take the median gap between an `in` frame and the `out`
  frame that echoes it. Feel is not evidence here; read the timestamps.
- Open **two Terminal tabs to the same machine** and type in both. Both stay live and
  independent; neither steals the other's output. The panel shows `2 sessions`.

**Gate 1 — podman ephemeral flow.**

```sh
podman build -f Containerfile -t ghcr.io/ejoliet/popfleet-agent:latest .   # or pull it once a release exists
podman run -d --name popfleet-agent -e POPFLEET_URL=https://fleet.example -e POPFLEET_TOKEN=$T ghcr.io/ejoliet/popfleet-agent:latest
```

- Container start to green dot on the panel: **under 5 s**.
- `podman kill popfleet-agent` → the dot goes red within two missed heartbeats,
  **~25 s** (the broker's `offlineAfter` is exactly 25 s).
- With a session open, stop the container: the terminal tab shows the
  `agent went offline` banner and the panel's session count drops. No zombie session,
  no black box.

> Gate 1 has been run and passed, with docker rather than podman: container start to
> online in 0.50 s, a real busybox prompt in-container, red dot 0.20 s after `docker kill`
> (the broker sees the socket drop instead of waiting out the heartbeats), and the same
> machine id on restart with no re-enrollment. Evidence in
> [implementation-notes.md](../implementation-notes.md).

**Gate 2 — reference Python agent.** Same three Gate 0 checks, with `contrib/agent.py`
in place of the Go agent, on a machine whose Python is 3.9:

```sh
python3 --version                    # 3.9 or newer
python3 -m py_compile contrib/agent.py
pip install websockets               # the only dependency
POPFLEET_URL=https://fleet.example POPFLEET_TOKEN=$T python3 contrib/agent.py
```

Then check the three things the Go agent does not exercise: run `stty size` in the
session after resizing the window (it must match the browser), run something that
prints non-UTF-8 bytes (`printf '\xff\x00'` — the terminal must not corrupt), and
`exit 7` (the panel's terminal tab must report exit code 7, not 0). Finally `kill -TERM`
the agent: PTYs die, the socket closes cleanly, the dot goes red at once instead of
after 25 s.

**Gate 3 — resilience.**

- Restart the broker (`Ctrl-C`, `./popfleet serve` again). Every agent reconnects on its
  own with backoff (1 s doubling to 30 s, ±20% jitter — watch the agent log). The panel
  repopulates with **no manual re-enrollment**. State survives in `popfleet.json`.
- With a session open, drop the agent's network for 60 s (`sudo ifconfig en0 down`, or
  toggle the hotspot). The browser tab must show a banner, not freeze. When the network
  returns, the machine goes green again and a **new** Terminal click works immediately.

**Gate 4 — firehose, then the real gate.**

```sh
# in session A
yes
# 60 s later, Ctrl-C — while typing normally in session B to the same machine
```

The panel stays responsive throughout and session B stays usable. Agent memory must be
flat, not climbing:

```sh
ps -o rss= -p $(pgrep -f 'popfleet agent')   # before, during, and 5 min after
```

Then the dogfood: 7 days on three real machines — a mac laptop, one EC2 box, one podman
container. Pass is **zero manual restarts of any agent** over the seven days. And the
part no command can measure: if you have stopped opening the panel by day 4, the product
thesis has failed regardless of what the other gates said.
