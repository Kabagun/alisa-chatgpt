// SPDX-License-Identifier: MIT
// Format attribution and license: see doc.go and ../../LICENSE.upstream.
package savedmap

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

const (
	MaxListBytes     = 1 << 20
	MaxEntries       = 32
	MaxFrameBytes    = 8 << 20
	MaxInflatedBytes = 32 << 20
	MaxPixelBytes    = 16 << 20
	MaxTailBytes     = 4 << 20
	MaxNesting       = 2
	maxReference     = 8 << 10
	maxName          = 1024
	maxKey           = 1024
	maxJSONDepth     = 48
	maxJSONTokens    = 200000
)

// Code is a fixed, non-sensitive failure reason. Error values never contain
// source data, cryptographic values, parser messages, or object references.
type Code string

const (
	CodeLimit              Code = "saved_map_limit"
	CodeInvalidJSON        Code = "saved_map_invalid_json"
	CodeDuplicateField     Code = "saved_map_duplicate_field"
	CodeInvalidList        Code = "saved_map_invalid_list"
	CodeInvalidFrame       Code = "saved_map_invalid_frame"
	CodeInvalidBase64      Code = "saved_map_invalid_base64"
	CodeInvalidCrypto      Code = "saved_map_invalid_crypto"
	CodeInvalidCompression Code = "saved_map_invalid_compression"
	CodeUnsupportedVersion Code = "saved_map_unsupported_version"
	CodeUnsupportedFrame   Code = "saved_map_unsupported_frame"
	CodeInvalidMetadata    Code = "saved_map_invalid_metadata"
)

// DuplicateCategory is a fixed, safe classification, never an input field name
// or path. It is populated only for duplicate-field errors.
type DuplicateCategory string

const (
	DuplicateList          DuplicateCategory = "list"
	DuplicateFrameMetadata DuplicateCategory = "frame_metadata"
	DuplicateFrameRooms    DuplicateCategory = "frame_rooms"
	DuplicateFrameVersion  DuplicateCategory = "frame_version"
	DuplicateFrameBinding  DuplicateCategory = "frame_binding"
	DuplicateSegmentAlias  DuplicateCategory = "segment_alias"
)

type Error struct {
	Code              Code
	DuplicateCategory DuplicateCategory
}

func (e *Error) Error() string { return string(e.Code) }
func fail(code Code) error     { return &Error{Code: code} }
func failDuplicate(category DuplicateCategory) error {
	return &Error{Code: CodeDuplicateField, DuplicateCategory: category}
}

// List is selected saved-map metadata, not evidence of the current floor.
type List struct {
	CurrentID string
	Server    int
	Entries   []Entry
}

// Entry contains opaque references. A caller must fetch ObjectReference through
// the interim-object branch unchanged, including any comma; it is not a key.
// EncodedFrame takes precedence when both inline and reference fields exist.
type Entry struct {
	EncodedFrame       string
	ObjectReference    string
	MapObjectReference string
	Name               string
	Angle              int
}

// ParseList parses the downloaded UTF-8 map-list object, not the MAP_LIST property
// descriptor. Empty CurrentID means the list supplied no selected identifier.
func ParseList(data []byte) (List, error) {
	if len(data) > MaxListBytes {
		return List{}, fail(CodeLimit)
	}
	obj, err := object(data)
	if err != nil {
		return List{}, err
	}
	var out List
	if raw, ok := obj["curr_id"]; ok {
		id, valid := integer(raw, -32768, 32767)
		if !valid {
			return List{}, fail(CodeInvalidList)
		}
		out.CurrentID = strconv.Itoa(id)
	}
	if raw, ok := obj["server"]; ok {
		value, valid := integer(raw, 0, 1)
		if !valid {
			return List{}, fail(CodeInvalidList)
		}
		out.Server = value
	}
	raw, exists := obj["mapstr"]
	if !exists || len(raw) == 0 || raw[0] != '[' {
		return List{}, fail(CodeInvalidList)
	}
	var records []json.RawMessage
	if json.Unmarshal(raw, &records) != nil {
		return List{}, fail(CodeInvalidList)
	}
	if len(records) > MaxEntries {
		return List{}, fail(CodeLimit)
	}
	out.Entries = make([]Entry, 0, len(records))
	for _, record := range records {
		var fields map[string]json.RawMessage
		if json.Unmarshal(record, &fields) != nil || fields == nil {
			return List{}, fail(CodeInvalidList)
		}
		var entry Entry
		for _, field := range []struct {
			name string
			dst  *string
			max  int
		}{
			{"map", &entry.EncodedFrame, MaxFrameBytes},
			{"rismobj", &entry.ObjectReference, maxReference},
			{"mapobj", &entry.MapObjectReference, maxReference},
			{"name", &entry.Name, maxName},
		} {
			if value, ok := fields[field.name]; ok {
				if !stringValue(value, field.dst, field.max) {
					return List{}, fail(CodeInvalidList)
				}
			}
		}
		if angle, ok := fields["angle"]; ok {
			value, valid := integer(angle, -360, 360)
			if !valid {
				return List{}, fail(CodeInvalidList)
			}
			entry.Angle = value
		}
		if entry.EncodedFrame == "" && (out.Server != 1 || entry.ObjectReference == "") {
			return List{}, fail(CodeInvalidList)
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

func stringValue(raw json.RawMessage, dst *string, max int) bool {
	if len(raw) < 2 || raw[0] != '"' || json.Unmarshal(raw, dst) != nil || !validSurrogates(raw) {
		return false
	}
	return len(*dst) <= max && utf8.ValidString(*dst)
}

// encoding/json replaces lone UTF-16 surrogates with U+FFFD. Reject them so
// decoding cannot silently mutate an opaque object reference or display name.
func validSurrogates(raw []byte) bool {
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

// integer also accepts a decimal string for upstream fields using int(value).
// Floats, exponents, booleans, null and whitespace inside strings are rejected.
func integer(raw json.RawMessage, min, max int) (int, bool) {
	var s string
	if len(raw) != 0 && raw[0] == '"' {
		if !stringValue(raw, &s, 32) {
			return 0, false
		}
	} else {
		s = string(raw)
	}
	if len(s) == 0 || len(s) > 32 {
		return 0, false
	}
	for i, ch := range s {
		if ch < '0' || ch > '9' {
			if i == 0 && ch == '-' && len(s) > 1 {
				continue
			}
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < int64(min) || v > int64(max) {
		return 0, false
	}
	return int(v), true
}

func object(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, fail(CodeInvalidJSON)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	count := 0
	if err := checkValue(dec, 0, &count); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fail(CodeInvalidJSON)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, fail(CodeInvalidJSON)
	}
	return obj, nil
}

// Reject duplicate keys everywhere, including metadata this reader otherwise
// ignores, so an integration cannot disagree about which value was supplied.
func checkValue(dec *json.Decoder, depth int, count *int) error {
	*count++
	if depth > maxJSONDepth || *count > maxJSONTokens {
		return fail(CodeLimit)
	}
	token, err := dec.Token()
	if err != nil {
		return fail(CodeInvalidJSON)
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return fail(CodeInvalidJSON)
			}
			name, ok := key.(string)
			if !ok {
				return fail(CodeInvalidJSON)
			}
			if seen[name] {
				return failDuplicate(DuplicateList)
			}
			seen[name] = true
			if err := checkValue(dec, depth+1, count); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fail(CodeInvalidJSON)
		}
	case '[':
		for dec.More() {
			if err := checkValue(dec, depth+1, count); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return fail(CodeInvalidJSON)
		}
	default:
		return fail(CodeInvalidJSON)
	}
	return nil
}
