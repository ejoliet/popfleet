# Embedding a popfleet terminal in your own app

The one-time session URL is the entire integration surface. Your app asks the broker
for a terminal on a machine, gets back a URL, and puts that URL in an iframe or opens
it in a tab. There is no SDK, no JS bundle to import, and nothing to keep in sync — the
[wire protocol](PROTOCOL.md) stays between the browser page and the broker.

## The one call

```
POST /api/machines/{id}/term
Authorization: Bearer $POPFLEET_ADMIN_TOKEN
{"cmd":"htop"}          <- optional; absent means a login shell

200 {"url":"/term/<sid>?k=<key>","sid":"<sid>"}
```

```sh
curl -s -X POST \
  -H "Authorization: Bearer $POPFLEET_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"htop"}' \
  https://fleet.example/api/machines/6f1c3a2b/term
# {"url":"/term/9a3f…?k=1d7c…","sid":"9a3f…"}
```

Get machine ids from `GET /api/machines`.

Rules that matter:

- `url` is **relative to the broker**. Prefix it with the broker origin if your app
  lives somewhere else.
- The key is good for **60 seconds** and is **consumed at the WebSocket upgrade** — the
  first load wins, every later attempt gets `403 invalid, expired or already-used
  session key`. Mint one per terminal you are about to show, not one per page you might
  render.
- Mint it **server-side**. `POPFLEET_ADMIN_TOKEN` opens the whole fleet; the session URL
  opens one shell on one machine once. Only the URL should ever reach a browser.
- A refresh of the terminal tab will not work: the key is spent. Mint a new one.
- v1 sets no `frame-ancestors` and no `X-Frame-Options`, so any page may frame the
  terminal. The one-time key, not the origin, is what gates access.

## In a page

Your backend exposes something like `POST /my-app/terminal/:machine` that forwards the
call above with the admin token and returns the absolute URL. The front end:

```js
const BROKER = "https://fleet.example";

async function openTerminal(machineId, mount) {
  // your backend holds POPFLEET_ADMIN_TOKEN and returns {url} from the broker
  const r = await fetch(`/my-app/terminal/${machineId}`, { method: "POST" });
  if (!r.ok) throw new Error("could not mint a session");
  const { url } = await r.json();

  const frame = document.createElement("iframe");
  frame.src = BROKER + url;          // 60 s to load it, and one load only
  frame.style.cssText = "width:100%;height:100%;border:0";
  frame.allow = "clipboard-write";
  mount.replaceChildren(frame);
  return frame;
}

// or, for a tab instead of an iframe:
//   window.open(BROKER + url, "_blank");
```

The page you load is the same terminal the panel uses: xterm.js over
`/ws/term/{sid}`, the browser owns the geometry, and when the shell exits or the agent
goes away it shows a banner rather than a dead black rectangle.
