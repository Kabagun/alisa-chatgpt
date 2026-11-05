package cloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"alisa-dreame-readonly/internal/bridge"
)

type memoryControlStore struct {
	preview          privatePreview
	last             int64
	saveCount, marks int
	failMark         bool
}

func (s *memoryControlStore) lock(string) (func(), error)         { return func() {}, nil }
func (s *memoryControlStore) load(string) (privatePreview, error) { return s.preview, nil }
func (s *memoryControlStore) save(p privatePreview) error         { s.preview = p; s.saveCount++; return nil }
func (s *memoryControlStore) lastAttempt(string) (int64, error)   { return s.last, nil }
func (s *memoryControlStore) markAttempt(_ string, at int64) error {
	if s.failMark {
		return sessionError("control_state_unavailable")
	}
	s.last = at
	s.marks++
	return nil
}

func TestControlSingleDispatchAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, mutation, outcome, reason string
		running, selection              bool
	}{
		{"accepted", "", "accepted", "start_unconfirmed", false, false},
		{"started", "", "started", "device_reported_running", true, true},
		{"running_unconfirmed", "", "segment_task_running_room_unconfirmed", "selected_room_unconfirmed", true, false},
		{"rejected", "rejected", "rejected", "device_rejected", false, false},
		{"unknown", "unknown", "unknown", "reply_unconfirmed", false, false},
		{"unknown_running", "unknown", "observed_running_reply_unknown", "device_reported_running_reply_unknown", true, true},
		{"changed", "changed", "not_sent", "plan_changed", false, false},
		{"stale", "stale", "not_sent", "preview_required", false, false},
		{"cooldown", "cooldown", "not_sent", "cooldown", false, false},
		{"marker_failure", "marker_failure", "not_sent", "control_state_unavailable", false, false},
		{"post_auth", "post_auth", "accepted", "start_unconfirmed", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryControlStore{}
			physical, mapActions := 0, 0
			changing := false
			list, _ := json.Marshal(map[string]any{"curr_id": 42, "mapstr": []any{map[string]any{"map": savedReadFrame(t, 42, "")}}})
			blob, _ := syntheticBlobReader(t, func(w http.ResponseWriter, r *http.Request) { w.Write(list) })
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case listPath:
					io.WriteString(w, strings.Replace(deviceList, "dreame.vacuum.synthetic", "dreame.vacuum.r2422", 1))
				case historyPath:
					io.WriteString(w, syntheticHistory())
				case mapDownloadURLPath:
					io.WriteString(w, `{"code":0,"data":"https://storage.example.com/list?signature=MUST_NOT_ESCAPE"}`)
				case "/dreame-iot-com-shard-1/device/sendCommand":
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					data := body["data"].(map[string]any)
					if data["method"] == "action" {
						params := data["params"].(map[string]any)
						if params["siid"] == float64(4) {
							physical++
							if store.marks != 1 {
								t.Fatal("physical send preceded cooldown marker")
							}
							b, _ := json.Marshal(params)
							if string(b) != `{"aiid":1,"did":"fixture-vacuum-1","in":[{"piid":1,"value":18},{"piid":10,"value":"{\"selects\":[[3,1,1,2,1]]}"}],"siid":4}` {
								t.Fatal("nonfixed physical payload")
							}
							if tc.mutation == "unknown" {
								io.WriteString(w, `{"code":0,"data":{}}`)
								return
							}
							code := 0
							if tc.mutation == "rejected" {
								code = 5
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"result": map[string]any{"code": code, "did": "fixture-vacuum-1", "siid": 4, "aiid": 1, "id": body["id"]}}})
							return
						}
						if params["siid"] != float64(6) || params["aiid"] != float64(1) {
							t.Fatal("unapproved action")
						}
						mapActions++
						extra := map[string]any{}
						if physical > 0 && tc.selection {
							extra["sa"] = []any{[]int{3, 1}}
						}
						frame := currentFrameText(t, 77, 42, extra, "", false)
						_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"result": map[string]any{"code": 0, "out": []any{map[string]any{"piid": 1, "value": frame}}}}})
						return
					}
					if data["method"] != "get_properties" {
						t.Fatal("unknown RPC")
					}
					params := data["params"].([]any)
					if response, ok := syntheticReadinessResponse(params); ok {
						if physical > 0 && tc.mutation == "post_auth" {
							w.WriteHeader(401)
							return
						}
						var env map[string]any
						_ = json.Unmarshal(response, &env)
						rows := env["data"].(map[string]any)["result"].([]any)
						if len(params) == 6 && physical > 0 && tc.running {
							for _, item := range rows {
								p := item.(map[string]any)
								if p["siid"] == float64(2) && p["piid"] == float64(1) {
									p["value"] = 1
								}
								if p["siid"] == float64(4) && p["piid"] == float64(1) {
									p["value"] = 18
								}
								if p["siid"] == float64(4) && p["piid"] == float64(7) {
									p["value"] = 3
								}
							}
						}
						if len(params) == 4 && changing && tc.mutation == "changed" {
							rows[0].(map[string]any)["value"] = 2
						}
						_ = json.NewEncoder(w).Encode(env)
						return
					}
					if len(params) == 2 {
						io.WriteString(w, savedReadProperties(t))
						return
					}
					if len(params) == 1 && params[0].(map[string]any)["siid"] == float64(13) {
						io.WriteString(w, `{"code":0,"data":{"result":[{"siid":13,"piid":1,"code":-4001}]}}`)
						return
					}
					t.Fatal("unknown/combined property selectors")
				default:
					t.Fatal("unapproved endpoint")
				}
			})
			profile := syntheticMapProfile()
			profile.ModelIVs = map[string]string{"dreame.vacuum.r2422": "synthetic-IV-123"}
			profile.ModelVersions = map[string]int{"dreame.vacuum.r2422": 1}
			_ = c.SetMapReadProfile(profile)
			c.mapBlobs = blob
			c.controlStore = store
			c.mapPollWait = func(_ context.Context, d time.Duration) error {
				if d < 2*time.Second {
					t.Fatal("confirmation spacing")
				}
				return nil
			}
			read := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"})
			if !read.OK || !read.Data.(MapMetadata).CleaningReadiness.VerifiedForControl || store.saveCount != 1 {
				t.Fatal("protected preview not created")
			}
			if tc.mutation == "stale" {
				store.preview.CreatedAt -= previewTTL.Milliseconds() + 1
			}
			if tc.mutation == "cooldown" {
				store.last = c.now().UnixMilli() - 1000
			}
			if tc.mutation == "marker_failure" {
				store.failMark = true
			}
			changing = true
			req := ControlRequest{1, "clean_rooms", "fixture-vacuum-1", "42", []string{"3"}}
			response := c.ExecuteControl(context.Background(), req)
			if !response.OK || response.ReadOnly || response.Mode != "dreame_control" {
				t.Fatal("control envelope")
			}
			out := response.Data.(ControlResult)
			if out.Outcome != tc.outcome || out.Reason != tc.reason || physical > 1 || store.saveCount != 1 || out.AutomaticRetry {
				t.Fatalf("control result %s/%s", out.Outcome, out.Reason)
			}
			if out.Attempted != (physical == 1) || mapActions > 3 {
				t.Fatal("attempt/action scope")
			}
			raw, _ := json.Marshal(response)
			for _, s := range []string{"MUST_NOT_ESCAPE", "selects", "synthetic-IV", "synthetic-owner", "storage.example.com"} {
				if strings.Contains(string(raw), s) {
					t.Fatal("control private data leaked")
				}
			}
			if physical == 1 {
				again := c.ExecuteControl(context.Background(), req).Data.(ControlResult)
				if again.Reason != "cooldown" || physical != 1 {
					t.Fatal("duplicate physical retry")
				}
			}
		})
	}
}
func TestStrictControlRequestAndAcknowledgement(t *testing.T) {
	valid := `{"schema":1,"op":"clean_rooms","device_id":"fixture-1","map_id":"-2","room_ids":["3"]}`
	if _, err := DecodeControlRequest(strings.NewReader(valid)); err != nil {
		t.Fatal("strict request rejected")
	}
	for _, raw := range []string{strings.Replace(valid, `"3"`, `"03"`, 1), strings.Replace(valid, `["3"]`, `["3","4"]`, 1), strings.Replace(valid, `"-2"`, `"02"`, 1), strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1), strings.Replace(valid, `"op":`, `"approval":true,"op":`, 1), valid + `{}`, strings.Repeat("x", 4097)} {
		if _, err := DecodeControlRequest(strings.NewReader(raw)); err == nil {
			t.Fatal("unsafe control request")
		}
	}
	for _, tc := range []struct{ result, want string }{{`{"code":0}`, "accepted"}, {`[{"code":0}]`, "accepted"}, {`{"code":5}`, "rejected"}, {`[]`, "unknown"}, {`[{"code":0},{"code":0}]`, "unknown"}, {`{"code":true}`, "unknown"}, {`{"code":null}`, "unknown"}, {`{"code":5,"code":0}`, "unknown"}, {`{"code":0,"did":"other"}`, "unknown"}, {`{"code":0,"siid":6}`, "unknown"}, {`{"code":0,"id":5}`, "unknown"}} {
		raw := []byte(`{"data":{"id":7,"result":` + tc.result + `}}`)
		if parseStartAcknowledgement(raw, "fixture-1", 7) != tc.want {
			t.Fatal("ack validation")
		}
	}
	c, _ := mockClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("normal read accepted control") })
	r := c.Execute(context.Background(), bridge.Request{Schema: 1, Op: "clean_rooms", Device: "fixture-1"})
	if r.OK || r.Error.Code != "operation_denied" {
		t.Fatal("normal read control bypass")
	}
	for _, n := range []int{0, 64} {
		req := ControlRequest{1, "clean_rooms", "fixture-1", "1", []string{strconv.Itoa(n)}}
		if c.ExecuteControl(context.Background(), req).OK {
			t.Fatal("segment bound")
		}
	}
}
