package cloud

import (
	"fmt"
	"io"
	"unicode/utf8"

	"alisa-dreame-readonly/internal/bridge"
)

const MaxAppProfile = 16 << 10

// MapReadProfile is trusted deployment configuration, never caller parameters.
// It is not included in issued sessions or sent to the vendor authentication API.
type MapReadProfile struct {
	StorageHosts  []string
	ModelIVs      map[string]string
	ModelVersions map[string]int
}

func (MapReadProfile) String() string               { return "[map profile redacted]" }
func (MapReadProfile) GoString() string             { return "[map profile redacted]" }
func (p MapReadProfile) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(p.String())) }
func (MapReadProfile) MarshalJSON() ([]byte, error) { return []byte(`"[map profile redacted]"`), nil }
func (p MapReadProfile) enabled() bool {
	return len(p.StorageHosts)+len(p.ModelIVs)+len(p.ModelVersions) > 0
}
func modelProfileKey(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func validMapProfile(p MapReadProfile) bool {
	if len(p.StorageHosts) > 4 || len(p.ModelIVs) > 16 || len(p.ModelVersions) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, host := range p.StorageHosts {
		if !dnsMetadataHost(host) || inspectSignedMapURL("https://"+host+"/").Hostname != host || seen[host] {
			return false
		}
		seen[host] = true
	}
	for model, iv := range p.ModelIVs {
		if !modelProfileKey(model) || len(iv) != 16 || !utf8.ValidString(iv) || !privateMapText(iv, 16) {
			return false
		}
	}
	for model, v := range p.ModelVersions {
		if !modelProfileKey(model) || v < 0 || v > 3 {
			return false
		}
	}
	return true
}
func ParseMapReadProfile(r io.Reader) (MapReadProfile, error) {
	var p AppProfile
	if bridge.DecodeOne(r, MaxAppProfile, &p, true) != nil || p.UpstreamCommit != UpstreamCommit {
		return MapReadProfile{}, sessionError("map_profile_invalid")
	}
	profile := MapReadProfile{StorageHosts: p.MapStorageHosts, ModelIVs: p.MapModelIVs, ModelVersions: p.MapModelVersions}
	if !validMapProfile(profile) {
		return MapReadProfile{}, sessionError("map_profile_invalid")
	}
	return profile, nil
}
func (c *Client) SetMapReadProfile(p MapReadProfile) error {
	if !validMapProfile(p) {
		return sessionError("map_profile_invalid")
	}
	copy := MapReadProfile{StorageHosts: append([]string(nil), p.StorageHosts...), ModelIVs: map[string]string{}, ModelVersions: map[string]int{}}
	for k, v := range p.ModelIVs {
		copy.ModelIVs[k] = v
	}
	for k, v := range p.ModelVersions {
		copy.ModelVersions[k] = v
	}
	c.mapProfile = copy
	c.mapBlobs = newMapBlobReader(copy.StorageHosts)
	return nil
}
