package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"alisa-dreame-readonly/internal/bridge"
)

// MapDiagnostics has safe counts/classes and an optional private hostname; no
// raw frames, object names, signed URLs, model keys, map IDs or room names.
type MapDiagnostics struct {
	Schema               int                      `json:"schema"`
	Reason               string                   `json:"reason"`
	History              MapHistoryDiagnostics    `json:"history"`
	Properties           MapPropertyDiagnostics   `json:"properties"`
	SavedObject          SavedObjectDiagnostics   `json:"saved_object"`
	SavedMaps            *SavedMapReadDiagnostics `json:"saved_maps,omitempty"`
	CurrentMap           *CurrentMapDiagnostics   `json:"current_map,omitempty"`
	mapSavingValue       bool
	mapSavingProbe       string
	mapSavingProbed      bool
	currentBaseline      privateCurrentCandidate
	currentBaselineKnown bool
	descriptorHash       [32]byte
	descriptorPresent    bool
}
type MapValueShapes struct {
	ArrayString int `json:"json_array_string"`
	Array       int `json:"array"`
	Object      int `json:"object"`
	StringOther int `json:"string_other"`
	NullMissing int `json:"null_or_missing"`
	ScalarOther int `json:"scalar_other"`
}
type MapHistoryDiagnostics struct {
	Outcome        string         `json:"outcome"`
	Records        int            `json:"records"`
	ValueShapes    MapValueShapes `json:"value_shapes"`
	Candidates     int            `json:"inline_candidates"`
	Encrypted      int            `json:"encrypted_suffix_frames"`
	Full           int            `json:"full_frames"`
	Incremental    int            `json:"incremental_frames"`
	Other          int            `json:"other_frames"`
	Invalid        int            `json:"invalid_frames"`
	DecodeFailures int            `json:"decode_failures"`
	DecodedMaps    int            `json:"decoded_maps"`
}
type MapPropertyShape struct {
	State     string `json:"state"`
	Shape     string `json:"value_shape"`
	Reference bool   `json:"object_reference_present"`
	MD5       bool   `json:"md5_present"`
}
type MapPropertyDiagnostics struct {
	Outcome    string           `json:"outcome"`
	ObjectName MapPropertyShape `json:"object_name"`
	MapList    MapPropertyShape `json:"map_list"`
}

func unavailableMapProperty() MapPropertyShape {
	return MapPropertyShape{State: "unavailable", Shape: "missing"}
}
func newMapDiagnostics() *MapDiagnostics {
	return &MapDiagnostics{Schema: 2, History: MapHistoryDiagnostics{Outcome: "request_unavailable"}, Properties: MapPropertyDiagnostics{Outcome: "request_unavailable", ObjectName: unavailableMapProperty(), MapList: unavailableMapProperty()}, SavedObject: newSavedObjectDiagnostics()}
}

func mapReadAuthenticationError(err error) bool {
	var fault *bridge.Fault
	return errors.As(err, &fault) && (fault.Code == "session_expired" || fault.Code == "session_reauthentication_required")
}
func mapReadOutcome(err error) string {
	var fault *bridge.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "cloud_response_invalid":
			return "response_invalid"
		case "cloud_response_limit":
			return "response_limit"
		}
	}
	return "request_unavailable"
}

func (c *Client) readMapHistory(ctx context.Context, device string, record deviceRecord, mapID int, result *MapMetadata) error {
	d := &result.Diagnostics.History
	uid := idString(record.UID)
	if uid == "" {
		d.Outcome = "owner_unavailable"
		return nil
	}
	// Exact existing history6.1 read, with no robot map-update action.
	params := map[string]any{"uid": uid, "did": device, "from": int64(1687019188), "limit": 20, "siid": "6", "piid": "1", "region": c.session.ServerRegion, "type": 3}
	raw, err := c.post(ctx, historyPath, params)
	if err != nil {
		if mapReadAuthenticationError(err) {
			return err
		}
		d.Outcome = mapReadOutcome(err)
		return nil
	}
	var env struct {
		Data struct {
			List []struct {
				Value json.RawMessage `json:"value"`
				Val   json.RawMessage `json:"val"`
			} `json:"list"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.List == nil {
		d.Outcome = "response_invalid"
		return nil
	}
	if len(env.Data.List) > 20 {
		d.Outcome = "response_limit"
		return nil
	}
	d.Outcome = "ok"
	d.Records = len(env.Data.List)
	seen := map[int]bool{}
	for _, item := range env.Data.List {
		value := item.Value
		if len(value) == 0 {
			value = item.Val
		}
		encoded, recognized := mapHistoryCandidate(value, &d.ValueShapes)
		if !recognized {
			result.UnsupportedFrames++
			continue
		}
		d.Candidates++
		switch bridge.InspectMapFrameKind(encoded) {
		case "encrypted_suffix":
			d.Encrypted++
			result.UnsupportedFrames++
			continue
		case "incremental":
			d.Incremental++
			result.UnsupportedFrames++
			continue
		case "other":
			d.Other++
			result.UnsupportedFrames++
			continue
		case "invalid":
			d.Invalid++
			result.UnsupportedFrames++
			continue
		case "full":
			d.Full++
		}
		id, err := bridge.InspectMapHeader(encoded)
		if err != nil {
			d.DecodeFailures++
			result.UnsupportedFrames++
			continue
		}
		if mapID != 0 && id != mapID || seen[id] {
			continue
		}
		rooms, err := bridge.ParseRooms(bridge.SavedMap{ID: id, Version: 0, Data: encoded})
		if err != nil {
			d.DecodeFailures++
			result.UnsupportedFrames++
			continue
		}
		rooms.DeviceID = device
		seen[id] = true
		result.Maps = append(result.Maps, rooms)
		d.DecodedMaps++
	}
	return nil
}

func mapHistoryCandidate(raw json.RawMessage, shapes *MapValueShapes) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		shapes.NullMissing++
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		var values []json.RawMessage
		if json.Unmarshal([]byte(text), &values) == nil && values != nil {
			shapes.ArrayString++
			var encoded string
			if len(values) > 0 && json.Unmarshal(values[0], &encoded) == nil && encoded != "" {
				return encoded, true
			}
			return "", false
		}
		shapes.StringOther++
		return "", false
	}
	switch raw[0] {
	case '[':
		shapes.Array++
	case '{':
		shapes.Object++
	default:
		shapes.ScalarOther++
	}
	return "", false
}

func (c *Client) readMapDiagnosticProperties(ctx context.Context, device, domain string, d *MapDiagnostics) (privateMapReference, error) {
	path, err := rpcPath(domain)
	if err != nil {
		d.Properties.Outcome = "routing_unavailable"
		return privateMapReference{}, nil
	}
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return privateMapReference{}, sessionError("request_id_unavailable")
	}
	id := int(binary.LittleEndian.Uint16(random[:])%100) + 2
	params := map[string]any{"did": device, "id": id, "data": map[string]any{"did": device, "id": id, "method": "get_properties", "params": bridge.MapDiagnosticReadParams()}}
	raw, err := c.post(ctx, path, params)
	if err != nil {
		if mapReadAuthenticationError(err) {
			return privateMapReference{}, err
		}
		d.Properties.Outcome = mapReadOutcome(err)
		if d.Properties.Outcome == "response_invalid" {
			d.Properties.Outcome = "response_invalid_envelope"
		}
		return privateMapReference{}, nil
	}
	var env struct {
		Data struct {
			Result []bridge.Property `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		d.Properties.Outcome = "response_invalid_json"
		return privateMapReference{}, nil
	}
	if env.Data.Result == nil {
		d.Properties.Outcome = "response_invalid_missing_result"
		return privateMapReference{}, nil
	}
	if len(env.Data.Result) > 2 {
		d.Properties.Outcome = "response_invalid_many_rows"
		return privateMapReference{}, nil
	}
	seen := map[int]bool{}
	var reference privateMapReference
	for _, p := range env.Data.Result {
		if p.SIID != 6 || (p.PIID != 3 && p.PIID != 8) || seen[p.PIID] {
			d.Properties.Outcome = "response_invalid_unknown_selector"
			if p.SIID == 6 && (p.PIID == 3 || p.PIID == 8) && seen[p.PIID] {
				d.Properties.Outcome = "response_invalid_duplicate_selector"
			}
			d.Properties.ObjectName = unavailableMapProperty()
			d.Properties.MapList = unavailableMapProperty()
			return privateMapReference{}, nil
		}
		seen[p.PIID] = true
		if p.Code == nil || *p.Code != 0 {
			continue
		}
		shape := inspectMapProperty(p.Value, p.PIID == 8)
		if p.PIID == 3 {
			d.Properties.ObjectName = shape
			var value string
			if len(bytes.TrimSpace(p.Value)) > 0 && !bytes.Equal(bytes.TrimSpace(p.Value), []byte("null")) && json.Unmarshal(p.Value, &value) == nil && (value == "" || privateMapText(value, 8192)) {
				d.currentBaselineKnown = true
				d.currentBaseline = privateCurrentCandidate{reference: value}
			}
		} else {
			d.Properties.MapList = shape
			reference = parsePrivateMapReference(p.Value)
			if reference.name != "" && !reference.invalid {
				d.descriptorHash = sha256.Sum256(p.Value)
				d.descriptorPresent = true
			}
		}
	}
	d.Properties.Outcome = "ok"
	return reference, nil
}

// The optional version probe is cached only in this operation's diagnostics.
// Failure cannot erase the independently authenticated descriptor reference.
func (c *Client) readMapVersionProbe(ctx context.Context, device, domain string, d *MapDiagnostics) {
	if d.mapSavingProbed {
		return
	}
	d.mapSavingProbed = true
	d.mapSavingProbe = "request_unavailable"
	d.mapSavingValue = false
	path, err := rpcPath(domain)
	if err != nil {
		return
	}
	var random [2]byte
	if _, err = rand.Read(random[:]); err != nil {
		return
	}
	id := int(binary.LittleEndian.Uint16(random[:])%100) + 2
	params := map[string]any{"did": device, "id": id, "data": map[string]any{"did": device, "id": id, "method": "get_properties", "params": bridge.MapSavingProbeReadParams()}}
	raw, err := c.postBounded(ctx, path, params, 64<<10)
	if err != nil {
		return
	}
	var env struct {
		Data struct {
			Result []bridge.Property `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.Result == nil || len(env.Data.Result) > 1 {
		d.mapSavingProbe = "response_invalid"
		return
	}
	if len(env.Data.Result) == 0 {
		d.mapSavingProbe = "missing"
		return
	}
	p := env.Data.Result[0]
	if p.SIID != 13 || p.PIID != 1 {
		d.mapSavingProbe = "response_invalid"
		return
	}
	if p.Code == nil || *p.Code != 0 {
		d.mapSavingProbe = "property_unavailable"
		return
	}
	value := bytes.TrimSpace(p.Value)
	if len(value) == 0 {
		d.mapSavingProbe = "response_invalid"
		return
	}
	if bytes.Equal(value, []byte("null")) {
		d.mapSavingProbe = "null_value"
		return
	}
	d.mapSavingProbe = "value_present"
	d.mapSavingValue = true
}

func privateMapText(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}
func inspectMapProperty(raw json.RawMessage, mapList bool) MapPropertyShape {
	s := MapPropertyShape{State: "invalid", Shape: "other"}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return unavailableMapProperty()
	}
	if bytes.Equal(raw, []byte("null")) {
		s.State = "empty"
		s.Shape = "null"
		return s
	}
	if len(raw) > 16<<10 {
		return s
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		s.Shape = "string"
		if text == "" {
			s.State = "empty"
			return s
		}
		if !privateMapText(text, 16<<10) {
			return s
		}
		if !mapList {
			s.State = "present"
			s.Reference = true
			return s
		}
		raw = []byte(text)
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
			return s
		}
		s.Shape = "json_object_string"
	} else if raw[0] == '{' {
		s.Shape = "object"
	} else if raw[0] == '[' {
		s.Shape = "array"
		return s
	} else {
		return s
	}
	var object struct {
		Object string `json:"object_name"`
		Alias  string `json:"obj_name"`
		MD5    string `json:"md5"`
	}
	if json.Unmarshal(raw, &object) != nil {
		return s
	}
	s.Reference = privateMapText(object.Object, 2048) || privateMapText(object.Alias, 2048)
	if len(object.MD5) == 32 {
		_, err := hex.DecodeString(object.MD5)
		s.MD5 = err == nil
	}
	if s.Reference {
		s.State = "present"
	} else {
		s.State = "empty"
	}
	return s
}

func mapDiagnosticReason(d *MapDiagnostics) string {
	h := d.History
	switch {
	case h.DecodedMaps > 0:
		return "inline_metadata_available"
	case h.Encrypted > 0:
		return "encrypted_inline_unsupported"
	case d.Properties.ObjectName.Reference || d.Properties.MapList.Reference:
		return "saved_object_required"
	case h.Incremental > 0 && h.Full == 0:
		return "incremental_only"
	case h.Outcome != "ok":
		return "history_unavailable"
	case h.Records == 0:
		return "no_map_history"
	case h.Candidates == 0:
		return "history_shape_unsupported"
	default:
		return "inline_decode_unsupported"
	}
}
