//go:build linux

package cloud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func secureFile(file *os.File, mode os.FileMode) bool {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&mode == mode
}

// LoadSessionFile requires a current-EUID-owned 0700 directory and 0600 regular
// file. It works for an unprivileged service account and rejects peer ownership.
// Linux O_NOFOLLOW prevents a final-component symlink swap.
func LoadSessionFile(path string, now time.Time) (*Session, error) {
	return loadSessionFile(path, now, false)
}

func LoadSessionForRefresh(path string, now time.Time) (*Session, error) {
	return loadSessionFile(path, now, true)
}

func loadSessionFile(path string, now time.Time, refresh bool) (*Session, error) {
	if !secureDirectory(filepath.Dir(path)) {
		return nil, sessionError("session_permissions")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, sessionError("session_unavailable")
	}
	file := os.NewFile(uintptr(fd), "protected-session")
	defer file.Close()
	if !secureFile(file, 0400) {
		return nil, sessionError("session_permissions")
	}
	return parseSession(file, now, refresh)
}

func secureDirectory(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// LockSession serializes credential import and token rotation between helpers.
func LockSession(path string) (func(), error) {
	if err := CheckSessionDestination(path); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(filepath.Dir(path), "session.lock"), syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, sessionError("session_lock_failed")
	}
	file := os.NewFile(uintptr(fd), "session-lock")
	if !secureFile(file, 0600) {
		file.Close()
		return nil, sessionError("session_permissions")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, sessionError("session_busy")
	}
	return func() { syscall.Flock(fd, syscall.LOCK_UN); file.Close() }, nil
}

func CheckSessionDestination(path string) error {
	if filepath.Base(path) != "session.json" || !secureDirectory(filepath.Dir(path)) {
		return sessionError("session_permissions")
	}
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0600 {
			return sessionError("session_permissions")
		}
	} else if !os.IsNotExist(err) {
		return sessionError("session_unavailable")
	}
	return nil
}

// SaveSessionFile is a local issued-token importer, not cloud authentication.
// The caller must create a current-EUID-owned 0700 directory. It never prints material.
func SaveSessionFile(path string, s Session, now time.Time) error {
	if err := s.Validate(now); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := CheckSessionDestination(path); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".session-*.tmp")
	if err != nil {
		return sessionError("session_write_failed")
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return sessionError("session_write_failed")
	}
	if err := json.NewEncoder(file).Encode(s); err != nil {
		file.Close()
		return sessionError("session_write_failed")
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return sessionError("session_write_failed")
	}
	if err := file.Close(); err != nil {
		return sessionError("session_write_failed")
	}
	if err := os.Rename(tmp, path); err != nil {
		return sessionError("session_write_failed")
	}
	directory, err := os.Open(dir)
	if err != nil {
		return sessionError("session_write_failed")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return sessionError("session_write_failed")
	}
	return nil
}
