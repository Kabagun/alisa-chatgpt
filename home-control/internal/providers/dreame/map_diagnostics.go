package dreame

import "encoding/json"

import "strings"

// Keep this schema independent of the nested CLI module. Every field is an
// enum, bounded count or boolean; unknown/missing fields fail closed.
type mapValueShapes struct {
	ArrayString int `json:"json_array_string"`
	Array       int `json:"array"`
	Object      int `json:"object"`
	StringOther int `json:"string_other"`
	NullMissing int `json:"null_or_missing"`
	ScalarOther int `json:"scalar_other"`
}
type mapHistoryDiagnostic struct {
	Outcome        string         `json:"outcome"`
	Records        int            `json:"records"`
	ValueShapes    mapValueShapes `json:"value_shapes"`
	Candidates     int            `json:"inline_candidates"`
	Encrypted      int            `json:"encrypted_suffix_frames"`
	Full           int            `json:"full_frames"`
	Incremental    int            `json:"incremental_frames"`
	Other          int            `json:"other_frames"`
	Invalid        int            `json:"invalid_frames"`
	DecodeFailures int            `json:"decode_failures"`
	DecodedMaps    int            `json:"decoded_maps"`
}
type mapPropertyShape struct {
	State     string `json:"state"`
	Shape     string `json:"value_shape"`
	Reference bool   `json:"object_reference_present"`
	MD5       bool   `json:"md5_present"`
}
type mapPropertyDiagnostic struct {
	Outcome    string           `json:"outcome"`
	ObjectName mapPropertyShape `json:"object_name"`
	MapList    mapPropertyShape `json:"map_list"`
}
type mapDiagnostic struct {
	Schema      int                     `json:"schema"`
	Reason      string                  `json:"reason"`
	History     mapHistoryDiagnostic    `json:"history"`
	Properties  mapPropertyDiagnostic   `json:"properties"`
	SavedObject *savedObjectDiagnostic  `json:"saved_object,omitempty"`
	SavedMaps   *savedMapReadDiagnostic `json:"saved_maps,omitempty"`
	CurrentMap  *currentMapDiagnostic   `json:"current_map,omitempty"`
}

type savedObjectDiagnostic struct {
	Outcome        string `json:"outcome"`
	Scheme         string `json:"scheme_class"`
	Host           string `json:"host_class"`
	Port           string `json:"port_class"`
	Hostname       string `json:"hostname,omitempty"`
	FetchPerformed bool   `json:"fetch_performed"`
}

func privateMetadataHostname(host string) bool {
	if len(host) < 3 || len(host) > 253 || strings.ToLower(host) != host || !strings.Contains(host, ".") {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".invalid", ".test", ".example", ".home.arpa", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	tld := host[strings.LastIndexByte(host, '.')+1:]
	if !strings.HasPrefix(tld, "xn--") {
		for _, r := range tld {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

func savedObjectDiagnosticValid(raw []byte, s savedObjectDiagnostic, p mapPropertyDiagnostic, allowFetch, redacted bool) bool {
	if (s.FetchPerformed && !allowFetch) || !enum(s.Outcome, "not_referenced", "reference_invalid", "request_unavailable", "response_invalid", "response_limit", "url_rejected", "metadata_available") || !enum(s.Scheme, "not_observed", "https", "http", "other", "missing", "invalid") || !enum(s.Host, "not_observed", "dns_name", "ip_literal", "reserved_or_invalid", "invalid", "missing") || !enum(s.Port, "not_observed", "default_443", "explicit_443", "other", "invalid") {
		return false
	}
	if s.Outcome == "metadata_available" {
		hostOK := exactKeys(raw, "outcome", "scheme_class", "host_class", "port_class", "fetch_performed", "hostname") && privateMetadataHostname(s.Hostname)
		if redacted {
			hostOK = exactKeys(raw, "outcome", "scheme_class", "host_class", "port_class", "fetch_performed") && s.Hostname == ""
		}
		return hostOK && p.Outcome == "ok" && p.MapList.State == "present" && p.MapList.Reference && p.MapList.Shape == "json_object_string" && s.Scheme == "https" && s.Host == "dns_name" && enum(s.Port, "default_443", "explicit_443")
	}
	if !exactKeys(raw, "outcome", "scheme_class", "host_class", "port_class", "fetch_performed") || s.Hostname != "" {
		return false
	}
	if enum(s.Outcome, "not_referenced", "reference_invalid", "request_unavailable", "response_invalid", "response_limit") && !(s.Scheme == "not_observed" && s.Host == "not_observed" && s.Port == "not_observed") {
		return false
	}
	if !enum(s.Outcome, "not_referenced", "reference_invalid") && !(p.Outcome == "ok" && p.MapList.State == "present" && p.MapList.Reference && p.MapList.Shape == "json_object_string") {
		return false
	}
	if s.Outcome == "url_rejected" && (s.Scheme == "not_observed" || s.Host == "not_observed" || s.Port == "not_observed") {
		return false
	}
	return true
}

func exactKeys(raw []byte, keys ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	return true
}
func field(raw []byte, key string) json.RawMessage {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	return object[key]
}
func enum(value string, allowed ...string) bool {
	for _, v := range allowed {
		if value == v {
			return true
		}
	}
	return false
}
func propertyDiagnosticValid(v mapPropertyShape) bool {
	if !enum(v.State, "present", "empty", "unavailable", "invalid") || !enum(v.Shape, "missing", "null", "string", "json_object_string", "object", "array", "other") {
		return false
	}
	switch v.State {
	case "present":
		return v.Reference && enum(v.Shape, "string", "json_object_string", "object")
	case "unavailable":
		return v.Shape == "missing" && !v.Reference && !v.MD5
	case "empty":
		return !v.Reference && enum(v.Shape, "null", "string", "json_object_string", "object")
	default:
		return !v.Reference && !v.MD5 && enum(v.Shape, "string", "json_object_string", "object", "array", "other")
	}
}
func diagnosticReason(d mapDiagnostic) string {
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
func sanitizeMapDiagnostic(raw json.RawMessage, mapCount, unsupported int, source, selected string, active bool) (json.RawMessage, bool) {
	var d mapDiagnostic
	if len(raw) > 4096 || decode(raw, &d, true) != nil {
		return nil, false
	}
	switch d.Schema {
	case 1:
		if d.SavedObject != nil || d.SavedMaps != nil || d.CurrentMap != nil || active || !exactKeys(raw, "schema", "reason", "history", "properties") {
			return nil, false
		}
	case 2:
		if d.SavedObject == nil || d.SavedMaps != nil || d.CurrentMap != nil || active || !exactKeys(raw, "schema", "reason", "history", "properties", "saved_object") {
			return nil, false
		}
	case 3:
		if d.SavedObject == nil || d.SavedMaps == nil || d.CurrentMap != nil || active || !exactKeys(raw, "schema", "reason", "history", "properties", "saved_object", "saved_maps") || source != "cloud_saved_map_metadata_only" {
			return nil, false
		}
	case 4:
		if d.SavedObject == nil || d.SavedMaps == nil || d.CurrentMap == nil || !exactKeys(raw, "schema", "reason", "history", "properties", "saved_object", "saved_maps", "current_map") || source != "cloud_saved_map_metadata_only" {
			return nil, false
		}
	default:
		return nil, false
	}
	history := field(raw, "history")
	properties := field(raw, "properties")
	if !exactKeys(history, "outcome", "records", "value_shapes", "inline_candidates", "encrypted_suffix_frames", "full_frames", "incremental_frames", "other_frames", "invalid_frames", "decode_failures", "decoded_maps") || !exactKeys(field(history, "value_shapes"), "json_array_string", "array", "object", "string_other", "null_or_missing", "scalar_other") || !exactKeys(properties, "outcome", "object_name", "map_list") {
		return nil, false
	}
	for _, key := range []string{"object_name", "map_list"} {
		if !exactKeys(field(properties, key), "state", "value_shape", "object_reference_present", "md5_present") {
			return nil, false
		}
	}
	h := d.History
	s := h.ValueShapes
	p := d.Properties
	if !enum(h.Outcome, "ok", "owner_unavailable", "request_unavailable", "response_invalid", "response_limit") || !enum(p.Outcome, "ok", "routing_unavailable", "request_unavailable", "response_invalid", "response_limit", "response_invalid_envelope", "response_invalid_json", "response_invalid_missing_result", "response_invalid_many_rows", "response_invalid_unknown_selector", "response_invalid_duplicate_selector") {
		return nil, false
	}
	counts := []int{h.Records, s.ArrayString, s.Array, s.Object, s.StringOther, s.NullMissing, s.ScalarOther, h.Candidates, h.Encrypted, h.Full, h.Incremental, h.Other, h.Invalid, h.DecodeFailures, h.DecodedMaps, unsupported}
	for _, n := range counts {
		if n < 0 || n > 20 {
			return nil, false
		}
	}
	if s.ArrayString+s.Array+s.Object+s.StringOther+s.NullMissing+s.ScalarOther != h.Records || h.Encrypted+h.Full+h.Incremental+h.Other+h.Invalid != h.Candidates || h.Candidates > s.ArrayString || h.DecodeFailures > h.Full || (d.Schema < 3 && h.DecodedMaps != mapCount) || h.DecodedMaps+h.DecodeFailures > h.Full {
		return nil, false
	}
	if unsupported != h.Records-h.Candidates+h.Encrypted+h.Incremental+h.Other+h.Invalid+h.DecodeFailures {
		return nil, false
	}
	if h.Outcome != "ok" && h.Records != 0 {
		return nil, false
	}
	if !propertyDiagnosticValid(p.ObjectName) || !propertyDiagnosticValid(p.MapList) {
		return nil, false
	}
	if p.Outcome != "ok" && (p.ObjectName.State != "unavailable" || p.MapList.State != "unavailable") {
		return nil, false
	}
	if d.SavedObject != nil && !savedObjectDiagnosticValid(field(raw, "saved_object"), *d.SavedObject, p, d.Schema >= 3, d.Schema == 4) {
		return nil, false
	}
	if d.Schema < 3 && (source != "cloud_history_6.1_metadata_only" || selected != "" || d.Reason != diagnosticReason(d)) {
		return nil, false
	}
	if d.Schema >= 3 && !savedMapReadDiagnosticValid(field(raw, "saved_maps"), *d.SavedMaps, *d.SavedObject, mapCount, selected, d.Reason) {
		return nil, false
	}
	if d.Schema == 4 && !currentMapDiagnosticValid(field(raw, "current_map"), *d.CurrentMap, mapCount, active) {
		return nil, false
	}
	normalized, err := json.Marshal(d)
	return normalized, err == nil
}
