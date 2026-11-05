//go:build linux

package cloud

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMapProfileFileRequiresCurrentUID0600NoSymlink(t *testing.T) {
	dir := privateTestDirectory(t)
	path := filepath.Join(dir, "map-profile.json")
	p := loginFixture().Profile
	mp := syntheticMapProfile()
	p.MapStorageHosts = mp.StorageHosts
	p.MapModelIVs = mp.ModelIVs
	p.MapModelVersions = mp.ModelVersions
	raw, _ := json.Marshal(p)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMapReadProfile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMapReadProfile(path); err == nil {
		t.Fatal("public-readable private profile accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMapReadProfile(alias); err == nil {
		t.Fatal("symlink profile accepted")
	}
}
