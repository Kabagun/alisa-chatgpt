package dreame

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"time"

	"smart-home-plugin/internal/provider"
)

type controlResult struct {
	Schema          int      `json:"schema"`
	Device          string   `json:"device_id"`
	Map             string   `json:"map_id"`
	Rooms           []string `json:"room_ids"`
	Observed        string   `json:"observed_at"`
	Outcome         string   `json:"outcome"`
	Reason          string   `json:"reason"`
	Attempted       bool     `json:"attempted"`
	Acknowledgement string   `json:"acknowledgement"`
	Running         bool     `json:"segment_task_running"`
	Confirmed       bool     `json:"selected_room_confirmed"`
	Binding         bool     `json:"map_binding_unchanged"`
	StatusReads     int      `json:"status_reads"`
	MapRefreshes    int      `json:"map_refreshes"`
	Retry           bool     `json:"automatic_retry"`
}

func validControlParams(p provider.Params) bool {
	m, e := strconv.ParseInt(p.MapID, 10, 16)
	if e != nil || strconv.FormatInt(m, 10) != p.MapID || len(p.RoomIDs) != 1 {
		return false
	}
	n, e := strconv.Atoi(p.RoomIDs[0])
	return e == nil && n >= 1 && n <= 63 && strconv.Itoa(n) == p.RoomIDs[0]
}
func unknownControl(p provider.Params) json.RawMessage {
	raw, _ := result(controlResult{Schema: 1, Device: p.DeviceID, Map: p.MapID, Rooms: p.RoomIDs, Observed: time.Now().UTC().Format(time.RFC3339), Outcome: "unknown", Reason: "helper_outcome_unconfirmed", Attempted: true, Acknowledgement: "unknown"})
	return raw
}
func (a *Adapter) control(ctx context.Context, session string, p provider.Params) (out json.RawMessage, err error) {
	path, remove, err := stageMapProfile(filepath.Dir(session), a.profile)
	if err != nil {
		return nil, err
	}
	invoked := false
	defer func() {
		if remove() != nil {
			if invoked {
				out = unknownControl(p)
				err = nil
			} else {
				out = nil
				err = provider.Err("provider_unavailable")
			}
		}
	}()
	request := struct {
		Schema int      `json:"schema"`
		Op     string   `json:"op"`
		Device string   `json:"device_id"`
		Map    string   `json:"map_id"`
		Rooms  []string `json:"room_ids"`
	}{1, "clean_rooms", p.DeviceID, p.MapID, p.RoomIDs}
	payload, _ := json.Marshal(request)
	invoked = true
	raw, runErr := a.run(ctx, a.reader, []string{"--control", "--session", session, "--map-profile", path}, payload)
	// Never enter the Execute retry/refresh loop after a control helper starts.
	if runErr != nil {
		return unknownControl(p), nil
	}
	var env struct {
		Schema   int             `json:"schema"`
		Mode     string          `json:"mode"`
		ReadOnly bool            `json:"read_only"`
		OK       *bool           `json:"ok"`
		Data     json.RawMessage `json:"data"`
		Error    *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if decode(raw, &env, true) != nil || env.Schema != 1 || env.Mode != "dreame_control" || env.ReadOnly || env.OK == nil || !*env.OK || env.Error != nil {
		return unknownControl(p), nil
	}
	return sanitizeControlResult(env.Data, p)
}
func sanitizeControlResult(raw []byte, p provider.Params) (json.RawMessage, error) {
	var v controlResult
	unknown := func() (json.RawMessage, error) { return unknownControl(p), nil }
	if len(raw) > 4096 || decode(raw, &v, true) != nil || !exactKeys(raw, "schema", "device_id", "map_id", "room_ids", "observed_at", "outcome", "reason", "attempted", "acknowledgement", "segment_task_running", "selected_room_confirmed", "map_binding_unchanged", "status_reads", "map_refreshes", "automatic_retry") || v.Schema != 1 || v.Device != p.DeviceID || v.Map != p.MapID || len(v.Rooms) != 1 || v.Rooms[0] != p.RoomIDs[0] || v.Retry || v.StatusReads < 0 || v.StatusReads > 1 || v.MapRefreshes < 0 || v.MapRefreshes > 1 {
		return unknown()
	}
	if _, e := time.Parse(time.RFC3339, v.Observed); e != nil {
		return unknown()
	}
	if !enum(v.Outcome, "not_sent", "accepted", "started", "unknown", "rejected", "segment_task_running_room_unconfirmed", "observed_running_reply_unknown") || !enum(v.Reason, "preview_required", "control_busy", "control_state_unavailable", "cooldown", "fresh_read_unavailable", "not_eligible", "plan_changed", "room_unavailable", "read_deadline", "reply_unconfirmed", "device_rejected", "start_unconfirmed", "selected_room_unconfirmed", "device_reported_running", "device_reported_running_reply_unknown") || !enum(v.Acknowledgement, "not_attempted", "accepted", "rejected", "unknown") {
		return unknown()
	}
	if !v.Attempted {
		if v.Outcome != "not_sent" || v.Acknowledgement != "not_attempted" || v.Running || v.Confirmed || v.Binding || v.StatusReads != 0 || v.MapRefreshes != 0 {
			return unknown()
		}
	} else {
		if v.Outcome == "not_sent" || v.Acknowledgement == "not_attempted" || v.Confirmed && (!v.Running || !v.Binding || v.MapRefreshes != 1) || v.Binding && v.MapRefreshes != 1 || v.Running && v.StatusReads != 1 || v.MapRefreshes > 0 && !v.Running {
			return unknown()
		}
		switch v.Outcome {
		case "accepted":
			if v.Acknowledgement != "accepted" || v.Running || v.Confirmed {
				return unknown()
			}
		case "rejected":
			if v.Acknowledgement != "rejected" || v.Running || v.Confirmed || v.StatusReads != 0 {
				return unknown()
			}
		case "unknown":
			if v.Acknowledgement != "unknown" || v.Running || v.Confirmed {
				return unknown()
			}
		case "started":
			if v.Acknowledgement != "accepted" || !v.Confirmed {
				return unknown()
			}
		case "segment_task_running_room_unconfirmed":
			if !v.Running || v.Confirmed || !enum(v.Acknowledgement, "accepted", "unknown") {
				return unknown()
			}
		case "observed_running_reply_unknown":
			if v.Acknowledgement != "unknown" || !v.Confirmed {
				return unknown()
			}
		}
	}
	return result(v)
}
