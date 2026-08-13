# v2 gates — hand-run checklist

Companion to docs/RDD-v2.md. Everything here is measured, not felt. The
harness lives at `contrib/gateharness` and plays the browser side of a
session (e2e negotiation, GCM, frame timestamps):

```sh
# mint a session against your deployed Worker, then:
go run ./contrib/gateharness 'wss://popfleet.<you>.workers.dev/ws/term/SID?k=KEY' SID "$POPFLEET_E2E_KEY" rtt
#                                                                    modes: normal | tamper | rtt
```

## Verified locally (wrangler dev + miniflare, 2026-08-12)

The full relay logic runs locally under `wrangler dev`; these results prove
the code paths, not production latency:

- [x] **E2E session end-to-end** (Go agent and Python agent): harness saw the
      `{"t":"e2e"}` negotiation frame, an encrypted round trip, and **no
      plaintext on the wire** (`normal` mode asserts the marker never appears
      in raw frame data).
- [x] **Tampered frame kills the session**: one flipped ciphertext byte →
      agent answers `err` "e2e decrypt failed (tampered frame)", renders
      nothing, PTY killed (`tamper` mode; also covered by
      `TestSessionE2ERoundTrip` in internal/agent).
- [x] **v1 agent rejected at hello with a clear message**: agent without
      `POPFLEET_E2E_KEY` logs `hello rejected: this relay requires end-to-end
      encryption: … set POPFLEET_E2E_KEY (or run the v1 Go broker on your LAN)`.
- [x] **One-time key semantics**: second upgrade with a consumed key → 403.
- [x] **Revoke** (`DELETE /api/machines/{id}`): 204, live socket dropped,
      reconnect rejected at hello (`unknown or revoked token`).
- [x] **Duplicate-token connect replaces the zombie**: old socket closed with
      "replaced by a newer connection", machine id survives, sessions die
      with a banner.
- [x] **Coalescing**: `yes | head -c 300000` arrived as ~4021 B/frame average
      (112 frames for 450 KB of PTY output) instead of frame-per-read.
- [x] **Keystroke RTT harness**: p50 = 20 ms loopback (16 ms of that is the
      coalescing flush window — the floor, not the gate).

## Gate v2-0 — needs the deployed Worker

- [ ] Keystroke RTT **< 250 ms p50 from LTE**: run harness `rtt` mode from a
      phone-tethered laptop against `wss://popfleet.<you>.workers.dev`.
- [ ] `wrangler tail` during typing shows **ciphertext only** (base64 blobs
      in `data`; no shell text, no cmd).
- [ ] Agent idle **6 h**, then instant terminal: hibernation vs heartbeat —
      the DO must have been evicted (no logs) yet the session opens at once.
- [ ] Tampered frame via harness `tamper` mode against production.

## Gate v2-1 — three agents

- [ ] Go, Python, container agents each pass `normal` + `tamper` with only
      `POPFLEET_URL` + `POPFLEET_E2E_KEY` env changes.
- [x] Diff stays reviewably small: `git diff v1..` on internal/agent is ~120
      changed lines (coalescing included).

## Gate v2-2 — ops semantics

- [x] Revoke / duplicate-token / firehose+typing (locally, above).
- [ ] `yes` 60 s firehose + typing in a second session against production;
      free-tier quota math into a comment in worker/src/fleet.js.
      Starting point: coalescing caps a firehose at ~62 incoming
      frames/s/session; hibernated idle fleet costs ≈ one DO wake per 15 s
      alarm sweep only while sockets are live.

## Gate v2-3 — 7-day dogfood

- [ ] No Chrome+SOCKS5 for these machines all week; any use recorded — that
      reason is v3's spec.
- [ ] 1-day DO storage soak numbers into implementation-notes (open question).
