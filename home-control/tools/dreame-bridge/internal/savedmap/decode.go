// SPDX-License-Identifier: MIT
// Format attribution and license: see doc.go and ../../LICENSE.upstream.
package savedmap

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const headerSize = 27

// Frame contains only identifiers and visible, pixel-mapped room metadata.
// Saved is a format status, not verification of which floor is currently active.
type Frame struct {
	MapID      string
	FrameID    int
	Version    int
	SavedMapID string
	Saved      bool
	Format     FormatMetadata
	Selection  SelectionMetadata
	Rooms      []Room
}

// SelectionMetadata is an optional observation of this frame's raw sa field.
// It is never inherited from a nested saved map or an earlier frame. Valid only
// describes a supported, unambiguous shape; even a nonempty list alone does not
// establish freshness, physical task start, or which operation caused activity.
type SelectionMetadata struct {
	Present    bool
	Valid      bool
	SegmentIDs []int
}

// SavedMapStatus is the source's ris value. The numeric states are intentionally
// not assigned stronger meanings than the pinned format establishes.
type SavedMapStatus int

const (
	SavedMapStatusUnspecified SavedMapStatus = -1
	SavedMapStatus0           SavedMapStatus = 0
	SavedMapStatus1           SavedMapStatus = 1
	SavedMapStatus2           SavedMapStatus = 2
)

// FormatMetadata preserves bounded evidence from the decoded frame. It does not
// decide freshness, current-floor binding, action availability, or authorization.
// Restored and Empty describe the outer frame before upstream display fallbacks;
// nested saved-map content never clears them or replaces the outer rooms.
type FormatMetadata struct {
	SavedMapStatus        SavedMapStatus
	SavedMapStatusPresent bool
	Temporary             bool
	TemporaryFromNested   bool
	New                   bool
	Restored              bool
	Empty                 bool
	// MarkersValid is false if a previously ignored suw/risp marker has an
	// unsupported shape in this frame or a decoded rism. Decode success and
	// existing room behavior remain unchanged. A false flag must not be read
	// as evidence that Temporary/New are known false.
	MarkersValid bool
}

// Room.ID is the actual pixel-derived segment ID. UniqueID is separate vendor
// room metadata and must never be substituted for the segment ID.
type Room struct {
	ID       int
	Name     string
	Type     int
	Index    int
	UniqueID string
}

type budget struct{ inflated, compressed int }

// DecodeFrame reads a complete I frame. versionHint must be the caller's explicit
// source-backed 0..3 selection; no format or key guessing is performed. Empty key
// means no separate key was supplied. Only then can a comma suffix provide a key.
// IV is required only for encrypted input and must contain exactly 16 UTF-8 bytes.
func DecodeFrame(encoded, key string, iv []byte, versionHint int) (Frame, error) {
	b := budget{inflated: 48 << 20, compressed: 16 << 20}
	return decodeFrame(encoded, key, iv, versionHint, 0, &b)
}

func decodeFrame(encoded, key string, iv []byte, version, depth int, b *budget) (Frame, error) {
	if version < 0 || version > 3 {
		return Frame{}, fail(CodeUnsupportedVersion)
	}
	if depth > MaxNesting || len(encoded) > MaxFrameBytes || len(key) > maxKey {
		return Frame{}, fail(CodeLimit)
	}
	if len(encoded) < 3 || !utf8.ValidString(encoded) || !utf8.ValidString(key) {
		return Frame{}, fail(CodeInvalidFrame)
	}
	// The source performs URL alphabet replacement before splitting a key
	// suffix. Consequently it also normalizes characters inside that suffix;
	// an explicitly supplied key is unaffected.
	encoded = strings.NewReplacer("_", "/", "-", "+").Replace(encoded)
	encrypted := key != ""
	if !encrypted {
		if comma := strings.IndexByte(encoded, ','); comma >= 0 {
			// Upstream split(',') takes only the second component as key.
			rest := encoded[comma+1:]
			if next := strings.IndexByte(rest, ','); next >= 0 {
				rest = rest[:next]
			}
			key, encoded, encrypted = rest, encoded[:comma], true
			if len(key) > maxKey {
				return Frame{}, fail(CodeLimit)
			}
		}
	}
	data, err := decodeBase64(encoded, MaxFrameBytes)
	if err != nil {
		return Frame{}, err
	}
	if len(data) > b.compressed {
		return Frame{}, fail(CodeLimit)
	}
	b.compressed -= len(data)
	if encrypted {
		if len(iv) != aes.BlockSize || !utf8.Valid(iv) || len(data) == 0 || len(data)%aes.BlockSize != 0 {
			return Frame{}, fail(CodeInvalidCrypto)
		}
		block, err := aes.NewCipher(deriveKey(key))
		if err != nil {
			return Frame{}, fail(CodeInvalidCrypto)
		}
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
		// The pinned reader immediately inflates the decrypted bytes. It does
		// not remove PKCS padding; trailing bytes are handled by zlib itself.
	}
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return Frame{}, fail(CodeInvalidCompression)
	}
	limit := min(MaxInflatedBytes, b.inflated)
	raw, readErr := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	closeErr := reader.Close()
	if len(raw) > limit {
		return Frame{}, fail(CodeLimit)
	}
	if readErr != nil || closeErr != nil {
		return Frame{}, fail(CodeInvalidCompression)
	}
	b.inflated -= len(raw)
	if len(raw) < headerSize {
		return Frame{}, fail(CodeInvalidFrame)
	}
	if raw[4] != 'I' {
		return Frame{}, fail(CodeUnsupportedFrame)
	}
	width := int(int16(binary.LittleEndian.Uint16(raw[19:21])))
	height := int(int16(binary.LittleEndian.Uint16(raw[21:23])))
	if width < 0 || height < 0 {
		return Frame{}, fail(CodeInvalidFrame)
	}
	pixelCount := int64(width) * int64(height)
	if pixelCount > MaxPixelBytes {
		return Frame{}, fail(CodeLimit)
	}
	imageEnd := int64(headerSize) + pixelCount
	if imageEnd > int64(len(raw)) {
		return Frame{}, fail(CodeInvalidFrame)
	}
	tail := raw[imageEnd:]
	if len(tail) > MaxTailBytes {
		return Frame{}, fail(CodeLimit)
	}
	fields, observation, err := observedFrameObject(tail)
	if err != nil {
		return Frame{}, err
	}
	meta, err := parseMetadata(fields)
	if err != nil {
		return Frame{}, err
	}
	if _, ok := fields["cleanareaorder"]; ok && version == 1 {
		// This is an explicit propagated transition in the pinned decoder.
		if len(fields["cleanareaorder"]) == 0 || fields["cleanareaorder"][0] != '[' {
			return Frame{}, fail(CodeInvalidMetadata)
		}
		version = 2
	}
	// saveMapId/cover/diff/curtain presence does not promote the hint to 3:
	// upstream only changes a local variable after partial.version was set.
	saved := !meta.restored && !meta.frameMap && meta.status == -1 && !meta.cleanLog
	out := Frame{
		MapID:   strconv.Itoa(int(int16(binary.LittleEndian.Uint16(raw[0:2])))),
		FrameID: int(int16(binary.LittleEndian.Uint16(raw[2:4]))),
		Version: version, Saved: saved, Rooms: make([]Room, 0),
		Selection: parseSelection(fields, observation.selectionDuplicate),
		Format: FormatMetadata{
			SavedMapStatus: SavedMapStatus(meta.status), SavedMapStatusPresent: meta.statusPresent,
			Temporary: meta.temporary, New: meta.newMap, Restored: meta.restored,
			Empty: true, MarkersValid: meta.markersValid,
		},
	}
	pixels := raw[headerSize:imageEnd]
	mapped := make(map[int]bool)
	for _, pixel := range pixels {
		if nonemptyPixel(pixel, version, meta, saved) {
			out.Format.Empty = false
		}
		if id := segmentID(pixel, version, meta, saved); id != 0 {
			mapped[id] = true
		}
	}
	roomInfo, err := parseRooms(fields)
	if err != nil {
		return Frame{}, err
	}
	for id := range mapped {
		if meta.hidden[id] {
			continue
		}
		room := roomInfo[id]
		room.ID = id
		if room.Name == "" {
			room.Name = defaultName(room)
		}
		out.Rooms = append(out.Rooms, room)
	}
	sort.Slice(out.Rooms, func(i, j int) bool { return out.Rooms[i].ID < out.Rooms[j].ID })
	if nested, ok := fields["rism"]; ok {
		var encoded string
		if !stringValue(nested, &encoded, MaxFrameBytes) || encoded == "" {
			return Frame{}, fail(CodeInvalidMetadata)
		}
		// Exact upstream branch: nested decode inherits version, not IV/key.
		inner, err := decodeFrame(encoded, "", nil, version, depth+1, b)
		if err != nil {
			return Frame{}, err
		}
		out.SavedMapID = inner.MapID
		out.Version = max(out.Version, inner.Version)
		// The pinned source propagates nested temporary_map independently of
		// its later room/display restoration. Keep that exact provenance.
		if inner.Format.Temporary {
			out.Format.Temporary = true
			out.Format.TemporaryFromNested = true
		}
		out.Format.MarkersValid = out.Format.MarkersValid && inner.Format.MarkersValid
	}
	if !saved && out.Version == 3 {
		// Upstream applies this assignment after rism, including clearing it
		// when the v3 linkage is absent. It is not the outer header map ID.
		out.SavedMapID = ""
		if rawID, ok := fields["saveMapId"]; ok {
			id, valid := integer(rawID, -32768, 32767)
			if !valid {
				return Frame{}, fail(CodeInvalidMetadata)
			}
			out.SavedMapID = strconv.Itoa(id)
		}
	}
	return out, nil
}

func deriveKey(key string) []byte {
	digest := sha256.Sum256([]byte(key))
	var text [64]byte
	hex.Encode(text[:], digest[:])
	return text[:32]
}

func decodeBase64(encoded string, maxBytes int) ([]byte, error) {
	if len(encoded) > maxBytes {
		return nil, fail(CodeLimit)
	}
	// Match the URL-safe replacement before standard, padded base64 decode.
	// Whitespace is accepted; other nonalphabet characters fail closed.
	var normalized strings.Builder
	normalized.Grow(len(encoded))
	for _, ch := range encoded {
		switch ch {
		case '_':
			ch = '/'
		case '-':
			ch = '+'
		case ' ', '\t', '\r', '\n', '\v', '\f':
			continue
		}
		if !(ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '+' || ch == '/' || ch == '=') {
			return nil, fail(CodeInvalidBase64)
		}
		normalized.WriteRune(ch)
	}
	data, err := base64.StdEncoding.DecodeString(normalized.String())
	if err != nil {
		return nil, fail(CodeInvalidBase64)
	}
	return data, nil
}

type metadata struct {
	frameMap, restored, recovery, cleanLog         bool
	temporary, newMap, statusPresent, markersValid bool
	status                                         int
	hidden                                         map[int]bool
}

func parseMetadata(fields map[string]json.RawMessage) (metadata, error) {
	m := metadata{status: -1, hidden: make(map[int]bool), markersValid: true}
	for _, field := range []struct {
		name string
		dst  *bool
	}{
		{"fsm", &m.frameMap}, {"rpur", &m.restored}, {"us", &m.recovery},
	} {
		if raw, ok := fields[field.name]; ok {
			if len(raw) == 0 || raw[0] == '"' {
				return metadata{}, fail(CodeInvalidMetadata)
			}
			value, valid := integer(raw, 0, 1)
			if !valid {
				return metadata{}, fail(CodeInvalidMetadata)
			}
			*field.dst = value == 1
		}
	}
	if raw, ok := fields["iscleanlog"]; ok {
		switch string(raw) {
		case "true", "1":
			m.cleanLog = true
		case "false", "0":
		default:
			return metadata{}, fail(CodeInvalidMetadata)
		}
	}
	if raw, ok := fields["ris"]; ok {
		m.statusPresent = true
		if len(raw) == 0 || raw[0] == '"' {
			return metadata{}, fail(CodeInvalidMetadata)
		}
		value, valid := integer(raw, -1, 2)
		if !valid {
			return metadata{}, fail(CodeInvalidMetadata)
		}
		m.status = value
	}
	// These fields were intentionally ignored by the saved-room reader.
	// Preserve decode behavior on unsupported shapes while marking the new
	// evidence as uncertain, instead of guessing or introducing new errors.
	if value, present, valid := optionalMarker(fields, "suw"); !valid {
		m.markersValid = false
	} else if present && (value == 5 || value == 6) {
		_, frameMarkerPresent := fields["fsm"]
		m.temporary = !frameMarkerPresent
	}
	if value, present, valid := optionalMarker(fields, "risp"); !valid {
		m.markersValid = false
	} else {
		m.newMap = present && value == 0
	}
	if raw, ok := fields["delsr"]; ok {
		if len(raw) == 0 || raw[0] != '[' {
			return metadata{}, fail(CodeInvalidMetadata)
		}
		var ids []json.RawMessage
		if json.Unmarshal(raw, &ids) != nil {
			return metadata{}, fail(CodeInvalidMetadata)
		}
		if len(ids) > 63 {
			return metadata{}, fail(CodeLimit)
		}
		for _, id := range ids {
			if len(id) == 0 || id[0] == '"' {
				return metadata{}, fail(CodeInvalidMetadata)
			}
			value, valid := integer(id, 1, 63)
			if !valid {
				return metadata{}, fail(CodeInvalidMetadata)
			}
			m.hidden[value] = true
		}
	}
	return m, nil
}

// optionalMarker supports native integral values without coercing strings,
// floats, booleans, or composite values. Null and absence have the source's
// known-false semantics for both relevant predicates. Unsupported input only
// invalidates the added evidence, never the previously supported frame decode.
func optionalMarker(fields map[string]json.RawMessage, name string) (value int64, present, valid bool) {
	raw, exists := fields[name]
	if !exists || string(raw) == "null" {
		return 0, false, true
	}
	if len(raw) == 0 || raw[0] == '"' {
		return 0, true, false
	}
	for i, ch := range raw {
		if ch < '0' || ch > '9' {
			if i == 0 && ch == '-' && len(raw) > 1 {
				continue
			}
			return 0, true, false
		}
	}
	v, err := strconv.ParseInt(string(raw), 10, 64)
	return v, true, err == nil
}

// nonemptyPixel mirrors empty_map assignments in the source's full-I pixel
// branches. A wall-only frame can be nonempty without containing mapped rooms.
func nonemptyPixel(pixel byte, version int, m metadata, saved bool) bool {
	if pixel == 0 {
		return false
	}
	if version == 3 || m.frameMap {
		return true
	}
	id := pixel & 0x3f
	if m.status == 0 || m.status == 1 {
		return id == 1 || id == 2 || id == 3
	}
	if (version == 0 && !saved && !m.recovery) || m.status == 2 {
		return id > 0
	}
	return true
}

func segmentID(pixel byte, version int, m metadata, saved bool) int {
	if pixel == 0 {
		return 0
	}
	if version == 3 {
		id, wall := int(pixel&0x1f), (pixel>>5)&3
		if id == 0 || id == 31 || wall == 3 {
			return 0
		}
		return id // Plain pixels and 100+id wall pixels both form segments.
	}
	if m.frameMap {
		id := int(pixel >> 2)
		if id >= 1 && id <= 60 {
			return id
		}
		return 0
	}
	if m.status == 0 || m.status == 1 || (version == 0 && !saved && !m.recovery) || m.status == 2 {
		return 0
	}
	id := int(pixel & 0x3f)
	return id // High-bit wall pixels become 100+id, then get_segments removes 100.
}

func parseRooms(fields map[string]json.RawMessage) (map[int]Room, error) {
	out := make(map[int]Room)
	raw, ok := fields["seg_inf"]
	if !ok {
		return out, nil
	}
	var records map[string]json.RawMessage
	if json.Unmarshal(raw, &records) != nil || records == nil {
		return nil, fail(CodeInvalidMetadata)
	}
	if len(records) > 63 {
		return nil, fail(CodeLimit)
	}
	for key, raw := range records {
		id, valid := integer(json.RawMessage(strconv.Quote(key)), 1, 63)
		if !valid {
			return nil, fail(CodeInvalidMetadata)
		}
		if _, duplicate := out[id]; duplicate {
			return nil, failDuplicate(DuplicateSegmentAlias)
		}
		var values map[string]json.RawMessage
		if json.Unmarshal(raw, &values) != nil || values == nil {
			return nil, fail(CodeInvalidMetadata)
		}
		room := Room{ID: id}
		for _, field := range []struct {
			name string
			dst  *int
		}{{"type", &room.Type}, {"index", &room.Index}} {
			if raw, ok := values[field.name]; ok {
				if string(raw) == "null" {
					continue
				}
				if field.name == "index" && len(raw) > 0 && raw[0] == '"' {
					return nil, fail(CodeInvalidMetadata)
				}
				value, valid := integer(raw, 0, 65535)
				if !valid {
					return nil, fail(CodeInvalidMetadata)
				}
				*field.dst = value
			}
		}
		if raw, ok := values["roomID"]; ok && string(raw) != "null" {
			if len(raw) != 0 && raw[0] == '"' {
				if !stringValue(raw, &room.UniqueID, 256) {
					return nil, fail(CodeInvalidMetadata)
				}
			} else {
				// Keep integral unique IDs exact without float64 conversion.
				value, err := strconv.ParseInt(string(raw), 10, 64)
				if err != nil {
					return nil, fail(CodeInvalidMetadata)
				}
				room.UniqueID = strconv.FormatInt(value, 10)
			}
		}
		if raw, ok := values["name"]; ok && string(raw) != "null" {
			var encoded string
			if !stringValue(raw, &encoded, 4*maxName) {
				return nil, fail(CodeInvalidMetadata)
			}
			// Names use standard base64 upstream, without the frame's URL
			// alphabet normalization.
			if strings.ContainsAny(encoded, "-_") {
				return nil, fail(CodeInvalidMetadata)
			}
			decoded, err := decodeBase64(encoded, 4*maxName)
			if err != nil || len(decoded) > maxName || !utf8.Valid(decoded) {
				return nil, fail(CodeInvalidMetadata)
			}
			room.Name = string(decoded)
		}
		out[id] = room
	}
	return out, nil
}

var typeNames = [...]string{"Room", "Living Room", "Primary Bedroom", "Study", "Kitchen", "Dining Hall", "Bathroom", "Balcony", "Corridor", "Utility Room", "Closet", "Meeting Room", "Office", "Fitness Area", "Recreation Area", "Secondary Bedroom"}

func defaultName(room Room) string {
	if room.Type > 0 && room.Type < len(typeNames) {
		name := typeNames[room.Type]
		if room.Index > 0 {
			name += " " + strconv.Itoa(room.Index+1)
		}
		return name
	}
	return "Room " + strconv.Itoa(room.ID)
}
