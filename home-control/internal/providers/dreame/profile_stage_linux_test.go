//go:build linux

package dreame

import (
	"context"
	"os"
	"path/filepath"
	"smart-home-plugin/internal/provider"
	"testing"
	"time"
)

func noStagedProfiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".map-profile-*.json"))
	if err != nil || len(matches) != 0 {
		t.Fatal("temporary map profile not cleaned")
	}
}
func TestStagedProfileCurrentUID0600AndAccountIsolation(t *testing.T) {
	a, dir := setup(t)
	other := filepath.Join(filepath.Dir(dir), "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	path, remove, err := stageMapProfile(dir, a.profile)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir || !privateSession(path) {
		t.Fatal("profile escaped account/private file ownership")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != string(a.profile) {
		t.Fatal("staged profile differs from validated configuration")
	}
	noStagedProfiles(t, other)
	if err := remove(); err != nil {
		t.Fatal("profile removal failed")
	}
	noStagedProfiles(t, dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stageMapProfile(dir, a.profile); err == nil {
		t.Fatal("profile staged in nonprivate account directory")
	}
}
func TestRoomsProfileCleanupOnSuccessFailureAndTimeout(t *testing.T) {
	for _, m := range []string{"normal", "malformed", "timeout"} {
		t.Run(m, func(t *testing.T) {
			a, dir := setup(t)
			mode(t, dir, m)
			if m == "timeout" {
				a.timeout = 50 * time.Millisecond
			}
			// Call read directly so timeout/malformed behavior occurs after profile
			// staging, rather than in the preceding catalog read.
			_, err := a.read(context.Background(), filepath.Join(dir, "session.json"), "rooms", "fixture-1")
			if m == "normal" && err != nil {
				t.Fatal(err)
			}
			if m != "normal" && err == nil {
				t.Fatal("fixture failure not exercised")
			}
			noStagedProfiles(t, dir)
		})
	}
}
func TestImmutableDeploymentProfileDoesNotBecomeHelperPath(t *testing.T) {
	a, dir := setup(t)
	deployment := filepath.Join(filepath.Dir(dir), "profile.json")
	if err := os.Chmod(deployment, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Execute(context.Background(), dir, provider.Rooms, provider.Params{DeviceID: "fixture-1"}); err != nil {
		t.Fatal("validated deployment profile could not be staged privately")
	}
	noStagedProfiles(t, dir)
}
