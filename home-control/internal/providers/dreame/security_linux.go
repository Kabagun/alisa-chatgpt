//go:build linux

package dreame

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"smart-home-plugin/internal/provider"
)

func trustedFile(path string) bool {
	i, e := os.Lstat(path)
	return e == nil && i.Mode().IsRegular() && i.Mode().Perm()&0022 == 0
}
func executable(path string) bool {
	i, e := os.Lstat(path)
	return e == nil && i.Mode().Perm()&0111 != 0
}
func owned(i os.FileInfo) bool {
	s, ok := i.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid()
}
func privateSession(path string) bool {
	i, e := os.Lstat(path)
	return e == nil && i.Mode().IsRegular() && i.Mode().Perm() == 0600 && owned(i)
}
func accountSession(dir string) (string, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", provider.Err("provider_unavailable")
	}
	i, e := os.Lstat(dir)
	if e != nil || !i.IsDir() || i.Mode().Perm() != 0700 || !owned(i) {
		return "", provider.Err("provider_unavailable")
	}
	resolved, e := filepath.EvalSymlinks(dir)
	if e != nil || resolved != dir {
		return "", provider.Err("provider_unavailable")
	}
	p := filepath.Join(dir, "session.json")
	if _, e = os.Lstat(p); e == nil && !privateSession(p) {
		return "", provider.Err("provider_unavailable")
	} else if e != nil && !os.IsNotExist(e) {
		return "", provider.Err("provider_unavailable")
	}
	return p, nil
}
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		e := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if e == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return e
	}
}
