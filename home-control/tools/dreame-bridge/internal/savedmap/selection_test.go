package savedmap

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRawSelectionPresenceAndFirstColumn(t *testing.T) {
	cases := []struct {
		tail           string
		present, valid bool
		ids            []int
	}{
		{`{"ris":2}`, false, false, nil},
		{`{"ris":2,"sa":[]}`, true, true, []int{}},
		{`{"ris":2,"sa":[[1]]}`, true, true, []int{1}},
		{`{"ris":2,"sa":[[63],[2],[1]]}`, true, true, []int{63, 2, 1}},
		{`{"ris":2,"sa":[[1,2,3,4]]}`, true, true, []int{1}},
		{`{"ris":2,"sa":[[1,{"unused":1,"unused":2},null,[99]]]}`, true, true, []int{1}},
		{`{"ris":2,"sa":[[3]],"geometry":{"sa":[[1]],"sa":[[2]]}}`, true, true, []int{3}},
		{`{"ris":2,"sa":[[3]],"seg_inf":{"1":{"sa":[[1]],"sa":[[2]]}}}`, true, true, []int{3}},
	}
	baseline, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2}`), "", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tc.tail), "", nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Selection.Present != tc.present || frame.Selection.Valid != tc.valid || !reflect.DeepEqual(frame.Selection.SegmentIDs, tc.ids) {
			t.Fatal("raw selection presence or first-column extraction mismatch")
		}
		if frame.Format != baseline.Format || frame.Saved != baseline.Saved || !reflect.DeepEqual(frame.Rooms, baseline.Rooms) || frame.MapID != baseline.MapID || frame.Version != baseline.Version || frame.SavedMapID != baseline.SavedMapID {
			t.Fatal("optional selection altered existing map decode")
		}
	}
	// Selection describes the raw reported IDs, not the current pixel-derived
	// rooms (which status 2 intentionally does not expose in version 1).
	other, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2,"sa":[[63]]}`), "", nil, 1)
	if err != nil || len(other.Rooms) != 0 || !other.Selection.Valid || len(other.Selection.SegmentIDs) != 1 || other.Selection.SegmentIDs[0] != 63 {
		t.Fatal("selection was filtered or substituted from mapped rooms")
	}
}

func TestUnsupportedSelectionOnlyInvalidatesOptionalEvidence(t *testing.T) {
	values := []string{
		`null`, `false`, `1`, `"[[1]]"`, `{}`, `[1]`, `["1"]`, `[[]]`,
		`[[0]]`, `[[64]]`, `[[-1]]`, `[[1.0]]`, `[[1e0]]`, `[[true]]`, `[["1"]]`,
		`[[{}]]`, `[[[]]]`, `[[1],[1]]`, `[[1],[2],[1]]`, `[[1],[]]`, `[[1],false]`,
	}
	baseline, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2,"risp":1}`), "", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"ris":2,"risp":1,"sa":`+value+`}`), "", nil, 3)
		if err != nil {
			t.Fatal("previously ignored selection changed frame success")
		}
		if !frame.Selection.Present || frame.Selection.Valid || frame.Selection.SegmentIDs != nil {
			t.Fatal("unsupported selection must expose no partial IDs")
		}
		if frame.Format != baseline.Format || frame.Saved != baseline.Saved || !reflect.DeepEqual(frame.Rooms, baseline.Rooms) || frame.MapID != baseline.MapID || frame.Version != baseline.Version {
			t.Fatal("unsupported selection changed prior room or proof metadata")
		}
	}
}

func TestDuplicateSelectionIsUncertainWithoutWeakeningStrictFields(t *testing.T) {
	for _, tail := range []string{
		`{"ris":2,"sa":[[1]],"sa":[[1]]}`,
		`{"ris":2,"sa":[[1]],"sa":[[2]]}`,
		`{"ris":2,"s\u0061":[[1]],"sa":[[1]]}`,
		`{"ris":2,"sa":null,"sa":[[1]]}`,
		`{"ris":2,"sa":[[1]],"sa":[]}`,
	} {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
		if err != nil || !frame.Selection.Present || frame.Selection.Valid || frame.Selection.SegmentIDs != nil || !frame.Format.MarkersValid || frame.Format.SavedMapStatus != SavedMapStatus2 {
			t.Fatal("duplicate sa changed prior frame behavior or supplied exact-room evidence")
		}
	}
	for _, tc := range []struct {
		tail     string
		category DuplicateCategory
	}{
		{`{"sa":[[1]],"sa":[[2]],"ris":2,"ris":2}`, DuplicateFrameMetadata},
		{`{"sa":[[1]],"seg_inf":{"1":{"name":"VGVzdA==","name":"VGVzdA=="}}}`, DuplicateFrameRooms},
		{`{"sa":[[1]],"saveMapId":99,"saveMapId":99}`, DuplicateFrameBinding},
		{`{"sa":[[1]],"cleanareaorder":[],"cleanareaorder":[]}`, DuplicateFrameVersion},
	} {
		_, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tc.tail), "", nil, 1)
		expectDuplicate(t, err, tc.category)
	}
}

func TestSelectionLimitsAndExistingJSONBounds(t *testing.T) {
	rows := make([]string, MaxSelectedSegments)
	for i := range rows {
		rows[i] = "[" + strconv.Itoa(i+1) + "]"
	}
	tail := `{"sa":[` + strings.Join(rows, ",") + `]}`
	frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
	if err != nil || !frame.Selection.Valid || len(frame.Selection.SegmentIDs) != MaxSelectedSegments {
		t.Fatal("bounded complete native selection was rejected")
	}
	tail = `{"sa":[` + strings.Join(rows, ",") + `,[1]]}`
	frame, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
	if err != nil || frame.Selection.Valid || frame.Selection.SegmentIDs != nil {
		t.Fatal("oversized optional selection changed frame success or escaped its bound")
	}
	deep := `{"sa":[[1,` + strings.Repeat("[", maxJSONDepth+1) + `0` + strings.Repeat("]", maxJSONDepth+1) + `]]}`
	_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, deep), "", nil, 1)
	expectCode(t, err, CodeLimit)
	tail = `{"sa":[[1,` + strings.TrimSuffix(strings.Repeat("0,", maxJSONTokens+1), ",") + `]]}`
	_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
	expectCode(t, err, CodeLimit)
}

func TestSelectionNeverCarriesOverOrInheritsNestedSavedSelection(t *testing.T) {
	for _, tc := range []struct {
		outer, inner   string
		present, valid bool
		ids            []int
	}{
		{``, `"sa":[[2]]`, false, false, nil},
		{`,"sa":[[1]]`, `"sa":[[2]]`, true, true, []int{1}},
		{`,"sa":[]`, `"sa":[[2]]`, true, true, []int{}},
		{`,"sa":[[1]]`, `"sa":[[2]],"sa":[[3]]`, true, true, []int{1}},
		{`,"sa":[[1]],"sa":[[2]]`, `"sa":[[3]]`, true, false, nil},
	} {
		innerRaw := frameBytes([]byte{2}, 1, 1, `{`+tc.inner+`}`)
		binary.LittleEndian.PutUint16(innerRaw[0:2], 99)
		inner := base64.StdEncoding.EncodeToString(compress(t, innerRaw))
		encodedInner, _ := json.Marshal(inner)
		tail := `{"ris":2,"rism":` + string(encodedInner) + tc.outer + `}`
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
		if err != nil || frame.SavedMapID != "99" || frame.Selection.Present != tc.present || frame.Selection.Valid != tc.valid || !reflect.DeepEqual(frame.Selection.SegmentIDs, tc.ids) || !frame.Format.MarkersValid {
			t.Fatal("outer raw selection inherited saved-map or prior selection")
		}
	}
	first, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"sa":[[1]]}`), "", nil, 1)
	if err != nil || !first.Selection.Valid {
		t.Fatal("initial selection decode failed")
	}
	second, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{}`), "", nil, 1)
	if err != nil || second.Selection.Present || second.Selection.Valid || second.Selection.SegmentIDs != nil {
		t.Fatal("missing current selection carried over a previous frame")
	}
}
