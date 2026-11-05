// Package store keeps bounded OAuth state and account directories private.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var ErrPrivate = errors.New("private_storage_unavailable")

const maxState = 4 << 20

type Client struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Redirects []string `json:"redirects"`
	Expires   int64    `json:"expires"`
}
type Flow struct {
	Client, Redirect, State, Challenge, Resource, Scope string
	CookieHash, CSRFHash                                string
	Expires                                             int64
}
type Subject struct {
	ID, Provider string
	Created      int64
	RetainUntil  int64
	Revoked      bool
}
type Code struct {
	Client, Redirect, Challenge, Resource, Scope, Subject string
	Expires                                               int64
}
type Token struct {
	Client, Resource, Scope, Subject string
	Expires                          int64
	Used                             bool
}
type State struct {
	Version  int
	Clients  map[string]Client
	Flows    map[string]Flow
	Subjects map[string]Subject
	Codes    map[string]Code
	Access   map[string]Token
	Refresh  map[string]Token
}

func empty() State {
	return State{1, map[string]Client{}, map[string]Flow{}, map[string]Subject{}, map[string]Code{}, map[string]Token{}, map[string]Token{}}
}
func valid(s State) bool {
	return s.Version == 1 && s.Clients != nil && s.Flows != nil && s.Subjects != nil && s.Codes != nil && s.Access != nil && s.Refresh != nil && len(s.Clients) <= 128 && len(s.Flows) <= 128 && len(s.Subjects) <= 256 && len(s.Codes) <= 256 && len(s.Access) <= 1024 && len(s.Refresh) <= 1024
}

type Store struct {
	mu      sync.Mutex
	root    *os.Root
	dir     string
	state   State
	pending map[string]bool
}

func private(info os.FileInfo, directory bool) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid()) && info.IsDir() == directory && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == map[bool]os.FileMode{true: 0700, false: 0600}[directory] && (directory || (info.Mode().IsRegular() && st.Nlink == 1))
}

func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil || dir == "" || filepath.Clean(dir) != dir {
		return nil, ErrPrivate
	}
	// Reject symlinks in every existing ancestor before pinning the private root.
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(abs, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if info, e := os.Lstat(current); e == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, ErrPrivate
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, ErrPrivate
		}
	}
	if os.MkdirAll(abs, 0700) != nil {
		return nil, ErrPrivate
	}
	info, err := os.Lstat(abs)
	if err != nil || !private(info, true) {
		return nil, ErrPrivate
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, ErrPrivate
	}
	s := &Store{root: r, dir: abs, state: empty(), pending: map[string]bool{}}
	failed := true
	defer func() {
		if failed {
			r.Close()
		}
	}()
	if pinned, e := r.Stat("."); e != nil || !os.SameFile(info, pinned) {
		return nil, ErrPrivate
	}
	if err := r.Mkdir("accounts", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrPrivate
	}
	if ai, e := r.Lstat("accounts"); e != nil || !private(ai, true) {
		return nil, ErrPrivate
	}
	if fi, e := r.Lstat("oauth-state.json"); e == nil {
		if !private(fi, false) || fi.Size() > maxState {
			return nil, ErrPrivate
		}
		f, e := r.OpenFile("oauth-state.json", os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if e != nil {
			return nil, ErrPrivate
		}
		defer f.Close()
		if pinned, e := f.Stat(); e != nil || !os.SameFile(fi, pinned) {
			return nil, ErrPrivate
		}
		d := json.NewDecoder(io.LimitReader(f, maxState+1))
		d.DisallowUnknownFields()
		if d.Decode(&s.state) != nil || !valid(s.state) {
			return nil, ErrPrivate
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			return nil, ErrPrivate
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, ErrPrivate
	}
	s.cleanupAccounts()
	failed = false
	return s, nil
}
func (s *Store) Close() error { return s.root.Close() }
func (s *Store) Read(fn func(State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.state)
}
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(s.state)
	if err != nil {
		return ErrPrivate
	}
	var next State
	if json.Unmarshal(data, &next) != nil {
		return ErrPrivate
	}
	if err = fn(&next); err != nil {
		return err
	}
	if !valid(next) {
		return ErrPrivate
	}
	data, err = json.Marshal(next)
	if err != nil || len(data) > maxState {
		return ErrPrivate
	}
	if fi, e := s.root.Lstat("oauth-state.json"); e == nil {
		if !private(fi, false) {
			return ErrPrivate
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return ErrPrivate
	}
	name := ".state-" + Random(16)
	if name == ".state-" {
		return ErrPrivate
	}
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrPrivate
	}
	defer s.root.Remove(name)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || s.root.Rename(name, "oauth-state.json") != nil {
		return ErrPrivate
	}
	parent, e := s.root.Open(".")
	if e != nil {
		return ErrPrivate
	}
	err = parent.Sync()
	parent.Close()
	if err != nil {
		return ErrPrivate
	}
	s.state = next
	s.cleanupAccounts()
	return nil
}

// Only unlinked generated account leaves are removed, through the pinned root.
// Active login attempts are held in pending until linked or explicitly failed.
func (s *Store) cleanupAccounts() {
	if info, e := s.root.Lstat("accounts"); e != nil || !private(info, true) {
		return
	}
	f, e := s.root.Open("accounts")
	if e != nil {
		return
	}
	entries, e := f.ReadDir(-1)
	f.Close()
	if e != nil {
		return
	}
	for _, entry := range entries {
		id := entry.Name()
		if !UUID(id) {
			continue
		}
		if _, linked := s.state.Subjects[id]; linked {
			delete(s.pending, id)
			continue
		}
		if !s.pending[id] {
			_ = s.root.RemoveAll("accounts/" + id)
		}
	}
}
func Random(bytes int) string {
	b := make([]byte, bytes)
	if _, e := rand.Read(b); e != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
func Hash(value string) string { h := sha256.Sum256([]byte(value)); return hex.EncodeToString(h[:]) }
func NewUUID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return ""
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	x := hex.EncodeToString(b)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func UUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return id[14] == '4' && strings.ContainsRune("89ab", rune(id[19]))
}
func (s *Store) NewAccount() (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, e := s.root.Lstat("accounts"); e != nil || !private(info, true) {
		return "", "", ErrPrivate
	}
	id := NewUUID()
	if !UUID(id) {
		return "", "", ErrPrivate
	}
	if s.root.Mkdir("accounts/"+id, 0700) != nil {
		return "", "", ErrPrivate
	}
	s.pending[id] = true
	return id, filepath.Join(s.dir, "accounts", id), nil
}
func (s *Store) AccountDir(id string) (string, error) {
	if !UUID(id) {
		return "", ErrPrivate
	}
	if info, e := s.root.Lstat("accounts"); e != nil || !private(info, true) {
		return "", ErrPrivate
	}
	info, e := s.root.Lstat("accounts/" + id)
	if e != nil || !private(info, true) {
		return "", ErrPrivate
	}
	return filepath.Join(s.dir, "accounts", id), nil
}

// RemoveUnlinkedAccount never accepts an arbitrary path or a linked subject.
func (s *Store) RemoveUnlinkedAccount(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !UUID(id) {
		return ErrPrivate
	}
	if _, ok := s.state.Subjects[id]; ok {
		return ErrPrivate
	}
	delete(s.pending, id)
	return s.root.RemoveAll("accounts/" + id)
}
