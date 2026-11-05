package savedmap

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func expectDuplicate(t *testing.T, err error, category DuplicateCategory) {
	t.Helper()
	expectCode(t, err, CodeDuplicateField)
	var typed *Error
	if !errors.As(err, &typed) || typed.DuplicateCategory != category {
		t.Fatal("fixed duplicate category mismatch")
	}
}

func TestFrameIgnoredDuplicatesDoNotChangeConsumedMetadata(t *testing.T) {
	cases := []string{
		`{"geometry":{"x":1,"x":2}}`,
		`{"geometry":{"x":1},"geometry":{"x":2}}`,
		`{"geometry":{"x":1,"x":2},"geometry":{"x":3,"x":4}}`,
		`{"geometry":[{"ris":0,"ris":2},{"rism":"unused","rism":"unused-again"}]}`,
		`{"name":"unused","name":"also-unused"}`,
		`{"timestamp_ms":1,"timestamp_ms":2,"cover":{"x":1,"x":2}}`,
		`{"seg_inf":{"1":{"direction":0,"direction":1,"geometry":{"type":1,"type":2}}}}`,
	}
	for _, tail := range cases {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
		if err != nil || frame.MapID != "42" || frame.FrameID != 7 || frame.Version != 1 || !frame.Saved || frame.SavedMapID != "" || len(frame.Rooms) != 1 || frame.Rooms[0].ID != 1 || frame.Rooms[0].Name != "Room 1" {
			t.Fatal("entirely ignored duplicate changed frame metadata")
		}
		m := frame.Format
		if m.SavedMapStatus != SavedMapStatusUnspecified || m.SavedMapStatusPresent || m.Temporary || m.TemporaryFromNested || m.New || m.Restored || m.Empty || !m.MarkersValid {
			t.Fatal("ignored duplicate changed proof flags")
		}
	}
	// Last-wins applies only to ignored fields. Repeated ignored room metadata
	// cannot replace an adjacent, consumed custom name or type/index metadata.
	tail := `{"seg_inf":{"1":{"name":"VGVzdA==","type":4,"index":1,"roomID":"synthetic-room","material":1,"material":2,"unused":{"name":"one","name":"two"}}}}`
	frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
	if err != nil || len(frame.Rooms) != 1 || frame.Rooms[0].Name != "Test" || frame.Rooms[0].Type != 4 || frame.Rooms[0].Index != 1 || frame.Rooms[0].UniqueID != "synthetic-room" {
		t.Fatal("ignored room branch altered consumed room metadata")
	}
}

func TestFrameConsumedRootDuplicatesAlwaysRemainStrict(t *testing.T) {
	cases := []struct {
		tail     string
		category DuplicateCategory
	}{
		{`{"fsm":0,"fsm":0}`, DuplicateFrameMetadata},
		{`{"rpur":0,"rpur":1}`, DuplicateFrameMetadata},
		{`{"us":0,"us":0}`, DuplicateFrameMetadata},
		{`{"iscleanlog":false,"iscleanlog":false}`, DuplicateFrameMetadata},
		{`{"ris":2,"ris":2}`, DuplicateFrameMetadata},
		{`{"suw":5,"suw":5}`, DuplicateFrameMetadata},
		{`{"risp":0,"risp":0}`, DuplicateFrameMetadata},
		{`{"r\u0069s":2,"ris":2}`, DuplicateFrameMetadata},
		{`{"delsr":[],"delsr":[]}`, DuplicateFrameRooms},
		{`{"seg_inf":{},"seg_inf":{}}`, DuplicateFrameRooms},
		{`{"cleanareaorder":[],"cleanareaorder":[]}`, DuplicateFrameVersion},
		{`{"rism":"same","rism":"same"}`, DuplicateFrameBinding},
		{`{"saveMapId":99,"saveMapId":99}`, DuplicateFrameBinding},
	}
	for _, tc := range cases {
		_, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tc.tail), "", nil, 1)
		expectDuplicate(t, err, tc.category)
	}
}

func TestFrameRelevantDescendantsAndSegmentAliasesRemainStrict(t *testing.T) {
	cases := []struct {
		tail     string
		category DuplicateCategory
	}{
		{`{"ris":{"unused":1,"unused":2}}`, DuplicateFrameMetadata},
		{`{"delsr":[{"unused":1,"unused":2}]}`, DuplicateFrameRooms},
		{`{"cleanareaorder":[{"unused":1,"unused":2}]}`, DuplicateFrameVersion},
		{`{"saveMapId":{"unused":1,"unused":2}}`, DuplicateFrameBinding},
		{`{"seg_inf":{"1":{},"1":{}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{"name":"VGVzdA==","name":"VGVzdA=="}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{"type":4,"type":4}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{"index":0,"index":0}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{"roomID":"same","roomID":"same"}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{"name":{"unused":1,"unused":2}}}}`, DuplicateFrameRooms},
		{`{"seg_inf":{"1":{},"01":{}}}`, DuplicateSegmentAlias},
	}
	for _, tc := range cases {
		_, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tc.tail), "", nil, 1)
		expectDuplicate(t, err, tc.category)
	}
}

func TestNestedFrameUsesSameScopedDuplicatePolicy(t *testing.T) {
	innerRaw := frameBytes([]byte{2}, 1, 1, `{"geometry":{"x":1,"x":2},"suw":5}`)
	binary.LittleEndian.PutUint16(innerRaw[0:2], 99)
	inner := base64.StdEncoding.EncodeToString(compress(t, innerRaw))
	tail, _ := json.Marshal(map[string]any{"ris": 2, "rism": inner})
	frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, string(tail)), "", nil, 1)
	if err != nil || frame.SavedMapID != "99" || !frame.Format.Temporary || !frame.Format.TemporaryFromNested || !frame.Format.MarkersValid || len(frame.Rooms) != 0 {
		t.Fatal("ignored nested duplicate changed binding evidence")
	}

	for _, tc := range []struct {
		inner    string
		category DuplicateCategory
	}{
		{`{"ris":2,"ris":2}`, DuplicateFrameMetadata},
		{`{"seg_inf":{"2":{"name":"VGVzdA==","name":"VGVzdA=="}}}`, DuplicateFrameRooms},
		{`{"saveMapId":99,"saveMapId":99}`, DuplicateFrameBinding},
	} {
		inner = frameText(t, []byte{2}, 1, 1, tc.inner)
		tail, _ = json.Marshal(map[string]any{"ris": 2, "rism": inner})
		_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, string(tail)), "", nil, 1)
		expectDuplicate(t, err, tc.category)
	}
}

func TestListRemainsStrictAndIgnoredFrameBranchesRemainBounded(t *testing.T) {
	for _, list := range []string{
		`{"mapstr":[],"geometry":1,"geometry":2}`,
		`{"mapstr":[{"map":"frame","geometry":{"x":1,"x":2}}]}`,
	} {
		_, err := ParseList([]byte(list))
		expectDuplicate(t, err, DuplicateList)
	}
	deep := `{"geometry":` + strings.Repeat("[", maxJSONDepth+1) + `0` + strings.Repeat("]", maxJSONDepth+1) + `}`
	_, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, deep), "", nil, 1)
	expectCode(t, err, CodeLimit)
	// Count both ignored occurrences even though only the last value would be
	// retained by json.Unmarshal. Neither occurrence alone exceeds the budget.
	items := strings.TrimSuffix(strings.Repeat("0,", maxJSONTokens/2+1), ",")
	tail := `{"geometry":[` + items + `],"geometry":[` + items + `]}`
	_, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
	expectCode(t, err, CodeLimit)
	invalid := append([]byte(`{"geometry":"`), 0xff)
	invalid = append(invalid, []byte(`"}`)...)
	_, err = frameObject(invalid)
	expectCode(t, err, CodeInvalidJSON)
	_, err = frameObject([]byte(`{"geometry":1,"geometry":2}[]`))
	expectCode(t, err, CodeInvalidJSON)
}
