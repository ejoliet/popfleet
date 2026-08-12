package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "popfleet.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// Gate 3: the enrollment token IS the durable machine identity. Restarting the
// broker must resolve the same token to the same id, with no re-enrollment.
func TestTokenIsDurableIdentityAcrossRestart(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	a, err := s.Mint("lab1")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	b, err := s.Mint("lab2")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if a.ID == b.ID || a.Token == b.Token {
		t.Fatalf("Mint reused identity: %+v %+v", a, b)
	}
	s.Touch(a.ID, "lab1-renamed", "1.2.3")

	reopened, err := Open(path) // broker restart
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.ByToken(a.Token)
	if !ok {
		t.Fatal("token unknown after restart: agent would have to re-enroll (Gate 3 fail)")
	}
	if got.ID != a.ID {
		t.Errorf("machine id churned across restart: got %q want %q", got.ID, a.ID)
	}
	if got.Name != "lab1-renamed" || got.AgentVer != "1.2.3" {
		t.Errorf("name/ver lost across restart: %+v", got)
	}
	if got.LastSeen.IsZero() {
		t.Error("last_seen lost across restart")
	}
	if _, ok := reopened.ByToken(b.Token); !ok {
		t.Error("second machine lost across restart")
	}
}

func TestByTokenRejectsWrongAndEmpty(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	m, _ := s.Mint("lab1")
	for _, tok := range []string{"", "nope", m.Token + "x", m.Token[:len(m.Token)-1]} {
		if _, ok := s.ByToken(tok); ok {
			t.Errorf("ByToken(%q) accepted a token that is not a machine's", tok)
		}
	}
	if _, ok := s.ByToken(m.Token); !ok {
		t.Error("ByToken rejected the real token")
	}
}

func TestTouchUnknownIDIsNoop(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	s.Touch("nosuchid", "x", "y") // must not panic or create a machine
	if len(s.List()) != 0 {
		t.Fatalf("Touch invented a machine: %+v", s.List())
	}
}

func TestTouchKeepsNameWhenBlank(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	m, _ := s.Mint("lab1")
	s.Touch(m.ID, "", "1.0.0")
	got, _ := s.ByID(m.ID)
	if got.Name != "lab1" {
		t.Errorf("heartbeat clobbered the name: %q", got.Name)
	}
	if got.AgentVer != "1.0.0" {
		t.Errorf("agent_ver not recorded: %q", got.AgentVer)
	}
	before := got.LastSeen
	time.Sleep(2 * time.Millisecond)
	s.Touch(m.ID, "", "")
	got, _ = s.ByID(m.ID)
	if !got.LastSeen.After(before) {
		t.Errorf("last_seen did not advance: %v -> %v", before, got.LastSeen)
	}
	if got.AgentVer != "1.0.0" {
		t.Errorf("blank ver clobbered agent_ver: %q", got.AgentVer)
	}
}

// DELETE /api/machines/{id} is the kill switch: token gone, machine gone, and
// gone after a restart too.
func TestDeleteRevokesTokenAndMachine(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	m, _ := s.Mint("lab1")
	keep, _ := s.Mint("lab2")

	if !s.Delete(m.ID) {
		t.Fatal("Delete reported no such machine")
	}
	if _, ok := s.ByToken(m.Token); ok {
		t.Error("revoked token still authenticates")
	}
	if _, ok := s.ByID(m.ID); ok {
		t.Error("revoked machine still resolvable by id")
	}
	if s.Delete(m.ID) {
		t.Error("second Delete reported success")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.ByToken(m.Token); ok {
		t.Error("revocation did not survive restart")
	}
	if _, ok := reopened.ByToken(keep.Token); !ok {
		t.Error("revocation took an unrelated machine with it")
	}
}

func TestListIsSortedCopy(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	for i := 0; i < 5; i++ {
		if _, err := s.Mint("m"); err != nil {
			t.Fatal(err)
		}
	}
	list := s.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("List not sorted by id: %v", list)
		}
	}
	list[0].Name = "mutated"
	if got, _ := s.ByID(list[0].ID); got.Name == "mutated" {
		t.Error("List handed out a pointer into store state")
	}
}

func TestOpenMissingFileIsEmptyStore(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Open on missing file: %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("fresh store is not empty")
	}
}

func TestOpenRejectsCorruptFileInsteadOfLosingIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "popfleet.json")
	// A half-written file must be reported, never silently treated as "no
	// machines" (that would revoke the whole fleet on the next write).
	if err := os.WriteFile(path, []byte(`[{"id":"abc","token":"tok`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted a truncated state file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Open destroyed the damaged file: %v", err)
	}
}

// tmp+rename: a crash mid-write leaves a stray .tmp and an intact old file, and
// the next write must recover without operator help.
func TestAtomicWriteSurvivesStrayTmpFile(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	first, _ := s.Mint("lab1")

	if err := os.WriteFile(path+".tmp", []byte("half-written gar"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The old file is untouched by the failed write.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("stray tmp made the store unreadable: %v", err)
	}
	if _, ok := reopened.ByToken(first.Token); !ok {
		t.Fatal("a partially written tmp file cost us the committed state")
	}
	// And the next mutation overwrites the stray tmp and commits cleanly.
	second, err := s.Mint("lab2")
	if err != nil {
		t.Fatalf("Mint after stray tmp: %v", err)
	}
	final, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after recovery: %v", err)
	}
	for _, m := range []Machine{first, second} {
		if _, ok := final.ByToken(m.Token); !ok {
			t.Errorf("machine %s lost after tmp recovery", m.ID)
		}
	}
	// The committed file is always complete JSON, never a prefix.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list []Machine
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("committed state file is not valid JSON: %v\n%s", err, b)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 machines on disk, got %d", len(list))
	}
}

// popfleet.json holds live enrollment tokens in clear (README): 0600, always.
func TestStateFileIsNotWorldReadable(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	if _, err := s.Mint("lab1"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state file holding enrollment tokens is mode %04o, want 0600", perm)
	}
}

// A pre-created popfleet.json.tmp must never decide the mode of the file that
// holds every enrollment token: the save has to replace it, not write through
// it. (os.WriteFile leaves an existing file's mode alone, which is why the
// save opens with O_EXCL.)
func TestHostileTmpFileCannotWidenStatePerms(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	if err := os.WriteFile(path+".tmp", nil, 0o666); err != nil {
		t.Fatal(err)
	}
	m, err := s.Mint("lab1")
	if err != nil {
		t.Fatalf("Mint over a hostile tmp: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state file is mode %04o after a 0666 tmp was planted, want 0600", perm)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.ByToken(m.Token); !ok {
		t.Error("the save did not actually commit")
	}
}

// A tmp symlink pointing outside the state dir must not be followed: that
// would write the fleet's tokens wherever the attacker chose.
func TestTmpSymlinkIsNotFollowed(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	outside := filepath.Join(t.TempDir(), "victim.txt")
	const untouched = "not the fleet's tokens\n"
	if err := os.WriteFile(outside, []byte(untouched), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path+".tmp"); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	m, err := s.Mint("lab1")
	if err != nil {
		t.Fatalf("Mint with a tmp symlink planted: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != untouched {
		t.Errorf("the save followed the symlink and wrote to %s:\n%s", outside, got)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("the state file itself is now a symlink")
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.ByToken(m.Token); !ok {
		t.Error("state did not land in the real state file")
	}
}

// O_EXCL would fail forever on a tmp left behind by a crashed save, so every
// save has to clear it first. Two saves in a row after planting one proves it.
func TestLeftoverTmpDoesNotWedgeSaves(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	if err := os.WriteFile(path+".tmp", []byte("crashed mid-write"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Mint("lab"); err != nil {
			t.Fatalf("Mint %d after a crashed save: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file survived the save: %v", err)
	}
	final, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(final.List()); n != 3 {
		t.Errorf("disk has %d machines, want 3", n)
	}
}

func TestConcurrentReadersAndWriters(t *testing.T) {
	t.Parallel()
	s, path := newStore(t)
	seed, _ := s.Mint("seed")

	const workers = 8
	var wg sync.WaitGroup
	tokens := make(chan string, workers*4)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				m, err := s.Mint("worker")
				if err != nil {
					t.Errorf("Mint: %v", err)
					return
				}
				tokens <- m.Token
				s.Touch(m.ID, "worker", "1.0.0")
			}
		}(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 16; j++ {
				s.List()
				s.ByToken(seed.Token)
				s.ByID(seed.ID)
				s.Touch(seed.ID, "", "")
			}
		}()
	}
	wg.Wait()
	close(tokens)

	final, err := Open(path)
	if err != nil {
		t.Fatalf("state file unreadable after concurrent writes: %v", err)
	}
	n := 0
	for tok := range tokens {
		if _, ok := final.ByToken(tok); !ok {
			t.Errorf("token minted concurrently is missing from disk")
		}
		n++
	}
	if n != workers*4 {
		t.Fatalf("minted %d tokens, want %d", n, workers*4)
	}
	if len(final.List()) != n+1 {
		t.Fatalf("disk has %d machines, want %d", len(final.List()), n+1)
	}
}
