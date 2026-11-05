//go:build linux

package cloud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControlPrivateStateAtomicLocksAndIsolation(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	if !secureDirectory(dir) {
		t.Skip("requires exact POSIX permissions")
	}
	x, err := newControlStore(dir)
	if err != nil {
		t.Fatal("private store")
	}
	s := x.(*fileControlStore)
	unlock, err := s.lock("fixture-1")
	if err != nil {
		t.Fatal("control lock")
	}
	if _, err = s.lock("fixture-1"); err == nil {
		t.Fatal("concurrent control accepted")
	}
	unlock()
	p := privatePreview{Schema: 1, Device: "fixture-1", Map: "42", Fingerprint: strings.Repeat("a", 64), CreatedAt: 1700000000000, Drafts: []ReadinessDraft{}}
	if s.save(p) != nil {
		t.Fatal("atomic preview save")
	}
	got, err := s.load("fixture-1")
	if err != nil || got.Fingerprint != p.Fingerprint {
		t.Fatal("protected load")
	}
	if _, err = s.load("foreign"); err == nil {
		t.Fatal("cross-device state")
	}
	if at, err := s.lastAttempt("fixture-1"); err != nil || at != 0 {
		t.Fatal("first dispatch marker")
	}
	if s.markAttempt("fixture-1", 123) != nil {
		t.Fatal("marker save")
	}
	if at, err := s.lastAttempt("fixture-1"); err != nil || at != 123 {
		t.Fatal("marker load")
	}
	for _, suffix := range []string{".preview.json", ".dispatch.json", ".lock"} {
		info, err := os.Lstat(s.path("fixture-1", suffix))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private permissions")
		}
	}
	paths, _ := filepath.Glob(filepath.Join(dir, ".control-*"))
	if len(paths) != 0 {
		t.Fatal("atomic scratch left")
	}
	_ = os.Chmod(s.path("fixture-1", ".preview.json"), 0644)
	if _, err = s.load("fixture-1"); err == nil {
		t.Fatal("loose state accepted")
	}
	if s.save(p) == nil {
		t.Fatal("loose target overwritten")
	}
	_ = os.Remove(s.path("fixture-1", ".preview.json"))
	_ = os.Symlink("unrelated", s.path("fixture-1", ".preview.json"))
	if _, err = s.load("fixture-1"); err == nil {
		t.Fatal("symlink read")
	}
	if s.save(p) == nil {
		t.Fatal("symlink target save")
	}
	_ = os.Remove(s.path("fixture-1", ".lock"))
	_ = os.Symlink("unrelated", s.path("fixture-1", ".lock"))
	if _, err = s.lock("fixture-1"); err == nil {
		t.Fatal("symlink lock")
	}
	_ = os.Remove(s.path("fixture-1", ".preview.json"))
	raw, _ := json.Marshal(map[string]any{"schema": 1, "raw": "MUST_NOT_ESCAPE"})
	_ = os.WriteFile(s.path("fixture-1", ".preview.json"), raw, 0600)
	if _, err = s.load("fixture-1"); err == nil {
		t.Fatal("unknown state schema")
	}
	_ = os.Chmod(dir, 0755)
	if _, err = newControlStore(dir); err == nil {
		t.Fatal("loose account dir")
	}
}
