# popfleet v2 relay — setup

The relay is a Cloudflare Worker + one Durable Object. There is no server,
no DNS record and no TLS setup: the URL comes from two names —

- **worker name**: `"name": "popfleet"` in [wrangler.jsonc](wrangler.jsonc)
- **account subdomain**: an account-level Cloudflare setting (mine: `ejoliet`)

which combine to `https://popfleet.ejoliet.workers.dev`. To change the
`popfleet` part, edit `name` in wrangler.jsonc. The `ejoliet` part is not in
this repo — it lives in the Cloudflare account (Dashboard → Workers & Pages →
"Your subdomain", one-time; the first deploy prompts for it if unset).
A custom domain is deliberately a non-goal (docs/RDD-v2.md).

## First deploy (once)

```sh
cd worker

# 1. panel + agent scripts -> public/ (they are committed, but re-sync
#    after touching internal/panel/* or contrib/agent.*)
./sync-assets.sh

# 2. login (opens browser, once per machine)
npx wrangler login

# 3. admin token: mint it, keep it in your password manager, hand it to wrangler
openssl rand -hex 32
npx wrangler secret put POPFLEET_ADMIN_TOKEN     # paste the value when asked

# 4. ship it
npx wrangler deploy
# prints the URL, e.g. https://popfleet.ejoliet.workers.dev
```

## Fleet E2E key (once, NOT a wrangler secret)

```sh
openssl rand -base64 32      # this is POPFLEET_E2E_KEY
```

Keep it in the password manager next to the admin token. It goes to **agents'
env and the panel prompt only** — never `wrangler secret put` it; the whole
point is that Cloudflare relays ciphertext it cannot read.

## Enroll machines

1. Open the panel URL, paste the admin token (asked once per tab).
2. **Add machine** → panel asks for the E2E key once (kept in localStorage),
   then shows copy-paste enrollment blocks that already carry both
   `POPFLEET_URL` and `POPFLEET_E2E_KEY`.
3. Run a block on the target box. Green dot within seconds.

## Day-2

```sh
npx wrangler deploy            # redeploy after code/asset changes
npx wrangler tail              # live logs — during a session you must see
                               # base64 blobs, never shell text (Gate v2-0)
npx wrangler dev --port 8788   # local relay for hacking; secrets come from
                               # .dev.vars (gitignored), e.g.:
                               #   POPFLEET_ADMIN_TOKEN=test-admin-token
```

Acceptance checks against the deployed relay (RTT, tamper, ciphertext):
[docs/GATES-v2.md](../docs/GATES-v2.md), harness in `contrib/gateharness`.

## Rotation / revocation

- Revoke one machine: panel Delete (or `DELETE /api/machines/{id}`).
- Machine was *compromised*: it knew the fleet key — revoke, then rotate:
  new `openssl rand -base64 32`, update every agent's env file, restart
  agents, paste the new key once in the panel (it re-prompts after the old
  key fails).
- Admin token rotation: `npx wrangler secret put POPFLEET_ADMIN_TOKEN` again.
