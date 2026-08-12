package broker

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ejoliet/popfleet/internal/proto"
	"github.com/ejoliet/popfleet/internal/store"
)

const adminToken = "test-admin-token-0123456789"

// ---- harness ----

type env struct {
	t     *testing.T
	b     *Broker
	st    *store.Store
	srv   *httptest.Server
	wsURL string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "popfleet.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return envWithStore(t, st)
}

func envWithStore(t *testing.T, st *store.Store) *env {
	t.Helper()
	b := New(st, adminToken)
	srv := httptest.NewServer(b.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, b: b, st: st, srv: srv, wsURL: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

func (e *env) req(method, path, body, bearer string) (*http.Response, string) {
	e.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, r)
	if err != nil {
		e.t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) admin(method, path, body string) (*http.Response, string) {
	e.t.Helper()
	return e.req(method, path, body, "Bearer "+adminToken)
}

// mint creates a machine through the real API and returns id and token.
func (e *env) mint(name string) (id, token string) {
	e.t.Helper()
	resp, body := e.admin("POST", "/api/tokens", fmt.Sprintf(`{"name":%q}`, name))
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST /api/tokens: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("mint response %q: %v", body, err)
	}
	m, ok := e.st.ByToken(out.Token)
	if !ok {
		e.t.Fatal("minted token is not in the store")
	}
	return m.ID, out.Token
}

type machineView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	LastSeen string `json:"last_seen"`
	AgentVer string `json:"agent_ver"`
	Sessions int    `json:"sessions"`
}

func (e *env) machines() []machineView {
	e.t.Helper()
	resp, body := e.admin("GET", "/api/machines", "")
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET /api/machines: %d %s", resp.StatusCode, body)
	}
	var out []machineView
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("machines response %q: %v", body, err)
	}
	return out
}

func (e *env) machine(id string) machineView {
	e.t.Helper()
	for _, m := range e.machines() {
		if m.ID == id {
			return m
		}
	}
	e.t.Fatalf("machine %s not in /api/machines", id)
	return machineView{}
}

// term mints a one-time session URL and splits it into sid and key.
func (e *env) term(id, cmd string) (sid, key string) {
	e.t.Helper()
	body := ""
	if cmd != "" {
		body = fmt.Sprintf(`{"cmd":%q}`, cmd)
	}
	resp, raw := e.admin("POST", "/api/machines/"+id+"/term", body)
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST term: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		URL string `json:"url"`
		Sid string `json:"sid"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		e.t.Fatalf("term response %q: %v", raw, err)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		e.t.Fatalf("term url %q: %v", out.URL, err)
	}
	if want := "/term/" + out.Sid; u.Path != want {
		e.t.Fatalf("term url path %q, want %q", u.Path, want)
	}
	key = u.Query().Get("k")
	if key == "" {
		e.t.Fatalf("term url has no key: %q", out.URL)
	}
	return out.Sid, key
}

// peer is a raw protocol speaker: a fake agent or a fake browser.
type peer struct {
	t   *testing.T
	ws  *websocket.Conn
	wmu sync.Mutex
}

func (p *peer) send(m proto.Msg) {
	p.t.Helper()
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := p.ws.WriteJSON(m); err != nil {
		p.t.Fatalf("send %s: %v", m.T, err)
	}
}

// trySend and tryRecv are for use off the test goroutine, where t.Fatalf would
// not stop the test and could hang whoever is waiting on the result.
func (p *peer) trySend(m proto.Msg) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return p.ws.WriteJSON(m)
}

func (p *peer) tryRecv() (proto.Msg, error) {
	var m proto.Msg
	b, err := p.readRaw()
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (p *peer) readRaw() ([]byte, error) {
	p.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, b, err := p.ws.ReadMessage()
	return b, err
}

// recv returns the next frame, failing the test on error or timeout.
func (p *peer) recv() proto.Msg {
	p.t.Helper()
	b, err := p.readRaw()
	if err != nil {
		p.t.Fatalf("recv: %v", err)
	}
	var m proto.Msg
	if err := json.Unmarshal(b, &m); err != nil {
		p.t.Fatalf("recv %q: %v", b, err)
	}
	return m
}

// want waits for a frame with tag t, skipping others (hb, stray out, ...).
func (p *peer) want(tag string) proto.Msg {
	p.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := p.recv()
		if m.T == tag {
			return m
		}
	}
	p.t.Fatalf("timed out waiting for %q frame", tag)
	return proto.Msg{}
}

// expectClosed asserts the peer's socket dies without another data frame.
func (p *peer) expectClosed() {
	p.t.Helper()
	if b, err := p.readRaw(); err == nil {
		p.t.Fatalf("connection still open, got frame %s", b)
	}
}

func (e *env) dial(path string) (*peer, *http.Response, error) {
	ws, resp, err := websocket.DefaultDialer.Dial(e.wsURL+path, nil)
	if err != nil {
		return nil, resp, err
	}
	e.t.Cleanup(func() { ws.Close() })
	return &peer{t: e.t, ws: ws}, resp, nil
}

// agent connects a fake agent and completes the hello handshake.
func (e *env) agent(token, name string) *peer {
	e.t.Helper()
	p, _, err := e.dial("/ws/agent")
	if err != nil {
		e.t.Fatalf("dial /ws/agent: %v", err)
	}
	p.send(proto.Msg{T: "hello", Token: token, Name: name, Ver: "9.9.9"})
	m := p.want("hello_ok")
	if m.ID == "" {
		e.t.Fatalf("hello_ok without machine id: %+v", m)
	}
	return p
}

// browser claims a session URL over the terminal socket.
func (e *env) browser(sid, key string) *peer {
	e.t.Helper()
	p, resp, err := e.dial("/ws/term/" + sid + "?k=" + key)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		e.t.Fatalf("dial /ws/term/%s: %v (status %d)", sid, err, code)
	}
	return p
}

// ---- constants pinned to the spec ----

func TestSpecConstants(t *testing.T) {
	t.Parallel()
	// docs/PROTOCOL.md: online is false once two 10 s heartbeats are missed.
	if offlineAfter != 25*time.Second {
		t.Errorf("offlineAfter = %v, spec says 25s", offlineAfter)
	}
	// "Keys expire 60 s after minting if never used."
	if keyTTL != 60*time.Second {
		t.Errorf("keyTTL = %v, spec says 60s", keyTTL)
	}
	if agentRead <= offlineAfter {
		t.Errorf("agentRead %v must outlast offlineAfter %v", agentRead, offlineAfter)
	}
}

// ---- admin auth ----

var apiRoutes = []struct{ method, path, body string }{
	{"POST", "/api/tokens", `{"name":"x"}`},
	{"GET", "/api/machines", ""},
	{"POST", "/api/machines/deadbeef/term", `{"cmd":"htop"}`},
	{"DELETE", "/api/machines/deadbeef", ""},
}

func TestAPIRequiresBearer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bad := map[string]string{
		"no header":       "",
		"empty bearer":    "Bearer ",
		"wrong token":     "Bearer wrong-token",
		"admin as basic":  "Basic " + adminToken,
		"no scheme":       adminToken,
		"lowercase":       "bearer " + adminToken,
		"prefix of admin": "Bearer " + adminToken[:len(adminToken)-1],
		"admin plus":      "Bearer " + adminToken + "x",
	}
	for _, r := range apiRoutes {
		for name, hdr := range bad {
			resp, _ := e.req(r.method, r.path, r.body, hdr)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: got %d, want 401", r.method, r.path, name, resp.StatusCode)
			}
		}
	}
}

func TestAPIAcceptsCorrectBearer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, r := range apiRoutes {
		resp, body := e.admin(r.method, r.path, r.body)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s %s rejected the real admin token: %s", r.method, r.path, body)
		}
	}
	// Unknown machine ids are 404, not 401 or 500.
	for _, r := range apiRoutes[2:] {
		resp, _ := e.admin(r.method, r.path, r.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s on unknown machine: got %d, want 404", r.method, r.path, resp.StatusCode)
		}
	}
}

func TestPanelAndHealthzAreUnauthenticated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, path := range []string{"/healthz", "/", "/term/abc123", "/index.js", "/term.js",
		"/agent.sh", "/agent.py", "/bin/" + selfName} {
		resp, body := e.req("GET", path, "", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s unauthenticated: got %d, want 200", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
	if resp, body := e.req("GET", "/healthz", "", ""); body != "ok" {
		t.Errorf("healthz said %q (%d), want ok", body, resp.StatusCode)
	}
	// The panel must not leak fleet data to an unauthenticated fetch.
	if _, body := e.req("GET", "/", "", ""); strings.Contains(body, adminToken) {
		t.Error("panel HTML contains the admin token")
	}
}

// The panel's enrollment blocks curl both installers off the broker itself, so
// they have to be servable without the admin token the operator has not pasted
// into the target box.
func TestEnrollmentScriptsAreServed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for path, want := range map[string]struct{ ctype, marker string }{
		"/agent.sh": {"text/x-shellscript", "POPFLEET_TOKEN"},
		"/agent.py": {"text/x-python", "POPFLEET_TOKEN"},
	} {
		resp, body := e.req("GET", path, "", "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, want.ctype) {
			t.Errorf("GET %s: content-type %q, want %s", path, ct, want.ctype)
		}
		if !strings.Contains(body, want.marker) {
			t.Errorf("GET %s served something that is not the agent (%d bytes)", path, len(body))
		}
	}
	if _, sh := e.req("GET", "/agent.sh", "", ""); !strings.HasPrefix(sh, "#!") {
		t.Error("/agent.sh has no shebang")
	}
	if _, py := e.req("GET", "/agent.py", "", ""); !strings.Contains(py, "python3") {
		t.Error("/agent.py is not the python agent")
	}
}

// ---- agent handshake ----

func TestAgentMustHelloFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, tok := e.mint("lab1")
	for _, first := range []proto.Msg{
		{T: "hb"},
		{T: "out", Sid: "x", Data: proto.Enc([]byte("hi"))},
		{T: "hello", Token: "not-a-real-token"},
		{T: "hello"},
		{T: "hello", Token: tok[:len(tok)-1]},
	} {
		p, _, err := e.dial("/ws/agent")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		p.send(first)
		p.expectClosed()
	}
}

func TestAgentHelloRegistersMachine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	e.agent(tok, "renamed-by-agent")

	m := e.machine(id)
	if !m.Online {
		t.Error("machine is not online right after hello")
	}
	if m.Name != "renamed-by-agent" {
		t.Errorf("name = %q, want the name the agent sent", m.Name)
	}
	if m.AgentVer != "9.9.9" {
		t.Errorf("agent_ver = %q", m.AgentVer)
	}
	if _, err := time.Parse(time.RFC3339, m.LastSeen); err != nil {
		t.Errorf("last_seen %q is not RFC3339: %v", m.LastSeen, err)
	}
	if m.Sessions != 0 {
		t.Errorf("sessions = %d, want 0", m.Sessions)
	}
}

func TestRevokedTokenCannotReconnect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	e.agent(tok, "lab1")
	if resp, body := e.admin("DELETE", "/api/machines/"+id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, body)
	}
	p, _, err := e.dial("/ws/agent")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	p.send(proto.Msg{T: "hello", Token: tok, Name: "lab1"})
	p.expectClosed()
	if got := e.machines(); len(got) != 0 {
		t.Errorf("revoked machine still listed: %+v", got)
	}
}

// ---- one-time session keys ----

func TestSessionKeyIsOneTime(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	e.agent(tok, "lab1")
	sid, key := e.term(id, "")

	e.browser(sid, key) // first upgrade wins

	_, resp, err := e.dial("/ws/term/" + sid + "?k=" + key)
	if err == nil {
		t.Fatal("second upgrade with the same key was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed key: want 403, got %v (%v)", resp, err)
	}
}

func TestSessionKeyRejections(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	e.agent(tok, "lab1")
	sid, key := e.term(id, "")

	cases := map[string]string{
		"no key":       "/ws/term/" + sid,
		"empty key":    "/ws/term/" + sid + "?k=",
		"wrong key":    "/ws/term/" + sid + "?k=" + strings.Repeat("a", len(key)),
		"other sid":    "/ws/term/deadbeefdeadbeef?k=" + key,
		"key truncate": "/ws/term/" + sid + "?k=" + key[:len(key)-1],
	}
	for name, path := range cases {
		_, resp, err := e.dial(path)
		if err == nil {
			t.Errorf("%s: upgrade accepted", name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: want 403, got %v", name, resp)
		}
	}
	// The real key still works afterwards: failed attempts must not burn it.
	e.browser(sid, key)
}

// TTL is checked without sleeping 60 s by aging the pending key in place; the
// broker exposes no clock seam, so this is a white-box test.
func TestSessionKeyExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	e.agent(tok, "lab1")
	sid, key := e.term(id, "")

	e.b.mu.Lock()
	p, ok := e.b.pend[sid]
	if ok {
		if d := time.Until(p.exp); d < 55*time.Second || d > keyTTL {
			e.t.Errorf("key expiry is %v away, want ~%v", d, keyTTL)
		}
		p.exp = time.Now().Add(-time.Millisecond) // as if 60 s had passed
		e.b.pend[sid] = p
	}
	e.b.mu.Unlock()
	if !ok {
		t.Fatal("minted key is not pending")
	}

	_, resp, err := e.dial("/ws/term/" + sid + "?k=" + key)
	if err == nil {
		t.Fatal("expired key was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired key: want 403, got %v", resp)
	}
}

func TestTermForUnknownMachine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp, _ := e.admin("POST", "/api/machines/nope/term", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

// ---- relay ----

func TestRelayBothDirections(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "htop")
	browser := e.browser(sid, key)

	open := agent.want("open")
	if open.Sid != sid {
		t.Fatalf("open sid = %q, want %q", open.Sid, sid)
	}
	if open.Cmd != "htop" {
		t.Errorf("open cmd = %q, want htop (the --cmd contract)", open.Cmd)
	}

	// agent -> browser, with bytes that are not valid UTF-8.
	payload := make([]byte, 256)
	for i := range payload {
		payload[i] = byte(i)
	}
	agent.send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc(payload)})
	out := browser.want("out")
	if out.Sid != "" {
		t.Errorf("browser frame carries a sid (%q); the browser socket is the session", out.Sid)
	}
	got, err := proto.Dec(out.Data)
	if err != nil {
		t.Fatalf("browser data: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("relay corrupted bytes:\n got %x\nwant %x", got, payload)
	}

	// browser -> agent
	browser.send(proto.Msg{T: "in", Data: proto.Enc([]byte("ls -la\n"))})
	in := agent.want("in")
	if in.Sid != sid {
		t.Errorf("in sid = %q, want %q", in.Sid, sid)
	}
	if b, _ := proto.Dec(in.Data); string(b) != "ls -la\n" {
		t.Errorf("keystrokes mangled: %q", b)
	}

	browser.send(proto.Msg{T: "resize", C: 120, R: 40})
	rs := agent.want("resize")
	if rs.Sid != sid || rs.C != 120 || rs.R != 40 {
		t.Errorf("resize relayed as %+v", rs)
	}

	if m := e.machine(id); m.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", m.Sessions)
	}

	// exit 0 must reach the browser as an explicit "code":0.
	agent.send(proto.Msg{T: "exit", Sid: sid, Code: proto.Int(0)})
	raw, err := browser.readRaw()
	if err != nil {
		t.Fatalf("browser never saw exit: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"code":0`)) {
		t.Fatalf("exit frame lost the zero code: %s", raw)
	}
	browser.expectClosed()
	if m := e.machine(id); m.Sessions != 0 {
		t.Errorf("sessions = %d after exit, want 0", m.Sessions)
	}
}

func TestExitCodePropagates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	for _, code := range []int{0, 3, 7, 127} {
		sid, key := e.term(id, fmt.Sprintf("exit %d", code))
		browser := e.browser(sid, key)
		agent.want("open")
		agent.send(proto.Msg{T: "exit", Sid: sid, Code: proto.Int(code)})
		m := browser.want("exit")
		if m.Code == nil {
			t.Fatalf("exit %d arrived without a code", code)
		}
		if *m.Code != code {
			t.Errorf("browser saw exit %d, want %d", *m.Code, code)
		}
	}
}

func TestFramesForUnknownSidAreIgnored(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	agent.want("open")

	// Output and exits for sids the broker never minted must be dropped, not
	// treated as an error that takes the agent socket down with it.
	agent.send(proto.Msg{T: "out", Sid: "ffffffffffffffff", Data: proto.Enc([]byte("ghost"))})
	agent.send(proto.Msg{T: "exit", Sid: "ffffffffffffffff", Code: proto.Int(1)})
	agent.send(proto.Msg{T: "wat", Sid: sid})
	agent.send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc([]byte("real"))})

	m := browser.want("out")
	if b, _ := proto.Dec(m.Data); string(b) != "real" {
		t.Fatalf("browser got %q, want the real session's output", b)
	}
	// The live session survived the noise.
	browser.send(proto.Msg{T: "in", Data: proto.Enc([]byte("x"))})
	if in := agent.want("in"); in.Sid != sid {
		t.Fatalf("agent socket broken after unknown-sid frames: %+v", in)
	}
}

func TestBrowserCloseKillsPTY(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	agent.want("open")

	browser.ws.Close() // operator closed the tab

	m := agent.want("close")
	if m.Sid != sid {
		t.Fatalf("close sid = %q, want %q", m.Sid, sid)
	}
	waitFor(t, "session count to drop", func() bool { return e.machine(id).Sessions == 0 })
}

func TestAgentDeathBannersBrowserAndGoesOffline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	agent.want("open")

	agent.ws.Close() // the agent's socket dies with a session attached

	m := browser.want("err")
	if m.Msg == "" {
		t.Error("err frame has no message; the tab would show a dead black box")
	}
	if !strings.Contains(m.Msg, "offline") {
		t.Errorf("err msg = %q, want the offline banner", m.Msg)
	}
	browser.expectClosed()
	waitFor(t, "machine to go offline", func() bool { return !e.machine(id).Online })
	if m := e.machine(id); m.Sessions != 0 {
		t.Errorf("sessions = %d after agent death, want 0", m.Sessions)
	}
}

func TestSessionToOfflineAgentFailsCleanly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, _ := e.mint("lab1") // enrolled but never connected
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	m := browser.want("err")
	if !strings.Contains(m.Msg, "offline") {
		t.Errorf("err msg = %q, want an offline banner", m.Msg)
	}
	browser.expectClosed()
}

func TestRevokeDropsAgentAndSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	agent.want("open")

	resp, _ := e.admin("DELETE", "/api/machines/"+id, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	if m := browser.want("err"); m.Msg == "" {
		t.Error("revoked session closed without a banner")
	}
	browser.expectClosed()
	agent.expectClosed()
}

// Gate 3: a NAT-zombied socket must not lock the machine out. The reconnect
// keeps the same machine id and the stale session dies with a banner.
func TestAgentReconnectReplacesZombie(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	zombie := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	zombie.want("open")

	fresh := e.agent(tok, "lab1") // same token, new socket

	if m := browser.want("err"); m.Msg == "" {
		t.Error("stale session died without a banner")
	}
	zombie.expectClosed()

	// The new socket owns the machine, under the same id, and works.
	sid2, key2 := e.term(id, "")
	browser2 := e.browser(sid2, key2)
	if m := fresh.want("open"); m.Sid != sid2 {
		t.Fatalf("new session went to the wrong sid: %+v", m)
	}
	fresh.send(proto.Msg{T: "out", Sid: sid2, Data: proto.Enc([]byte("alive"))})
	if m := browser2.want("out"); m.Data != proto.Enc([]byte("alive")) {
		t.Fatalf("relay broken after reconnect: %+v", m)
	}
	if got := e.machine(id); !got.Online {
		t.Error("machine offline after reconnect")
	}
	if n := len(e.machines()); n != 1 {
		t.Errorf("reconnect created %d machines, want 1 (Gate 3: no re-enroll)", n)
	}
}

// Gate 0/4 scaled down: two concurrent sessions to one agent, one of them
// firehosed, and both stay live and independent.
func TestTwoConcurrentSessionsSurviveAFirehose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")

	sidA, keyA := e.term(id, "yes")
	browserA := e.browser(sidA, keyA)
	sidB, keyB := e.term(id, "")
	browserB := e.browser(sidB, keyB)
	for i := 0; i < 2; i++ {
		agent.want("open")
	}
	if m := e.machine(id); m.Sessions != 2 {
		t.Fatalf("sessions = %d, want 2", m.Sessions)
	}

	// One agent goroutine echoes browser B's keystrokes while another floods A.
	echoed := make(chan error, 1)
	go func() {
		for {
			m, err := agent.tryRecv()
			if err != nil {
				echoed <- err
				return
			}
			if m.T == "in" && m.Sid == sidB {
				echoed <- agent.trySend(proto.Msg{T: "out", Sid: sidB, Data: m.Data})
				return
			}
		}
	}()

	const frames, size = 400, 4096
	chunk := bytes.Repeat([]byte("y\n"), size/2)
	drained := make(chan int, 1)
	go func() { // browser A keeps up, as a real xterm.js does
		n := 0
		for n < frames {
			m, err := browserA.tryRecv()
			if err != nil || m.T != "out" {
				break
			}
			n++
		}
		drained <- n
	}()
	go func() {
		for i := 0; i < frames; i++ {
			if agent.trySend(proto.Msg{T: "out", Sid: sidA, Data: proto.Enc(chunk)}) != nil {
				return
			}
		}
	}()

	// Session B must stay usable while A is firehosed.
	browserB.send(proto.Msg{T: "in", Data: proto.Enc([]byte("echo hi\n"))})
	if m := browserB.want("out"); m.T != "out" {
		t.Fatalf("session B starved during the firehose: %+v", m)
	}
	if err := <-echoed; err != nil {
		t.Fatalf("agent socket broke under the firehose: %v", err)
	}
	if n := <-drained; n != frames {
		t.Fatalf("session A got %d/%d frames", n, frames)
	}
	if m := e.machine(id); m.Sessions != 2 {
		t.Errorf("sessions = %d after the firehose, want 2 (neither died)", m.Sessions)
	}
}

// selfName is the only binary name this broker will ever serve.
var selfName = "popfleet-" + runtime.GOOS + "-" + runtime.GOARCH

// agent.sh installs from the broker itself because no GitHub release exists.
// os.Executable() under `go test` is the test binary, which is exactly the
// point: whatever binary is running must be what /bin/<self> hands out.
func TestBinServesThisBrokersOwnBinary(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	resp, err := e.srv.Client().Get(e.srv.URL + "/bin/" + selfName)
	if err != nil {
		t.Fatalf("GET /bin/%s: %v", selfName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /bin/%s: %d, want 200", selfName, resp.StatusCode)
	}
	served := sha256.New()
	n, err := io.Copy(served, resp.Body)
	if err != nil {
		t.Fatalf("reading the served binary: %v", err)
	}
	if n == 0 {
		t.Fatal("served an empty binary")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	f, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	onDisk := sha256.New()
	size, err := io.Copy(onDisk, f)
	if err != nil {
		t.Fatal(err)
	}
	if n != size || !bytes.Equal(served.Sum(nil), onDisk.Sum(nil)) {
		t.Errorf("served %d bytes (%x), want the running binary's %d (%x)",
			n, served.Sum(nil), size, onDisk.Sum(nil))
	}
}

// The broker can only copy itself, so every other name is a 404 — including
// the ones that would be a directory traversal if the check were anything
// looser than string equality. %2F keeps the traversal inside one path
// segment, so it reaches the handler instead of being routed away.
func TestBinRefusesAnythingButItself(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret := "root:x:0:0:root:/root:/bin/bash"
	for _, name := range []string{
		"popfleet-plan9-vax",
		"popfleet",
		selfName + "x",
		strings.ToUpper(selfName),
		"..%2F..%2F..%2Fetc%2Fpasswd",
		"popfleet-..%2F..%2Fetc%2Fpasswd",
		"popfleet-" + runtime.GOOS + "-" + runtime.GOARCH + "%2F..%2F..%2Fetc%2Fpasswd",
		".%2E%2F.%2E%2Fetc%2Fpasswd",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
	} {
		resp, body := e.req("GET", "/bin/"+name, "", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET /bin/%s: %d, want 404", name, resp.StatusCode)
		}
		if strings.Contains(body, secret) || strings.Contains(body, "\x7fELF") {
			t.Errorf("GET /bin/%s served real file contents", name)
		}
		if !strings.Contains(body, selfName) {
			t.Errorf("GET /bin/%s: unhelpful 404 body %q", name, body)
		}
	}
	// Unescaped traversal is cleaned away by the router before it ever gets a
	// chance; assert it too, since that is what an attacker would try first.
	resp, body := e.req("GET", "/bin/../../etc/passwd", "", "")
	if resp.StatusCode == http.StatusOK && strings.Contains(body, secret) {
		t.Fatal("path traversal escaped the /bin route")
	}
}

// An enrolled agent must not be able to touch a session that belongs to a
// different machine. Sids are 64 random bits and only ever sent to the owning
// agent, so this is defence in depth — but a compromised box in the fleet is
// exactly when it has to hold.
func TestAgentCannotTouchAnotherMachinesSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	idA, tokA := e.mint("A")
	idB, tokB := e.mint("B")
	agentA := e.agent(tokA, "A")
	rogue := e.agent(tokB, "B")

	sid, key := e.term(idA, "")
	browser := e.browser(sid, key)
	agentA.want("open")

	rogue.send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc([]byte("INJECTED"))})
	rogue.send(proto.Msg{T: "exit", Sid: sid, Code: proto.Int(0)})

	// Whatever the browser sees next must have come from machine A. The rogue
	// frames were sent first, so anything relayed would arrive ahead of it.
	agentA.send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc([]byte("from A"))})
	m := browser.recv()
	if m.T != "out" {
		t.Fatalf("rogue agent ended another machine's session: got %+v", m)
	}
	if b, _ := proto.Dec(m.Data); string(b) != "from A" {
		t.Fatalf("rogue agent wrote %q into another machine's session", b)
	}

	// The session is still registered, still owned by A, and still two-way.
	if n := e.machine(idA).Sessions; n != 1 {
		t.Fatalf("machine A has %d sessions after the rogue exit, want 1", n)
	}
	browser.send(proto.Msg{T: "in", Data: proto.Enc([]byte("still typing"))})
	if in := agentA.want("in"); in.Sid != sid {
		t.Fatalf("keystrokes stopped reaching agent A: %+v", in)
	}
	// And the rogue's own socket is untouched: bad sids are ignored, not fatal.
	rogue.send(proto.Msg{T: "hb"})
	if !e.machine(idB).Online {
		t.Error("the rejected frames took the sending agent's socket down with them")
	}
}

// A browser whose outbound queue is full must lose its own session and nothing
// else: the agent socket is shared, so it may never stall. Simulated with an
// unbuffered, unread queue so the drop is deterministic instead of depending on
// how much the kernel happens to buffer.
func TestWedgedBrowserLosesOnlyItsOwnSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, tok := e.mint("lab1")
	agent := e.agent(tok, "lab1")
	sid, key := e.term(id, "")
	browser := e.browser(sid, key)
	agent.want("open")

	const wedged = "wedgedwedged1234"
	e.b.mu.Lock()
	e.b.sess[wedged] = &session{conn: &conn{out: make(chan proto.Msg)}, sid: wedged, mid: id}
	e.b.mu.Unlock()

	agent.send(proto.Msg{T: "out", Sid: wedged, Data: proto.Enc([]byte("firehose"))})

	if m := agent.want("close"); m.Sid != wedged {
		t.Fatalf("agent asked to close %q, want the wedged session %q", m.Sid, wedged)
	}
	e.b.mu.Lock()
	_, still := e.b.sess[wedged]
	e.b.mu.Unlock()
	if still {
		t.Error("wedged session was not detached")
	}

	// The healthy session and the shared agent socket are untouched.
	agent.send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc([]byte("still here"))})
	if b, _ := proto.Dec(browser.want("out").Data); string(b) != "still here" {
		t.Fatalf("healthy session lost output: %q", b)
	}
}

// ---- offline threshold ----

// online must flip at 25 s of silence. No clock seam exists, so the machine's
// last_seen is planted through the state file and the agent socket is
// registered directly rather than via hello (which would Touch it).
func TestOnlineFlipsAtOfflineAfter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		age       time.Duration
		connected bool
		want      bool
	}{
		{"fresh heartbeat, socket up", time.Second, true, true},
		{"just inside the window", offlineAfter - time.Second, true, true},
		{"two heartbeats missed", offlineAfter + time.Second, true, false},
		{"socket gone, fresh heartbeat", time.Second, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "popfleet.json")
			seen := time.Now().UTC().Add(-c.age).Format(time.RFC3339Nano)
			state := fmt.Sprintf(`[{"id":"abc12345","name":"lab1","token":"tok","last_seen":%q,"agent_ver":"1.0.0"}]`, seen)
			if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			e := envWithStore(t, st)
			if c.connected {
				// A registered socket with no reader/writer goroutines: only
				// its presence in the map matters here.
				e.b.mu.Lock()
				e.b.agents["abc12345"] = &agentConn{conn: &conn{out: make(chan proto.Msg, 1)}, mid: "abc12345"}
				e.b.mu.Unlock()
			}
			if got := e.machine("abc12345").Online; got != c.want {
				t.Errorf("online = %v after %v of silence (socket up: %v), want %v",
					got, c.age, c.connected, c.want)
			}
		})
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
