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

## v2 relay: `cmd` must be encrypted

Against the v2 Worker relay, agents speak [protocol v1e](RDD-v2.md): the
`cmd` value must be `base64( nonce(12) || AES-256-GCM )` under the fleet key,
or the agent refuses to run it (a plaintext `cmd` fails the GCM tag — by
design, the relay cannot tell the difference and never sees the key). Sealing
binds the cmd to the `sid`, which the mint call creates — so attaching a cmd
is two calls: mint (learn the sid), seal, mint again with `{"sid","cmd"}`.
The second call retires the first key and returns a fresh URL (the one-time
60 s semantics restart). The sealing 10-liner (Python):

```python
import base64, hashlib, hmac, os
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

def seal_cmd(fleet_key_b64: str, sid: str, cmd: str) -> str:
    ikm = base64.b64decode(fleet_key_b64)
    prk = hmac.new(sid.encode(), ikm, hashlib.sha256).digest()   # HKDF extract
    key = hmac.new(prk, b"popfleet-v2\x01", hashlib.sha256).digest()  # expand
    nonce = os.urandom(12)
    return base64.b64encode(nonce + AESGCM(key).encrypt(nonce, cmd.encode(), None)).decode()
```

```sh
r=$(curl -s -X POST -H "Authorization: Bearer $POPFLEET_ADMIN_TOKEN" $URL/api/machines/$MID/term)
sid=$(jq -r .sid <<<"$r")
sealed=$(python3 -c "...seal_cmd('$POPFLEET_E2E_KEY', '$sid', 'htop')...")
curl -s -X POST -H "Authorization: Bearer $POPFLEET_ADMIN_TOKEN" \
  -d "{\"sid\":\"$sid\",\"cmd\":\"$sealed\"}" $URL/api/machines/$MID/term
# -> fresh {"url": "/term/<sid>?k=<key>"} — open that one
```

Against the v1 Go broker, plaintext `{"cmd":"htop"}` keeps working unchanged.

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
