// SPDX-License-Identifier: MIT
// Format attribution and license: see doc.go and ../../LICENSE.upstream.
package savedmap

import (
	"bytes"
	"encoding/json"
)

const MaxSelectedSegments = 63

// The pinned full-I reader takes sa[0] from each sa row. Only bounded native
// segment integers are useful as exact-room evidence here; unsupported shapes
// and duplicate observations invalidate this optional value without changing
// existing map decoding. Whole-frame validation already bounded every byte and
// token, including ignored row columns, before this function is called.
func parseSelection(fields map[string]json.RawMessage, duplicate bool) SelectionMetadata {
	raw, exists := fields["sa"]
	if !exists {
		return SelectionMetadata{}
	}
	out := SelectionMetadata{Present: true}
	if duplicate || len(raw) == 0 || raw[0] != '[' {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if token, err := dec.Token(); err != nil || token != json.Delim('[') {
		return out
	}
	ids := make([]int, 0)
	var seen [MaxSelectedSegments + 1]bool
	for dec.More() {
		if len(ids) >= MaxSelectedSegments {
			return out
		}
		var row json.RawMessage
		if dec.Decode(&row) != nil || len(row) == 0 || row[0] != '[' {
			return out
		}
		rowDecoder := json.NewDecoder(bytes.NewReader(row))
		if token, err := rowDecoder.Token(); err != nil || token != json.Delim('[') || !rowDecoder.More() {
			return out
		}
		var first json.RawMessage
		if rowDecoder.Decode(&first) != nil || len(first) == 0 || first[0] == '"' {
			return out
		}
		id, valid := integer(first, 1, MaxSelectedSegments)
		if !valid || seen[id] {
			return out
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if token, err := dec.Token(); err != nil || token != json.Delim(']') {
		return out
	}
	out.Valid = true
	out.SegmentIDs = ids
	return out
}
