package dreame

import (
	"encoding/json"
	"time"
)

type readinessValue struct {
	Quality string `json:"quality"`
	Value   *int   `json:"value,omitempty"`
}
type readinessPreview struct {
	Kind           string `json:"kind"`
	Operation      string `json:"operation"`
	Model          string `json:"model"`
	Mode           int    `json:"mode"`
	Fan            int    `json:"fan"`
	Water          int    `json:"water"`
	Repeat         int    `json:"repeat"`
	Order          int    `json:"order"`
	Battery        int    `json:"battery"`
	SettingsSource string `json:"settings_source"`
	Firmware       string `json:"firmware_acceptance"`
}
type cleaningReadiness struct {
	Schema   int    `json:"schema"`
	Outcome  string `json:"outcome"`
	Observed string `json:"observed_at"`
	Verified bool   `json:"verified_for_control"`
	State    struct {
		State    readinessValue `json:"state"`
		Status   readinessValue `json:"status"`
		Task     readinessValue `json:"task_status"`
		Error    readinessValue `json:"error"`
		Battery  readinessValue `json:"battery_percent"`
		Charging readinessValue `json:"charging_status"`
		Paused   readinessValue `json:"cleaning_paused"`
		Drainage readinessValue `json:"drainage"`
	} `json:"state"`
	Settings struct {
		Mode   readinessValue `json:"mode"`
		Tank   readinessValue `json:"tank"`
		Fan    readinessValue `json:"fan"`
		Water  readinessValue `json:"water"`
		Custom readinessValue `json:"customized"`
	} `json:"settings"`
	Reads struct {
		State   string `json:"state"`
		Globals string `json:"globals"`
		Custom  string `json:"customized"`
		Paused  string `json:"paused"`
	} `json:"reads"`
	Drafts []struct {
		Segment int              `json:"segment_id"`
		Preview readinessPreview `json:"preview"`
	} `json:"drafts"`
}

func readinessValueValid(raw []byte, v readinessValue, allowUnsupported bool) bool {
	if !enum(v.Quality, "observed", "missing", "invalid", "unsupported") || v.Quality == "unsupported" && !allowUnsupported {
		return false
	}
	if v.Quality == "observed" {
		return exactKeys(raw, "quality", "value") && v.Value != nil && *v.Value >= -2147483648 && *v.Value <= 2147483647
	}
	return v.Value == nil && exactKeys(raw, "quality")
}
func sanitizeCleaningReadiness(raw []byte, active bool, maps []savedMap) (json.RawMessage, bool) {
	var v cleaningReadiness
	if len(raw) > 24<<10 || !active || len(maps) != 1 || decode(raw, &v, true) != nil || !exactKeys(raw, "schema", "outcome", "observed_at", "verified_for_control", "state", "settings", "reads", "drafts") || v.Schema != 1 || v.Drafts == nil || len(v.Drafts) > 63 {
		return nil, false
	}
	if _, err := time.Parse(time.RFC3339, v.Observed); err != nil {
		return nil, false
	}
	if !enum(v.Outcome, "read_unavailable", "snapshot_unavailable", "draft_available", "cleanplan_invalid_input", "cleanplan_untrusted_input", "cleanplan_model_unsupported", "cleanplan_identity_invalid", "cleanplan_identity_mismatch", "cleanplan_snapshot_invalid", "cleanplan_snapshot_mismatch", "cleanplan_snapshot_stale", "cleanplan_catalog_unproven", "cleanplan_catalog_ambiguous", "cleanplan_map_unproven", "cleanplan_map_ineligible", "cleanplan_room_unproven", "cleanplan_room_ambiguous", "cleanplan_room_invalid", "cleanplan_observation_missing", "cleanplan_observation_unsupported", "cleanplan_observation_invalid", "cleanplan_state_not_resting", "cleanplan_state_conflict", "cleanplan_settings_unsupported", "cleanplan_settings_conflict", "cleanplan_per_room_required", "cleanplan_per_room_unobserved", "cleanplan_repeat_mismatch", "cleanplan_draft_invalid") {
		return nil, false
	}
	stateRaw, settingsRaw, readsRaw := field(raw, "state"), field(raw, "settings"), field(raw, "reads")
	if !exactKeys(stateRaw, "state", "status", "task_status", "error", "battery_percent", "charging_status", "cleaning_paused", "drainage") || !exactKeys(settingsRaw, "mode", "tank", "fan", "water", "customized") || !exactKeys(readsRaw, "state", "globals", "customized", "paused") {
		return nil, false
	}
	for _, item := range []struct {
		raw         []byte
		v           readinessValue
		unsupported bool
	}{
		{field(stateRaw, "state"), v.State.State, false}, {field(stateRaw, "status"), v.State.Status, false}, {field(stateRaw, "task_status"), v.State.Task, false}, {field(stateRaw, "error"), v.State.Error, false}, {field(stateRaw, "battery_percent"), v.State.Battery, false}, {field(stateRaw, "charging_status"), v.State.Charging, false}, {field(stateRaw, "cleaning_paused"), v.State.Paused, false}, {field(stateRaw, "drainage"), v.State.Drainage, true},
		{field(settingsRaw, "mode"), v.Settings.Mode, false}, {field(settingsRaw, "tank"), v.Settings.Tank, false}, {field(settingsRaw, "fan"), v.Settings.Fan, false}, {field(settingsRaw, "water"), v.Settings.Water, false}, {field(settingsRaw, "customized"), v.Settings.Custom, false},
	} {
		if !readinessValueValid(item.raw, item.v, item.unsupported) {
			return nil, false
		}
	}
	if v.State.Drainage.Quality != "unsupported" {
		return nil, false
	}
	for _, outcome := range []string{v.Reads.State, v.Reads.Globals, v.Reads.Custom, v.Reads.Paused} {
		if !enum(outcome, "ok", "request_unavailable", "response_invalid") {
			return nil, false
		}
	}
	if (v.Outcome == "draft_available") != (len(v.Drafts) > 0) {
		return nil, false
	}
	if v.Verified && v.Outcome != "draft_available" {
		return nil, false
	}
	if len(v.Drafts) > 0 && (len(v.Drafts) != len(maps[0].Rooms) || v.Reads.State != "ok" || v.Reads.Globals != "ok" || v.Reads.Custom != "ok" || v.Reads.Paused != "ok") {
		return nil, false
	}
	owned := map[int]bool{}
	for _, room := range maps[0].Rooms {
		owned[room.ID] = true
	}
	seen := map[int]bool{}
	var drafts []json.RawMessage
	_ = json.Unmarshal(field(raw, "drafts"), &drafts)
	for i, d := range v.Drafts {
		p := d.Preview
		if !exactKeys(drafts[i], "segment_id", "preview") || !exactKeys(field(drafts[i], "preview"), "kind", "operation", "model", "mode", "fan", "water", "repeat", "order", "battery", "settings_source", "firmware_acceptance") || !owned[d.Segment] || seen[d.Segment] || p.Kind != "DRAFT" || p.Operation != "single_room" || p.Model != "dreame.vacuum.r2422" || p.Mode < 0 || p.Mode > 2 || p.Fan < 0 || p.Fan > 3 || p.Water < 1 || p.Water > 3 || p.Repeat != 1 || p.Order != 1 || p.Battery < 0 || p.Battery > 100 || p.SettingsSource != "observed_global" || p.Firmware != "unverified" {
			return nil, false
		}
		seen[d.Segment] = true
		for _, pair := range []struct {
			value readinessValue
			want  int
		}{{v.Settings.Mode, p.Mode}, {v.Settings.Fan, p.Fan}, {v.Settings.Water, p.Water}, {v.State.Battery, p.Battery}, {v.Settings.Custom, 0}, {v.State.Paused, 0}, {v.State.Error, 0}, {v.State.Task, 0}} {
			if pair.value.Quality != "observed" || pair.value.Value == nil || *pair.value.Value != pair.want {
				return nil, false
			}
		}
		if v.Settings.Tank.Quality != "observed" || v.Settings.Tank.Value == nil || p.Mode == 0 && *v.Settings.Tank.Value != 0 || p.Mode != 0 && *v.Settings.Tank.Value != 1 && *v.Settings.Tank.Value != 10 {
			return nil, false
		}
		if v.State.State.Quality != "observed" || v.State.Status.Quality != "observed" || v.State.Charging.Quality != "observed" || v.State.State.Value == nil || v.State.Status.Value == nil || v.State.Charging.Value == nil {
			return nil, false
		}
		state, status, charging := *v.State.State.Value, *v.State.Status.Value, *v.State.Charging.Value
		if state != 2 && state != 6 && state != 13 || status != 0 && status != 6 && status != 14 && status != 17 || charging != 1 && charging != 2 && charging != 3 || (state == 6 || state == 13 || status == 6) && charging == 2 {
			return nil, false
		}
	}
	b, err := json.Marshal(v)
	return b, err == nil
}
