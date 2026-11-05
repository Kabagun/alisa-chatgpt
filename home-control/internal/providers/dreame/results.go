package dreame

import (
	"encoding/json"
	"strconv"
	"time"

	"smart-home-plugin/internal/provider"
)

type device struct {
	ID    string `json:"device_id"`
	Model string `json:"model"`
	Name  string `json:"name"`
}

func result(v any) (json.RawMessage, error) {
	b, e := json.Marshal(v)
	if e != nil || len(b) > provider.MaxResultBytes {
		return nil, provider.Err("provider_unavailable")
	}
	return b, nil
}
func malformed() (json.RawMessage, error) { return nil, provider.Err("provider_unavailable") }
func devices(raw []byte) (json.RawMessage, []device, error) {
	var v []device
	if decode(raw, &v, false) != nil || v == nil || len(v) > 256 {
		return nil, nil, provider.Err("provider_unavailable")
	}
	seen := map[string]bool{}
	for _, d := range v {
		if !deviceID(d.ID) || seen[d.ID] || !text(d.Model, 128, false) || !text(d.Name, 256, true) {
			return nil, nil, provider.Err("provider_unavailable")
		}
		seen[d.ID] = true
	}
	b, e := result(v)
	return b, v, e
}

var statusNames = map[string]bool{"state": true, "error": true, "battery_percent": true, "charging_status": true, "status": true, "task_status": true}

func status(raw []byte, id string) (json.RawMessage, error) {
	var v struct {
		ID          string         `json:"device_id"`
		Observed    string         `json:"observed_at"`
		Values      map[string]int `json:"values"`
		Unavailable []string       `json:"unavailable"`
		Note        string         `json:"model_note"`
	}
	if decode(raw, &v, false) != nil || v.ID != id || len(v.Values) > 6 || len(v.Unavailable) > 6 || v.Note != "raw_baseline_values_require_model_validation" {
		return malformed()
	}
	if _, e := time.Parse(time.RFC3339, v.Observed); e != nil {
		return malformed()
	}
	for k, n := range v.Values {
		if !statusNames[k] || n < -2147483648 || n > 2147483647 || k == "battery_percent" && (n < 0 || n > 100) {
			return malformed()
		}
	}
	seen := map[string]bool{}
	for _, k := range v.Unavailable {
		if !statusNames[k] || seen[k] {
			return malformed()
		}
		if _, ok := v.Values[k]; ok {
			return malformed()
		}
		seen[k] = true
	}
	return result(v)
}

type room struct {
	ID     int    `json:"segment_id"`
	Unique string `json:"unique_room_id,omitempty"`
	Name   string `json:"name"`
	Type   int    `json:"type_code"`
}
type savedMap struct {
	Device   string `json:"device_id"`
	ID       int    `json:"map_id"`
	Version  int    `json:"map_version"`
	Active   bool   `json:"active_map"`
	Metadata bool   `json:"metadata_only"`
	Verified bool   `json:"verified_for_control"`
	Rooms    []room `json:"rooms"`
}

func rooms(raw []byte, id string) (json.RawMessage, error) {
	var v struct {
		Device        string          `json:"device_id"`
		Source        string          `json:"source"`
		Active        bool            `json:"active_map_verified"`
		Maps          []savedMap      `json:"maps"`
		Unsupported   int             `json:"unsupported_frames"`
		Diagnostics   json.RawMessage `json:"diagnostics,omitempty"`
		SelectedSaved string          `json:"selected_saved_map_id,omitempty"`
		Readiness     json.RawMessage `json:"cleaning_readiness,omitempty"`
	}
	if decode(raw, &v, false) != nil || v.Device != id || !enum(v.Source, "cloud_history_6.1_metadata_only", "cloud_saved_map_metadata_only") || v.Maps == nil || len(v.Maps) > 20 || v.Unsupported < 0 || v.Unsupported > 10000 {
		return malformed()
	}
	if v.Diagnostics != nil {
		var ok bool
		v.Diagnostics, ok = sanitizeMapDiagnostic(v.Diagnostics, len(v.Maps), v.Unsupported, v.Source, v.SelectedSaved, v.Active)
		if !ok {
			return malformed()
		}
	} else if len(v.Maps) == 0 || v.Source != "cloud_history_6.1_metadata_only" || v.SelectedSaved != "" || v.Active {
		return malformed()
	}
	seen := map[int]bool{}
	for _, m := range v.Maps {
		minID := 1
		maxVersion := 255
		if v.Source == "cloud_saved_map_metadata_only" {
			minID = -32768
			maxVersion = 3
		}
		if m.Device != id || m.ID < minID || m.ID > 32767 || seen[m.ID] || m.Version < 0 || m.Version > maxVersion || m.Active != v.Active || !m.Metadata || m.Verified || m.Rooms == nil || len(m.Rooms) > 63 {
			return malformed()
		}
		seen[m.ID] = true
		roomIDs := map[int]bool{}
		for _, r := range m.Rooms {
			if r.ID < 1 || r.ID > 63 || roomIDs[r.ID] || !text(r.Unique, 128, true) || !text(r.Name, 256, true) || r.Type < 0 || r.Type > 65535 {
				return malformed()
			}
			roomIDs[r.ID] = true
		}
	}
	if v.Source == "cloud_saved_map_metadata_only" && len(v.Maps) > 0 && (len(v.Maps) != 1 || strconv.Itoa(v.Maps[0].ID) != v.SelectedSaved) {
		return malformed()
	}
	if v.Readiness != nil {
		var ok bool
		v.Readiness, ok = sanitizeCleaningReadiness(v.Readiness, v.Active, v.Maps)
		if !ok {
			return malformed()
		}
	}
	return result(v)
}
