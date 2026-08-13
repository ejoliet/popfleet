// popfleet v2 Worker: admin auth + routing. All fleet state and both
// WebSocket endpoints live in the single Fleet Durable Object; this layer
// only checks the admin bearer (the DO trusts what the Worker forwards)
// and rewrites /term/{sid} to the terminal page asset.
//
// Trust model (docs/RDD-v2.md): this code relays ciphertext. Terminal bytes
// and cmd values are AES-256-GCM under a key that never reaches Cloudflare;
// `wrangler tail` during a live session shows base64 ciphertext only.

import { Fleet } from "./fleet.js";
export { Fleet };

async function sha256(s) {
  return crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
}

// Constant-time admin check, exactly v1's shape: compare sha256 sums, so
// length is hidden and the compare cannot short-circuit.
async function adminOK(request, env) {
  const h = request.headers.get("Authorization") || "";
  if (!h.startsWith("Bearer ")) return false;
  const got = await sha256(h.slice(7));
  const want = await sha256(env.POPFLEET_ADMIN_TOKEN || "");
  return crypto.subtle.timingSafeEqual(got, want);
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const path = url.pathname;

    if (path === "/healthz") return new Response("ok");

    // Terminal page: same HTML for every sid; the one-time key in the query
    // string is what guards the session, not the URL shape. Fetch "/term",
    // not "/term.html": asset html_handling 307-redirects explicit .html
    // paths, and that redirect must never reach the browser — it would strip
    // the sid from location.pathname and break the HKDF salt.
    if (path.startsWith("/term/")) {
      return env.ASSETS.fetch(new Request(new URL("/term", url), request));
    }

    const fleet = env.FLEET.get(env.FLEET.idFromName("fleet"));

    if (path === "/ws/agent" || path.startsWith("/ws/term/")) {
      if (request.headers.get("Upgrade") !== "websocket") {
        return new Response("websocket endpoint", { status: 426 });
      }
      return fleet.fetch(request); // enrollment token / session key checked inside
    }

    if (path.startsWith("/api/")) {
      if (!env.POPFLEET_ADMIN_TOKEN) {
        return new Response("POPFLEET_ADMIN_TOKEN secret is not set (wrangler secret put POPFLEET_ADMIN_TOKEN)", { status: 500 });
      }
      if (!(await adminOK(request, env))) {
        return new Response("unauthorized", { status: 401 });
      }
      return fleet.fetch(request);
    }

    return env.ASSETS.fetch(request);
  },
};
