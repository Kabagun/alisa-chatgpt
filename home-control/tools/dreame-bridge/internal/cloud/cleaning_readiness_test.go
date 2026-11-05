package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/savedmap"
)

func syntheticReadinessResponse(params []any) ([]byte, bool) {
	values := map[string]int{"state": 2, "error": 0, "battery_percent": 82, "charging_status": 2, "status": 0, "task_status": 0, "fan": 1, "water": 2, "mode": 0, "tank": 0, "customized": 0, "cleaning_paused": 0}
	for _, selectors := range [][]readinessSelector{readinessState, readinessGlobals, readinessCustom, readinessPaused} {
		want, _ := json.Marshal(readinessParams(selectors))
		got, _ := json.Marshal(params)
		if string(want) != string(got) {
			continue
		}
		rows := []any{}
		for _, s := range selectors {
			rows = append(rows, map[string]any{"siid": s.siid, "piid": s.piid, "code": 0, "value": values[s.name]})
		}
		raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"result": rows}})
		return raw, true
	}
	return nil, false
}
func syntheticReadinessMap(t *testing.T, now int64) MapMetadata {
	t.Helper()
	f, err := savedmap.DecodeFrame(currentFrameText(t, 77, 42, nil, "", false), "", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	return MapMetadata{ActiveMapVerified: true, SelectedSavedMapID: "42", Maps: []bridge.Rooms{{MapID: 42, MapVersion: 1, Active: true, MetadataOnly: true, Items: []bridge.Room{{SegmentID: 3, Name: "Synthetic kitchen"}}}}, Diagnostics: &MapDiagnostics{CurrentMap: &CurrentMapDiagnostics{Freshness: "action_response"}}, currentProof: &privateCurrentProof{frame: f, readAt: now}}
}
func TestCleaningReadinessFixedReadsAndDraftOnly(t *testing.T) {
	for _, tc := range []struct{ name, mutation, outcome string }{
		{"valid", "", "draft_available"}, {"customized", "custom", "cleanplan_per_room_required"}, {"custom_invalid", "custom_invalid", "cleanplan_observation_invalid"}, {"paused", "paused", "cleanplan_state_not_resting"}, {"global_missing", "global_missing", "cleanplan_observation_missing"}, {"mode_conflict", "tank", "cleanplan_settings_conflict"}, {"auth_soft", "auth", "cleanplan_observation_missing"}, {"identity", "identity", "cleanplan_identity_invalid"}, {"stale", "stale", "cleanplan_snapshot_stale"}, {"ambiguous", "ambiguous", "cleanplan_catalog_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c, _ := mockClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/dreame-iot-com-shard-1/device/sendCommand" {
					t.Error("nonfixed endpoint")
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				data := body["data"].(map[string]any)
				if data["method"] != "get_properties" {
					t.Fatal("action/write attempted")
				}
				params := data["params"].([]any)
				response, ok := syntheticReadinessResponse(params)
				if !ok {
					t.Fatal("nonfixed/combined readiness selectors")
				}
				wantSelectors := [][]readinessSelector{readinessState, readinessGlobals, readinessCustom, readinessPaused}
				want, _ := json.Marshal(readinessParams(wantSelectors[calls-1]))
				got, _ := json.Marshal(params)
				if string(want) != string(got) {
					t.Fatal("read order/optional isolation")
				}
				if tc.mutation == "auth" && calls == 2 {
					w.WriteHeader(401)
					return
				}
				var env map[string]any
				_ = json.Unmarshal(response, &env)
				rows := env["data"].(map[string]any)["result"].([]any)
				if calls == 3 && tc.mutation == "custom" {
					rows[0].(map[string]any)["value"] = 1
				}
				if calls == 3 && tc.mutation == "custom_invalid" {
					env["data"] = map[string]any{"result": map[string]any{"private": "MUST_NOT_ESCAPE"}}
				}
				if calls == 4 && tc.mutation == "paused" {
					rows[0].(map[string]any)["value"] = 1
				}
				if calls == 2 && tc.mutation == "tank" {
					rows[3].(map[string]any)["value"] = 1
				}
				if calls == 2 && tc.mutation == "global_missing" {
					rows[0].(map[string]any)["value"] = nil
				}
				_ = json.NewEncoder(w).Encode(env)
			})
			at := c.now().UnixMilli()
			m := syntheticReadinessMap(t, at)
			record := deviceRecord{UID: json.RawMessage(`"synthetic-owner"`), Model: "dreame.vacuum.r2422", Domain: "shard-1.iot.dreame.tech"}
			if tc.mutation == "identity" {
				record.UID = json.RawMessage(`"unsafe@example.com"`)
			}
			if tc.mutation == "stale" {
				at -= 31_000
			}
			matches := 1
			if tc.mutation == "ambiguous" {
				matches = 2
			}
			c.readCleaningReadiness(context.Background(), "fixture-vacuum-1", record, at, matches, &m)
			if calls != 4 || m.CleaningReadiness == nil || m.CleaningReadiness.Outcome != tc.outcome || m.CleaningReadiness.VerifiedForControl || !m.ActiveMapVerified || m.Maps[0].VerifiedForControl {
				t.Fatalf("readiness outcome/scope: %s", m.CleaningReadiness.Outcome)
			}
			r := m.CleaningReadiness
			if r.State.Drainage.Quality != "unsupported" || r.State.Drainage.Value != nil {
				t.Fatal("no-drain source policy")
			}
			if tc.outcome == "draft_available" {
				if len(r.Drafts) != 1 || r.Drafts[0].SegmentID != 3 || r.Drafts[0].Preview.Kind != "DRAFT" || r.Drafts[0].Preview.FirmwareAcceptance != "unverified" {
					t.Fatal("draft preview")
				}
			} else if len(r.Drafts) != 0 {
				t.Fatal("unavailable leaked draft")
			}
			raw, _ := json.Marshal(r)
			for _, s := range []string{"synthetic-owner", "fixture-vacuum-1", "selects", "payload", "MUST_NOT_ESCAPE", "snapshot", "syntheticKey"} {
				if strings.Contains(string(raw), s) && s != "snapshot" {
					t.Fatal("private data/action payload leaked")
				}
			}
			if tc.mutation == "custom_invalid" && (r.Settings.Fan.Quality != "observed" || r.Reads.Customized != "response_invalid") {
				t.Fatal("optional poisoned required batch")
			}
		})
	}
}
func TestReadinessQualitiesNoDefaults(t *testing.T) {
	for _, tc := range []struct{ row, quality string }{
		{`{"siid":4,"piid":26,"code":0,"value":0}`, "observed"},
		{`{"siid":4,"piid":26,"code":0,"value":null}`, "missing"},
		{`{"siid":4,"piid":26,"code":-4001}`, "invalid"},
		{`{"siid":4,"piid":26,"code":0,"value":false}`, "invalid"},
		{`{"siid":4,"piid":26,"code":0,"value":2147483648}`, "invalid"},
	} {
		v, o := parseReadinessValues([]byte(`{"data":{"result":[`+tc.row+`]}}`), readinessCustom)
		if o != "ok" || v["customized"].Quality != tc.quality || (v["customized"].Value != nil) != (tc.quality == "observed") {
			t.Fatal("quality/coercion")
		}
	}
	for _, raw := range []string{`{"data":{}}`, `{"data":{"result":[{"siid":4,"piid":26},{"siid":4,"piid":26}]}}`, `{"data":{"result":[{"siid":4,"piid":17}]}}`} {
		v, o := parseReadinessValues([]byte(raw), readinessCustom)
		if o != "response_invalid" || v["customized"].Quality != "invalid" {
			t.Fatal("invalid shape accepted")
		}
	}
	if !reflect.DeepEqual(readinessParams(readinessState), bridge.StatusReadParams()) {
		t.Fatal("status baseline changed")
	}
}
func TestReadinessGatesAndDeadline(t *testing.T) {
	c, _ := mockClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unverified map made request") })
	m := MapMetadata{}
	c.readCleaningReadiness(context.Background(), "fixture", deviceRecord{}, 0, 0, &m)
	if m.CleaningReadiness != nil {
		t.Fatal("unverified map readiness")
	}
	m = syntheticReadinessMap(t, c.now().UnixMilli())
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c.readCleaningReadiness(ctx, "fixture", deviceRecord{Domain: "shard-1.iot.dreame.tech", UID: json.RawMessage(`"owner"`), Model: "dreame.vacuum.r2422"}, c.now().UnixMilli(), 1, &m)
	if m.CleaningReadiness == nil || len(m.CleaningReadiness.Drafts) != 0 || m.CleaningReadiness.Reads.State != "request_unavailable" {
		t.Fatal("deadline escaped/guessed settings")
	}
}
