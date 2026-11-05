package savedmap

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestFormatStatusPresenceAndDeclaredFlags(t *testing.T) {
	cases := []struct {
		name, tail                           string
		status                               SavedMapStatus
		present, temporary, newMap, restored bool
	}{
		{"absent", `{}`, SavedMapStatusUnspecified, false, false, false, false},
		{"explicit minus one", `{"ris":-1}`, SavedMapStatusUnspecified, true, false, false, false},
		{"zero", `{"ris":0}`, SavedMapStatus0, true, false, false, false},
		{"one", `{"ris":1}`, SavedMapStatus1, true, false, false, false},
		{"two", `{"ris":2}`, SavedMapStatus2, true, false, false, false},
		{"temporary five", `{"suw":5}`, SavedMapStatusUnspecified, false, true, false, false},
		{"temporary six", `{"suw":6}`, SavedMapStatusUnspecified, false, true, false, false},
		{"temporary suppressed by zero fsm", `{"suw":5,"fsm":0}`, SavedMapStatusUnspecified, false, false, false, false},
		{"temporary suppressed by frame map", `{"suw":6,"fsm":1}`, SavedMapStatusUnspecified, false, false, false, false},
		{"other suw", `{"suw":7}`, SavedMapStatusUnspecified, false, false, false, false},
		{"null markers", `{"suw":null,"risp":null}`, SavedMapStatusUnspecified, false, false, false, false},
		{"new", `{"risp":0}`, SavedMapStatusUnspecified, false, false, true, false},
		{"negative zero new", `{"risp":-0}`, SavedMapStatusUnspecified, false, false, true, false},
		{"other risp", `{"risp":1}`, SavedMapStatusUnspecified, false, false, false, false},
		{"declared restored", `{"ris":2,"rpur":1}`, SavedMapStatus2, true, false, false, true},
		{"not restored", `{"rpur":0}`, SavedMapStatusUnspecified, false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tc.tail), "", nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			m := frame.Format
			if m.SavedMapStatus != tc.status || m.SavedMapStatusPresent != tc.present || m.Temporary != tc.temporary || m.New != tc.newMap || m.Restored != tc.restored || !m.MarkersValid || m.TemporaryFromNested || m.Empty {
				t.Fatal("format flags differ from declared source predicates")
			}
		})
	}
}

func TestUnknownNewMarkersPreserveRoomDecodeAndExposeUncertainty(t *testing.T) {
	cases := []string{
		`{"suw":"5"}`, `{"suw":5.0}`, `{"suw":5e0}`, `{"suw":true}`, `{"suw":[]}`, `{"suw":{}}`,
		`{"risp":"0"}`, `{"risp":0.0}`, `{"risp":false}`, `{"risp":[]}`, `{"risp":{}}`,
		`{"suw":9223372036854775808}`, `{"risp":-9223372036854775809}`,
	}
	for _, tail := range cases {
		frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, tail), "", nil, 1)
		if err != nil || frame.MapID != "42" || !frame.Saved || len(frame.Rooms) != 1 || frame.Rooms[0].ID != 1 || frame.Rooms[0].Name != "Room 1" {
			t.Fatal("previously ignored marker changed existing frame decode")
		}
		if frame.Format.MarkersValid || frame.Format.Temporary || frame.Format.New {
			t.Fatal("unsupported marker must remain explicitly uncertain")
		}
	}
	// An unsupported new marker does not clear independently known evidence.
	frame, err := DecodeFrame(frameText(t, []byte{1}, 1, 1, `{"suw":"unknown","risp":0,"rpur":1,"ris":2}`), "", nil, 1)
	if err != nil || frame.Format.MarkersValid || !frame.Format.New || !frame.Format.Restored || frame.Format.SavedMapStatus != SavedMapStatus2 {
		t.Fatal("independent declared flags were lost")
	}
}

func TestSourcePixelEmptinessIsSeparateFromRoomPresence(t *testing.T) {
	cases := []struct {
		name, tail             string
		version, width, height int
		pixels                 []byte
		empty                  bool
		rooms                  int
	}{
		{"zero dimensions", `{}`, 1, 0, 0, nil, true, 0},
		{"zero pixels", `{}`, 1, 1, 1, []byte{0}, true, 0},
		{"two by two empty special case", `{}`, 1, 2, 2, []byte{0, 0, 0, 0}, true, 0},
		{"two by two nonempty", `{}`, 1, 2, 2, []byte{0, 0, 1, 0}, false, 1},
		{"generic wall only", `{}`, 1, 1, 1, []byte{0x80}, false, 0},
		{"status zero unknown pixel", `{"ris":0}`, 1, 1, 1, []byte{4}, true, 0},
		{"status one unknown pixel", `{"ris":1}`, 1, 1, 1, []byte{4}, true, 0},
		{"status zero new pixel", `{"ris":0}`, 1, 1, 1, []byte{1}, false, 0},
		{"status one wall pixel", `{"ris":1}`, 1, 1, 1, []byte{2}, false, 0},
		{"status two no low bits", `{"ris":2}`, 1, 1, 1, []byte{0x80}, true, 0},
		{"status two low bits", `{"ris":2}`, 1, 1, 1, []byte{4}, false, 0},
		{"version zero current no low bits", `{"rpur":1}`, 0, 1, 1, []byte{0x80}, true, 0},
		{"version zero recovery generic branch", `{"rpur":1,"us":1}`, 0, 1, 1, []byte{0x80}, false, 0},
		{"frame map wall only", `{"fsm":1}`, 1, 1, 1, []byte{252}, false, 0},
		{"version three outside carpet bit", `{}`, 3, 1, 1, []byte{0x80}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := DecodeFrame(frameText(t, tc.pixels, tc.width, tc.height, tc.tail), "", nil, tc.version)
			if err != nil || frame.Format.Empty != tc.empty || len(frame.Rooms) != tc.rooms {
				t.Fatal("source empty_map branch mismatch")
			}
		})
	}
}

func TestNestedMetadataCannotClearOuterRejectionEvidence(t *testing.T) {
	innerRaw := frameBytes([]byte{2}, 1, 1, `{"suw":5,"risp":0}`)
	binary.LittleEndian.PutUint16(innerRaw[0:2], 99)
	inner := base64.StdEncoding.EncodeToString(compress(t, innerRaw))
	tail, _ := json.Marshal(map[string]any{"ris": 2, "rpur": 1, "risp": 1, "rism": inner})
	frame, err := DecodeFrame(frameText(t, []byte{0}, 1, 1, string(tail)), "", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := frame.Format
	if !m.Temporary || !m.TemporaryFromNested || !m.Restored || !m.Empty || m.New || !m.MarkersValid || m.SavedMapStatus != SavedMapStatus2 || !m.SavedMapStatusPresent {
		t.Fatal("nested metadata cleared or substituted outer format evidence")
	}
	if frame.MapID != "42" || frame.SavedMapID != "99" || frame.Saved || len(frame.Rooms) != 0 {
		t.Fatal("format metadata changed existing linkage or room behavior")
	}

	// Nested temporary uncertainty must not become known false at the outer
	// frame merely because the nested saved-map linkage itself decoded.
	inner = frameText(t, []byte{2}, 1, 1, `{"suw":"5"}`)
	tail, _ = json.Marshal(map[string]any{"ris": 2, "rism": inner})
	frame, err = DecodeFrame(frameText(t, []byte{1}, 1, 1, string(tail)), "", nil, 1)
	if err != nil || frame.Format.MarkersValid || frame.Format.Temporary || frame.Format.TemporaryFromNested || frame.SavedMapID != "42" {
		t.Fatal("nested marker uncertainty was hidden")
	}
}
