# popfleet v2 — Zero-Onboarding Broker (Cloudflare Worker + Durable Object)

> v1's product is zero-onboarding; v1's broker is not — it needs a box, DNS, Caddy or
> tailscale. v2 replaces that with `wrangler deploy`: the control plane becomes a
> Worker at `https://popfleet.<you>.workers.dev`, TLS included, no server owned.
> The trust-model debt named in the v1 README comes due: session bytes now cross
> shared infrastructure, so the E2E layer ships in the same release.

**Author**: Emmanuel Joliet
**Date**: 2026-08-11
**Status**: v2 spec — agent implements after Gate v2-0
**Baseline**: PROTOCOL.md v1 (frozen), v1 broker/agents as shipped in `ejoliet/popfleet`

---

## What changes, what survives

| Component | v1 | v2 |
|---|---|---|
| Broker | Go binary + Caddy/tailscale + a box | Worker + one Durable Object (DO); `wrangler deploy`, done |
| Panel | embedded in Go binary | same HTML/JS, served as Worker static assets |
| State | popfleet.json + mutex | DO storage (token **hashes**, machine rows, session keys) |
| Wire protocol | PROTOCOL.md v1 | v1 shape unchanged; `data`/`cmd` values become E2E ciphertext ("v1e") |
| Agents (Go, Python, container) | — | unchanged except: E2E encrypt/decrypt + output coalescing + `POPFLEET_E2E_KEY` |
| Trust model | TLS to a broker you own; broker sees plaintext | relay sees ciphertext only; Cloudflare can meter, never read |
| Go broker | — | **kept** as LAN/jumpbox mode; same agents work against either |

## Protocol v1e (extension, not a break)

Frame tags, fields, and flow are exactly PROTOCOL.md v1. One change of value encoding:

- Every `data` field and the `open` frame's `cmd` field carry
  `base64( nonce(12) || AES-256-GCM ciphertext )` instead of `base64(plaintext bytes)`.
- Per-session key: `HKDF-SHA256(POPFLEET_E2E_KEY, salt=sid, info="popfleet-v2")`.
  Nonce: 12 random bytes per frame (crypto/rand; WebCrypto getRandomValues).
- Everything else — tags, `sid`, `resize` geometry, `exit.code`, heartbeats — is
  routing metadata the relay may see. Stated in the trust model, not hidden.
- Version negotiation: agent `hello` gains `"e2e":true`; relay echoes it in `hello_ok`.
  A v1 agent against the Worker relay is rejected at hello with a clear message
  (plaintext through shared infra is not a supported mode).

AIDEV-NOTE: unknown-tags-ignored (v1 rule) is what makes this a value-encoding change,
not a protocol fork. GCM auth failure on any frame = kill the session with `err`, never
render garbage.

## E2E key lifecycle

- `POPFLEET_E2E_KEY`: 32 bytes, base64, generated once by the operator
  (`openssl rand -base64 32`). It is **fleet identity for confidentiality**;
  enrollment tokens remain identity for *presence*.
- Agents: env only, same 0600 env-file handling as `POPFLEET_TOKEN` (launchd/systemd
  paths unchanged).
- Panel: prompted once, kept in `localStorage` (unlike the admin token's
  sessionStorage — losing it per-tab would make every terminal unreadable; document
  the trade). Never sent on any request; decryption is WebCrypto in `term.js`
  (poptail Gate 2 pattern, already proven Safari/Chrome/Firefox).
- Worker/DO: never sees it. `wrangler tail` on a live session shows ciphertext — that
  is Gate v2-0's check, not a slogan.
- Rotation = new key, restart agents with new env, paste once in panel. Revoke of a
  single machine stays `DELETE /api/machines/{id}` (presence), but a revoked machine
  knew the fleet key — rotate after revoking a *compromised* box. README states this.

## Worker + DO design notes (for the implementing agent)

- One DO class `Fleet`, single named instance — it is the v1 broker's mutex-guarded
  memory made durable. `/ws/agent` and `/ws/term/{sid}` upgrade inside the DO;
  HTTP API routes run in the Worker and forward to the DO.
- **WebSocket Hibernation API is mandatory**, not optional: agent sockets must survive
  DO eviction, and `hb` frames should be absorbed via `setWebSocketAutoResponse`-style
  handling where possible so an idle fleet costs ~nothing. `last_seen` granularity may
  degrade from 10 s to "on real traffic + periodic alarm"; the 25 s offline rule is
  re-implemented with a DO alarm sweep. DEVIATION from v1 timing is acceptable; the
  panel contract (green/red within ~25 s) is not.
- Admin token: `wrangler secret put POPFLEET_ADMIN_TOKEN`; compare sha256 sums,
  constant-time, exactly as v1.
- Session keys: DO storage with 60 s TTL via alarm sweep; first upgrade consumes —
  v1 semantics verbatim.
- **Output coalescing in the agents** (new, small, benefits v1 too): buffer PTY output
  and flush every 16 ms or 4 KiB, whichever first. Reason: a `yes` firehose as
  frame-per-read through a DO is a request-count and duration cost problem; coalesced
  it is a non-issue. This is an agent-side change with no protocol impact.
- `/agent.sh` and `/agent.py` become Worker assets. The v1 trick of serving a copy of
  the broker's own executable at `/bin/…` **dies** — a Worker has no binary of itself.
  Regression is covered: the release CI already publishes raw per-arch binaries, and
  `POPFLEET_BIN_URL` still overrides. agent.sh's fallback chain becomes
  release → `POPFLEET_BIN_URL` → exit-with-instructions.

## Trust model (v2 delta, stated plainly)

- Cloudflare relays and can meter (frame counts, timing, sizes) but cannot read
  terminal bytes or commands. Traffic analysis of an interactive shell is real
  (keystroke timing); out of scope for a personal fleet, said out loud in the README.
- The Worker route is public. Defense stays what it was: token hashes, constant-time
  compares, one-time 60 s session keys — plus E2E meaning a fully compromised relay
  replays ciphertext it cannot forge (GCM tags fail closed).
- LAN/jumpbox users keep the v1 Go broker; v2 does not deprecate it.

## Gates

| # | Spike | GO/NO-GO |
|---|---|---|
| v2-0 | Worker+DO relay, Go agent with E2E, one session end-to-end | Keystroke RTT **< 250 ms p50** from LTE (measure as GATES.md Gate 0: socket frame timestamps, not feel); `wrangler tail` during typing shows **ciphertext only**; agent idle **6 h** then instant terminal (hibernation vs heartbeat proven); tampered frame (flip one byte via a test hook) kills session with `err`, renders nothing |
| v2-1 | Three agents (Go, Python, container) against the Worker | All pass with only `POPFLEET_URL` + `POPFLEET_E2E_KEY` env changes; v1-vs-v2 diff in each agent stays reviewably small (~100 LOC Go) |
| v2-2 | Ops semantics survive the port | Revoke drops live socket + rejects reconnect at hello; duplicate-token connect replaces zombie (v1 rule); `yes` 60 s firehose in one session + typing in second: responsive, and daily CF free-tier quota math shown in a comment |
| v2-3 | 7-day dogfood from outside the house, no VPN, no jumpbox | The Chrome+SOCKS5 workflow is not used for these machines all week; if it is, record why — that reason is v3's spec |

## Non-goals (v2)

WebRTC/P2P direct path (v3, funded only by v2-3 latency/trust evidence) ·
multi-fleet/multi-user · TURN anything · session receipts · encrypting `resize`/`exit`
metadata · custom domain (workers.dev is the zero-onboarding point).

## Open questions

- [ ] `cmd` in `POST /api/machines/{id}/term`: panel encrypts it (has the key) — but
      the *API-integration* caller (INTEGRATION.md) must then also hold the E2E key
      to send `cmd`. Lean: integration doc gains a 10-line "encrypt cmd" snippet;
      plaintext-cmd-over-API rejected.
- [ ] Free-plan DO storage class + limits at personal-fleet scale: verify with a
      1-day soak before Gate v2-3, numbers into implementation-notes.
- [ ] Does `sid` in the term URL + hibernation survive Worker redeploys mid-session?
      Acceptable answer: session dies with banner, reconnect works.

## Next steps

1. Gate v2-0 spike: minimal DO relay speaking PROTOCOL.md v1e + Go agent E2E branch.
   Reuse poptail's WebCrypto decrypt in term.js nearly verbatim.
2. Agent coalescing (16 ms/4 KiB) — land it on main first; v1 benefits immediately.
3. v2-1/v2-2, then move `docs/RDD.md` untouched and add this file as `docs/RDD-v2.md`;
   implementation-notes keeps accruing one line per decision, DEVIATION-marked.
