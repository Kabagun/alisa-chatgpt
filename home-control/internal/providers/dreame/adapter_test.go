//go:build linux

package dreame

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"smart-home-plugin/internal/provider"
)

const fakeProfile = `{"upstream_commit":"f5b8472b8e2e4bb3dff7bc2a81a5fd594eca9585","signing_key":"synthetic-key123","basic_authorization":"c3ludGhldGljOm9ubHk=","user_agent":"synthetic-agent","app_version":"1","password_salt":"synthetic-salt12","default_tenant":"synthetic"}`
const catalog = `[{"device_id":"fixture-1","model":"synthetic.vacuum","name":"Synthetic vacuum","access_token":"MUST_NOT_ESCAPE"}]`
const statusFixture = `{"device_id":"fixture-1","observed_at":"2026-10-31T00:00:00Z","values":{"battery_percent":82,"state":1},"unavailable":["error"],"model_note":"raw_baseline_values_require_model_validation","password":"MUST_NOT_ESCAPE"}`
const roomsFixture = `{"device_id":"fixture-1","source":"cloud_history_6.1_metadata_only","active_map_verified":false,"maps":[{"device_id":"fixture-1","map_id":17,"map_version":0,"active_map":false,"metadata_only":true,"verified_for_control":false,"rooms":[{"segment_id":3,"unique_room_id":"synthetic-room","name":"Synthetic kitchen","type_code":4}]}],"unsupported_frames":1,"owner":"MUST_NOT_ESCAPE"}`

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if e := os.WriteFile(p, []byte(s), mode); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(p, mode); e != nil {
		t.Fatal(e)
	}
}
func setup(t *testing.T) (*Adapter, string) {
	t.Helper()
	base := t.TempDir()
	if e := os.Chmod(base, 0700); e != nil {
		t.Fatal(e)
	}
	if i, _ := os.Stat(base); i.Mode().Perm() != 0700 {
		t.Skip("requires POSIX filesystem with exact permissions")
	}
	dir := filepath.Join(base, "account")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	profile := filepath.Join(base, "profile.json")
	write(t, profile, fakeProfile, 0600)
	reader := filepath.Join(base, "reader")
	provisioner := filepath.Join(base, "provisioner")
	write(t, reader, helper("reader"), 0700)
	write(t, provisioner, helper("provisioner"), 0700)
	a, e := New(Config{ReaderPath: reader, ProvisionerPath: provisioner, ProfilePath: profile, Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(dir, "session.json"), `{"synthetic":true}`, 0600)
	return a, dir
}

// Synthetic executable fixtures only: no vendor binary, credential or network.
func helper(kind string) string {
	return `#!/bin/sh
set -eu
[ "${DREAME_TEST_SECRET-unset}" = unset ] || exit 90
case "$*" in *synthetic-password*|*synthetic@example.invalid*) exit 91;; esac
refresh=false
has_map_profile=false
map_profile_path=''
if [ '` + kind + `' = reader ] && [ "$#" = 4 ]; then
 [ "$1" = --session ]; [ "$3" = --map-profile ]; map_profile_path=$4; session=$2; has_map_profile=true
else
if [ "$1" = --refresh ]; then [ "$#" = 3 ]; [ "$2" = --session ]; session=$3; refresh=true
else [ "$#" = 2 ]; [ "$1" = --session ]; session=$2; fi
fi
dir=${session%/*}
[ "$session" = "$dir/session.json" ] || exit 92
if [ "$has_map_profile" = true ]; then case "$map_profile_path" in "$dir"/.map-profile-*.json) ;; *) exit 95;; esac; [ -r "$map_profile_path" ]; fi
payload=$(cat)
mode=normal
if [ -f "$dir/mode" ]; then mode=$(cat "$dir/mode"); fi
if [ "$mode" = timeout ]; then sleep 2; printf escaped > "$dir/escaped"; exit 1; fi
if [ "$mode" = oversize ]; then head -c 40000 /dev/zero; exit 1; fi
if [ "$mode" = stderr_limit ]; then head -c 70000 /dev/zero >&2; exit 1; fi
if [ "$mode" = malformed ]; then printf '%s' 'synthetic-password MUST_NOT_ESCAPE'; exit 1; fi
if [ "$mode" = unknown_error ]; then printf '%s' '{"schema":1,"mode":"dreame_cloud","read_only":true,"ok":false,"error":{"code":"MUST_NOT_ESCAPE"}}'; exit 1; fi
if [ '` + kind + `' = provisioner ]; then
 if [ "$refresh" = true ]; then [ -z "$payload" ]; printf 'refresh\n' >> "$dir/events"; touch "$dir/refreshed"
 else
  case "$payload" in *'"username":"synthetic@example.invalid"'*'"password":"synthetic-password"'*) ;; *) exit 93;; esac
  printf 'login\n' >> "$dir/events"
 fi
 umask 077; printf '%s' '{"synthetic":true}' > "$session"
 if [ "$mode" = unverified_success ]; then
  printf '%s' '{"schema":1,"mode":"dreame_provision","read_only":true,"ok":true,"data":{"session_saved":true,"password_stored":false,"refresh_available":true,"resolved_server_region":"eu","routing_verified":false}}'
  exit 0
 fi
 case "$payload" in *'"server_region":""'*|*'"server_region":"auto"'*)
  printf '%s' '{"schema":1,"mode":"dreame_provision","read_only":true,"ok":true,"data":{"session_saved":true,"password_stored":false,"refresh_available":true,"resolved_server_region":"eu","routing_verified":true}}'
  exit 0;; esac
 printf '%s' '{"schema":1,"mode":"dreame_provision","read_only":true,"ok":true,"data":{"session_saved":true,"password_stored":false,"refresh_available":true}}'
 exit 0
fi
[ "$refresh" = false ]
case "$payload" in *'"op":"rooms"'*) [ "$has_map_profile" = true ];; *) [ "$has_map_profile" = false ];; esac
case "$payload" in
 *'"op":"devices"'*)
  printf 'devices\n' >> "$dir/events"
  if [ "$mode" = always_expired ] || { [ "$mode" = expired ] && [ ! -f "$dir/refreshed" ]; }; then
   printf '%s' '{"schema":1,"mode":"dreame_cloud","read_only":true,"ok":false,"error":{"code":"session_expired"}}'; exit 1
  fi
  if [ "$mode" = foreign_after_refresh ] && [ -f "$dir/refreshed" ]; then data='[]'
  else data='` + catalog + `'; fi;;
 *'"op":"status"'*)
  printf 'status\n' >> "$dir/events"
  if [ "$mode" = foreign_after_refresh ] && [ ! -f "$dir/refreshed" ]; then
   printf '%s' '{"schema":1,"mode":"dreame_cloud","read_only":true,"ok":false,"error":{"code":"session_reauthentication_required"}}'; exit 1
  fi
  data='` + statusFixture + `';;
 *'"op":"rooms"'*) printf 'rooms\n' >> "$dir/events"; data='` + roomsFixture + `'
  if [ "$mode" = map_diagnostics ]; then data='` + mapDiagnosticFixture + `'; fi;;
 *) exit 94;;
esac
printf '%s' '{"schema":1,"mode":"dreame_cloud","read_only":true,"ok":true,"data":' "$data" '}'
`
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || provider.SafeCode(err) != want {
		t.Fatalf("safe code = %s, want %s", provider.SafeCode(err), want)
	}
	if strings.Contains(fmt.Sprintf("%+v", err), "synthetic-password") || strings.Contains(err.Error(), "MUST_NOT_ESCAPE") {
		t.Fatal("unsafe error")
	}
}
func events(t *testing.T, dir string) string {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, "events"))
	if os.IsNotExist(e) {
		return ""
	}
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func mode(t *testing.T, dir, m string) { write(t, filepath.Join(dir, "mode"), m, 0600) }

func TestAuthenticateFixedArgumentsStdinAndEnvironment(t *testing.T) {
	a, dir := setup(t)
	t.Setenv("DREAME_TEST_SECRET", "synthetic-environment-secret")
	for _, r := range []string{"by", "eu", "ru", "us", "cn", "sg", "kr"} {
		if e := a.Authenticate(context.Background(), dir, provider.Credentials{Username: "synthetic@example.invalid", Password: "synthetic-password", Region: r}); e != nil {
			t.Fatal(e)
		}
	}
	if events(t, dir) != strings.Repeat("login\n", 7) {
		t.Fatal("unexpected child invocations")
	}
	code(t, a.Authenticate(context.Background(), dir, provider.Credentials{Region: "guess"}), "invalid_region")
	code(t, a.Authenticate(context.Background(), dir, provider.Credentials{Region: "eu", Username: "bad", Password: "synthetic-password"}), "invalid_credentials")
	if events(t, dir) != strings.Repeat("login\n", 7) {
		t.Fatal("invalid input invoked child")
	}
}

func TestAuthenticateAutomaticConnectionRequiresVerifiedOrigin(t *testing.T) {
	for _, region := range []string{"", "auto"} {
		t.Run("region="+region, func(t *testing.T) {
			a, dir := setup(t)
			t.Setenv("DREAME_TEST_SECRET", "synthetic-environment-secret")
			if err := a.Authenticate(context.Background(), dir, provider.Credentials{Username: "synthetic@example.invalid", Password: "synthetic-password", Region: region}); err != nil {
				t.Fatal(err)
			}
			if events(t, dir) != "login\n" {
				t.Fatal("automatic credentials retried")
			}
		})
	}
	a, dir := setup(t)
	mode(t, dir, "unverified_success")
	err := a.Authenticate(context.Background(), dir, provider.Credentials{Username: "synthetic@example.invalid", Password: "synthetic-password"})
	code(t, err, "account_connection_unverified")
	if events(t, dir) != "login\n" {
		t.Fatal("unverified origin retried")
	}
	for _, raw := range []string{
		`{"schema":1,"mode":"dreame_provision","read_only":true,"ok":true,"data":{"session_saved":true,"password_stored":false,"refresh_available":true}}`,
		`{"schema":1,"mode":"dreame_provision","read_only":true,"ok":false,"error":{"code":"authentication_connection_unverified"}}`,
	} {
		code(t, provisionResult([]byte(raw), true, "eu"), "account_connection_unverified")
	}
}
func TestReadShapesSanitizedAndLegacyHelperControlUnknown(t *testing.T) {
	a, dir := setup(t)
	for _, op := range []provider.Operation{provider.Devices, provider.Status, provider.Rooms} {
		p := provider.Params{}
		if op != provider.Devices {
			p.DeviceID = "fixture-1"
		}
		b, e := a.Execute(context.Background(), dir, op, p)
		if e != nil {
			t.Fatal(e)
		}
		if !json.Valid(b) || strings.Contains(string(b), "MUST_NOT_ESCAPE") {
			t.Fatal("unsafe response")
		}
		if op == provider.Rooms && (!strings.Contains(string(b), `"verified_for_control":false`) || !strings.Contains(string(b), `"active_map_verified":false`)) {
			t.Fatal("room evidence promoted")
		}
	}
	b, e := a.Execute(context.Background(), dir, provider.CleanRooms, provider.Params{DeviceID: "fixture-1", MapID: "17", RoomIDs: []string{"3"}})
	if e != nil || !strings.Contains(string(b), `"outcome":"unknown"`) {
		t.Fatal("legacy control failure must not invite retry")
	}
	if events(t, dir) != "devices\ndevices\nstatus\ndevices\nrooms\ndevices\n" {
		t.Fatal("unexpected read/write dispatch")
	}
}
func TestForeignDeviceAndPerAccountIsolation(t *testing.T) {
	a, dir := setup(t)
	for _, op := range []provider.Operation{provider.Status, provider.Rooms, provider.CleanRooms} {
		p := provider.Params{DeviceID: "foreign-1"}
		if op == provider.CleanRooms {
			p.MapID = "17"
			p.RoomIDs = []string{"3"}
		}
		_, e := a.Execute(context.Background(), dir, op, p)
		code(t, e, "device_not_found")
	}
	other := filepath.Join(filepath.Dir(dir), "other")
	if e := os.Mkdir(other, 0700); e != nil {
		t.Fatal(e)
	}
	_, e := a.Execute(context.Background(), other, provider.Devices, provider.Params{})
	code(t, e, "invalid_credentials")
	if events(t, dir) != "devices\ndevices\ndevices\n" || events(t, other) != "" {
		t.Fatal("cross-account invocation")
	}
}
func TestRefreshOnceAndRecheckMembership(t *testing.T) {
	for _, m := range []string{"expired", "always_expired", "foreign_after_refresh"} {
		t.Run(m, func(t *testing.T) {
			a, dir := setup(t)
			mode(t, dir, m)
			_, e := a.Execute(context.Background(), dir, provider.Status, provider.Params{DeviceID: "fixture-1"})
			switch m {
			case "expired":
				if e != nil {
					t.Fatal(e)
				}
			case "always_expired":
				code(t, e, "invalid_credentials")
			case "foreign_after_refresh":
				code(t, e, "device_not_found")
			}
			got := events(t, dir)
			if strings.Count(got, "refresh\n") != 1 || strings.Contains(got, "login") {
				t.Fatal("refresh count or password fallback")
			}
			if m == "foreign_after_refresh" && got != "devices\nstatus\nrefresh\ndevices\n" {
				t.Fatal("membership not rechecked")
			}
		})
	}
}
func TestMalformedBoundsRedactionAndTimeout(t *testing.T) {
	for _, m := range []string{"malformed", "oversize", "stderr_limit", "unknown_error", "timeout"} {
		t.Run(m, func(t *testing.T) {
			a, dir := setup(t)
			mode(t, dir, m)
			a.timeout = 80 * time.Millisecond
			start := time.Now()
			b, e := a.Execute(context.Background(), dir, provider.Devices, provider.Params{})
			want := "provider_unavailable"
			if m == "timeout" {
				want = "upstream_unavailable"
			}
			code(t, e, want)
			if len(b) != 0 || time.Since(start) > 1500*time.Millisecond {
				t.Fatal("unbounded execution/output")
			}
			if m == "timeout" {
				time.Sleep(2100 * time.Millisecond)
				if _, e := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(e) {
					t.Fatal("child survived cancellation")
				}
			}
		})
	}
}
func TestPrivatePathsAndProfileValidation(t *testing.T) {
	a, dir := setup(t)
	if e := os.Chmod(filepath.Join(dir, "session.json"), 0644); e != nil {
		t.Fatal(e)
	}
	_, e := a.Execute(context.Background(), dir, provider.Devices, provider.Params{})
	code(t, e, "provider_unavailable")
	if e := os.Remove(filepath.Join(dir, "session.json")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "session.json")); e != nil {
		t.Fatal(e)
	}
	_, e = a.Execute(context.Background(), dir, provider.Devices, provider.Params{})
	code(t, e, "provider_unavailable")
	for _, raw := range []string{`{}`, strings.Replace(fakeProfile, upstreamCommit, "wrong", 1), fakeProfile + ` {}`, strings.Replace(fakeProfile, `"1"`, `"bad"`, 1)} {
		if validProfile([]byte(raw)) {
			t.Fatal("invalid profile accepted")
		}
	}
	_, e = New(Config{ReaderPath: "relative", ProvisionerPath: a.provisioner, ProfilePath: "relative"})
	code(t, e, "provider_unavailable")
}
func TestRejectPromotedRoomsAndBadStatus(t *testing.T) {
	_, e := rooms([]byte(strings.Replace(roomsFixture, `"verified_for_control":false`, `"verified_for_control":true`, 1)), "fixture-1")
	code(t, e, "provider_unavailable")
	_, e = status([]byte(strings.Replace(statusFixture, `"battery_percent":82`, `"battery_percent":999`, 1)), "fixture-1")
	code(t, e, "provider_unavailable")
	_, _, e = devices([]byte(`[{"device_id":"bad/id","model":"x","name":"x"}]`))
	code(t, e, "provider_unavailable")
}

func TestPeerOwnedAccountAndSessionRejected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing ownership requires test UID0")
	}
	a, dir := setup(t)
	session := filepath.Join(dir, "session.json")
	if err := os.Chown(session, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	_, err := a.Execute(context.Background(), dir, provider.Devices, provider.Params{})
	code(t, err, "provider_unavailable")
	if err := os.Chown(session, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	_, err = a.Execute(context.Background(), dir, provider.Devices, provider.Params{})
	code(t, err, "provider_unavailable")
	if err := os.Chown(dir, 0, 0); err != nil {
		t.Fatal(err)
	}
	if events(t, dir) != "" {
		t.Fatal("peer-owned storage reached child")
	}
}
