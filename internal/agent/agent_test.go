package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ejoliet/popfleet/internal/proto"
)

func TestAgentWSURL(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"http://fleet.example":             "ws://fleet.example/ws/agent",
		"https://fleet.example":            "wss://fleet.example/ws/agent",
		"https://fleet.example/":           "wss://fleet.example/ws/agent",
		"https://fleet.example///":         "wss://fleet.example/ws/agent",
		"ws://127.0.0.1:7333":              "ws://127.0.0.1:7333/ws/agent",
		"wss://fleet.example/base":         "wss://fleet.example/base/ws/agent",
		"https://fleet.example:8443/base/": "wss://fleet.example:8443/base/ws/agent",
	}
	for in, want := range ok {
		got, err := agentWSURL(in)
		if err != nil {
			t.Errorf("agentWSURL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("agentWSURL(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "fleet.example", "ftp://fleet.example", "://nope"} {
		if got, err := agentWSURL(bad); err == nil {
			t.Errorf("agentWSURL(%q) = %q, want an error", bad, got)
		}
	}
}

// docs/PROTOCOL.md: reconnect with backoff 1 s, doubling, cap 30 s. Jitter
// stays in Run and is not tested here.
func TestNextBackoff(t *testing.T) {
	t.Parallel()
	want := []time.Duration{2, 4, 8, 16, 30, 30}
	d := time.Second
	for i, w := range want {
		d = nextBackoff(d)
		if d != w*time.Second {
			t.Fatalf("step %d: backoff = %v, want %v", i+1, d, w*time.Second)
		}
	}
	if got := nextBackoff(maxBackoff); got != maxBackoff {
		t.Errorf("nextBackoff(cap) = %v, want the cap to hold", got)
	}
	if got := nextBackoff(5 * time.Minute); got != maxBackoff {
		t.Errorf("nextBackoff(5m) = %v, want it clamped to %v", got, maxBackoff)
	}
	if maxBackoff != 30*time.Second {
		t.Errorf("maxBackoff = %v, spec says 30s", maxBackoff)
	}
}

// ---- PTY ----

// collect runs pump over a real PTY and returns everything it wrote to the wire.
func collect(t *testing.T, p *ptySession, sid string) []proto.Msg {
	t.Helper()
	var mu sync.Mutex
	var msgs []proto.Msg
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(p, sid, func(m proto.Msg) error {
			mu.Lock()
			msgs = append(msgs, m)
			mu.Unlock()
			return nil
		}, func() {})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		p.cmd.Process.Kill()
		t.Fatal("pump never finished")
	}
	mu.Lock()
	defer mu.Unlock()
	return msgs
}

func lastExit(t *testing.T, msgs []proto.Msg) int {
	t.Helper()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].T == "exit" {
			if msgs[i].Code == nil {
				t.Fatal("exit frame has no code")
			}
			return *msgs[i].Code
		}
	}
	t.Fatalf("no exit frame in %+v", msgs)
	return 0
}

func output(msgs []proto.Msg) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.T == "out" {
			b, _ := proto.Dec(m.Data)
			sb.Write(b)
		}
	}
	return sb.String()
}

func requirePTY(t *testing.T) {
	t.Helper()
	p, err := spawn("true")
	if err != nil {
		t.Skipf("no usable PTY here: %v", err)
	}
	p.cmd.Process.Kill()
	p.f.Close()
	p.cmd.Wait()
}

func TestSpawnCmdOutputAndExitZero(t *testing.T) {
	requirePTY(t)
	p, err := spawn("echo popfleet-marker")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	msgs := collect(t, p, "s1")
	if got := output(msgs); !strings.Contains(got, "popfleet-marker") {
		t.Errorf("PTY output %q does not contain the echoed marker", got)
	}
	if code := lastExit(t, msgs); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if msgs[len(msgs)-1].T != "exit" {
		t.Error("exit is not the last frame for the sid")
	}
	for _, m := range msgs {
		if m.Sid != "s1" {
			t.Fatalf("frame without the session id: %+v", m)
		}
	}
}

func TestSpawnCmdExitCodePropagates(t *testing.T) {
	requirePTY(t)
	for _, want := range []int{0, 1, 3, 7} {
		p, err := spawn(fmt.Sprintf("exit %d", want))
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if got := lastExit(t, collect(t, p, "s")); got != want {
			t.Errorf("exit code = %d, want %d", got, want)
		}
	}
}

func TestSpawnNonUTF8Output(t *testing.T) {
	requirePTY(t)
	// The reason data is base64 on the wire: a shell can print anything.
	p, err := spawn(`printf '\377\376\001'`)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	msgs := collect(t, p, "s")
	if !strings.Contains(output(msgs), "\xff\xfe\x01") {
		t.Errorf("non-UTF8 bytes were corrupted: %q", output(msgs))
	}
}

// ---- session loop against a fake broker ----

type fakeBroker struct {
	t     *testing.T
	srv   *httptest.Server
	conns chan *websocket.Conn
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{t: t, conns: make(chan *websocket.Conn, 4)}
	stop := make(chan struct{})
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/agent" {
			http.NotFound(w, r)
			return
		}
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		fb.conns <- ws
		<-stop // keep the handler alive; the test owns the socket
		ws.Close()
	}))
	t.Cleanup(fb.srv.Close)
	t.Cleanup(func() { close(stop) })
	return fb
}

func (fb *fakeBroker) url() string { return fb.srv.URL }

func (fb *fakeBroker) accept() *websocket.Conn {
	fb.t.Helper()
	select {
	case ws := <-fb.conns:
		return ws
	case <-time.After(5 * time.Second):
		fb.t.Fatal("agent never dialled")
		return nil
	}
}

func read(t *testing.T, ws *websocket.Conn) proto.Msg {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var m proto.Msg
	if err := ws.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

// wantFrame reads until tag shows up, ignoring heartbeats and other traffic.
func wantFrame(t *testing.T, ws *websocket.Conn, tag string) proto.Msg {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := read(t, ws); m.T == tag {
			return m
		}
	}
	t.Fatalf("no %q frame", tag)
	return proto.Msg{}
}

type running struct {
	helloOK bool
	err     error
}

// start runs one agent session against the fake broker and hands back the
// broker side of the socket plus the session's eventual result.
func (fb *fakeBroker) start(token string) (*websocket.Conn, chan running) {
	fb.t.Helper()
	wsURL, err := agentWSURL(fb.url())
	if err != nil {
		fb.t.Fatal(err)
	}
	res := make(chan running, 1)
	go func() {
		ok, err := session(wsURL, token, "testbox")
		res <- running{ok, err}
	}()
	return fb.accept(), res
}

func TestSessionHelloAndOpenRelay(t *testing.T) {
	requirePTY(t)
	fb := newFakeBroker(t)
	ws, res := fb.start("tok123")

	hello := read(t, ws)
	if hello.T != "hello" || hello.Token != "tok123" || hello.Name != "testbox" {
		t.Fatalf("first frame = %+v, want hello with token and name", hello)
	}
	if hello.Ver != Version {
		t.Errorf("hello ver = %q, want %q", hello.Ver, Version)
	}
	ws.WriteJSON(proto.Msg{T: "hello_ok", ID: "m1"})

	// Unknown sids must be ignored, never fatal (docs/PROTOCOL.md).
	ws.WriteJSON(proto.Msg{T: "in", Sid: "ghost", Data: proto.Enc([]byte("x"))})
	ws.WriteJSON(proto.Msg{T: "resize", Sid: "ghost", C: 80, R: 24})
	ws.WriteJSON(proto.Msg{T: "close", Sid: "ghost"})
	ws.WriteJSON(proto.Msg{T: "somethingnew", Sid: "ghost"})

	ws.WriteJSON(proto.Msg{T: "open", Sid: "s1", Cmd: "echo popfleet-marker; exit 5"})
	var out strings.Builder
	var code int
	for {
		m := read(t, ws)
		if m.Sid != "s1" {
			t.Fatalf("unexpected frame %+v", m)
		}
		if m.T == "out" {
			b, err := proto.Dec(m.Data)
			if err != nil {
				t.Fatalf("agent sent undecodable data: %v", err)
			}
			out.Write(b)
			continue
		}
		if m.T == "exit" {
			if m.Code == nil {
				t.Fatal("exit frame without a code")
			}
			code = *m.Code
			break
		}
	}
	if !strings.Contains(out.String(), "popfleet-marker") {
		t.Errorf("output %q missing the marker", out.String())
	}
	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}

	ws.Close()
	select {
	case r := <-res:
		if !r.helloOK {
			t.Error("session did not report a successful hello (backoff would not reset)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not return after the socket closed")
	}
}

func TestSessionCloseKillsPTY(t *testing.T) {
	requirePTY(t)
	fb := newFakeBroker(t)
	ws, res := fb.start("tok")
	read(t, ws) // hello
	ws.WriteJSON(proto.Msg{T: "hello_ok", ID: "m1"})

	ws.WriteJSON(proto.Msg{T: "open", Sid: "s1", Cmd: "printf READY; cat"})
	first := wantFrame(t, ws, "out")
	if b, _ := proto.Dec(first.Data); !strings.Contains(string(b), "READY") {
		t.Fatalf("first output %q, want READY", b)
	}
	// A duplicate open for a live sid is ignored, not a second shell.
	ws.WriteJSON(proto.Msg{T: "open", Sid: "s1", Cmd: "echo SECOND-SHELL"})

	ws.WriteJSON(proto.Msg{T: "in", Sid: "s1", Data: proto.Enc([]byte("ping\n"))})
	if b, _ := proto.Dec(wantFrame(t, ws, "out").Data); !strings.Contains(string(b), "ping") {
		t.Errorf("keystrokes did not reach the PTY: %q", b)
	}

	ws.WriteJSON(proto.Msg{T: "close", Sid: "s1"})
	var saw strings.Builder
	for {
		m := read(t, ws)
		if m.T == "out" {
			b, _ := proto.Dec(m.Data)
			saw.Write(b)
			continue
		}
		if m.T == "exit" {
			if m.Sid != "s1" {
				t.Fatalf("exit for the wrong sid: %+v", m)
			}
			// docs/PROTOCOL.md: 128+N for a signal death, never a bare -1.
			// close SIGKILLs the shell, so the browser must see 137.
			if m.Code == nil {
				t.Fatal("exit after close has no code")
			}
			if *m.Code != 137 {
				t.Errorf("exit code after a SIGKILLed shell = %d, want 137 (128+SIGKILL)", *m.Code)
			}
			break
		}
	}
	if strings.Contains(saw.String(), "SECOND-SHELL") {
		t.Error("duplicate open spawned a second shell on a live sid")
	}
	// Input for the now-dead sid is ignored rather than fatal.
	ws.WriteJSON(proto.Msg{T: "in", Sid: "s1", Data: proto.Enc([]byte("x"))})
	ws.WriteJSON(proto.Msg{T: "open", Sid: "s2", Cmd: "echo still-alive"})
	if b, _ := proto.Dec(wantFrame(t, ws, "out").Data); !strings.Contains(string(b), "still-alive") {
		t.Errorf("agent broken after close: %q", b)
	}

	ws.Close()
	<-res
}

func TestSessionRejectsBadHelloResponse(t *testing.T) {
	t.Parallel()
	fb := newFakeBroker(t)
	ws, res := fb.start("revoked")
	read(t, ws)
	ws.WriteJSON(proto.Msg{T: "err", Msg: "unknown token"})
	select {
	case r := <-res:
		if r.helloOK {
			t.Error("session claimed hello succeeded after a rejection")
		}
		if r.err == nil {
			t.Error("session returned no error for a rejected hello")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session hung after a rejected hello")
	}
}

func TestSessionDialFailureIsNotHelloOK(t *testing.T) {
	t.Parallel()
	// Nothing is listening: a dial failure must not reset the backoff.
	ok, err := session("ws://127.0.0.1:1/ws/agent", "tok", "n")
	if ok {
		t.Error("failed dial reported a successful hello")
	}
	if err == nil {
		t.Error("failed dial returned no error")
	}
}

// Socket loss must kill every PTY (docs/PROTOCOL.md agent rules), and that has
// to include what the shell was running: an orphaned build or `tail -f` on the
// operator's machine is exactly what "no PTY of ours outlives its socket" is
// there to prevent.
//
// The `&& wait` keeps the shell from exec'ing itself into sleep, so the
// long-running process is a grandchild of the agent, as it is whenever an
// operator types a command into a live shell.
func TestSocketLossKillsPTYChildren(t *testing.T) {
	requirePTY(t)
	marker := marker()
	t.Cleanup(func() { exec.Command("pkill", "-f", "sleep "+marker).Run() })

	fb := newFakeBroker(t)
	ws, res := fb.start("tok")
	read(t, ws)
	ws.WriteJSON(proto.Msg{T: "hello_ok", ID: "m1"})
	ws.WriteJSON(proto.Msg{T: "open", Sid: "s1", Cmd: "sleep " + marker + " & printf READY; wait"})
	wantFrame(t, ws, "out")
	if !sleeperAlive(t, marker) {
		t.Fatalf("test setup: no `sleep %s` process to watch", marker)
	}

	ws.Close()
	select {
	case <-res:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not return after socket loss")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !sleeperAlive(t, marker) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("`sleep %s` outlived the agent socket: the PTY's children are orphaned, "+
		"not killed (internal/agent/agent.go:105 kills the shell pid, not its process group)", marker)
}

// marker is a sleep duration nothing else on the machine would plausibly use,
// so pgrep can find exactly our child.
func marker() string { return fmt.Sprintf("%d", 40000+time.Now().UnixNano()%9000) }

func sleeperAlive(t *testing.T, marker string) bool {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available; cannot observe PTY children")
	}
	out, _ := exec.Command("pgrep", "-f", "sleep "+marker).Output()
	return len(strings.TrimSpace(string(out))) > 0
}
