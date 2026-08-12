// Package store persists machines (id, name, token, last_seen, agent_ver)
// in a single JSON file guarded by a mutex. Writes are atomic (tmp+rename).
// ponytail: whole-file rewrite per mutation; fine for a personal fleet,
// switch to SQLite if machine count ever makes this visible.
package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"
)

type Machine struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Token    string    `json:"token"`
	LastSeen time.Time `json:"last_seen"`
	AgentVer string    `json:"agent_ver"`
}

type Store struct {
	mu   sync.Mutex
	path string
	m    map[string]*Machine // by id
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, m: map[string]*Machine{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Machine
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	for _, m := range list {
		s.m[m.ID] = m
	}
	return s, nil
}

// save writes the file atomically. Caller must hold s.mu.
func (s *Store) save() error {
	list := make([]*Machine, 0, len(s.m))
	for _, m := range s.m {
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	// O_EXCL, not os.WriteFile: this file holds every enrollment token, and
	// WriteFile follows symlinks and leaves an existing file's mode alone, so a
	// pre-created 0666 popfleet.json.tmp would publish the whole fleet's tokens.
	// Remove first so a crashed save doesn't wedge us out of O_EXCL forever.
	tmp := s.path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(b)
}

// Mint creates a machine record; the enrollment token IS the durable identity.
func (s *Store) Mint(name string) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := &Machine{ID: randHex(8), Name: name, Token: randHex(24)}
	s.m[m.ID] = m
	if err := s.save(); err != nil {
		delete(s.m, m.ID)
		return Machine{}, err
	}
	return *m, nil
}

// ByToken finds the machine for an enrollment token (constant-time compare).
func (s *Store) ByToken(tok string) (Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.m {
		if subtle.ConstantTimeCompare([]byte(m.Token), []byte(tok)) == 1 {
			return *m, true
		}
	}
	return Machine{}, false
}

func (s *Store) ByID(id string) (Machine, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok {
		return Machine{}, false
	}
	return *m, true
}

// Touch updates last_seen and, when non-empty, name and agent_ver.
func (s *Store) Touch(id, name, ver string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.m[id]
	if !ok {
		return
	}
	m.LastSeen = time.Now().UTC()
	if name != "" {
		m.Name = name
	}
	if ver != "" {
		m.AgentVer = ver
	}
	_ = s.save() // ponytail: last_seen loss on a failed save is harmless
}

// Delete revokes the token and forgets the machine.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return false
	}
	delete(s.m, id)
	_ = s.save()
	return true
}

func (s *Store) List() []Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Machine, 0, len(s.m))
	for _, m := range s.m {
		list = append(list, *m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}
