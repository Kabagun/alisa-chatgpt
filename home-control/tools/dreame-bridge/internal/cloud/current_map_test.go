package cloud

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"alisa-dreame-readonly/internal/bridge"
)

func currentFrameText(t *testing.T, outerID, savedID int, extra map[string]any, key string, empty bool) string {
	t.Helper()
	meta := map[string]any{"ris": 2}
	if savedID >= 0 {
		meta["rism"] = savedReadFrame(t, savedID, "")
	}
	for k, v := range extra {
		if v == nil {
			delete(meta, k)
		} else {
			meta[k] = v
		}
	}
	tail, _ := json.Marshal(meta)
	raw := make([]byte, 31)
	binary.LittleEndian.PutUint16(raw, uint16(outerID))
	binary.LittleEndian.PutUint16(raw[2:], 7)
	raw[4] = 'I'
	binary.LittleEndian.PutUint16(raw[19:], 2)
	binary.LittleEndian.PutUint16(raw[21:], 2)
	if !empty {
		raw[27] = 3
	}
	raw = append(raw, tail...)
	var out bytes.Buffer
	z := zlib.NewWriter(&out)
	_, _ = z.Write(raw)
	_ = z.Close()
	data := out.Bytes()
	if key != "" {
		for len(data)%16 != 0 {
			data = append(data, 0xa5)
		}
		sum := sha256.Sum256([]byte(key))
		block, err := aes.NewCipher([]byte(hex.EncodeToString(sum[:])[:32]))
		if err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, []byte("synthetic-IV-123")).CryptBlocks(data, data)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestCurrentMapFixedActionAndStrictBinding(t *testing.T) {
	for _, tc := range []struct {
		name                                                                              string
		outcome                                                                           string
		extra                                                                             map[string]any
		savedID, actionStatus                                                             int
		empty, reference, changeDescriptor, dualConflict, ackOnly, stale, unknownBaseline bool
	}{
		{name: "inline_verified", outcome: "active_map_verified", savedID: 42},
		{name: "selection_valid", outcome: "active_map_verified", savedID: 42, extra: map[string]any{"sa": []any{[]int{3, 1}}}},
		{name: "selection_invalid", outcome: "active_map_verified", savedID: 42, extra: map[string]any{"sa": "unsupported-shape"}},
		{name: "reference_verified", outcome: "active_map_verified", savedID: 42, reference: true},
		{name: "status_missing", outcome: "format_unverified", savedID: 42, extra: map[string]any{"ris": nil}},
		{name: "temporary", outcome: "format_unverified", savedID: 42, extra: map[string]any{"suw": 6}},
		{name: "new", outcome: "format_unverified", savedID: 42, extra: map[string]any{"risp": 0}},
		{name: "restored", outcome: "format_unverified", savedID: 42, extra: map[string]any{"rpur": 1}},
		{name: "empty", outcome: "format_unverified", savedID: 42, empty: true},
		{name: "markers_invalid", outcome: "markers_invalid", savedID: 42, extra: map[string]any{"suw": []int{1}}},
		{name: "link_mismatch", outcome: "linkage_mismatch", savedID: 11},
		{name: "no_outer_id_fallback", outcome: "linkage_missing", savedID: -1},
		{name: "descriptor_changed", outcome: "descriptor_changed", savedID: 42, changeDescriptor: true},
		{name: "dual_conflict", outcome: "candidate_conflict", savedID: 42, reference: true, dualConflict: true},
		{name: "changed_poll", outcome: "active_map_verified", savedID: 42, reference: true, ackOnly: true},
		{name: "stale_poll", outcome: "stale_reference", savedID: 42, ackOnly: true, stale: true},
		{name: "unknown_baseline", outcome: "freshness_unverified", savedID: 42, ackOnly: true, unknownBaseline: true},
		{name: "action_auth_no_replay", outcome: "action_unavailable", savedID: 42, actionStatus: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := ""
			if tc.reference || tc.ackOnly {
				key = "syntheticKey"
			}
			outerID := 77
			if tc.savedID < 0 {
				outerID = 42
			}
			current := currentFrameText(t, outerID, tc.savedID, tc.extra, key, tc.empty)
			inline := current
			if tc.reference || tc.ackOnly {
				inline = ""
			}
			if tc.dualConflict {
				inline = currentFrameText(t, 77, 11, nil, "", false)
			}
			list, _ := json.Marshal(map[string]any{"curr_id": 42, "mapstr": []any{map[string]any{"map": savedReadFrame(t, 42, "")}}})
			var actionCalls, coreCalls, polls, blobGets atomic.Int32
			blob, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) {
				blobGets.Add(1)
				switch r.URL.Path {
				case "/list":
					w.Write(list)
				case "/current":
					io.WriteString(w, current)
				default:
					t.Error("unrelated blob fetched")
				}
			})
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, strings.Replace(deviceList, "dreame.vacuum.synthetic", "dreame.vacuum.r2422", 1))
				case historyPath:
					io.WriteString(w, syntheticHistory())
				case mapDownloadURLPath:
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					path := "/list"
					if body["filename"] != "synthetic-list-reference" {
						if body["filename"] != "current-object" {
							t.Error("current reference comma suffix not split exactly")
						}
						path = "/current"
					}
					raw, _ := json.Marshal(map[string]any{"code": 0, "data": "https://storage.example.com" + path + "?signature=MUST_NOT_ESCAPE"})
					w.Write(raw)
				case "/dreame-iot-com-shard-1/device/sendCommand":
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Fatal("invalid RPC")
					}
					data := body["data"].(map[string]any)
					if data["method"] == "action" {
						actionCalls.Add(1)
						var want any
						_ = json.Unmarshal([]byte(`{"did":"fixture-vacuum-1","siid":6,"aiid":1,"in":[{"piid":2,"value":"{\"req_type\":1,\"frame_type\":\"I\",\"force_type\":1}"}]}`), &want)
						if data["did"] != "fixture-vacuum-1" || data["id"] != body["id"] || len(data) != 4 || !reflect.DeepEqual(data["params"], want) || len(body) != 5 {
							t.Fatal("nonfixed action, movement/cleaning payload or request identity")
						}
						if tc.actionStatus != 0 {
							w.WriteHeader(tc.actionStatus)
							io.WriteString(w, "private")
							return
						}
						outputs := []any{}
						if !tc.ackOnly {
							if inline != "" {
								outputs = append(outputs, map[string]any{"piid": 1, "value": inline})
							}
							if tc.reference {
								outputs = append(outputs, map[string]any{"piid": 3, "value": "current-object,syntheticKey"})
							}
						}
						response, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": map[string]any{"code": 0, "did": "fixture-vacuum-1", "siid": 6, "aiid": 1, "id": body["id"], "out": outputs}}})
						w.Write(response)
						return
					}
					if data["method"] != "get_properties" {
						t.Fatal("unexpected RPC method")
					}
					params := data["params"].([]any)
					if response, ok := syntheticReadinessResponse(params); ok {
						w.Write(response)
						return
					}
					if len(params) == 2 {
						core := coreCalls.Add(1)
						descriptor := `{"object_name":"synthetic-list-reference","md5":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
						if core > 1 && tc.changeDescriptor {
							descriptor = `{"object_name":"synthetic-list-reference","md5":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`
						}
						object := map[string]any{"siid": 6, "piid": 3, "code": 0, "value": "old-object,oldKey"}
						if tc.unknownBaseline {
							object = map[string]any{"siid": 6, "piid": 3, "code": -4001}
						}
						raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": []any{object, map[string]any{"siid": 6, "piid": 8, "code": 0, "value": descriptor}}}})
						w.Write(raw)
						return
					}
					if len(params) != 1 {
						t.Fatal("combined/unknown selectors")
					}
					p := params[0].(map[string]any)
					if p["siid"] == float64(13) && p["piid"] == float64(1) {
						io.WriteString(w, `{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":-4001}]}}`)
						return
					}
					if p["siid"] != float64(6) || p["piid"] != float64(3) || p["did"] != "78" {
						t.Fatal("nonfixed passive poll")
					}
					polls.Add(1)
					ref := "current-object,syntheticKey"
					if tc.stale {
						ref = "old-object,oldKey"
					}
					raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": []any{map[string]any{"siid": 6, "piid": 3, "code": 0, "value": ref}}}})
					w.Write(raw)
				default:
					t.Error("unexpected endpoint or action")
				}
			})
			profile := syntheticMapProfile()
			profile.ModelIVs = map[string]string{"dreame.vacuum.r2422": "synthetic-IV-123"}
			profile.ModelVersions = map[string]int{"dreame.vacuum.r2422": 1}
			_ = c.SetMapReadProfile(profile)
			c.mapBlobs = blob
			var waits int
			c.mapPollWait = func(ctx context.Context, delay time.Duration) error {
				if delay < 2*time.Second {
					t.Fatal("passive polls too close")
				}
				waits++
				return nil
			}
			r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if !r.OK {
				t.Fatal("post-action failure escaped as retryable error")
			}
			m := r.Data.(MapMetadata)
			d := m.Diagnostics.CurrentMap
			verified := tc.outcome == "active_map_verified"
			if d == nil || d.Outcome != tc.outcome || actionCalls.Load() != 1 || d.ActionRequests != 1 || m.ActiveMapVerified != verified || m.Maps[0].Active != verified || m.Maps[0].VerifiedForControl {
				t.Fatal("current binding/action scope incorrect")
			}
			if d.FullFrame {
				if d.Selection == nil {
					t.Fatal("decoded selection diagnostics absent")
				}
				s := d.Selection
				if tc.name == "selection_valid" {
					if !s.Present || !s.Valid || s.Count != 1 {
						t.Fatal("raw selection count")
					}
				} else if tc.name == "selection_invalid" {
					if !s.Present || s.Valid || s.Count != 0 {
						t.Fatal("invalid selection guessed")
					}
				} else if s.Present || s.Valid || s.Count != 0 {
					t.Fatal("absent raw selection guessed")
				}
			} else if d.Selection != nil {
				t.Fatal("undecoded frame has selection")
			}
			if !tc.ackOnly && polls.Load() != 0 || int(polls.Load()) != waits || d.PassivePolls != waits || waits > 3 {
				t.Fatal("passive polling budget changed")
			}
			if tc.stale && waits != 3 {
				t.Fatal("stale polling limit not exercised")
			}
			raw, _ := json.Marshal(r)
			for _, private := range []string{"storage.example.com", "syntheticKey", "synthetic-IV-123", "current-object", "MUST_NOT_ESCAPE", current} {
				if private != "" && strings.Contains(string(raw), private) {
					t.Fatal("current-map private evidence leaked")
				}
			}
		})
	}
}

func TestCurrentRefreshGateAndSharedBudget(t *testing.T) {
	var calls atomic.Int32
	c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); t.Error("gated action reached transport") })
	p := syntheticMapProfile()
	p.ModelIVs = map[string]string{"dreame.vacuum.r2422": "synthetic-IV-123"}
	p.ModelVersions = map[string]int{"dreame.vacuum.r2422": 1}
	_ = c.SetMapReadProfile(p)
	makeResult := func() MapMetadata {
		m := MapMetadata{Maps: []bridge.Rooms{{MapID: 42, MetadataOnly: true}}, SelectedSavedMapID: "42", Diagnostics: newMapDiagnostics()}
		m.Diagnostics.SavedMaps = &SavedMapReadDiagnostics{SelectedMatched: true, VersionHint: 1, Outcome: "selected_map_available"}
		m.Diagnostics.descriptorPresent = true
		return m
	}
	m := makeResult()
	c.refreshCurrentMap(context.Background(), "fixture-vacuum-1", deviceRecord{Model: "dreame.vacuum.other"}, &m)
	if m.Diagnostics.CurrentMap.Outcome != "model_unsupported" || calls.Load() != 0 {
		t.Fatal("cross-model action allowed")
	}
	m = makeResult()
	m.Diagnostics.descriptorPresent = false
	c.refreshCurrentMap(context.Background(), "fixture-vacuum-1", deviceRecord{Model: "dreame.vacuum.r2422"}, &m)
	if m.Diagnostics.CurrentMap.Outcome != "profile_unavailable" || calls.Load() != 0 {
		t.Fatal("missing snapshot accepted")
	}
	m = makeResult()
	m.Diagnostics.SavedMaps.BlobGetAttempts = 4
	d := &CurrentMapDiagnostics{}
	if _, ok := c.currentBlob(context.Background(), privateMapURL{value: "https://storage.example.com/private"}, &m, d); ok || d.Outcome != "budget_exhausted" || m.Diagnostics.SavedMaps.BlobGetAttempts != 4 || m.Diagnostics.SavedMaps.Outcome != "selected_map_available" {
		t.Fatal("current read escaped shared budget or corrupted saved result")
	}
}

func TestCurrentActionRejectedMalformedLegacyAndAuthNeverRetry(t *testing.T) {
	for _, tc := range []struct {
		name, body, outcome string
		status              int
	}{
		{"rejected", `{"code":0,"data":{"result":{"code":-1,"out":[]}}}`, "action_rejected", 200},
		{"missing_out", `{"code":0,"data":{"result":{"code":0}}}`, "action_response_invalid", 200},
		{"foreign_identity", `{"code":0,"data":{"result":{"code":0,"did":"foreign","out":[]}}}`, "action_response_invalid", 200},
		{"legacy", `{"code":0,"data":{"result":{"code":0,"out":[{"piid":13,"value":"legacy-private"}]}}}`, "legacy_unsupported", 200},
		{"failed_output", `{"code":0,"data":{"result":{"code":0,"out":[{"piid":1,"code":-1,"value":"private"}]}}}`, "action_output_failed", 200},
		{"auth", `private`, "action_unavailable", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var actions atomic.Int32
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				actions.Add(1)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			p := syntheticMapProfile()
			p.ModelIVs = map[string]string{"dreame.vacuum.r2422": "synthetic-IV-123"}
			p.ModelVersions = map[string]int{"dreame.vacuum.r2422": 1}
			_ = c.SetMapReadProfile(p)
			m := MapMetadata{Maps: []bridge.Rooms{{MapID: 42, MetadataOnly: true}}, SelectedSavedMapID: "42", Diagnostics: newMapDiagnostics()}
			m.Diagnostics.SavedMaps = &SavedMapReadDiagnostics{SelectedMatched: true, VersionHint: 1}
			m.Diagnostics.descriptorPresent = true
			c.refreshCurrentMap(context.Background(), "fixture-vacuum-1", deviceRecord{Model: "dreame.vacuum.r2422", Domain: "shard-1.iot.dreame.tech"}, &m)
			if actions.Load() != 1 || m.Diagnostics.CurrentMap.Outcome != tc.outcome || m.ActiveMapVerified || m.Maps[0].VerifiedForControl {
				t.Fatal("action failure retried or promoted")
			}
		})
	}
}
