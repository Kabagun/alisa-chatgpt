package cloud

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func fixedProfileTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func syntheticMapProfile() MapReadProfile {
	return MapReadProfile{StorageHosts: []string{"storage.example.com"}, ModelIVs: map[string]string{"dreame.vacuum.synthetic": "synthetic-IV-123"}, ModelVersions: map[string]int{"dreame.vacuum.synthetic": 1}}
}
func TestMapProfileHasExactHostsVersionPoliciesAndNoSessionPersistence(t *testing.T) {
	p := syntheticMapProfile()
	if !validMapProfile(p) {
		t.Fatal("synthetic profile invalid")
	}
	for _, mutate := range []func(*MapReadProfile){
		func(p *MapReadProfile) { p.StorageHosts = []string{"*.example.com"} }, func(p *MapReadProfile) { p.StorageHosts = []string{"https://storage.example.com/private"} }, func(p *MapReadProfile) { p.StorageHosts = []string{"127.0.0.1"} }, func(p *MapReadProfile) { p.StorageHosts = []string{"storage.example.com", "storage.example.com"} }, func(p *MapReadProfile) { p.ModelIVs = map[string]string{"model": "bad"} }, func(p *MapReadProfile) { p.ModelVersions = map[string]int{"model": 4} },
	} {
		copy := syntheticMapProfile()
		mutate(&copy)
		if validMapProfile(copy) {
			t.Fatal("unsafe profile accepted")
		}
	}
	profile := loginFixture().Profile
	profile.MapStorageHosts = p.StorageHosts
	profile.MapModelIVs = p.ModelIVs
	profile.MapModelVersions = p.ModelVersions
	raw, _ := json.Marshal(profile)
	parsed, err := ParseMapReadProfile(strings.NewReader(string(raw)))
	if err != nil || len(parsed.ModelIVs) != 1 {
		t.Fatal("merged profile not recognized")
	}
	for _, value := range []any{profile, parsed} {
		if strings.Contains(fmt.Sprintf("%v %+v %#v", value, value, value), "synthetic-IV") || strings.Contains(fmt.Sprintf("%+v", value), "storage.example.com") {
			t.Fatal("private profile formatting leaked")
		}
	}
	input := loginFixture()
	input.Profile = profile
	if err := input.validate(); err != nil {
		t.Fatal("optional map extension changed login contract")
	}
	// Session assembly deliberately retains only the existing auth profile.
	var token TokenImport
	token.Schema = 1
	token.ServerRegion = "eu"
	token.Username = "synthetic@example.test"
	token.IssuedAt = "2026-01-01T00:00:00Z"
	token.Profile = profile
	token.Token.AccessToken = "synthetic-access"
	token.Token.ExpiresIn = 3600
	token.Token.TenantID = "synthetic-tenant"
	raw, _ = json.Marshal(token)
	s, err := AssembleIssuedSession(strings.NewReader(string(raw)), fixedProfileTime())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(s)
	for _, private := range []string{"map_storage_hosts", "map_model_ivs", "map_model_versions", "synthetic-IV", "storage.example.com"} {
		if strings.Contains(string(saved), private) {
			t.Fatal("map profile persisted in account session")
		}
	}
}
