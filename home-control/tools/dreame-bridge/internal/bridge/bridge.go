// Package bridge implements a deliberately offline, read-only Dreame protocol probe.
package bridge

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFixture = 2 << 20
const MaxRequest = 4096
const maxMap = 2 << 20

type Request struct {
	Schema int    `json:"schema"`
	Op     string `json:"op"`
	Device string `json:"device_id,omitempty"`
	MapID  int    `json:"map_id,omitempty"`
}

type Fault struct {
	Code string `json:"code"`
}

func (e *Fault) Error() string { return e.Code }
func fail(code string) error   { return &Fault{Code: code} }

type Response struct {
	Schema   int    `json:"schema"`
	Mode     string `json:"mode"`
	ReadOnly bool   `json:"read_only"`
	OK       bool   `json:"ok"`
	Data     any    `json:"data,omitempty"`
	Error    *Fault `json:"error,omitempty"`
}

type Fixture struct {
	Schema     int                        `json:"schema"`
	Synthetic  bool                       `json:"synthetic"`
	ObservedAt string                     `json:"observed_at"`
	Devices    json.RawMessage            `json:"devices"`
	Status     map[string]json.RawMessage `json:"status"`
	Maps       map[string][]SavedMap      `json:"maps"`
}

type SavedMap struct {
	ID      int    `json:"map_id"`
	Version int    `json:"version"`
	Active  bool   `json:"active"`
	Data    string `json:"encoded_map"`
}

type Device struct {
	ID    string `json:"device_id"`
	Model string `json:"model"`
	Name  string `json:"name"`
}

type Property struct {
	SIID  int             `json:"siid"`
	PIID  int             `json:"piid"`
	Code  *int            `json:"code"`
	Value json.RawMessage `json:"value"`
}

type Status struct {
	DeviceID  string         `json:"device_id"`
	Observed  string         `json:"observed_at"`
	Values    map[string]int `json:"values"`
	Missing   []string       `json:"unavailable"`
	ModelNote string         `json:"model_note"`
}

type Room struct {
	SegmentID int    `json:"segment_id"`
	UniqueID  string `json:"unique_room_id,omitempty"`
	Name      string `json:"name"`
	Type      int    `json:"type_code"`
}

type Rooms struct {
	DeviceID           string `json:"device_id"`
	MapID              int    `json:"map_id"`
	MapVersion         int    `json:"map_version"`
	Active             bool   `json:"active_map"`
	MetadataOnly       bool   `json:"metadata_only"`
	VerifiedForControl bool   `json:"verified_for_control"`
	Items              []Room `json:"rooms"`
}

// DecodeOne rejects extra objects, unknown request fields, and unbounded input.
func DecodeOne(r io.Reader, limit int, dst any, strict bool) error {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil || len(b) > limit {
		return fail("input_limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if strict {
		d.DisallowUnknownFields()
	}
	if err := d.Decode(dst); err != nil {
		return fail("invalid_json")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fail("extra_json")
	}
	return nil
}

func LoadFixture(r io.Reader) (*Fixture, error) {
	var f Fixture
	if err := DecodeOne(r, MaxFixture, &f, true); err != nil {
		return nil, err
	}
	if f.Schema != 1 || !f.Synthetic {
		return nil, fail("synthetic_fixture_required")
	}
	if _, err := time.Parse(time.RFC3339, f.ObservedAt); err != nil {
		return nil, fail("invalid_observed_at")
	}
	return &f, nil
}

func Execute(f *Fixture, req Request) Response {
	r := Response{Schema: 1, Mode: "offline_fixture", ReadOnly: true}
	r.Data, r.Error = execute(f, req)
	r.OK = r.Error == nil
	return r
}

func execute(f *Fixture, req Request) (any, *Fault) {
	bad := func(code string) (any, *Fault) { return nil, &Fault{Code: code} }
	if req.Schema != 1 {
		return bad("unsupported_schema")
	}
	if req.Op == "capabilities" && req.Device == "" && req.MapID == 0 {
		return map[string]any{"operations": []string{"devices", "status", "rooms"}, "network_enabled": false, "robot_commands": false, "credentials_supported": false}, nil
	}
	if req.Op != "devices" && req.Op != "status" && req.Op != "rooms" {
		return bad("operation_denied")
	}
	if req.Op == "devices" && (req.Device != "" || req.MapID != 0) {
		return bad("unexpected_selector")
	}
	if req.Op != "rooms" && req.MapID != 0 {
		return bad("unexpected_selector")
	}
	if f == nil {
		return bad("fixture_required")
	}
	devices, err := ParseDevices(f.Devices)
	if err != nil {
		return nil, fault(err)
	}
	if req.Op == "devices" {
		return devices, nil
	}
	if !validID(req.Device) {
		return bad("invalid_device_id")
	}
	found := false
	for _, d := range devices {
		if d.ID == req.Device {
			found = true
		}
	}
	if !found {
		return bad("device_not_found")
	}
	if req.Op == "status" {
		raw, ok := f.Status[req.Device]
		if !ok {
			return bad("status_unavailable")
		}
		s, err := ParseStatus(req.Device, f.ObservedAt, raw)
		if err != nil {
			return nil, fault(err)
		}
		return s, nil
	}
	var selected *SavedMap
	for _, m := range f.Maps[req.Device] {
		if (req.MapID > 0 && m.ID == req.MapID) || (req.MapID == 0 && m.Active) {
			if selected != nil {
				return bad("ambiguous_map")
			}
			copy := m
			selected = &copy
		}
	}
	if req.MapID < 0 || req.MapID > 32767 {
		return bad("invalid_map_id")
	}
	if selected == nil {
		return bad("map_unavailable")
	}
	rooms, err := ParseRooms(*selected)
	if err != nil {
		return nil, fault(err)
	}
	rooms.DeviceID = req.Device
	return rooms, nil
}

func fault(err error) *Fault {
	var f *Fault
	if errors.As(err, &f) {
		return f
	}
	return &Fault{Code: "invalid_fixture"}
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func textOK(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

func scalarID(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, validID(s)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return "", false
	}
	s = string(n)
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return s, validID(s)
}

// ParseDevices accepts the upstream cloud device-list envelope, dropping account,
// MAC, serial, and other fields that do not belong in this bridge's interface.
func ParseDevices(raw []byte) ([]Device, error) {
	var env struct {
		Code *int `json:"code"`
		Data struct {
			Page struct {
				Records []struct {
					DID    json.RawMessage `json:"did"`
					Model  string          `json:"model"`
					Custom string          `json:"customName"`
					Info   struct {
						Display string `json:"displayName"`
					} `json:"deviceInfo"`
				} `json:"records"`
			} `json:"page"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code == nil || *env.Code != 0 || env.Data.Page.Records == nil {
		return nil, fail("devices_invalid")
	}
	result := make([]Device, 0, len(env.Data.Page.Records))
	seen := map[string]bool{}
	for _, d := range env.Data.Page.Records {
		id, ok := scalarID(d.DID)
		if !ok || seen[id] || !textOK(d.Model, 128) || d.Model == "" {
			return nil, fail("devices_invalid")
		}
		name := d.Custom
		if name == "" {
			name = d.Info.Display
		}
		if !textOK(name, 128) {
			return nil, fail("devices_invalid")
		}
		seen[id] = true
		result = append(result, Device{ID: id, Model: d.Model, Name: name})
	}
	return result, nil
}

// Baseline MIOT identifiers are evidence, not a claim that every model supports
// every field. Failed/missing properties remain unavailable; zero is not invented.
var statusKeys = map[string]string{
	"2.1": "state", "2.2": "error", "3.1": "battery_percent",
	"3.2": "charging_status", "4.1": "status", "4.7": "task_status",
}

func ParseStatus(device, observed string, raw []byte) (Status, error) {
	var env struct {
		Code *int `json:"code"`
		Data struct {
			Result []Property `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code == nil || *env.Code != 0 || env.Data.Result == nil {
		return Status{}, fail("status_invalid")
	}
	s := Status{DeviceID: device, Observed: observed, Values: map[string]int{}, Missing: []string{}, ModelNote: "raw_baseline_values_require_model_validation"}
	seen := map[string]bool{}
	for _, p := range env.Data.Result {
		key := fmt.Sprintf("%d.%d", p.SIID, p.PIID)
		name, ok := statusKeys[key]
		if !ok {
			continue
		}
		if seen[key] {
			return Status{}, fail("duplicate_property")
		}
		seen[key] = true
		if p.Code == nil || *p.Code != 0 {
			continue
		}
		var v int
		if json.Unmarshal(p.Value, &v) != nil || bytes.Equal(bytes.TrimSpace(p.Value), []byte("null")) {
			return Status{}, fail("status_invalid")
		}
		if name == "battery_percent" && (v < 0 || v > 100) {
			return Status{}, fail("status_invalid")
		}
		s.Values[name] = v
	}
	for _, name := range statusKeys {
		if _, ok := s.Values[name]; !ok {
			s.Missing = append(s.Missing, name)
		}
	}
	sort.Strings(s.Missing)
	return s, nil
}

// StatusReadParams is a fixed status selector. There is no caller-supplied
// method, property, action, endpoint, shell command, or authentication capability.
func StatusReadParams() []map[string]any {
	keys := []struct {
		did        string
		siid, piid int
	}{{"0", 2, 1}, {"1", 2, 2}, {"2", 3, 1}, {"3", 3, 2}, {"5", 4, 1}, {"11", 4, 7}}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"did": k.did, "siid": k.siid, "piid": k.piid})
	}
	return out
}

// MapDiagnosticReadParams selects only the pinned OBJECT_NAME (6.3) and
// MAP_LIST (6.8) properties. It never requests a map-update action or writes a
// value; the did strings are the upstream property enumeration values.
func MapDiagnosticReadParams() []map[string]any {
	return []map[string]any{
		{"did": "78", "siid": 6, "piid": 3},
		{"did": "83", "siid": 6, "piid": 8},
	}
}

// MapSavingProbeReadParams is deliberately separate from the proven descriptor
// batch: an unsupported optional property must not poison6.3/6.8 retrieval.
func MapSavingProbeReadParams() []map[string]any {
	return []map[string]any{{"did": "131", "siid": 13, "piid": 1}}
}

var roomTypes = []string{"Room", "Living Room", "Primary Bedroom", "Study", "Kitchen", "Dining Hall", "Bathroom", "Balcony", "Corridor", "Utility Room", "Closet", "Meeting Room", "Office", "Fitness Area", "Recreation Area", "Secondary Bedroom"}

// ParseRooms extracts only seg_inf metadata from an unencrypted full map frame.
// Pixel topology, hidden/unmapped room rules, encryption, nested rism maps, and
// incremental frames are intentionally unsupported and never authorize control.
func ParseRooms(saved SavedMap) (Rooms, error) {
	r := Rooms{MapID: saved.ID, MapVersion: saved.Version, Active: saved.Active, MetadataOnly: true, Items: []Room{}}
	if len(saved.Data) > MaxFixture {
		return r, fail("map_input_limit")
	}
	if saved.ID <= 0 || saved.ID > 32767 || saved.Version < 0 || saved.Version > 3 {
		return r, fail("map_format_unsupported")
	}
	if strings.Contains(saved.Data, ",") {
		return r, fail("encrypted_map_unsupported")
	}
	encoded := strings.NewReplacer("-", "+", "_", "/").Replace(saved.Data)
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return r, fail("map_invalid")
	}
	z, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return r, fail("map_invalid")
	}
	defer z.Close()
	raw, err := io.ReadAll(io.LimitReader(z, maxMap+1))
	if err != nil || len(raw) > maxMap || len(raw) < 27 {
		return r, fail("map_limit_or_invalid")
	}
	if int(int16(binary.LittleEndian.Uint16(raw[:2]))) != saved.ID {
		return r, fail("map_id_mismatch")
	}
	// The upstream decoder uses byte 4 as frame type: 'I' is a complete frame.
	if raw[4] != 'I' {
		return r, fail("incremental_map_unsupported")
	}
	w, h := int(int16(binary.LittleEndian.Uint16(raw[19:21]))), int(int16(binary.LittleEndian.Uint16(raw[21:23])))
	if w <= 0 || h <= 0 || w > 2048 || h > 2048 || w*h > 1<<20 || 27+w*h >= len(raw) {
		return r, fail("map_dimensions_invalid")
	}
	var meta struct {
		Segments map[string]struct {
			Type   *int            `json:"type"`
			Index  int             `json:"index"`
			Name   string          `json:"name"`
			RoomID json.RawMessage `json:"roomID"`
		} `json:"seg_inf"`
	}
	if json.Unmarshal(raw[27+w*h:], &meta) != nil || meta.Segments == nil {
		return r, fail("room_metadata_unavailable")
	}
	if len(meta.Segments) > 63 {
		return r, fail("room_metadata_invalid")
	}
	for key, s := range meta.Segments {
		id, err := strconv.Atoi(key)
		maxID := 63
		if saved.Version == 3 {
			maxID = 31
		}
		if err != nil || id <= 0 || id > maxID || strconv.Itoa(id) != key {
			return r, fail("room_metadata_invalid")
		}
		typ := 0
		if s.Type != nil {
			typ = *s.Type
		}
		if typ < 0 || typ >= len(roomTypes) || s.Index < 0 || s.Index > 63 {
			return r, fail("room_type_unsupported")
		}
		room := Room{SegmentID: id, Type: typ, Name: roomTypes[typ]}
		if typ == 0 {
			room.Name = fmt.Sprintf("Room %d", id)
		} else if s.Index > 0 {
			room.Name = fmt.Sprintf("%s %d", room.Name, s.Index+1)
		}
		if len(s.RoomID) > 0 && !bytes.Equal(s.RoomID, []byte("null")) {
			var ok bool
			room.UniqueID, ok = scalarID(s.RoomID)
			if !ok {
				return r, fail("room_metadata_invalid")
			}
		}
		if s.Name != "" {
			name, err := base64.StdEncoding.DecodeString(s.Name)
			if err != nil || !textOK(string(name), 128) {
				return r, fail("room_name_invalid")
			}
			room.Name = string(name)
		}
		r.Items = append(r.Items, room)
	}
	sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].SegmentID < r.Items[j].SegmentID })
	return r, nil
}

// InspectMapHeader obtains identity without guessing the model's map version.
// Version zero in live metadata means unknown; pixels are never interpreted.
func InspectMapHeader(encoded string) (int, error) {
	if len(encoded) > MaxFixture || strings.Contains(encoded, ",") {
		return 0, fail("map_format_unsupported")
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(encoded))
	if err != nil {
		return 0, fail("map_invalid")
	}
	z, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return 0, fail("map_invalid")
	}
	defer z.Close()
	header := make([]byte, 27)
	if _, err := io.ReadFull(z, header); err != nil || header[4] != 'I' {
		return 0, fail("map_format_unsupported")
	}
	id := int(int16(binary.LittleEndian.Uint16(header[:2])))
	if id <= 0 {
		return 0, fail("map_invalid")
	}
	return id, nil
}

// InspectMapFrameKind returns an enum only. A comma is an encryption suffix
// hint in the pinned format, not proof that a supplied key would decrypt it.
// Reading only the bounded header never exposes map IDs, pixels or names.
func InspectMapFrameKind(encoded string) string {
	if len(encoded) > MaxFixture {
		return "invalid"
	}
	if strings.Contains(encoded, ",") {
		return "encrypted_suffix"
	}
	compressed, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "/").Replace(encoded))
	if err != nil {
		return "invalid"
	}
	z, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "invalid"
	}
	defer z.Close()
	var header [27]byte
	if _, err := io.ReadFull(z, header[:]); err != nil {
		return "invalid"
	}
	switch header[4] {
	case 'I':
		return "full"
	case 'P':
		return "incremental"
	default:
		return "other"
	}
}
