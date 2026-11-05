package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateStateAndAccountIsolation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.Update(func(st *State) error { st.Clients["synthetic"] = Client{ID: "synthetic"}; return nil }); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{dir, filepath.Join(dir, "accounts"), filepath.Join(dir, "oauth-state.json")} {
		i, e := os.Lstat(path)
		want := os.FileMode(0700)
		if strings.HasSuffix(path, ".json") {
			want = 0600
		}
		if e != nil || i.Mode().Perm() != want {
			t.Fatal("storage permissions incorrect")
		}
	}
	a, ad, e := s.NewAccount()
	if e != nil {
		t.Fatal(e)
	}
	b, bd, e := s.NewAccount()
	if e != nil || a == b || ad == bd {
		t.Fatal("account directories collided")
	}
	if _, e = s.AccountDir("../escape"); e == nil {
		t.Fatal("caller path accepted")
	}
	if e = s.Update(func(st *State) error { st.Subjects[a] = Subject{ID: a, Provider: "fake"}; return nil }); e != nil {
		t.Fatal(e)
	}
	if s.RemoveUnlinkedAccount(a) == nil {
		t.Fatal("linked account removed")
	}
	if s.RemoveUnlinkedAccount(b) != nil {
		t.Fatal("failed attempt cleanup failed")
	}
	if _, e = os.Lstat(bd); !os.IsNotExist(e) {
		t.Fatal("failed attempt remained")
	}
}
func TestUnsafeStateFailsClosed(t *testing.T) {
	for _, kind := range []string{"mode", "symlink", "state-mode", "state-symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			if os.Mkdir(dir, 0700) != nil {
				t.Fatal("fixture")
			}
			switch kind {
			case "mode":
				_ = os.Chmod(dir, 0755)
			case "symlink":
				_ = os.Remove(dir)
				_ = os.Symlink(t.TempDir(), dir)
			case "state-mode":
				_ = os.WriteFile(filepath.Join(dir, "oauth-state.json"), []byte("{}"), 0644)
			case "state-symlink":
				_ = os.Symlink("absent", filepath.Join(dir, "oauth-state.json"))
			}
			if s, e := Open(dir); e == nil {
				s.Close()
				t.Fatal("unsafe storage accepted")
			}
		})
	}
}
func TestAtomicUpdateFailureDoesNotMutateState(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "private"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Update(func(st *State) error { st.Clients["discard"] = Client{}; return ErrPrivate }) == nil {
		t.Fatal("failed update accepted")
	}
	_ = s.Read(func(st State) error {
		if len(st.Clients) != 0 {
			t.Fatal("failed update mutated state")
		}
		return nil
	})
}
