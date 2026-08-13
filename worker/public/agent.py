#!/usr/bin/env python3
"""popfleet reference agent: dials out, holds one WebSocket, serves PTYs.

Embed it in your own app (config is env-only, never argv):

    import os, threading, agent
    os.environ["POPFLEET_URL"] = "https://fleet.example"     # broker base URL
    os.environ["POPFLEET_TOKEN"] = "<enrollment token from the panel>"
    os.environ["POPFLEET_NAME"] = "my-app"        # optional, defaults to hostname
    os.environ["POPFLEET_E2E_KEY"] = "<base64>"   # required for the Worker relay
    threading.Thread(target=agent.main, daemon=True).start()

Standalone: POPFLEET_URL=... POPFLEET_TOKEN=... python3 agent.py
Requires:   Python 3.9+ and `pip install websockets`; with POPFLEET_E2E_KEY
            set, also `pip install cryptography` (AES-256-GCM).
Protocol:   docs/PROTOCOL.md v1; with POPFLEET_E2E_KEY, the v1e payload
            encryption from docs/RDD-v2.md on top (frame shapes unchanged).
"""

import asyncio
import base64
import errno
import fcntl
import hashlib
import hmac
import json
import os
import pty
import random
import signal
import socket
import struct
import sys
import termios
from urllib.parse import urlsplit, urlunsplit

import websockets

VER = "2.0.0"
HEARTBEAT_S = 10
BACKOFF_START_S = 1.0
BACKOFF_MAX_S = 30.0
READ_SIZE = 65536
FLUSH_S = 0.016          # coalesce PTY output: flush every 16 ms ...
FLUSH_BYTES = 4096       # ... or 4 KiB, whichever comes first
# hb is this exact byte string: the Worker relay absorbs it with a
# byte-exact WebSocket auto-response so an idle fleet never wakes the DO.
HB_FRAME = '{"t":"hb"}'


class E2E(object):
    """Protocol v1e payload encryption (docs/RDD-v2.md): data/cmd values are
    base64( nonce(12) || AES-256-GCM ciphertext ) under a per-session key
    HKDF-SHA256(fleet key, salt=sid, info=b"popfleet-v2")."""

    def __init__(self, fleet_key, sid):
        # Single-block HKDF-SHA256: extract then one expand round (32 bytes).
        prk = hmac.new(sid.encode(), fleet_key, hashlib.sha256).digest()
        okm = hmac.new(prk, b"popfleet-v2\x01", hashlib.sha256).digest()
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        self.aead = AESGCM(okm)

    def seal(self, data):
        nonce = os.urandom(12)
        return base64.b64encode(nonce + self.aead.encrypt(nonce, data, None)).decode()

    def open(self, wire):
        """Plaintext bytes, or None for any forged/tampered/wrong-key frame."""
        try:
            raw = base64.b64decode(wire)
            return self.aead.decrypt(raw[:12], raw[12:], None)
        except Exception:
            return None


def fleet_key_from_env():
    raw = os.environ.get("POPFLEET_E2E_KEY", "").strip()
    if not raw:
        return None
    try:
        key = base64.b64decode(raw, validate=True)
    except Exception:
        raise SystemExit("popfleet: POPFLEET_E2E_KEY is not valid base64")
    if len(key) != 32:
        raise SystemExit("popfleet: POPFLEET_E2E_KEY must be 32 bytes "
                         "(generate with: openssl rand -base64 32)")
    try:
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM  # noqa: F401
    except ImportError:
        raise SystemExit("popfleet: POPFLEET_E2E_KEY is set but the "
                         "'cryptography' package is missing: pip install cryptography")
    return key


def log(msg):
    sys.stderr.write("popfleet: %s\n" % msg)
    sys.stderr.flush()


def require(name):
    value = os.environ.get(name, "").strip()
    if not value:
        raise SystemExit(
            "popfleet: %s is not set. Config is env-only: secrets passed as "
            "command-line flags are visible to every user in `ps`." % name)
    return value


def ws_url(raw):
    """Broker base URL -> the agent endpoint, exactly as the Go agent does it.

    https://fleet.example -> wss://fleet.example/ws/agent
    """
    parts = urlsplit(raw)
    scheme = {"http": "ws", "https": "wss", "ws": "ws", "wss": "wss"}.get(parts.scheme)
    if scheme is None or not parts.netloc:
        raise SystemExit("popfleet: bad POPFLEET_URL %r "
                         "(want http/https/ws/wss + host)" % raw)
    return urlunsplit((scheme, parts.netloc,
                       parts.path.rstrip("/") + "/ws/agent", "", ""))


class Pty(object):
    """One login shell (or `cmd`) on its own pseudo-terminal."""

    def __init__(self, cmd):
        self.pid, self.fd = pty.fork()
        if self.pid == 0:                       # child: _exec never returns
            self._exec(cmd)
        os.set_blocking(self.fd, False)

    @staticmethod
    def _exec(cmd):
        shell = os.environ.get("SHELL") or "/bin/sh"
        os.environ.setdefault("TERM", "xterm-256color")
        for secret in ("POPFLEET_TOKEN", "POPFLEET_URL", "POPFLEET_E2E_KEY"):
            os.environ.pop(secret, None)        # the shell has no use for these
        try:
            if cmd:
                os.execvp(shell, [shell, "-c", cmd])
            # leading '-' is the ancient convention for "this is a login shell"
            os.execvp(shell, ["-" + os.path.basename(shell)])
        except OSError:
            pass
        os._exit(127)

    def read(self):
        """Bytes from the shell; b"" once it is gone; None if not ready yet."""
        try:
            return os.read(self.fd, READ_SIZE)
        except OSError as e:
            if e.errno == errno.EAGAIN:
                return None
            return b""                          # EIO on Linux == pty closed

    def write(self, data):
        try:
            os.write(self.fd, data)
        except OSError:
            pass                                # shell died mid-keystroke

    def resize(self, cols, rows):
        try:
            fcntl.ioctl(self.fd, termios.TIOCSWINSZ,
                        struct.pack("HHHH", rows, cols, 0, 0))
        except OSError:
            pass

    def kill(self):
        """Kill the shell and reap it. Returns its exit code."""
        try:
            os.killpg(self.pid, signal.SIGKILL)  # pty.fork() made it a session leader
        except OSError:
            pass                                 # already exited
        try:
            os.close(self.fd)
        except OSError:
            pass
        # ponytail: blocking waitpid. The child is already signalled or already
        # dead, so this returns at once; no reaper thread earns its keep here.
        try:
            _, status = os.waitpid(self.pid, 0)
        except OSError:
            return 0
        code = os.waitstatus_to_exitcode(status)
        return code if code >= 0 else 128 - code  # killed by signal N -> 128+N


class Agent(object):
    """One broker connection and the PTYs that live and die with it."""

    def __init__(self, ws, loop, fleet_key=None):
        self.ws = ws
        self.loop = loop
        self.fleet_key = fleet_key      # None = plaintext v1 (LAN broker)
        self.ptys = {}                  # sid -> Pty
        self.enc = {}                   # sid -> E2E (only when fleet_key set)
        self.obuf = {}                  # sid -> bytearray, coalesced PTY output
        self.oflush = {}                # sid -> TimerHandle for the 16 ms flush
        self.outbox = asyncio.Queue()   # frames waiting for the socket
        self.enrolled = False

    def send(self, **frame):
        self.outbox.put_nowait(json.dumps(frame))

    async def pump_out(self):
        while True:
            await self.ws.send(await self.outbox.get())

    async def heartbeat(self):
        while True:
            await asyncio.sleep(HEARTBEAT_S)
            self.outbox.put_nowait(HB_FRAME)

    async def pump_in(self):
        async for raw in self.ws:
            try:
                frame = json.loads(raw)
            except ValueError:
                continue
            self.handle(frame)

    def handle(self, m):
        tag = m.get("t")
        sid = m.get("sid")
        if tag == "hello_ok":
            self.enrolled = True
            log("enrolled as machine %s" % m.get("id"))
        elif tag == "open":
            self.open(sid, m.get("cmd"))
        elif tag == "in":
            p = self.ptys.get(sid)                  # unknown sid: ignored
            if p is None:
                pass
            elif sid in self.enc:
                data = self.enc[sid].open(m.get("data", ""))
                if data is None:                    # GCM fail: kill, never render
                    self.e2e_kill(sid, "e2e decrypt failed (tampered frame)")
                else:
                    p.write(data)
            else:
                p.write(base64.b64decode(m.get("data", "")))
        elif tag == "resize":
            p = self.ptys.get(sid)
            if p is not None:
                p.resize(int(m.get("c", 80)), int(m.get("r", 24)))
        elif tag == "close":
            self.reap(sid, notify=True)             # killed is still exited
        # unknown tags are ignored on purpose (forward compatibility)

    def open(self, sid, cmd):
        if not sid or sid in self.ptys:
            return                                  # duplicate open: ignored
        enc = None
        if self.fleet_key is not None:              # protocol v1e: cmd is ciphertext
            enc = E2E(self.fleet_key, sid)
            if cmd:
                plain = enc.open(cmd)
                if plain is None:                   # never run what we cannot authenticate
                    self.send(t="err", sid=sid,
                              msg="e2e decrypt failed (key mismatch or tampered cmd)")
                    return
                cmd = plain.decode("utf-8", "replace")
        try:
            p = Pty(cmd)
        except OSError as e:
            log("open %s failed: %s" % (sid, e))
            self.send(t="exit", sid=sid, code=127)
            return
        self.ptys[sid] = p
        if enc is not None:
            self.enc[sid] = enc
        self.loop.add_reader(p.fd, self.drain, sid)

    def drain(self, sid):
        p = self.ptys.get(sid)
        if p is None:
            return
        data = p.read()
        if data is None:
            return
        if data:
            # Coalesce: a `yes` firehose as frame-per-read is a per-frame cost
            # through the Worker relay; batch 16 ms / 4 KiB, whichever first.
            buf = self.obuf.setdefault(sid, bytearray())
            buf.extend(data)
            if len(buf) >= FLUSH_BYTES:
                self.flush(sid)
            elif sid not in self.oflush:
                self.oflush[sid] = self.loop.call_later(FLUSH_S, self.flush, sid)
        else:
            self.reap(sid, notify=True)             # EOF: the shell exited

    def flush(self, sid):
        timer = self.oflush.pop(sid, None)
        if timer is not None:
            timer.cancel()
        buf = self.obuf.pop(sid, None)
        if not buf:
            return
        data = bytes(buf)
        enc = self.enc.get(sid)
        wire = enc.seal(data) if enc else base64.b64encode(data).decode()
        self.send(t="out", sid=sid, data=wire)

    def e2e_kill(self, sid, why):
        """GCM auth failure: report err, kill the PTY, render nothing."""
        log("%s: %s" % (sid, why))
        self.obuf.pop(sid, None)                    # pending output dies with it
        self.send(t="err", sid=sid, msg=why)
        self.reap(sid, notify=False)

    def reap(self, sid, notify):
        p = self.ptys.pop(sid, None)
        if p is None:
            return
        self.flush(sid)                             # last buffered bytes first
        self.enc.pop(sid, None)
        self.loop.remove_reader(p.fd)               # before the fd is closed
        code = p.kill()
        if notify:
            self.send(t="exit", sid=sid, code=code)

    def shutdown(self):
        for sid in list(self.ptys):
            self.reap(sid, notify=False)


async def run_connection(url, token, name, stop, fleet_key):
    """One connection, start to finish. Returns True if we ever enrolled."""
    agent = None
    try:
        async with websockets.connect(url, open_timeout=15, close_timeout=5) as ws:
            agent = Agent(ws, asyncio.get_running_loop(), fleet_key)
            hello = {"t": "hello", "token": token, "name": name, "ver": VER}
            if fleet_key is not None:
                hello["e2e"] = True     # protocol v1e: payloads are ciphertext
            await ws.send(json.dumps(hello))
            tasks = [asyncio.ensure_future(co) for co in (
                agent.pump_in(), agent.pump_out(), agent.heartbeat(), stop.wait())]
            done, pending = await asyncio.wait(
                tasks, return_when=asyncio.FIRST_COMPLETED)
            for t in pending:
                t.cancel()
            for t in done:
                if t.exception() is not None:
                    log("socket lost: %s" % t.exception())
    except Exception as e:      # DNS, refused, TLS, bad handshake: all retryable
        log("connect failed: %s" % e)
    finally:
        if agent is not None:
            agent.shutdown()    # no PTY of ours outlives its socket
    return agent is not None and agent.enrolled


async def serve(url, token, name, fleet_key):
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop.set)
        except (NotImplementedError, RuntimeError, ValueError):
            pass                # embedded in a worker thread: the host owns signals
    delay = BACKOFF_START_S
    while not stop.is_set():
        if await run_connection(url, token, name, stop, fleet_key):
            delay = BACKOFF_START_S     # a connection that enrolled was healthy
        if stop.is_set():
            break
        pause = delay * random.uniform(0.8, 1.2)        # +/-20% jitter
        log("reconnecting in %.1fs" % pause)
        try:
            await asyncio.wait_for(stop.wait(), timeout=pause)
        except asyncio.TimeoutError:
            pass                # slept the full backoff; go round again
        delay = min(delay * 2, BACKOFF_MAX_S)
    log("stopped")


def main():
    url = ws_url(require("POPFLEET_URL"))
    token = require("POPFLEET_TOKEN")
    name = os.environ.get("POPFLEET_NAME", "").strip() or socket.gethostname()
    fleet_key = fleet_key_from_env()
    log("agent %s -> %s as %s (e2e %s)" %
        (VER, url, name, "on" if fleet_key else "off"))
    asyncio.run(serve(url, token, name, fleet_key))


if __name__ == "__main__":
    main()
