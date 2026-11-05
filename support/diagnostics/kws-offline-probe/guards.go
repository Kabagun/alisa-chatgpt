package main

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const probeRoot = "/tmp/alisa-kws-offline-probe"
const fixtureName = "fixture.wav"
const maximumFixtureBytes = 2 * 1024 * 1024

func privateDirectory(path string) bool {
	var st syscall.Stat_t
	return syscall.Lstat(path, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR && st.Mode&07777 == 0700 && st.Uid == uint32(os.Geteuid())
}

func privateFile(path string, limit int64) bool {
	var st syscall.Stat_t
	return syscall.Lstat(path, &st) == nil && privateRegular(&st, limit)
}

func privateRegular(st *syscall.Stat_t, limit int64) bool {
	return st.Mode&syscall.S_IFMT == syscall.S_IFREG && st.Mode&07777 == 0600 && st.Uid == uint32(os.Geteuid()) && st.Nlink == 1 && st.Size >= 0 && st.Size <= limit
}

func loadFixture(root string) ([]int16, bool) {
	// Open the private directory and its fixed child without following symlinks.
	// The caller's CLI requires uid 0, making these root-owned at runtime.
	dirfd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer syscall.Close(dirfd)
	var dirStat syscall.Stat_t
	if syscall.Fstat(dirfd, &dirStat) != nil || dirStat.Mode&syscall.S_IFMT != syscall.S_IFDIR || dirStat.Mode&07777 != 0700 || dirStat.Uid != uint32(os.Geteuid()) {
		return nil, false
	}
	fd, err := syscall.Openat(dirfd, fixtureName, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	f := os.NewFile(uintptr(fd), fixtureName)
	defer f.Close()
	var st syscall.Stat_t
	if syscall.Fstat(fd, &st) != nil || !privateRegular(&st, maximumFixtureBytes) {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maximumFixtureBytes+1))
	defer clear(b)
	if err != nil || len(b) > maximumFixtureBytes || int64(len(b)) != st.Size {
		return nil, false
	}
	return parseWave(b)
}

func preflightModel(root string) bool {
	if !privateDirectory(root) {
		return false
	}
	for _, path := range []string{
		"/system/vendor/quasar/libs/libiconv.so", "/system/vendor/quasar/libs/libtflite.so", "/system/vendor/quasar/libs/libyandex_evil.so",
		filepath.Join(stockModel, "flags.txt"), filepath.Join(stockModel, "acoustic_model.nnet"), filepath.Join(stockModel, "words.txt"),
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	_, ok := modelFlags(stockModel)
	return ok
}

func protectOwnProcess() bool {
	if syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}) != nil {
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

func quietNativeOutput() (io.WriteCloser, bool) {
	fd, err := syscall.Dup(1)
	if err != nil {
		return nil, false
	}
	syscall.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), "structured-output")
	null, err := syscall.Open("/dev/null", syscall.O_WRONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		f.Close()
		return nil, false
	}
	defer syscall.Close(null)
	if syscall.Dup2(null, 1) != nil || syscall.Dup2(null, 2) != nil {
		f.Close()
		return nil, false
	}
	return f, true
}
