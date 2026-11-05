//go:build !linux

package cloud

func LoadMapReadProfile(string) (MapReadProfile, error) {
	return MapReadProfile{}, sessionError("protected_file_requires_linux")
}
