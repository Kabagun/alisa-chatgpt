package dreame

import (
	"os"
	"smart-home-plugin/internal/provider"
)

// Deployment profiles can be immutable platform-owned files with runtime ACLs.
// The helper instead receives this current-UID0600 copy in the account's private
// 0700 directory. No password/token/session is copied or changed.
func stageMapProfile(dir string, profile []byte) (string, func() error, error) {
	if _, err := accountSession(dir); err != nil {
		return "", nil, err
	}
	if len(profile) == 0 || len(profile) > 16<<10 {
		return "", nil, provider.Err("provider_unavailable")
	}
	f, err := os.CreateTemp(dir, ".map-profile-*.json")
	if err != nil {
		return "", nil, provider.Err("provider_unavailable")
	}
	path := f.Name()
	remove := func() error {
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	fail := func() (string, func() error, error) {
		_ = f.Close()
		_ = remove()
		return "", nil, provider.Err("provider_unavailable")
	}
	if f.Chmod(0600) != nil {
		return fail()
	}
	if _, err := f.Write(profile); err != nil {
		return fail()
	}
	if f.Close() != nil {
		return fail()
	}
	if !privateSession(path) {
		return fail()
	}
	return path, remove, nil
}
