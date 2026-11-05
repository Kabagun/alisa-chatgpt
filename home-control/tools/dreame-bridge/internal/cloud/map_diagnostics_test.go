package cloud

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"alisa-dreame-readonly/internal/bridge"
)

func syntheticDiagnosticFrame(kind byte, metadata string) string {
	raw := make([]byte, 31)
	binary.LittleEndian.PutUint16(raw, 17)
	raw[4] = kind
	binary.LittleEndian.PutUint16(raw[19:], 2)
	binary.LittleEndian.PutUint16(raw[21:], 2)
	raw = append(raw, metadata...)
	var out bytes.Buffer
	z := zlib.NewWriter(&out)
	_, _ = z.Write(raw)
	_ = z.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes())
}
func syntheticHistory(values ...any) string {
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, map[string]any{"value": value})
	}
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"list": items}})
	return string(raw)
}
func syntheticArrayString(value string) string {
	raw, _ := json.Marshal([]string{value})
	return string(raw)
}
func syntheticMapProperties(object, list any) string {
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": []any{map[string]any{"siid": 6, "piid": 3, "code": 0, "value": object}, map[string]any{"siid": 6, "piid": 8, "code": 0, "value": list}}}})
	return string(raw)
}
func assertFixedMapDiagnosticRPC(t *testing.T, r *http.Request) {
	t.Helper()
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 5 {
		t.Fatal("invalid diagnostic RPC")
	}
	data, _ := body["data"].(map[string]any)
	if data["method"] != "get_properties" || len(data) != 4 || data["did"] != "fixture-vacuum-1" || data["id"] != body["id"] {
		t.Fatal("non-read diagnostic RPC")
	}
	raw, _ := json.Marshal(bridge.MapDiagnosticReadParams())
	var expected any
	_ = json.Unmarshal(raw, &expected)
	if !reflect.DeepEqual(data["params"], expected) {
		t.Fatal("nonfixed map properties or write value")
	}
}

func TestRoomsDiagnosticsDistinguishUnavailableSourcesWithoutPrivateData(t *testing.T) {
	emptyProperties := syntheticMapProperties("", "")
	privateProperties := syntheticMapProperties("synthetic-object-MUST_NOT_ESCAPE,synthetic-key-MUST_NOT_ESCAPE", `{"object_name":"synthetic-list-MUST_NOT_ESCAPE","md5":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signed_url":"https://untrusted.invalid/MUST_NOT_ESCAPE"}`)
	for _, tc := range []struct {
		name, history, properties, reason string
		maps, unsupported                 int
	}{
		{"empty", syntheticHistory(), emptyProperties, "no_map_history", 0, 0},
		{"encrypted", syntheticHistory(syntheticArrayString("opaque-frame,synthetic-key-MUST_NOT_ESCAPE")), privateProperties, "encrypted_inline_unsupported", 0, 1},
		{"incremental", syntheticHistory(syntheticArrayString(syntheticDiagnosticFrame('P', `{}`))), emptyProperties, "incremental_only", 0, 1},
		{"object_reference", syntheticHistory(), privateProperties, "saved_object_required", 0, 0},
		{"shape", syntheticHistory(map[string]any{"private": "MUST_NOT_ESCAPE"}), emptyProperties, "history_shape_unsupported", 0, 1},
		{"nested_map", syntheticHistory(syntheticArrayString(syntheticDiagnosticFrame('I', `{"rism":"MUST_NOT_ESCAPE"}`))), emptyProperties, "inline_decode_unsupported", 0, 1},
		{"inline", syntheticHistory(syntheticArrayString(syntheticDiagnosticFrame('I', `{"seg_inf":{"3":{"type":4}}}`))), emptyProperties, "inline_metadata_available", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, deviceList)
				case historyPath:
					io.WriteString(w, tc.history)
				case "/dreame-iot-com-shard-1/device/sendCommand":
					assertFixedMapDiagnosticRPC(t, r)
					io.WriteString(w, tc.properties)
				case mapDownloadURLPath:
					io.WriteString(w, `{"code":0,"data":"https://storage.example.com/private-MUST_NOT_ESCAPE?signature=MUST_NOT_ESCAPE"}`)
				default:
					t.Error("unexpected endpoint, download or robot action")
				}
			})
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			expectedCalls := int32(3)
			if tc.properties == privateProperties {
				expectedCalls = 4
			}
			if !r.OK || calls.Load() != expectedCalls {
				t.Fatal("diagnostic read failed or request scope changed")
			}
			m := r.Data.(MapMetadata)
			if m.ActiveMapVerified || len(m.Maps) != tc.maps || m.UnsupportedFrames != tc.unsupported || m.Diagnostics == nil || m.Diagnostics.Schema != 2 || m.Diagnostics.Reason != tc.reason {
				t.Fatal("incorrect unverified diagnostic")
			}
			for _, saved := range m.Maps {
				if saved.Active || saved.VerifiedForControl || !saved.MetadataOnly {
					t.Fatal("diagnostic promoted map control")
				}
			}
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), "MUST_NOT_ESCAPE") || strings.Contains(string(raw), "untrusted.invalid") || strings.Contains(string(raw), "synthetic-access-token") {
				t.Fatal("private map reference/frame/key leaked")
			}
		})
	}
}

func TestMapDiagnosticValueShapesAndBounds(t *testing.T) {
	var shapes MapValueShapes
	for _, raw := range []string{`"[\"encoded\"]"`, `[]`, `{}`, `"other"`, `null`, `7`} {
		mapHistoryCandidate(json.RawMessage(raw), &shapes)
	}
	if shapes != (MapValueShapes{ArrayString: 1, Array: 1, Object: 1, StringOther: 1, NullMissing: 1, ScalarOther: 1}) {
		t.Fatal("value shape counters wrong")
	}
	for _, tc := range []struct {
		raw            string
		list           bool
		state, shape   string
		reference, md5 bool
	}{
		{`"synthetic-object-MUST_NOT_ESCAPE"`, false, "present", "string", true, false},
		{`"{\"obj_name\":\"synthetic-object-MUST_NOT_ESCAPE\",\"md5\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"}"`, true, "present", "json_object_string", true, true},
		{`{"object_name":"synthetic-object-MUST_NOT_ESCAPE"}`, true, "present", "object", true, false},
		{`""`, true, "empty", "string", false, false},
		{`null`, true, "empty", "null", false, false},
		{`[]`, true, "invalid", "array", false, false},
		{`"private-not-json-MUST_NOT_ESCAPE"`, true, "invalid", "string", false, false},
		{`"` + strings.Repeat("x", (16<<10)+1) + `"`, true, "invalid", "other", false, false},
	} {
		got := inspectMapProperty(json.RawMessage(tc.raw), tc.list)
		if got.State != tc.state || got.Shape != tc.shape || got.Reference != tc.reference || got.MD5 != tc.md5 {
			t.Fatal("unsafe/incorrect property shape")
		}
	}
	values := make([]any, 21)
	for i := range values {
		values[i] = "synthetic-private"
	}
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case listPath:
			io.WriteString(w, deviceList)
		case historyPath:
			io.WriteString(w, syntheticHistory(values...))
		default:
			assertFixedMapDiagnosticRPC(t, r)
			io.WriteString(w, `{"code":0,"data":{"result":[{"siid":6,"piid":99,"code":0,"value":"MUST_NOT_ESCAPE"}]}}`)
		}
	})
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	d := r.Data.(MapMetadata).Diagnostics
	if d.History.Outcome != "response_limit" || d.History.Records != 0 || d.Properties.Outcome != "response_invalid_unknown_selector" || d.Properties.MapList.State != "unavailable" {
		t.Fatal("diagnostic limits/unknown selectors ignored")
	}
}

func TestMapDiagnosticsKeepReauthenticationAndForeignDeviceBoundary(t *testing.T) {
	for _, unauthorized := range []string{"history", "properties"} {
		t.Run(unauthorized, func(t *testing.T) {
			var calls atomic.Int32
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, deviceList)
				case historyPath:
					if unauthorized == "history" {
						w.WriteHeader(401)
					} else {
						io.WriteString(w, syntheticHistory())
					}
				default:
					assertFixedMapDiagnosticRPC(t, r)
					w.WriteHeader(401)
				}
			})
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if r.OK || r.Error == nil || r.Error.Code != "session_reauthentication_required" {
				t.Fatal("authentication failure hidden in diagnostics")
			}
			before := calls.Load()
			r = c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "foreign-device"})
			if r.OK || r.Error.Code != "device_not_found" || calls.Load() != before+1 {
				t.Fatal("foreign device reached map reads")
			}
		})
	}
}
