package dreame

import (
	"encoding/json"
	"testing"
)

func TestCurrentSelectionDiagnosticIsOptionalBoundedAndIDFree(t *testing.T) {
	for _, selection := range []any{
		map[string]any{"present": false, "valid": false, "count": 0},
		map[string]any{"present": true, "valid": false, "count": 0},
		map[string]any{"present": true, "valid": true, "count": 0},
		map[string]any{"present": true, "valid": true, "count": 1},
		map[string]any{"present": true, "valid": true, "count": 63},
	} {
		var v map[string]any
		_ = json.Unmarshal([]byte(activeRoomsFixture(t)), &v)
		v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["selection"] = selection
		raw, _ := json.Marshal(v)
		if _, err := rooms(raw, "fixture-1"); err != nil {
			t.Fatal("valid selection diagnostic rejected")
		}
	}
	for _, selection := range []any{
		nil,
		map[string]any{"present": nil, "valid": false, "count": 0},
		map[string]any{"present": true, "valid": nil, "count": 0},
		map[string]any{"present": true, "valid": true, "count": nil},
		map[string]any{"present": false, "valid": true, "count": 0},
		map[string]any{"present": true, "valid": false, "count": 1},
		map[string]any{"present": true, "valid": true, "count": 64},
		map[string]any{"present": true, "valid": true, "count": -1},
		map[string]any{"present": true, "valid": true, "count": "1"},
		map[string]any{"present": true, "valid": true, "count": 1, "segment_ids": []int{3}},
		map[string]any{"present": true, "valid": true},
	} {
		var v map[string]any
		_ = json.Unmarshal([]byte(activeRoomsFixture(t)), &v)
		v["diagnostics"].(map[string]any)["current_map"].(map[string]any)["selection"] = selection
		raw, _ := json.Marshal(v)
		if _, err := rooms(raw, "fixture-1"); err == nil {
			t.Fatal("unsafe selection diagnostic accepted")
		}
	}
}
