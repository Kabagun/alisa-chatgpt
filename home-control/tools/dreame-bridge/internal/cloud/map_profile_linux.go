//go:build linux

package cloud

import (
	"os"
	"path/filepath"
	"syscall"
)

func LoadMapReadProfile(path string) (MapReadProfile, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return MapReadProfile{}, sessionError("map_profile_permissions")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return MapReadProfile{}, sessionError("map_profile_unavailable")
	}
	f := os.NewFile(uintptr(fd), "protected-map-profile")
	defer f.Close()
	if !secureFile(f, 0400) {
		return MapReadProfile{}, sessionError("map_profile_permissions")
	}
	return ParseMapReadProfile(f)
}
