package dreame

import (
	"encoding/json"
	"strings"
	"testing"
)

func readinessRoomsFixture(t *testing.T) string {
	t.Helper()
	var v map[string]any
	_ = json.Unmarshal([]byte(activeRoomsFixture(t)), &v)
	observed := func(n int) any { return map[string]any{"quality": "observed", "value": n} }
	v["cleaning_readiness"] = map[string]any{
		"schema": 1, "outcome": "draft_available", "observed_at": "2026-10-31T12:00:00Z", "verified_for_control": false,
		"state":    map[string]any{"state": observed(2), "status": observed(0), "task_status": observed(0), "error": observed(0), "battery_percent": observed(82), "charging_status": observed(2), "cleaning_paused": observed(0), "drainage": map[string]any{"quality": "unsupported"}},
		"settings": map[string]any{"mode": observed(0), "tank": observed(0), "fan": observed(1), "water": observed(2), "customized": observed(0)},
		"reads":    map[string]any{"state": "ok", "globals": "ok", "customized": "ok", "paused": "ok"},
		"drafts":   []any{map[string]any{"segment_id": 3, "preview": map[string]any{"kind": "DRAFT", "operation": "single_room", "model": "dreame.vacuum.r2422", "mode": 0, "fan": 1, "water": 2, "repeat": 1, "order": 1, "battery": 82, "settings_source": "observed_global", "firmware_acceptance": "unverified"}}},
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
func TestCleaningReadinessStrictShapeQualityAndScope(t *testing.T) {
	valid := readinessRoomsFixture(t)
	raw, err := rooms([]byte(valid), "fixture-1")
	if err != nil || !strings.Contains(string(raw), `"kind":"DRAFT"`) || strings.Contains(string(raw), `"selects"`) {
		t.Fatal("bounded draft rejected or payload present")
	}
	for _, mutate := range []func(map[string]any){
		func(r map[string]any) {
			r["verified_for_control"] = true
			r["outcome"] = "snapshot_unavailable"
			r["drafts"] = []any{}
		},
		func(r map[string]any) { r["schema"] = 2 },
		func(r map[string]any) { r["account_id"] = "MUST_NOT_ESCAPE" },
		func(r map[string]any) { r["outcome"] = "MUST_NOT_ESCAPE" },
		func(r map[string]any) { r["observed_at"] = "bad" },
		func(r map[string]any) { r["state"].(map[string]any)["raw"] = 1 },
		func(r map[string]any) {
			r["settings"].(map[string]any)["fan"].(map[string]any)["quality"] = "unsupported"
		},
		func(r map[string]any) { r["settings"].(map[string]any)["fan"].(map[string]any)["value"] = true },
		func(r map[string]any) { r["settings"].(map[string]any)["fan"].(map[string]any)["value"] = 2147483648 },
		func(r map[string]any) {
			r["settings"].(map[string]any)["fan"].(map[string]any)["raw_value"] = "MUST_NOT_ESCAPE"
		},
		func(r map[string]any) { r["state"].(map[string]any)["drainage"].(map[string]any)["value"] = 0 },
		func(r map[string]any) { r["reads"].(map[string]any)["globals"] = "MUST_NOT_ESCAPE" },
		func(r map[string]any) { r["drafts"].([]any)[0].(map[string]any)["segment_id"] = 4 },
		func(r map[string]any) {
			r["drafts"].([]any)[0].(map[string]any)["preview"].(map[string]any)["payload"] = "MUST_NOT_ESCAPE"
		},
		func(r map[string]any) { r["drafts"].([]any)[0].(map[string]any)["preview"].(map[string]any)["fan"] = 3 },
		func(r map[string]any) {
			r["drafts"].([]any)[0].(map[string]any)["preview"].(map[string]any)["repeat"] = 2
		},
		func(r map[string]any) { r["settings"].(map[string]any)["customized"].(map[string]any)["value"] = 1 },
		func(r map[string]any) { r["settings"].(map[string]any)["tank"].(map[string]any)["value"] = 1 },
		func(r map[string]any) { r["drafts"] = []any{} },
	} {
		var v map[string]any
		_ = json.Unmarshal([]byte(valid), &v)
		mutate(v["cleaning_readiness"].(map[string]any))
		raw, _ := json.Marshal(v)
		if _, err := rooms(raw, "fixture-1"); err == nil {
			t.Fatal("unsafe readiness accepted")
		}
	}
}
func TestReadinessUnavailablePreservesObservedSettings(t *testing.T) {
	var v map[string]any
	_ = json.Unmarshal([]byte(readinessRoomsFixture(t)), &v)
	r := v["cleaning_readiness"].(map[string]any)
	r["outcome"] = "cleanplan_observation_invalid"
	r["drafts"] = []any{}
	r["settings"].(map[string]any)["customized"] = map[string]any{"quality": "invalid"}
	r["reads"].(map[string]any)["customized"] = "response_invalid"
	raw, _ := json.Marshal(v)
	out, err := rooms(raw, "fixture-1")
	if err != nil || !strings.Contains(string(out), `"fan":{"quality":"observed","value":1}`) {
		t.Fatal("independent valid batch lost")
	}
	for _, q := range []string{"missing", "invalid"} {
		r["settings"].(map[string]any)["customized"] = map[string]any{"quality": q}
		raw, _ := json.Marshal(v)
		if _, err = rooms(raw, "fixture-1"); err != nil {
			t.Fatal("unavailable quality rejected")
		}
	}
	// The same readiness must never appear with an unverified map.
	v["active_map_verified"] = false
	raw, _ = json.Marshal(v)
	if _, err = rooms(raw, "fixture-1"); err == nil {
		t.Fatal("unverified map readiness accepted")
	}
}

func TestReadinessEligibilityIsSeparateFromFirmwareEvidence(t *testing.T) {
	var v map[string]any
	_ = json.Unmarshal([]byte(readinessRoomsFixture(t)), &v)
	v["cleaning_readiness"].(map[string]any)["verified_for_control"] = true
	raw, _ := json.Marshal(v)
	out, err := rooms(raw, "fixture-1")
	if err != nil || !strings.Contains(string(out), `"firmware_acceptance":"unverified"`) || !strings.Contains(string(out), `"metadata_only":true,"verified_for_control":false`) {
		t.Fatal("eligibility/firmware/map distinction")
	}
}
