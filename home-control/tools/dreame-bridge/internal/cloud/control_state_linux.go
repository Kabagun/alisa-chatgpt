//go:build linux

package cloud

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type fileControlStore struct{ dir string }

func newControlStore(dir string) (controlStore, error) {
	if !secureDirectory(dir) {
		return nil, sessionError("control_state_unavailable")
	}
	return &fileControlStore{dir}, nil
}
func (s *fileControlStore) path(device, suffix string) string {
	sum := sha256.Sum256([]byte(device))
	return filepath.Join(s.dir, "control-"+hex.EncodeToString(sum[:])+suffix)
}
func (s *fileControlStore) lock(device string) (func(), error) {
	if !secureDirectory(s.dir) {
		return nil, sessionError("control_state_unavailable")
	}
	fd, err := syscall.Open(s.path(device, ".lock"), syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, sessionError("control_state_unavailable")
	}
	f := os.NewFile(uintptr(fd), "control-lock")
	if !secureFile(f, 0600) {
		f.Close()
		return nil, sessionError("control_state_unavailable")
	}
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, sessionError("session_busy")
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
}
func (s *fileControlStore) read(device, suffix string, dst any, missingOK bool) error {
	if !secureDirectory(s.dir) {
		return sessionError("control_state_unavailable")
	}
	fd, err := syscall.Open(s.path(device, suffix), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if missingOK && os.IsNotExist(err) {
			return nil
		}
		return sessionError("control_state_unavailable")
	}
	f := os.NewFile(uintptr(fd), "control-state")
	defer f.Close()
	if !secureFile(f, 0400) {
		return sessionError("control_state_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil || len(raw) > 32<<10 {
		return sessionError("control_state_unavailable")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		return sessionError("control_state_unavailable")
	}
	return nil
}
func (s *fileControlStore) write(device, suffix string, v any) error {
	if !secureDirectory(s.dir) {
		return sessionError("control_state_unavailable")
	}
	path := s.path(device, suffix)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return sessionError("control_state_unavailable")
		}
		fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if e != nil {
			return sessionError("control_state_unavailable")
		}
		f := os.NewFile(uintptr(fd), "control-state")
		ok := secureFile(f, 0400)
		f.Close()
		if !ok {
			return sessionError("control_state_unavailable")
		}
	} else if !os.IsNotExist(err) {
		return sessionError("control_state_unavailable")
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 32<<10 {
		return sessionError("control_state_unavailable")
	}
	defer clear(raw)
	f, err := os.CreateTemp(s.dir, ".control-*.tmp")
	if err != nil {
		return sessionError("control_state_unavailable")
	}
	name := f.Name()
	defer os.Remove(name)
	if f.Chmod(0600) != nil {
		f.Close()
		return sessionError("control_state_unavailable")
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return sessionError("control_state_unavailable")
	}
	if f.Sync() != nil {
		f.Close()
		return sessionError("control_state_unavailable")
	}
	if f.Close() != nil || os.Rename(name, path) != nil {
		return sessionError("control_state_unavailable")
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return sessionError("control_state_unavailable")
	}
	defer d.Close()
	if d.Sync() != nil {
		return sessionError("control_state_unavailable")
	}
	return nil
}
func (s *fileControlStore) load(device string) (privatePreview, error) {
	var p privatePreview
	err := s.read(device, ".preview.json", &p, false)
	return p, err
}
func (s *fileControlStore) save(p privatePreview) error { return s.write(p.Device, ".preview.json", p) }
func (s *fileControlStore) lastAttempt(device string) (int64, error) {
	var p struct {
		At int64 `json:"attempted_at_ms"`
	}
	err := s.read(device, ".dispatch.json", &p, true)
	return p.At, err
}
func (s *fileControlStore) markAttempt(device string, at int64) error {
	return s.write(device, ".dispatch.json", struct {
		At int64 `json:"attempted_at_ms"`
	}{at})
}
