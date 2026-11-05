package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/cleanplan"
	"alisa-dreame-readonly/internal/savedmap"
)

// Only the proven current frame stays in memory. It is never a wire DTO.
type privateCurrentProof struct {
	frame  savedmap.Frame
	readAt int64
}
type privateDrafts map[int]*cleanplan.Draft

type ReadinessValue struct {
	Quality string `json:"quality"`
	Value   *int   `json:"value,omitempty"`
}
type ReadinessState struct {
	State    ReadinessValue `json:"state"`
	Status   ReadinessValue `json:"status"`
	Task     ReadinessValue `json:"task_status"`
	Error    ReadinessValue `json:"error"`
	Battery  ReadinessValue `json:"battery_percent"`
	Charging ReadinessValue `json:"charging_status"`
	Paused   ReadinessValue `json:"cleaning_paused"`
	Drainage ReadinessValue `json:"drainage"`
}
type ReadinessSettings struct {
	Fan        ReadinessValue `json:"fan"`
	Water      ReadinessValue `json:"water"`
	Mode       ReadinessValue `json:"mode"`
	Tank       ReadinessValue `json:"tank"`
	Customized ReadinessValue `json:"customized"`
}
type ReadinessReads struct {
	State      string `json:"state"`
	Globals    string `json:"globals"`
	Customized string `json:"customized"`
	Paused     string `json:"paused"`
}
type ReadinessDraft struct {
	SegmentID int               `json:"segment_id"`
	Preview   cleanplan.Preview `json:"preview"`
}
type CleaningReadiness struct {
	Schema             int               `json:"schema"`
	Outcome            string            `json:"outcome"`
	ObservedAt         string            `json:"observed_at"`
	VerifiedForControl bool              `json:"verified_for_control"`
	State              ReadinessState    `json:"state"`
	Settings           ReadinessSettings `json:"settings"`
	Reads              ReadinessReads    `json:"reads"`
	Drafts             []ReadinessDraft  `json:"drafts"`
}
type readinessSelector struct {
	name, did  string
	siid, piid int
}

var readinessGlobals = []readinessSelector{{"fan", "8", 4, 4}, {"water", "9", 4, 5}, {"mode", "27", 4, 23}, {"tank", "10", 4, 6}}
var readinessCustom = []readinessSelector{{"customized", "30", 4, 26}}
var readinessPaused = []readinessSelector{{"cleaning_paused", "21", 4, 17}}
var readinessState = []readinessSelector{{"state", "0", 2, 1}, {"error", "1", 2, 2}, {"battery_percent", "2", 3, 1}, {"charging_status", "3", 3, 2}, {"status", "5", 4, 1}, {"task_status", "11", 4, 7}}

func qualityValue(quality string) ReadinessValue { return ReadinessValue{Quality: quality} }
func observedValue(n int) ReadinessValue         { return ReadinessValue{Quality: "observed", Value: &n} }
func readinessValues(selectors []readinessSelector, quality string) map[string]ReadinessValue {
	out := map[string]ReadinessValue{}
	for _, s := range selectors {
		out[s.name] = qualityValue(quality)
	}
	return out
}
func readinessParams(selectors []readinessSelector) []map[string]any {
	out := make([]map[string]any, 0, len(selectors))
	for _, s := range selectors {
		out = append(out, map[string]any{"did": s.did, "siid": s.siid, "piid": s.piid})
	}
	return out
}

// Unsupported is never inferred from a generic error code, missing row, or null.
func parseReadinessValues(raw []byte, selectors []readinessSelector) (map[string]ReadinessValue, string) {
	invalid := readinessValues(selectors, "invalid")
	var env struct {
		Data struct {
			Result []bridge.Property `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.Result == nil || len(env.Data.Result) > len(selectors) {
		return invalid, "response_invalid"
	}
	out := readinessValues(selectors, "missing")
	seen := map[string]bool{}
	for _, p := range env.Data.Result {
		name := ""
		for _, s := range selectors {
			if p.SIID == s.siid && p.PIID == s.piid {
				name = s.name
				break
			}
		}
		if name == "" || seen[name] {
			return invalid, "response_invalid"
		}
		seen[name] = true
		if p.Code == nil || *p.Code != 0 {
			out[name] = qualityValue("invalid")
			continue
		}
		value := bytes.TrimSpace(p.Value)
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			continue
		}
		var n int
		if json.Unmarshal(value, &n) != nil || n < -2147483648 || n > 2147483647 {
			out[name] = qualityValue("invalid")
			continue
		}
		out[name] = observedValue(n)
	}
	return out, "ok"
}

// Every request is a fixed get_properties selector and all failures are soft:
// returning a session fault here could cause an unsafe replay of map refresh.
func (c *Client) readReadinessValues(ctx context.Context, device, domain string, selectors []readinessSelector, typedStatus bool) (map[string]ReadinessValue, string, int64) {
	at := c.now().UnixMilli()
	path, err := rpcPath(domain)
	if err != nil {
		return readinessValues(selectors, "missing"), "request_unavailable", at
	}
	id, err := mapRequestID()
	if err != nil {
		return readinessValues(selectors, "missing"), "request_unavailable", at
	}
	params := readinessParams(selectors)
	if typedStatus {
		params = bridge.StatusReadParams()
	}
	raw, err := c.postBounded(ctx, path, map[string]any{"did": device, "id": id, "data": map[string]any{"did": device, "id": id, "method": "get_properties", "params": params}}, 64<<10)
	if err != nil {
		return readinessValues(selectors, "missing"), "request_unavailable", at
	}
	if typedStatus {
		if _, err = bridge.ParseStatus(device, c.now().UTC().Format(time.RFC3339), raw); err != nil {
			return readinessValues(selectors, "invalid"), "response_invalid", at
		}
	}
	values, outcome := parseReadinessValues(raw, selectors)
	return values, outcome, at
}
func plannerInt(v ReadinessValue) cleanplan.IntObservation {
	q := cleanplan.Invalid
	switch v.Quality {
	case "observed":
		q = cleanplan.Observed
	case "missing":
		q = cleanplan.Missing
	case "unsupported":
		q = cleanplan.Unsupported
	}
	n := 0
	if v.Value != nil {
		n = *v.Value
	}
	return cleanplan.IntObservation{Quality: q, Value: n}
}
func plannerPaused(v ReadinessValue) cleanplan.BoolObservation {
	n := plannerInt(v)
	if n.Quality == cleanplan.Observed && n.Value != 0 && n.Value != 1 {
		n.Quality = cleanplan.Invalid
	}
	return cleanplan.BoolObservation{Quality: n.Quality, Value: n.Value == 1}
}
func (c *Client) readCleaningReadiness(ctx context.Context, device string, record deviceRecord, catalogAt int64, matches int, result *MapMetadata) {
	if !result.ActiveMapVerified || result.currentProof == nil || len(result.Maps) != 1 {
		return
	}
	r := &CleaningReadiness{Schema: 1, Outcome: "read_unavailable", ObservedAt: c.now().UTC().Format(time.RFC3339), Drafts: []ReadinessDraft{}}
	result.CleaningReadiness = r
	result.controlDrafts = privateDrafts{}
	state, so, stateAt := c.readReadinessValues(ctx, device, record.Domain, readinessState, true)
	stateReadAt := stateAt
	global, go_, globalAt := c.readReadinessValues(ctx, device, record.Domain, readinessGlobals, false)
	custom, co, customAt := c.readReadinessValues(ctx, device, record.Domain, readinessCustom, false)
	paused, po, pausedAt := c.readReadinessValues(ctx, device, record.Domain, readinessPaused, false)
	r.Reads = ReadinessReads{so, go_, co, po}
	r.State = ReadinessState{state["state"], state["status"], state["task_status"], state["error"], state["battery_percent"], state["charging_status"], paused["cleaning_paused"], qualityValue("unsupported")}
	r.Settings = ReadinessSettings{global["fan"], global["water"], global["mode"], global["tank"], custom["customized"]}
	// The pinned exact model declares no drainage facility; no drain RPC exists.
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		r.Outcome = "snapshot_unavailable"
		return
	}
	binding := cleanplan.Binding{AccountID: idString(record.UID), DeviceID: device, Model: record.Model}
	snapshotID := hex.EncodeToString(random[:])
	snapshot := func(at int64) cleanplan.Snapshot {
		return cleanplan.Snapshot{Binding: binding, ID: snapshotID, ReadAtMillis: at}
	}
	frame := result.currentProof.frame
	statusQuality, markerQuality := cleanplan.Missing, cleanplan.Invalid
	if frame.Format.SavedMapStatusPresent {
		statusQuality = cleanplan.Observed
	}
	if frame.Format.MarkersValid {
		markerQuality = cleanplan.Observed
	}
	source := cleanplan.RefreshResponse
	if result.Diagnostics.CurrentMap.Freshness == "cached_reference_changed" {
		source = cleanplan.AdvancedCachedRead
	}
	settingsAt := globalAt
	if customAt < settingsAt {
		settingsAt = customAt
	}
	if pausedAt < stateAt {
		stateAt = pausedAt
	}
	base := cleanplan.Observations{NowMillis: c.now().UnixMilli(), Catalog: cleanplan.CatalogEvidence{Snapshot: snapshot(catalogAt), Matches: matches}, Map: cleanplan.MapEvidence{Snapshot: snapshot(result.currentProof.readAt), Source: source, FrameType: 73, Version: frame.Version, CurrentMapID: frame.MapID, CurrentSavedMapID: frame.SavedMapID, SelectedSavedMapID: result.SelectedSavedMapID, SavedStatus: cleanplan.IntObservation{Quality: cleanplan.Observed, Value: int(frame.Format.SavedMapStatus)}, Markers: cleanplan.Observed, Temporary: frame.Format.Temporary || frame.Format.TemporaryFromNested, New: frame.Format.New, Restored: frame.Format.Restored, Empty: frame.Format.Empty}, State: cleanplan.StateEvidence{Snapshot: snapshot(stateAt), State: plannerInt(r.State.State), Status: plannerInt(r.State.Status), Task: plannerInt(r.State.Task), Error: plannerInt(r.State.Error), Battery: plannerInt(r.State.Battery), Charging: plannerInt(r.State.Charging), CleaningPaused: plannerPaused(r.State.Paused), Drainage: cleanplan.IntObservation{Quality: cleanplan.Unsupported}}, Settings: cleanplan.SettingsEvidence{Snapshot: snapshot(settingsAt), Encoding: cleanplan.PlainWaterVolume, Mode: plannerInt(r.Settings.Mode), Tank: plannerInt(r.Settings.Tank), Fan: plannerInt(r.Settings.Fan), Water: plannerInt(r.Settings.Water), Customized: plannerInt(r.Settings.Customized)}}
	base.Map.SavedStatus.Quality, base.Map.Markers = statusQuality, markerQuality
	for _, at := range []int64{catalogAt, result.currentProof.readAt, stateReadAt, globalAt, customAt, pausedAt} {
		if at <= 0 || at > base.NowMillis || base.NowMillis-at > cleanplan.MaxAgeMillis {
			r.Outcome = "cleanplan_snapshot_stale"
			return
		}
	}
	// Saved-frame Rooms are already filtered to visible, pixel-mapped segments.
	// The proven current saved-map link establishes current membership; current
	// outer pixels are not substituted for the linked saved-map membership.
	for _, room := range result.Maps[0].Items {
		in := base
		in.Room = cleanplan.RoomEvidence{Snapshot: snapshot(result.currentProof.readAt), SelectedSavedMapID: result.SelectedSavedMapID, SegmentID: room.SegmentID, Matches: 1, InCurrent: frame.SavedMapID == result.SelectedSavedMapID, InSelectedSaved: true, Visible: true, Mapped: true}
		draft, err := cleanplan.Build(in)
		if err != nil {
			r.Drafts = []ReadinessDraft{}
			r.Outcome = string(cleanplan.ErrorCode(err))
			return
		}
		preview, err := draft.Preview()
		if err != nil {
			r.Drafts = []ReadinessDraft{}
			r.Outcome = string(cleanplan.ErrorCode(err))
			return
		}
		r.Drafts = append(r.Drafts, ReadinessDraft{SegmentID: room.SegmentID, Preview: preview})
		result.controlDrafts[room.SegmentID] = draft
	}
	if len(r.Drafts) > 0 {
		r.Outcome = "draft_available"
	} else {
		r.Outcome = "cleanplan_room_unproven"
	}
}
