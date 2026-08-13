#!/bin/sh
# Populate worker/public/ from the single-source panel (internal/panel) and
# agent scripts (contrib/). The panel files are identical on both brokers —
# v2 behavior (E2E, key-carrying enrollment blocks) is negotiated at runtime
# via the relay's {"t":"e2e"} frame and /config.json, which only exist here.
# Run before `wrangler deploy`; the copies are committed so a deploy never
# depends on having run this.
set -eu
cd "$(dirname "$0")"
SRC=../internal/panel

rm -rf public
mkdir -p public/vendor

cp "$SRC/index.html" "$SRC/term.html" "$SRC/index.js" "$SRC/term.js" public/
cp "$SRC"/vendor/xterm.js "$SRC"/vendor/xterm.css "$SRC"/vendor/addon-fit.js public/vendor/
cp ../contrib/agent.sh ../contrib/agent.py public/

# v2 marker: the panel adds POPFLEET_E2E_KEY to enrollment blocks when this
# file exists (the v1 Go broker 404s it).
printf '{"e2e":true}\n' > public/config.json

# Same CSP the Go broker sets in internal/panel/panel.go. Deliberately no
# frame-ancestors: "open the session URL in an iframe" is the integration
# surface; the one-time 60 s key guards the session (PROTOCOL.md).
cat > public/_headers <<'EOF'
/*
  Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws: wss:; img-src 'self' data:
EOF

echo "worker/public synced from internal/panel + contrib"
