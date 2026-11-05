//go:build linux

package dreame

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-home-plugin/internal/provider"
)

const controlFixture = `{"schema":1,"device_id":"fixture-1","map_id":"42","room_ids":["3"],"observed_at":"2026-10-31T12:00:00Z","outcome":"accepted","reason":"start_unconfirmed","attempted":true,"acknowledgement":"accepted","segment_task_running":false,"selected_room_confirmed":false,"map_binding_unchanged":false,"status_reads":1,"map_refreshes":0,"automatic_retry":false}`

func TestControlProcessNeverRetriesAndCleansProfile(t *testing.T) {
	for _, modeName := range []string{"control_ok", "control_bad", "control_timeout", "control_expired"} {
		t.Run(modeName, func(t *testing.T) {
			a, dir := setup(t)
			prefix := `if [ "${1-unset}" = --control ]; then
[ "$#" = 5 ]; [ "$2" = --session ]; [ "$4" = --map-profile ]
session=$3; dir=${session%/*}; case "$5" in "$dir"/.map-profile-*.json) ;; *) exit 94;; esac
[ -r "$5" ]; [ "${DREAME_TEST_SECRET-unset}" = unset ]; payload=$(cat)
[ "$payload" = '{"schema":1,"op":"clean_rooms","device_id":"fixture-1","map_id":"42","room_ids":["3"]}' ]
printf 'control\n' >> "$dir/events"
mode=$(cat "$dir/mode")
if [ "$mode" = control_timeout ]; then sleep 2; exit 1; fi
if [ "$mode" = control_bad ]; then printf 'MUST_NOT_ESCAPE'; exit 1; fi
if [ "$mode" = control_expired ]; then printf '%s' '{"schema":1,"mode":"dreame_control","read_only":false,"ok":false,"error":{"code":"session_expired"}}'; exit 1; fi
printf '%s' '{"schema":1,"mode":"dreame_control","read_only":false,"ok":true,"data":` + controlFixture + `}'
exit 0
fi
`
			write(t, a.reader, strings.Replace(helper("reader"), "set -eu\n", "set -eu\n"+prefix, 1), 0700)
			mode(t, dir, modeName)
			t.Setenv("DREAME_TEST_SECRET", "MUST_NOT_ESCAPE")
			if modeName == "control_timeout" {
				a.timeout = 80 * time.Millisecond
			}
			p := provider.Params{DeviceID: "fixture-1", MapID: "42", RoomIDs: []string{"3"}}
			out, err := a.Execute(context.Background(), dir, provider.CleanRooms, p)
			if err != nil || strings.Contains(string(out), "MUST_NOT_ESCAPE") || events(t, dir) != "devices\ncontrol\n" {
				t.Fatal("control retry/refresh/leak")
			}
			want := "unknown"
			if modeName == "control_ok" {
				want = "accepted"
			}
			if !strings.Contains(string(out), `"outcome":"`+want+`"`) {
				t.Fatal("unknown/accepted semantics")
			}
			paths, _ := filepath.Glob(filepath.Join(dir, ".map-profile-*"))
			if len(paths) != 0 {
				t.Fatal("control profile leaked")
			}
		})
	}
}
func TestControlResultStrictShapeAndNoCompletionClaim(t *testing.T) {
	p := provider.Params{DeviceID: "fixture-1", MapID: "42", RoomIDs: []string{"3"}}
	if out, err := sanitizeControlResult([]byte(controlFixture), p); err != nil || !strings.Contains(string(out), `"outcome":"accepted"`) {
		t.Fatal("valid acceptance")
	}
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["outcome"] = "completed" }, func(v map[string]any) { v["device_id"] = "foreign" }, func(v map[string]any) { v["room_ids"] = []string{"4"} }, func(v map[string]any) { v["automatic_retry"] = true }, func(v map[string]any) { v["selected_room_confirmed"] = true }, func(v map[string]any) { v["acknowledgement"] = "MUST_NOT_ESCAPE" }, func(v map[string]any) { v["payload"] = "MUST_NOT_ESCAPE" }, func(v map[string]any) { v["status_reads"] = 7 }, func(v map[string]any) { v["outcome"] = "started" }, func(v map[string]any) { delete(v, "attempted") },
	} {
		var v map[string]any
		_ = json.Unmarshal([]byte(controlFixture), &v)
		mutate(v)
		raw, _ := json.Marshal(v)
		out, err := sanitizeControlResult(raw, p)
		if err != nil || !strings.Contains(string(out), `"outcome":"unknown"`) || strings.Contains(string(out), "MUST_NOT_ESCAPE") {
			t.Fatal("unsafe result not normalized unknown")
		}
	}
	for _, p := range []provider.Params{{MapID: "42", RoomIDs: []string{"03"}}, {MapID: "42", RoomIDs: []string{"3", "4"}}, {MapID: "042", RoomIDs: []string{"3"}}, {MapID: "42", RoomIDs: []string{"64"}}} {
		if validControlParams(p) {
			t.Fatal("control shape bound")
		}
	}
}
func TestInvalidControlShapeDoesNotInvokeHelper(t *testing.T) {
	a, dir := setup(t)
	_, err := a.Execute(context.Background(), dir, provider.CleanRooms, provider.Params{DeviceID: "fixture-1", MapID: "42", RoomIDs: []string{"3", "4"}})
	code(t, err, "invalid_params")
	if events(t, dir) != "" {
		t.Fatal("invalid params helper invocation")
	}
	_ = os.Remove(filepath.Join(dir, "mode"))
}
