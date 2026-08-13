#!/bin/sh
# popfleet agent installer.
#   curl -sSf https://fleet.example/agent.sh | POPFLEET_URL=https://fleet.example sh -s -- <TOKEN>
# Detects OS/arch, fetches the static binary, writes a 0600 env file with the
# token (never in argv), and installs a systemd unit (Linux) or launchd
# LaunchAgent (macOS).
set -eu

TOKEN="${1:-${POPFLEET_TOKEN:-}}"
URL="${POPFLEET_URL:-${2:-}}"
NAME="${POPFLEET_NAME:-$(hostname -s 2>/dev/null || hostname)}"
E2E_KEY="${POPFLEET_E2E_KEY:-}"

[ -n "$TOKEN" ] || { echo "usage: agent.sh <TOKEN>   (POPFLEET_URL must be set in env)" >&2; exit 1; }
[ -n "$URL" ]   || { echo "error: POPFLEET_URL is not set" >&2; exit 1; }
if [ -z "$E2E_KEY" ]; then
  echo "note: POPFLEET_E2E_KEY is not set. The v2 Worker relay rejects agents" >&2
  echo "      without it; only a v1 LAN broker accepts plaintext sessions." >&2
fi

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) echo "error: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "error: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

# Try the broker first: it can serve a copy of itself when the target box runs
# the same OS/arch, which means enrollment works with no published release.
# Fall back to the GitHub release for cross-platform boxes.
RELEASE_URL="https://github.com/ejoliet/popfleet/releases/latest/download/popfleet-$OS-$ARCH"
if [ -n "${POPFLEET_BIN_URL:-}" ]; then
  BIN_URLS="$POPFLEET_BIN_URL"
else
  BIN_URLS="$URL/bin/popfleet-$OS-$ARCH $RELEASE_URL"
fi

if [ "$(id -u)" = 0 ]; then
  BIN=/usr/local/bin/popfleet
else
  BIN="$HOME/.local/bin/popfleet"
  mkdir -p "$HOME/.local/bin"
fi

GOT=
for u in $BIN_URLS; do
  echo "fetching $u -> $BIN"
  if curl -fsSL -o "$BIN.tmp" "$u"; then GOT=$u; break; fi
  echo "  not available there"
done
if [ -z "$GOT" ]; then
  rm -f "$BIN.tmp"
  echo "error: no popfleet-$OS-$ARCH binary available." >&2
  echo "  The broker only serves its own platform, and no GitHub release exists yet." >&2
  echo "  Build one and point the installer at it:" >&2
  echo "    GOOS=$OS GOARCH=$ARCH CGO_ENABLED=0 go build -o popfleet ." >&2
  echo "    scp popfleet thisbox:  # then: POPFLEET_BIN_URL=file://... or install by hand" >&2
  exit 1
fi
chmod 755 "$BIN.tmp"
mv "$BIN.tmp" "$BIN"

umask 077  # env file carries the token: 0600, owner only

if [ "$OS" = linux ]; then
  if [ "$(id -u)" != 0 ]; then
    echo "error: Linux install writes /etc/popfleet.env and a systemd unit; re-run as root" >&2
    exit 1
  fi
  cat > /etc/popfleet.env <<EOF
POPFLEET_URL=$URL
POPFLEET_TOKEN=$TOKEN
POPFLEET_NAME=$NAME
${E2E_KEY:+POPFLEET_E2E_KEY=$E2E_KEY}
EOF
  chmod 600 /etc/popfleet.env
  cat > /etc/systemd/system/popfleet-agent.service <<EOF
[Unit]
Description=popfleet agent (outbound-only fleet terminal)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/popfleet.env
ExecStart=$BIN agent
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now popfleet-agent
  echo "popfleet agent installed and started (systemd: popfleet-agent)"
else
  ENVFILE="$HOME/.popfleet.env"
  cat > "$ENVFILE" <<EOF
export POPFLEET_URL=$URL
export POPFLEET_TOKEN=$TOKEN
export POPFLEET_NAME=$NAME
${E2E_KEY:+export POPFLEET_E2E_KEY=$E2E_KEY}
EOF
  chmod 600 "$ENVFILE"
  PLIST="$HOME/Library/LaunchAgents/com.popfleet.agent.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  # Token stays in the 0600 env file; the plist only sources it.
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.popfleet.agent</string>
  <key>ProgramArguments</key><array>
    <string>/bin/sh</string><string>-c</string>
    <string>. "\$HOME/.popfleet.env" &amp;&amp; exec "$BIN" agent</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "popfleet agent installed and started (launchd: com.popfleet.agent)"
fi
