// Package session persists session metadata. Each session has its own
// directory, so concurrent spawns never contend on a shared state file.
//
//	sessions/<id>/session.json   metadata
//	sessions/<id>/...            backend-private state
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Session is one agent's view of a repository.
type Session struct {
	ID        string    `json:"id"`
	Repo      string    `json:"repo"`
	Base      string    `json:"base"`    // commit sha the view started from
	Branch    string    `json:"branch"`  // branch created inside the view
	Backend   string    `json:"backend"` // backend name
	ViewPath  string    `json:"view_path"`
	CreatedAt time.Time `json:"created_at"`
}

// Store reads and writes sessions under a tingly-fuse home.
type Store struct{ home string }

func NewStore(home string) *Store { return &Store{home: home} }

func (s *Store) root() string { return filepath.Join(s.home, "sessions") }

// Dir is the session's private directory.
func (s *Store) Dir(id string) string { return filepath.Join(s.root(), id) }

// NewID returns a short random id.
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Reserve atomically claims id by creating its directory.
func (s *Store) Reserve(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if err := os.MkdirAll(s.root(), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(s.Dir(id), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("session %q already exists", id)
		}
		return err
	}
	return nil
}

// Save writes the session's metadata, replacing it atomically.
func (s *Store) Save(sess *Session) error {
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir(sess.ID), "session.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get loads a session.
func (s *Store) Get(id string) (*Session, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	data, err := os.ReadFile(filepath.Join(s.Dir(id), "session.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no session %q", id)
	} else if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("read session %q: %w", id, err)
	}
	return &sess, nil
}

// List returns all sessions, oldest first. Directories without metadata
// (a spawn in progress or one that failed) are skipped.
func (s *Store) List() ([]*Session, error) {
	entries, err := os.ReadDir(s.root())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sess, err := s.Get(e.Name())
		if err != nil {
			continue
		}
		out = append(out, sess)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Delete removes the session's directory and everything in it.
func (s *Store) Delete(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	return os.RemoveAll(s.Dir(id))
}
