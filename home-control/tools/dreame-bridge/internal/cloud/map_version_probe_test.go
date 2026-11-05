package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"alisa-dreame-readonly/internal/bridge"
)

func TestOptionalMapVersionProbeCannotPoisonProvenDescriptorRead(t *testing.T) {
	frame := savedReadFrame(t, 42, "")
	list, _ := json.Marshal(map[string]any{"curr_id": 42, "mapstr": []any{map[string]any{"map": frame}}})
	for _, tc := range []struct {
		name, body, quality string
		status              int
	}{
		{"unsupported_no_value", `{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":-4001}]}}`, "property_unavailable", 200},
		{"malformed_result_shape", `{"code":0,"data":{"result":{"private":"MUST_NOT_ESCAPE"}}}`, "response_invalid", 200},
		{"wrong_selector", `{"code":0,"data":{"result":[{"siid":13,"piid":9,"code":0,"value":0}]}}`, "response_invalid", 200},
		{"extra_rows", `{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":0,"value":0},{"siid":13,"piid":1,"code":0,"value":0}]}}`, "response_invalid", 200},
		{"http_failure", `private`, "request_unavailable", 400},
		{"optional_unauthenticated", `private`, "request_unavailable", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var coreCalls, probeCalls, locators, gets atomic.Int32
			blob, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) { gets.Add(1); w.Write(list) })
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, deviceList)
				case historyPath:
					io.WriteString(w, syntheticHistory())
				case "/dreame-iot-com-shard-1/device/sendCommand":
					var body struct {
						Data struct {
							Method string                     `json:"method"`
							Params []struct{ SIID, PIID int } `json:"params"`
						} `json:"data"`
					}
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.Data.Method != "get_properties" {
						t.Fatal("non-read request")
					}
					p := body.Data.Params
					if len(p) == 2 && p[0].SIID == 6 && p[0].PIID == 3 && p[1].SIID == 6 && p[1].PIID == 8 {
						coreCalls.Add(1)
						io.WriteString(w, savedReadProperties(t))
						return
					}
					if len(p) != 1 || p[0].SIID != 13 || p[0].PIID != 1 {
						t.Fatal("optional property batched with core descriptor")
					}
					probeCalls.Add(1)
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				case mapDownloadURLPath:
					locators.Add(1)
					io.WriteString(w, `{"code":0,"data":"https://storage.example.com/list?signature=synthetic"}`)
				default:
					t.Error("unexpected path or action")
				}
			})
			_ = c.SetMapReadProfile(syntheticMapProfile())
			c.mapBlobs = blob
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if !r.OK {
				t.Fatal(r.Error)
			}
			m := r.Data.(MapMetadata)
			s := m.Diagnostics.SavedMaps
			if coreCalls.Load() != 1 || probeCalls.Load() != 1 || locators.Load() != 1 || gets.Load() != 1 || m.Diagnostics.Properties.Outcome != "ok" || !m.Diagnostics.Properties.MapList.Reference || len(m.Maps) != 1 || m.SelectedSavedMapID != "42" {
				t.Fatal("optional failure erased core reference or blocked saved-map retrieval")
			}
			if s.MapSavingProbe != tc.quality || s.VersionHint != 1 || s.VersionSource != "model_default" || m.ActiveMapVerified || m.Maps[0].VerifiedForControl {
				t.Fatal("probe failure lost provenance or promoted verification")
			}
		})
	}
}

func TestOptionalMapVersionProbeIsCachedPerOperation(t *testing.T) {
	var calls atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":0,"value":false}]}}`)
	})
	d := newMapDiagnostics()
	c.readMapVersionProbe(context.Background(), "fixture-vacuum-1", "shard-1.iot.dreame.tech", d)
	c.readMapVersionProbe(context.Background(), "fixture-vacuum-1", "shard-1.iot.dreame.tech", d)
	if calls.Load() != 1 || d.mapSavingProbe != "value_present" || !d.mapSavingValue {
		t.Fatal("optional probe repeated or nonnullfalse lost")
	}
}

func TestCoreMapResponseInvalidHasFixedDiagnosticSubcodes(t *testing.T) {
	for _, tc := range []struct{ body, code string }{
		{`{"data":{}}`, "response_invalid_envelope"},
		{`{"code":0,"data":{"result":{"private":"MUST_NOT_ESCAPE"}}}`, "response_invalid_json"},
		{`{"code":0,"data":{}}`, "response_invalid_missing_result"},
		{`{"code":0,"data":{"result":[{},{},{}]}}`, "response_invalid_many_rows"},
		{`{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":0,"value":0}]}}`, "response_invalid_unknown_selector"},
		{`{"code":0,"data":{"result":[{"siid":6,"piid":3,"code":-1},{"siid":6,"piid":3,"code":-1}]}}`, "response_invalid_duplicate_selector"},
	} {
		c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
			assertFixedMapDiagnosticRPC(t, r)
			io.WriteString(w, tc.body)
		})
		d := newMapDiagnostics()
		reference, err := c.readMapDiagnosticProperties(context.Background(), "fixture-vacuum-1", "shard-1.iot.dreame.tech", d)
		if err != nil || reference.name != "" || d.Properties.Outcome != tc.code || d.Properties.MapList.State != "unavailable" || d.Properties.ObjectName.State != "unavailable" {
			t.Fatal("invalid core response not safely classified")
		}
	}
}
