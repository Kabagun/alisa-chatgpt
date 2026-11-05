package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/savedmap"
)

const requestFullMapJSON = `{"req_type":1,"frame_type":"I","force_type":1}`

type privateCurrentCandidate struct{ inline, reference string }

func (privateCurrentCandidate) String() string               { return "[current map candidate redacted]" }
func (privateCurrentCandidate) GoString() string             { return "[current map candidate redacted]" }
func (c privateCurrentCandidate) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(c.String())) }

type CurrentMapDiagnostics struct {
	Outcome              string                      `json:"outcome"`
	TargetProfileMatched bool                        `json:"target_profile_matched"`
	ActionRequested      bool                        `json:"action_requested"`
	ActionAcknowledged   bool                        `json:"action_acknowledged"`
	ActionRequests       int                         `json:"action_requests"`
	InlineOutputs        int                         `json:"inline_outputs"`
	ReferenceOutputs     int                         `json:"reference_outputs"`
	OtherOutputs         int                         `json:"other_outputs"`
	PassivePolls         int                         `json:"passive_polls"`
	Freshness            string                      `json:"freshness"`
	FullFrame            bool                        `json:"full_frame"`
	SavedStatusPresent   bool                        `json:"saved_status_present"`
	SavedStatus          int                         `json:"saved_status"`
	MarkersValid         bool                        `json:"markers_valid"`
	Temporary            bool                        `json:"temporary"`
	NestedTemporary      bool                        `json:"nested_temporary"`
	New                  bool                        `json:"new"`
	Restored             bool                        `json:"restored"`
	Empty                bool                        `json:"empty"`
	LinkagePresent       bool                        `json:"linkage_present"`
	LinkageMatches       bool                        `json:"linkage_matches"`
	DescriptorUnchanged  bool                        `json:"descriptor_unchanged"`
	ActiveVerified       bool                        `json:"active_verified"`
	LastError            string                      `json:"last_error"`
	DuplicateCategory    string                      `json:"duplicate_category,omitempty"`
	Selection            *CurrentSelectionDiagnostic `json:"selection,omitempty"`
}

// Optional raw outer-frame observation only. No segment IDs, inferred target,
// names or renderer-carried selection are exposed by this diagnostic.
type CurrentSelectionDiagnostic struct {
	Present bool `json:"present"`
	Valid   bool `json:"valid"`
	Count   int  `json:"count"`
}

func currentDuplicateCategory(err error) string {
	var typed *savedmap.Error
	if !errors.As(err, &typed) || typed.Code != savedmap.CodeDuplicateField {
		return ""
	}
	switch value := string(typed.DuplicateCategory); value {
	case "list", "frame_metadata", "frame_rooms", "frame_version", "frame_binding", "segment_alias":
		return value
	default:
		return ""
	}
}

func mapRequestID() (int, error) {
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return 0, sessionError("request_id_unavailable")
	}
	return int(binary.LittleEndian.Uint16(random[:])%100) + 2, nil
}
func fixedFullMapRPC(device string, id int) map[string]any {
	return map[string]any{"did": device, "id": id, "data": map[string]any{"did": device, "id": id, "method": "action", "params": map[string]any{"did": device, "siid": 6, "aiid": 1, "in": []any{map[string]any{"piid": 2, "value": requestFullMapJSON}}}}}
}
func actionIdentity(raw json.RawMessage, device string) bool {
	return len(raw) == 0 || idString(raw) == device
}
func actionNumber(raw json.RawMessage, want int) bool {
	if len(raw) == 0 {
		return true
	}
	var value int
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &value) == nil && value == want
}

func parseFullMapAction(raw []byte, device string, id int, d *CurrentMapDiagnostics) (privateCurrentCandidate, bool) {
	var env struct {
		DID  json.RawMessage `json:"did"`
		Data struct {
			ID     json.RawMessage `json:"id"`
			DID    json.RawMessage `json:"did"`
			Result *struct {
				Code *int            `json:"code"`
				DID  json.RawMessage `json:"did"`
				SIID json.RawMessage `json:"siid"`
				AIID json.RawMessage `json:"aiid"`
				ID   json.RawMessage `json:"id"`
				Out  []struct {
					PIID  *int            `json:"piid"`
					SIID  json.RawMessage `json:"siid"`
					Code  *int            `json:"code"`
					DID   json.RawMessage `json:"did"`
					Value json.RawMessage `json:"value"`
				} `json:"out"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.Result == nil {
		d.Outcome = "action_response_invalid"
		return privateCurrentCandidate{}, false
	}
	r := env.Data.Result
	if r.Code == nil || !actionIdentity(r.DID, device) || !actionIdentity(env.DID, device) || !actionIdentity(env.Data.DID, device) || !actionNumber(r.SIID, 6) || !actionNumber(r.AIID, 1) || !actionNumber(r.ID, id) || !actionNumber(env.Data.ID, id) {
		d.Outcome = "action_response_invalid"
		return privateCurrentCandidate{}, false
	}
	if *r.Code != 0 {
		d.Outcome = "action_rejected"
		return privateCurrentCandidate{}, false
	}
	if r.Out == nil || len(r.Out) > 8 {
		d.Outcome = "action_response_invalid"
		return privateCurrentCandidate{}, false
	}
	d.ActionAcknowledged = true
	var candidate privateCurrentCandidate
	seen := map[int]bool{}
	for _, output := range r.Out {
		if output.PIID == nil || seen[*output.PIID] || !actionNumber(output.SIID, 6) || !actionIdentity(output.DID, device) {
			d.Outcome = "action_response_invalid"
			return privateCurrentCandidate{}, false
		}
		seen[*output.PIID] = true
		if output.Code != nil && *output.Code != 0 {
			d.Outcome = "action_output_failed"
			return privateCurrentCandidate{}, false
		}
		switch *output.PIID {
		case 1, 3:
			value := bytes.TrimSpace(output.Value)
			if bytes.Equal(value, []byte("null")) {
				continue
			}
			var text string
			if json.Unmarshal(value, &text) != nil {
				d.Outcome = "action_response_invalid"
				return privateCurrentCandidate{}, false
			}
			limit := 2 << 20
			if *output.PIID == 3 {
				limit = 8192
			}
			if text != "" && !privateMapText(text, limit) {
				d.Outcome = "action_response_invalid"
				return privateCurrentCandidate{}, false
			}
			if text == "" {
				continue
			}
			if *output.PIID == 1 {
				candidate.inline = text
				d.InlineOutputs++
			} else {
				candidate.reference = text
				d.ReferenceOutputs++
			}
		case 2:
			var value string
			if bytes.Equal(bytes.TrimSpace(output.Value), []byte("null")) || json.Unmarshal(output.Value, &value) != nil || len(value) > 4096 {
				d.Outcome = "action_response_invalid"
				return privateCurrentCandidate{}, false
			}
			d.OtherOutputs++
		case 5:
			var value int64
			if bytes.Equal(bytes.TrimSpace(output.Value), []byte("null")) || json.Unmarshal(output.Value, &value) != nil {
				d.Outcome = "action_response_invalid"
				return privateCurrentCandidate{}, false
			}
			d.OtherOutputs++
		case 13:
			d.Outcome = "legacy_unsupported"
			return privateCurrentCandidate{}, false
		default:
			d.Outcome = "output_unsupported"
			return privateCurrentCandidate{}, false
		}
	}
	return candidate, true
}

func waitMapPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c *Client) pollCurrentReference(ctx context.Context, device, domain string) (privateCurrentCandidate, string) {
	path, err := rpcPath(domain)
	if err != nil {
		return privateCurrentCandidate{}, "poll_unavailable"
	}
	id, err := mapRequestID()
	if err != nil {
		return privateCurrentCandidate{}, "poll_unavailable"
	}
	params := map[string]any{"did": device, "id": id, "data": map[string]any{"did": device, "id": id, "method": "get_properties", "params": []any{map[string]any{"did": "78", "siid": 6, "piid": 3}}}}
	raw, err := c.postBounded(ctx, path, params, 64<<10)
	if err != nil {
		return privateCurrentCandidate{}, "poll_unavailable"
	}
	var env struct {
		Data struct {
			Result []bridge.Property `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.Result == nil || len(env.Data.Result) > 1 {
		return privateCurrentCandidate{}, "poll_response_invalid"
	}
	if len(env.Data.Result) == 0 {
		return privateCurrentCandidate{}, ""
	}
	p := env.Data.Result[0]
	if p.SIID != 6 || p.PIID != 3 {
		return privateCurrentCandidate{}, "poll_response_invalid"
	}
	if p.Code == nil || *p.Code != 0 {
		return privateCurrentCandidate{}, ""
	}
	var value string
	if json.Unmarshal(p.Value, &value) != nil || value != "" && !privateMapText(value, 8192) {
		return privateCurrentCandidate{}, "poll_response_invalid"
	}
	return privateCurrentCandidate{reference: value}, ""
}

func (c *Client) currentBlob(ctx context.Context, locator privateMapURL, result *MapMetadata, d *CurrentMapDiagnostics) ([]byte, bool) {
	s := result.Diagnostics.SavedMaps
	outcome, last := s.Outcome, s.LastError
	body, ok := c.fetchSavedBlob(ctx, locator, 2<<20, result.Diagnostics)
	if !ok {
		d.LastError = s.LastError
		d.Outcome = "fetch_failed"
		if s.Outcome == "budget_exhausted" || s.Outcome == "read_deadline" {
			d.Outcome = s.Outcome
		}
	}
	s.Outcome = outcome
	s.LastError = last
	return body, ok
}
func (c *Client) decodeCurrent(ctx context.Context, device, model string, candidate privateCurrentCandidate, result *MapMetadata, d *CurrentMapDiagnostics) (savedmap.Frame, bool) {
	iv := []byte(c.mapProfile.ModelIVs[model])
	defer clear(iv)
	hint := result.Diagnostics.SavedMaps.VersionHint
	encoded, key := candidate.inline, ""
	if candidate.reference != "" {
		parts := strings.Split(candidate.reference, ",")
		if len(parts) > 2 || !privateMapText(parts[0], 2048) {
			d.Outcome = "reference_invalid"
			return savedmap.Frame{}, false
		}
		if len(parts) == 2 {
			key = parts[1]
			if !privateMapText(key, 1024) {
				d.Outcome = "reference_invalid"
				return savedmap.Frame{}, false
			}
		}
		locator, metadata, err := c.locateMapObject(ctx, device, model, privateMapReference{name: parts[0]})
		if err != nil || metadata.Outcome != "metadata_available" {
			d.Outcome = "locator_unavailable"
			return savedmap.Frame{}, false
		}
		body, ok := c.currentBlob(ctx, locator, result, d)
		if !ok {
			return savedmap.Frame{}, false
		}
		encoded = string(body)
		clear(body)
	}
	frame, err := savedmap.DecodeFrame(encoded, key, iv, hint)
	if err != nil {
		d.Outcome = "decode_failed"
		d.LastError = safeMapReadError(err)
		d.DuplicateCategory = currentDuplicateCategory(err)
		return savedmap.Frame{}, false
	}
	if candidate.reference != "" && candidate.inline != "" {
		inline, err := savedmap.DecodeFrame(candidate.inline, "", iv, hint)
		if err != nil || inline.MapID != frame.MapID || inline.FrameID != frame.FrameID || inline.SavedMapID != frame.SavedMapID || inline.Format != frame.Format {
			d.Outcome = "candidate_conflict"
			if err != nil {
				d.LastError = safeMapReadError(err)
				d.DuplicateCategory = currentDuplicateCategory(err)
			}
			return savedmap.Frame{}, false
		}
	}
	return frame, true
}

func (c *Client) refreshCurrentMap(ctx context.Context, device string, record deviceRecord, result *MapMetadata) {
	d := &CurrentMapDiagnostics{Outcome: "not_attempted", Freshness: "none", SavedStatus: -1}
	result.Diagnostics.CurrentMap = d
	if record.Model != "dreame.vacuum.r2422" {
		d.Outcome = "model_unsupported"
		return
	}
	s := result.Diagnostics.SavedMaps
	if s == nil || !s.SelectedMatched || len(result.Maps) != 1 || result.SelectedSavedMapID == "" {
		d.Outcome = "saved_map_unavailable"
		return
	}
	if len(c.mapProfile.ModelIVs[record.Model]) != 16 || len(c.mapProfile.StorageHosts) == 0 || !result.Diagnostics.descriptorPresent || s.VersionHint < 0 || s.VersionHint > 3 {
		d.Outcome = "profile_unavailable"
		return
	}
	d.TargetProfileMatched = true
	if ctx.Err() != nil {
		d.Outcome = "read_deadline"
		return
	}
	path, err := rpcPath(record.Domain)
	if err != nil {
		d.Outcome = "action_unavailable"
		return
	}
	id, err := mapRequestID()
	if err != nil {
		d.Outcome = "action_unavailable"
		return
	}
	d.ActionRequested = true
	d.ActionRequests = 1
	readAt := c.now().UnixMilli()
	// Never propagate a post-action auth fault: provider refresh must not replay
	// this ambiguous action. Later failures return diagnostic data only.
	raw, err := c.postBounded(ctx, path, fixedFullMapRPC(device, id), 2<<20)
	if err != nil {
		d.Outcome = "action_unavailable"
		return
	}
	candidate, ok := parseFullMapAction(raw, device, id, d)
	if !ok {
		return
	}
	if candidate.inline != "" || candidate.reference != "" {
		d.Freshness = "action_response"
	} else {
		pause := c.mapPollWait
		if pause == nil {
			pause = waitMapPoll
		}
		for i := 0; i < 3; i++ {
			if pause(ctx, 2*time.Second) != nil {
				d.Outcome = "read_deadline"
				return
			}
			d.PassivePolls++
			polled, problem := c.pollCurrentReference(ctx, device, record.Domain)
			if problem != "" {
				d.Outcome = problem
				return
			}
			if polled.reference == "" {
				continue
			}
			if !result.Diagnostics.currentBaselineKnown {
				d.Freshness = "cached_unverified"
				d.Outcome = "freshness_unverified"
				return
			}
			if polled.reference == result.Diagnostics.currentBaseline.reference {
				d.Freshness = "cached_unchanged"
				continue
			}
			d.Freshness = "cached_reference_changed"
			candidate = polled
			break
		}
		if candidate.reference == "" {
			d.Outcome = "map_unavailable"
			if d.Freshness == "cached_unchanged" {
				d.Outcome = "stale_reference"
			}
			return
		}
	}
	frame, ok := c.decodeCurrent(ctx, device, record.Model, candidate, result, d)
	if !ok {
		return
	}
	d.FullFrame = true
	d.Selection = &CurrentSelectionDiagnostic{Present: frame.Selection.Present, Valid: frame.Selection.Valid, Count: len(frame.Selection.SegmentIDs)}
	f := frame.Format
	d.SavedStatusPresent = f.SavedMapStatusPresent
	d.SavedStatus = int(f.SavedMapStatus)
	d.MarkersValid = f.MarkersValid
	d.Temporary = f.Temporary
	d.NestedTemporary = f.TemporaryFromNested
	d.New = f.New
	d.Restored = f.Restored
	d.Empty = f.Empty
	d.LinkagePresent = frame.SavedMapID != ""
	d.LinkageMatches = d.LinkagePresent && frame.SavedMapID == result.SelectedSavedMapID
	if !d.MarkersValid {
		d.Outcome = "markers_invalid"
		return
	}
	if !d.SavedStatusPresent || d.SavedStatus != 2 || d.Temporary || d.NestedTemporary || d.New || d.Restored || d.Empty {
		d.Outcome = "format_unverified"
		return
	}
	if !d.LinkagePresent {
		d.Outcome = "linkage_missing"
		return
	}
	if !d.LinkageMatches {
		d.Outcome = "linkage_mismatch"
		return
	}
	recheck := newMapDiagnostics()
	_, err = c.readMapDiagnosticProperties(ctx, device, record.Domain, recheck)
	if err != nil || recheck.Properties.Outcome != "ok" || !recheck.descriptorPresent {
		d.Outcome = "descriptor_unavailable"
		return
	}
	if recheck.descriptorHash != result.Diagnostics.descriptorHash {
		d.Outcome = "descriptor_changed"
		return
	}
	d.DescriptorUnchanged = true
	d.ActiveVerified = true
	d.Outcome = "active_map_verified"
	result.ActiveMapVerified = true
	result.Maps[0].Active = true
	result.currentProof = &privateCurrentProof{frame: frame, readAt: readAt}
}
