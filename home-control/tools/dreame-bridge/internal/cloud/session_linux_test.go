//go:build linux

package cloud

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func privateTestDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Skip("filesystem does not implement Linux mode bits")
	}
	return dir
}

func TestProtectedSessionFileRoundTripAndRejections(t *testing.T) {
	dir := privateTestDirectory(t)
	t.Logf("session checks running as effective UID %d", os.Geteuid())
	path := filepath.Join(dir, "session.json")
	unlock, lockErr := LockSession(path)
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	if release, err := LockSession(path); err == nil {
		release()
		t.Fatal("concurrent token update accepted")
	}
	unlock()
	unlock, lockErr = LockSession(path)
	if lockErr != nil {
		t.Fatal("session lock was not released")
	}
	unlock()
	s := syntheticSession()
	s.RefreshToken = "synthetic-refresh"
	if err := SaveSessionFile(path, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("session is not 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		t.Fatal("session owner is not the process effective UID")
	}
	loaded, err := LoadSessionFile(path, time.Now())
	if err != nil || loaded.AccessToken != s.AccessToken {
		t.Fatal("secure round trip failed")
	}
	for _, mode := range []os.FileMode{0400, 0700, 0640, 0644, 0666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSessionFile(path, time.Now()); err == nil {
			t.Fatal("non-0600 session accepted")
		}
		if err := SaveSessionFile(path, s, time.Now()); err == nil {
			t.Fatal("unsafe destination replaced")
		}
	}
	os.Chmod(path, 0600)
	for _, mode := range []os.FileMode{0500, 0710, 0755, 0777} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSessionFile(path, time.Now()); err == nil {
			t.Fatal("non-0700 directory accepted")
		}
	}
	os.Chmod(dir, 0700)
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 65534, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSessionFile(path, time.Now()); err == nil {
			t.Fatal("peer-owned session accepted")
		}
		if err := CheckSessionDestination(path); err == nil {
			t.Fatal("peer-owned destination accepted")
		}
		if err := os.Chown(path, 0, -1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(dir, 65534, -1); err != nil {
			t.Fatal(err)
		}
		if secureDirectory(dir) {
			t.Fatal("peer-owned directory accepted")
		}
		if err := os.Chown(dir, 0, -1); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionFile(link, time.Now()); err == nil {
		t.Fatal("symlink session accepted")
	}
	if err := SaveSessionFile(filepath.Join(dir, "other.json"), s, time.Now()); err == nil {
		t.Fatal("arbitrary destination name accepted")
	}
	s.ExpiresAt = "2000-01-01T00:00:00Z"
	// Write an expired synthetic session to exercise the refresh-only loader.
	raw := `{"schema":1,"server_region":"eu","expires_at":"2000-01-01T00:00:00Z","access_token":"synthetic-access","refresh_token":"synthetic-refresh","tenant_id":"synthetic-tenant","profile":{"upstream_commit":"` + UpstreamCommit + `","signing_key":"synthetic-key-16!","basic_authorization":"c3ludGhldGlj","user_agent":"Synthetic/Test","dreame_meta":"cv=a_synthetic"}}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionFile(path, time.Now()); err == nil {
		t.Fatal("reader accepted expired session")
	}
	if _, err := LoadSessionForRefresh(path, time.Now()); err != nil {
		t.Fatal("refresh could not load expired token")
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".session-") {
			t.Fatal("secret temporary file retained")
		}
	}
}

func TestSessionAtomicReadDuringReplacement(t *testing.T) {
	dir := privateTestDirectory(t)
	path := filepath.Join(dir, "session.json")
	s := syntheticSession()
	if err := SaveSessionFile(path, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	failures := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := LoadSessionFile(path, time.Now()); err != nil {
				select {
				case failures <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 40; i++ {
		s.AccessToken = map[bool]string{true: "synthetic-access-A", false: "synthetic-access-B"}[i%2 == 0]
		if err := SaveSessionFile(path, s, time.Now()); err != nil {
			close(stop)
			workers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	workers.Wait()
	select {
	case <-failures:
		t.Fatal("reader observed an invalid partial replacement")
	default:
	}
}
