#!/usr/bin/env python3
"""popfleet reference agent: dials out, holds one WebSocket, serves PTYs.

Embed it in your own app (config is env-only, never argv):

    import os, threading, agent
    os.environ["POPFLEET_URL"] = "https://fleet.example"     # broker base URL
    os.environ["POPFLEET_TOKEN"] = "<enrollment token from the panel>"
    os.environ["POPFLEET_NAME"] = "my-app"        # optional, defaults to hostname
    threading.Thread(target=agent.main, daemon=True).start()

Standalone: POPFLEET_URL=... POPFLEET_TOKEN=... python3 agent.py
Requires:   Python 3.9+ and `pip install websockets` (the only dependency).
Protocol:   docs/PROTOCOL.md, frozen v1. This file implements exactly that.
"""

import asyncio
import base64
import errno
import fcntl
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

VER = "1.0.0"
HEARTBEAT_S = 10
BACKOFF_START_S = 1.0
BACKOFF_MAX_S = 30.0
READ_SIZE = 65536


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
        for secret in ("POPFLEET_TOKEN", "POPFLEET_URL"):
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

    def __init__(self, ws, loop):
        self.ws = ws
        self.loop = loop
        self.ptys = {}                  # sid -> Pty
        self.outbox = asyncio.Queue()   # frames waiting for the socket
        self.enrolled = False

    def send(self, **frame):
        self.outbox.put_nowait(frame)

    async def pump_out(self):
        while True:
            await self.ws.send(json.dumps(await self.outbox.get()))

    async def heartbeat(self):
        while True:
            await asyncio.sleep(HEARTBEAT_S)
            self.send(t="hb")

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
            if p is not None:
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
        try:
            p = Pty(cmd)
        except OSError as e:
            log("open %s failed: %s" % (sid, e))
            self.send(t="exit", sid=sid, code=127)
            return
        self.ptys[sid] = p
        self.loop.add_reader(p.fd, self.drain, sid)

    def drain(self, sid):
        p = self.ptys.get(sid)
        if p is None:
            return
        data = p.read()
        if data is None:
            return
        if data:
            self.send(t="out", sid=sid, data=base64.b64encode(data).decode())
        else:
            self.reap(sid, notify=True)             # EOF: the shell exited

    def reap(self, sid, notify):
        p = self.ptys.pop(sid, None)
        if p is None:
            return
        self.loop.remove_reader(p.fd)               # before the fd is closed
        code = p.kill()
        if notify:
            self.send(t="exit", sid=sid, code=code)

    def shutdown(self):
        for sid in list(self.ptys):
            self.reap(sid, notify=False)


async def run_connection(url, token, name, stop):
    """One connection, start to finish. Returns True if we ever enrolled."""
    agent = None
    try:
        async with websockets.connect(url, open_timeout=15, close_timeout=5) as ws:
            agent = Agent(ws, asyncio.get_running_loop())
            await ws.send(json.dumps(
                {"t": "hello", "token": token, "name": name, "ver": VER}))
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


async def serve(url, token, name):
    stop = asyncio.Event()
    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop.set)
        except (NotImplementedError, RuntimeError, ValueError):
            pass                # embedded in a worker thread: the host owns signals
    delay = BACKOFF_START_S
    while not stop.is_set():
        if await run_connection(url, token, name, stop):
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
    log("agent %s -> %s as %s" % (VER, url, name))
    asyncio.run(serve(url, token, name))


if __name__ == "__main__":
    main()
