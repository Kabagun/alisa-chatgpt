package main

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const probeRoot = "/tmp/alisa-kws-probe"

func privateDirectory(path string) bool {
	var st unix.Stat_t
	return unix.Lstat(path, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0700 && st.Uid == uint32(os.Geteuid())
}

func privateFile(path string, size int64) bool {
	var st unix.Stat_t
	return unix.Lstat(path, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777 == 0600 && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1 && st.Size <= size
}

// 0=absent, 1=safe stop requested, -1=unsafe. Never reads file contents.
func stopState(root string) int {
	if !privateDirectory(root) {
		return -1
	}
	path := filepath.Join(root, "stop")
	var st unix.Stat_t
	err := unix.Lstat(path, &st)
	if err == unix.ENOENT {
		return 0
	}
	if err != nil || !privateFile(path, 0) {
		return -1
	}
	return 1
}

func preflight(root string) bool {
	if stopState(root) < 0 {
		return false
	}
	if _, ok := modelFlags(stockModel); !ok {
		return false
	}
	for _, path := range []string{
		"/system/vendor/quasar/libs/libiconv.so", "/system/vendor/quasar/libs/libtflite.so", "/system/vendor/quasar/libs/libyandex_evil.so",
		stockModel + "/acoustic_model.nnet", stockModel + "/words.txt",
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func protectOwnProcess() bool {
	if unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) != nil {
		return false
	}
	f, err := os.OpenFile("/proc/self/oom_score_adj", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	n, err := f.Write([]byte("750\n"))
	closeErr := f.Close()
	return err == nil && closeErr == nil && n == 4
}

// Keep one structured output fd; discard every vendor stdout/stderr write.
func quietNativeOutput() (io.WriteCloser, bool) {
	fd, err := unix.Dup(1)
	if err != nil {
		return nil, false
	}
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "structured-output")
	null, err := unix.Open("/dev/null", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		f.Close()
		return nil, false
	}
	defer unix.Close(null)
	if unix.Dup2(null, 1) != nil || unix.Dup2(null, 2) != nil {
		f.Close()
		return nil, false
	}
	return f, true
}
