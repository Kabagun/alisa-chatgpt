package dreame

import (
	"encoding/json"
	"testing"
)

func activeRoomsFixture(t *testing.T) string {
	t.Helper()
	var value map[string]any
	_ = json.Unmarshal([]byte(savedRoomsFixture(t)), &value)
	value["active_map_verified"] = true
	value["maps"].([]any)[0].(map[string]any)["active_map"] = true
	d := value["diagnostics"].(map[string]any)
	d["schema"] = 4
	delete(d["saved_object"].(map[string]any), "hostname")
	d["current_map"] = map[string]any{"outcome": "active_map_verified", "target_profile_matched": true, "action_requested": true, "action_acknowledged": true, "action_requests": 1, "inline_outputs": 1, "reference_outputs": 0, "other_outputs": 0, "passive_polls": 0, "freshness": "action_response", "full_frame": true, "saved_status_present": true, "saved_status": 2, "markers_valid": true, "temporary": false, "nested_temporary": false, "new": false, "restored": false, "empty": false, "linkage_present": true, "linkage_matches": true, "descriptor_unchanged": true, "active_verified": true, "last_error": ""}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func TestActiveMapProofRequiresEveryGateButNeverControl(t *testing.T) {
	valid := activeRoomsFixture(t)
	if _, err := rooms([]byte(valid), "fixture-1"); err != nil {
		t.Fatal("complete active metadata proof rejected")
	}
	for _, mutation := range []func(map[string]any){
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["saved_status"] = 1
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["saved_status_present"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["markers_valid"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["temporary"] = true
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["nested_temporary"] = true
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["new"] = true
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["restored"] = true
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["empty"] = true
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["linkage_matches"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["descriptor_unchanged"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["freshness"] = "cached_unchanged"
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["action_requests"] = 2
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["passive_polls"] = 4
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_object"].(map[string]any)["hostname"] = "MUST_NOT_ESCAPE"
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["key"] = "MUST_NOT_ESCAPE"
		},
		func(v map[string]any) { v["maps"].([]any)[0].(map[string]any)["verified_for_control"] = true },
	} {
		var value map[string]any
		_ = json.Unmarshal([]byte(valid), &value)
		mutation(value)
		raw, _ := json.Marshal(value)
		if _, err := rooms(raw, "fixture-1"); err == nil {
			t.Fatal("partial/unsafe proof or control promotion accepted")
		}
	}
}

func TestCurrentDuplicateDiagnosticCategoryIsOptionalAndFixed(t *testing.T) {
	var value map[string]any
	_ = json.Unmarshal([]byte(activeRoomsFixture(t)), &value)
	value["active_map_verified"] = false
	value["maps"].([]any)[0].(map[string]any)["active_map"] = false
	c := value["diagnostics"].(map[string]any)["current_map"].(map[string]any)
	c["outcome"] = "decode_failed"
	c["active_verified"] = false
	c["full_frame"] = false
	c["saved_status_present"] = false
	c["saved_status"] = -1
	c["markers_valid"] = false
	c["linkage_present"] = false
	c["linkage_matches"] = false
	c["descriptor_unchanged"] = false
	c["last_error"] = "saved_map_duplicate_field"
	for _, category := range []string{"", "list", "frame_metadata", "frame_rooms", "frame_version", "frame_binding", "segment_alias"} {
		c["duplicate_category"] = category
		raw, _ := json.Marshal(value)
		if _, err := rooms(raw, "fixture-1"); err != nil {
			t.Fatal("fixed duplicate category rejected")
		}
	}
	c["duplicate_category"] = "private.key.path"
	raw, _ := json.Marshal(value)
	if _, err := rooms(raw, "fixture-1"); err == nil {
		t.Fatal("dynamic duplicate path accepted")
	}
	c["duplicate_category"] = "frame_binding"
	c["last_error"] = "saved_map_invalid_frame"
	raw, _ = json.Marshal(value)
	if _, err := rooms(raw, "fixture-1"); err == nil {
		t.Fatal("category allowed for unrelated error")
	}
}
