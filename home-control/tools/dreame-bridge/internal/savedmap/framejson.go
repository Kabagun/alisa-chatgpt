// SPDX-License-Identifier: MIT
// Format attribution and license: see doc.go and ../../LICENSE.upstream.
package savedmap

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// Frame metadata consumes a deliberately small subset of the vendor JSON.
// Unlike the strict map-list parser, this parser permits duplicate keys only
// in branches that this reader entirely ignores. Python's pinned json.loads
// accepts those keys with last-wins semantics. Core proof-bearing values remain
// strict regardless of whether repeated values happen to be identical. Optional
// raw selection records ambiguity without changing existing frame success.
func frameObject(data []byte) (map[string]json.RawMessage, error) {
	obj, _, err := observedFrameObject(data)
	return obj, err
}

type frameObservation struct{ selectionDuplicate bool }

// Optional selection was previously ignored. Ambiguity in that field is an
// observation, not a new frame-decode error or weaker handling of other proofs.
func observedFrameObject(data []byte) (map[string]json.RawMessage, frameObservation, error) {
	var observation frameObservation
	if !utf8.Valid(data) {
		return nil, observation, fail(CodeInvalidJSON)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	count := 0
	if err := checkFrameValue(dec, 0, &count, frameRoot, &observation); err != nil {
		return nil, observation, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, observation, fail(CodeInvalidJSON)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || obj == nil {
		return nil, observation, fail(CodeInvalidJSON)
	}
	return obj, observation, nil
}

type frameScope uint8

const (
	frameRoot frameScope = iota
	frameIgnored
	frameMetadata
	frameRooms
	frameVersion
	frameBinding
	frameSegments
	frameRoom
	frameOptionalSelection
)

// frameField is the complete consumed-field inventory for frame JSON. Adding a
// decoder field requires adding its duplicate protection here as well. Strict
// subtrees protect all descendants; ignored subtrees never change back to root
// policy merely because an ignored object happens to contain a familiar name.
func frameField(scope frameScope, name string) (frameScope, DuplicateCategory) {
	switch scope {
	case frameRoot:
		switch name {
		case "fsm", "rpur", "us", "iscleanlog", "ris", "suw", "risp":
			return frameMetadata, DuplicateFrameMetadata
		case "delsr":
			return frameRooms, DuplicateFrameRooms
		case "cleanareaorder":
			return frameVersion, DuplicateFrameVersion
		case "rism", "saveMapId":
			return frameBinding, DuplicateFrameBinding
		case "seg_inf":
			return frameSegments, DuplicateFrameRooms
		case "sa":
			return frameOptionalSelection, ""
		}
	case frameSegments:
		return frameRoom, DuplicateFrameRooms
	case frameRoom:
		switch name {
		case "name", "type", "index", "roomID":
			return frameRooms, DuplicateFrameRooms
		}
	case frameMetadata:
		return scope, DuplicateFrameMetadata
	case frameRooms:
		return scope, DuplicateFrameRooms
	case frameVersion:
		return scope, DuplicateFrameVersion
	case frameBinding:
		return scope, DuplicateFrameBinding
	}
	return frameIgnored, ""
}

func checkFrameValue(dec *json.Decoder, depth int, count *int, scope frameScope, observation *frameObservation) error {
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
		selectionSeen := false
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return fail(CodeInvalidJSON)
			}
			name, ok := key.(string)
			if !ok {
				return fail(CodeInvalidJSON)
			}
			child, category := frameField(scope, name)
			if child == frameOptionalSelection {
				if selectionSeen {
					observation.selectionDuplicate = true
				}
				selectionSeen = true
			}
			if category != "" {
				if seen[name] {
					return failDuplicate(category)
				}
				seen[name] = true
			}
			if err := checkFrameValue(dec, depth+1, count, child, observation); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fail(CodeInvalidJSON)
		}
	case '[':
		for dec.More() {
			if err := checkFrameValue(dec, depth+1, count, scope, observation); err != nil {
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
