package cloud

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"alisa-dreame-readonly/internal/bridge"
)

type ControlRequest struct {
	Schema int      `json:"schema"`
	Op     string   `json:"op"`
	Device string   `json:"device_id"`
	Map    string   `json:"map_id"`
	Rooms  []string `json:"room_ids"`
}

func DecodeControlRequest(r io.Reader) (ControlRequest, error) {
	var out ControlRequest
	raw, err := io.ReadAll(io.LimitReader(r, bridge.MaxRequest+1))
	if err != nil || len(raw) > bridge.MaxRequest {
		return out, sessionError("invalid_control_request")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return out, sessionError("invalid_control_request")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || (key != "schema" && key != "op" && key != "device_id" && key != "map_id" && key != "room_ids") {
			return out, sessionError("invalid_control_request")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return out, sessionError("invalid_control_request")
		}
	}
	if _, err = d.Token(); err != nil || d.Decode(new(any)) != io.EOF || len(seen) != 5 || json.Unmarshal(raw, &out) != nil || !out.valid() {
		return ControlRequest{}, sessionError("invalid_control_request")
	}
	return out, nil
}

type ControlResult struct {
	Schema                int      `json:"schema"`
	Device                string   `json:"device_id"`
	Map                   string   `json:"map_id"`
	Rooms                 []string `json:"room_ids"`
	ObservedAt            string   `json:"observed_at"`
	Outcome               string   `json:"outcome"`
	Reason                string   `json:"reason"`
	Attempted             bool     `json:"attempted"`
	Acknowledgement       string   `json:"acknowledgement"`
	SegmentTaskRunning    bool     `json:"segment_task_running"`
	SelectedRoomConfirmed bool     `json:"selected_room_confirmed"`
	MapBindingUnchanged   bool     `json:"map_binding_unchanged"`
	StatusReads           int      `json:"status_reads"`
	MapRefreshes          int      `json:"map_refreshes"`
	AutomaticRetry        bool     `json:"automatic_retry"`
}

func (r ControlRequest) valid() bool {
	m, e := strconv.ParseInt(r.Map, 10, 16)
	if e != nil || strconv.FormatInt(m, 10) != r.Map || len(r.Rooms) != 1 {
		return false
	}
	n, e := strconv.Atoi(r.Rooms[0])
	return e == nil && n >= 1 && n <= 63 && strconv.Itoa(n) == r.Rooms[0] && r.Schema == 1 && r.Op == "clean_rooms" && idString(json.RawMessage(strconv.Quote(r.Device))) == r.Device && r.Device != ""
}

// ExecuteControl is separate from the normal read/fixture protocol. A caller
// must supply only a protected session and a trusted profile; public MCP scope
// authorization is enforced by the backend before this helper is invoked.
func (c *Client) ExecuteControl(parent context.Context, req ControlRequest) bridge.Response {
	r := bridge.Response{Schema: 1, Mode: "dreame_control", ReadOnly: false}
	if !req.valid() {
		r.Error = &bridge.Fault{Code: "invalid_control_request"}
		return r
	}
	ctx, cancel := context.WithTimeout(parent, 28*time.Second)
	defer cancel()
	out := ControlResult{Schema: 1, Device: req.Device, Map: req.Map, Rooms: req.Rooms, ObservedAt: c.now().UTC().Format(time.RFC3339), Outcome: "not_sent", Reason: "preview_required", Acknowledgement: "not_attempted"}
	finish := func() bridge.Response {
		out.ObservedAt = c.now().UTC().Format(time.RFC3339)
		r.OK = true
		r.Data = out
		return r
	}
	if c.controlStore == nil {
		return finish()
	}
	unlock, err := c.controlStore.lock(req.Device)
	if err != nil {
		out.Reason = "control_busy"
		return finish()
	}
	defer unlock()
	expected, err := c.controlStore.load(req.Device)
	now := c.now().UnixMilli()
	fp, e := hex.DecodeString(expected.Fingerprint)
	if err != nil || expected.Schema != 1 || expected.Device != req.Device || expected.Map != req.Map || len(fp) != 32 || e != nil || hex.EncodeToString(fp) != expected.Fingerprint || expected.CreatedAt <= 0 || now < expected.CreatedAt || now-expected.CreatedAt > previewTTL.Milliseconds() || len(expected.Drafts) == 0 || len(expected.Drafts) > 63 {
		return finish()
	}
	last, err := c.controlStore.lastAttempt(req.Device)
	if err != nil || last < 0 {
		out.Reason = "control_state_unavailable"
		return finish()
	}
	if last > 0 && (now < last || now-last < dispatchCooldown.Milliseconds()) {
		out.Reason = "cooldown"
		return finish()
	}
	// A fresh validation must never overwrite/promote the reviewed snapshot.
	c.controlPreparing = true
	fresh, err := c.execute(ctx, bridge.Request{Schema: 1, Op: "rooms", Device: req.Device})
	c.controlPreparing = false
	if err != nil {
		out.Reason = "fresh_read_unavailable"
		return finish()
	}
	m, ok := fresh.(MapMetadata)
	if !ok || !m.ActiveMapVerified || m.currentProof == nil || m.CleaningReadiness == nil || m.CleaningReadiness.Outcome != "draft_available" || m.SelectedSavedMapID != req.Map || m.ownedRecord.Model != "dreame.vacuum.r2422" {
		out.Reason = "not_eligible"
		return finish()
	}
	if previewFingerprint(req.Device, &m) != expected.Fingerprint {
		out.Reason = "plan_changed"
		return finish()
	}
	segment, _ := strconv.Atoi(req.Rooms[0])
	draft := m.controlDrafts[segment]
	if draft == nil {
		out.Reason = "room_unavailable"
		return finish()
	}
	preview, err := draft.Preview()
	if err != nil {
		out.Reason = "not_eligible"
		return finish()
	}
	matched := 0
	for _, d := range expected.Drafts {
		if d.SegmentID == segment && d.Preview == preview {
			matched++
		}
	}
	if matched != 1 {
		out.Reason = "plan_changed"
		return finish()
	}
	logical, err := draft.LogicalPayloadJSON(c.now().UnixMilli())
	if err != nil {
		out.Reason = "not_eligible"
		return finish()
	}
	defer clear(logical)
	var payload struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(logical, &payload) != nil || payload.Method != "action" {
		out.Reason = "not_eligible"
		return finish()
	}
	path, err := rpcPath(m.ownedRecord.Domain)
	if err != nil {
		out.Reason = "not_eligible"
		return finish()
	}
	id, err := mapRequestID()
	if err != nil {
		out.Reason = "not_eligible"
		return finish()
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 8*time.Second {
		out.Reason = "read_deadline"
		return finish()
	}
	if err = c.controlStore.markAttempt(req.Device, c.now().UnixMilli()); err != nil {
		out.Reason = "control_state_unavailable"
		return finish()
	}
	// From here on every error is a non-retryable normal unknown result. The
	// protected cooldown marker survives even a timeout, kill or malformed reply.
	out.Attempted = true
	out.Outcome = "unknown"
	out.Reason = "reply_unconfirmed"
	out.Acknowledgement = "unknown"
	raw, err := c.postBounded(ctx, path, map[string]any{"did": req.Device, "id": id, "data": map[string]any{"did": req.Device, "id": id, "method": payload.Method, "params": payload.Params}}, 64<<10)
	if err == nil {
		out.Acknowledgement = parseStartAcknowledgement(raw, req.Device, id)
	}
	if out.Acknowledgement == "rejected" {
		out.Outcome = "rejected"
		out.Reason = "device_rejected"
		return finish()
	}
	if out.Acknowledgement == "accepted" {
		out.Outcome = "accepted"
		out.Reason = "start_unconfirmed"
	}
	c.confirmRoomStart(ctx, req, segment, preview.Mode, &m, &out)
	return finish()
}
func parseStartAcknowledgement(raw []byte, device string, id int) string {
	if !uniqueControlReply(raw) {
		return "unknown"
	}
	var env struct {
		DID  json.RawMessage `json:"did"`
		Data struct {
			DID    json.RawMessage `json:"did"`
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || !actionIdentity(env.DID, device) || !actionIdentity(env.Data.DID, device) || !actionNumber(env.Data.ID, id) {
		return "unknown"
	}
	result := bytes.TrimSpace(env.Data.Result)
	if len(result) > 0 && result[0] == '[' {
		var rows []json.RawMessage
		if json.Unmarshal(result, &rows) != nil || len(rows) != 1 {
			return "unknown"
		}
		result = rows[0]
	}
	var r struct {
		Code *int            `json:"code"`
		DID  json.RawMessage `json:"did"`
		ID   json.RawMessage `json:"id"`
		SIID json.RawMessage `json:"siid"`
		AIID json.RawMessage `json:"aiid"`
	}
	if json.Unmarshal(result, &r) != nil || r.Code == nil || !actionIdentity(r.DID, device) || !actionNumber(r.ID, id) || !actionNumber(r.SIID, 4) || !actionNumber(r.AIID, 1) {
		return "unknown"
	}
	if *r.Code != 0 {
		return "rejected"
	}
	return "accepted"
}
func observedEquals(v ReadinessValue, n int) bool {
	return v.Quality == "observed" && v.Value != nil && *v.Value == n
}

// Duplicate acknowledgement fields cannot become acceptance via last-wins.
func uniqueControlReply(raw []byte) bool {
	if len(raw) > 64<<10 {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(int) bool
	walk = func(depth int) bool {
		nodes++
		if depth > 16 || nodes > 2048 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		delim, compound := t.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				name, ok := key.(string)
				if e != nil || !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			end, e := d.Token()
			return e == nil && end == json.Delim(']')
		}
		return false
	}
	return walk(0) && d.Decode(new(any)) == io.EOF
}
func (c *Client) confirmRoomStart(ctx context.Context, req ControlRequest, segment, mode int, m *MapMetadata, out *ControlResult) {
	pause := c.mapPollWait
	if pause == nil {
		pause = waitMapPoll
	}
	if pause(ctx, 2*time.Second) != nil {
		return
	}
	state, so, _ := c.readReadinessValues(ctx, req.Device, m.ownedRecord.Domain, readinessState, true)
	paused, po, _ := c.readReadinessValues(ctx, req.Device, m.ownedRecord.Domain, readinessPaused, false)
	out.StatusReads = 1
	working := []int{1, 7, 12}
	if so != "ok" || po != "ok" || mode < 0 || mode > 2 || !observedEquals(state["status"], 18) || !observedEquals(state["task_status"], 3) || !observedEquals(state["state"], working[mode]) || !observedEquals(state["error"], 0) || !observedEquals(paused["cleaning_paused"], 0) {
		return
	}
	out.SegmentTaskRunning = true
	out.Outcome = "segment_task_running_room_unconfirmed"
	out.Reason = "selected_room_unconfirmed"
	if ctx.Err() != nil {
		return
	}
	// One optional full-I refresh is read-only map solicitation, never another
	// physical cleaning/movement action. Its blob counters share the same budget.
	out.MapRefreshes = 1
	m.currentProof = nil
	m.ActiveMapVerified = false
	c.refreshCurrentMap(ctx, req.Device, m.ownedRecord, m)
	if !m.ActiveMapVerified || m.currentProof == nil || m.SelectedSavedMapID != req.Map {
		return
	}
	out.MapBindingUnchanged = true
	selection := m.currentProof.frame.Selection
	if !selection.Present || !selection.Valid || len(selection.SegmentIDs) != 1 || selection.SegmentIDs[0] != segment {
		return
	}
	out.SelectedRoomConfirmed = true
	if out.Acknowledgement == "accepted" {
		out.Outcome = "started"
		out.Reason = "device_reported_running"
	} else {
		out.Outcome = "observed_running_reply_unknown"
		out.Reason = "device_reported_running_reply_unknown"
	}
}
