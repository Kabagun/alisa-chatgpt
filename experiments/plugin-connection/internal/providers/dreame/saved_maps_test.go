package dreame

import (
	"encoding/json"
	"strings"
	"testing"
)

func savedRoomsFixture(t *testing.T) string {
	t.Helper()
	var value map[string]any
	_ = json.Unmarshal([]byte(mapDiagnosticFixture), &value)
	value["source"] = "cloud_saved_map_metadata_only"
	value["selected_saved_map_id"] = "42"
	value["maps"] = []any{map[string]any{"device_id": "fixture-1", "map_id": 42, "map_version": 1, "active_map": false, "metadata_only": true, "verified_for_control": false, "rooms": []any{map[string]any{"segment_id": 3, "name": "Synthetic kitchen", "type_code": 4, "unique_room_id": "synthetic-room"}}}}
	d := value["diagnostics"].(map[string]any)
	d["schema"] = 3
	d["reason"] = "saved_metadata_available"
	d["saved_object"] = validSavedObjectFixture()
	d["saved_object"].(map[string]any)["fetch_performed"] = true
	d["saved_maps"] = map[string]any{"outcome": "selected_map_available", "version_hint": 1, "version_source": "model_default", "map_saving_probe": "property_unavailable", "list_entries": 3, "frames_examined": 2, "frames_decoded": 2, "frame_failures": 0, "blob_get_attempts": 2, "blob_downloads": 2, "accepted_blob_bytes": 500, "selected_matched": true, "last_error": ""}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func TestSavedRoomSchemaRequiresSelectedIDAndKeepsControlUnverified(t *testing.T) {
	valid := savedRoomsFixture(t)
	raw, err := rooms([]byte(valid), "fixture-1")
	if err != nil || !strings.Contains(string(raw), `"selected_saved_map_id":"42"`) || !strings.Contains(string(raw), `"verified_for_control":false`) {
		t.Fatal("saved-room result rejected/promoted")
	}
	for _, mutation := range []func(map[string]any){
		func(v map[string]any) { v["selected_saved_map_id"] = "11" },
		func(v map[string]any) { v["active_map_verified"] = true },
		func(v map[string]any) { v["source"] = "cloud_history_6.1_metadata_only" },
		func(v map[string]any) { v["maps"].([]any)[0].(map[string]any)["verified_for_control"] = true },
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["selected_matched"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["blob_get_attempts"] = 5
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["accepted_blob_bytes"] = 8 << 20
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["raw_blob"] = "MUST_NOT_ESCAPE"
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["last_error"] = "MUST_NOT_ESCAPE"
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_object"].(map[string]any)["fetch_performed"] = false
		},
		func(v map[string]any) {
			v["diagnostics"].(map[string]any)["saved_maps"].(map[string]any)["version_source"] = "map_saving_nonnull"
		},
	} {
		var value map[string]any
		_ = json.Unmarshal([]byte(valid), &value)
		mutation(value)
		raw, _ := json.Marshal(value)
		if _, err := rooms(raw, "fixture-1"); err == nil {
			t.Fatal("unsafe saved-room metadata accepted")
		}
	}
}

func TestExtendedProfileStaysBoundedAndExact(t *testing.T) {
	var value map[string]any
	_ = json.Unmarshal([]byte(fakeProfile), &value)
	value["map_storage_hosts"] = []string{"storage.example.com"}
	value["map_model_ivs"] = map[string]string{"dreame.vacuum.synthetic": "synthetic-IV-123"}
	value["map_model_versions"] = map[string]int{"dreame.vacuum.synthetic": 1}
	raw, _ := json.Marshal(value)
	if !validProfile(raw) {
		t.Fatal("private map extensions rejected")
	}
	value["map_storage_hosts"] = []string{"*.example.com"}
	raw, _ = json.Marshal(value)
	if validProfile(raw) {
		t.Fatal("wildcard storage host accepted")
	}
	value["map_storage_hosts"] = []string{"storage.example.com"}
	value["map_model_versions"] = map[string]int{"dreame.vacuum.synthetic": 4}
	raw, _ = json.Marshal(value)
	if validProfile(raw) {
		t.Fatal("unknown version guessed")
	}
}
