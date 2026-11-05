package cleanplan_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"alisa-dreame-readonly/internal/cleanplan"
)

func integer(value int) cleanplan.IntObservation {
	return cleanplan.IntObservation{Quality: cleanplan.Observed, Value: value}
}

func boolean(value bool) cleanplan.BoolObservation {
	return cleanplan.BoolObservation{Quality: cleanplan.Observed, Value: value}
}

func fixture() cleanplan.Observations {
	snapshot := cleanplan.Snapshot{
		Binding: cleanplan.Binding{AccountID: "synthetic-account", DeviceID: "synthetic-device", Model: cleanplan.Model},
		ID:      "synthetic-snapshot", ReadAtMillis: 100_000,
	}
	return cleanplan.Observations{
		NowMillis: 100_100,
		Catalog:   cleanplan.CatalogEvidence{Snapshot: snapshot, Matches: 1},
		Map: cleanplan.MapEvidence{
			Snapshot: snapshot, Source: cleanplan.RefreshResponse, FrameType: 73, Version: 1,
			CurrentMapID: "-1", CurrentSavedMapID: "-32768", SelectedSavedMapID: "-32768",
			SavedStatus: integer(2), Markers: cleanplan.Observed,
		},
		Room: cleanplan.RoomEvidence{
			Snapshot: snapshot, SelectedSavedMapID: "-32768", SegmentID: 7, Matches: 1,
			InCurrent: true, InSelectedSaved: true, Visible: true, Mapped: true,
		},
		State: cleanplan.StateEvidence{
			Snapshot: snapshot, State: integer(6), Status: integer(6), Task: integer(0),
			Error: integer(0), Battery: integer(75), Charging: integer(1),
			CleaningPaused: boolean(false), Drainage: cleanplan.IntObservation{Quality: cleanplan.Unsupported},
		},
		Settings: cleanplan.SettingsEvidence{
			Snapshot: snapshot, Encoding: cleanplan.PlainWaterVolume,
			Mode: integer(2), Tank: integer(1), Fan: integer(0), Water: integer(2), Customized: integer(0),
		},
	}
}

func roomSettings(in cleanplan.Observations) *cleanplan.PerRoomSettings {
	return &cleanplan.PerRoomSettings{
		Snapshot: in.Map.Snapshot, SelectedSavedMapID: in.Map.SelectedSavedMapID,
		SegmentID: in.Room.SegmentID, Origin: cleanplan.GenuineObservedEntry,
		Fan: integer(3), EncodedWater: integer(4), Repeat: integer(1),
		Mode: cleanplan.IntObservation{Quality: cleanplan.Unsupported},
	}
}

func built(t *testing.T, in cleanplan.Observations) *cleanplan.Draft {
	t.Helper()
	draft, err := cleanplan.Build(in)
	if err != nil || draft == nil {
		t.Fatalf("synthetic valid observation rejected: %v", err)
	}
	return draft
}

func expectCode(t *testing.T, in cleanplan.Observations, code cleanplan.Code) {
	t.Helper()
	draft, err := cleanplan.Build(in)
	if draft != nil || err == nil || cleanplan.ErrorCode(err) != code {
		t.Fatalf("expected fixed code %s, got %v", code, err)
	}
	var typed *cleanplan.Error
	if !errors.As(err, &typed) || typed.Code() != code || err.Error() != string(code) {
		t.Fatal("error is not a strict typed safe code")
	}
}

func TestGlobalDraftPreviewAndFixedPayload(t *testing.T) {
	in := fixture()
	draft := built(t, in)
	preview, err := draft.Preview()
	if err != nil || preview.Kind != "DRAFT" || preview.Operation != "single_room" ||
		preview.Model != cleanplan.Model || preview.Mode != 2 || preview.Fan != 0 || preview.Water != 2 ||
		preview.Repeat != 1 || preview.Order != 1 || preview.Battery != 75 ||
		preview.SettingsSource != "observed_global" || preview.FirmwareAcceptance != "unverified" {
		t.Fatal("preview did not retain the concrete draft settings and uncertainty")
	}
	payload, err := draft.LogicalPayloadJSON(in.NowMillis)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"method":"action","params":{"did":"synthetic-device","siid":4,"aiid":1,"in":[{"piid":1,"value":18},{"piid":10,"value":"{\"selects\":[[7,1,0,2,1]]}"}]}}`
	if string(payload) != want {
		t.Fatal("logical payload is not exactly the single-room START_CUSTOM shape")
	}
	for _, forbidden := range []string{"set_properties", "map_id", "timestamp", "areas", "synthetic-account", "synthetic-snapshot", "-32768"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatal("logical payload included an unrelated operation or private proof field")
		}
	}
	// Returned bytes and input observations cannot mutate a previously built plan.
	payload[0] = '!'
	in.Room.SegmentID = 9
	in.Settings.Fan.Value = 3
	again, err := draft.LogicalPayloadJSON(in.NowMillis)
	if err != nil || string(again) != want {
		t.Fatal("draft retained mutable caller data or output storage")
	}
}

func TestSignedMapIDsAndSegmentBounds(t *testing.T) {
	for _, id := range []string{"-32768", "-1", "0", "32767"} {
		in := fixture()
		in.Map.CurrentMapID, in.Map.CurrentSavedMapID, in.Map.SelectedSavedMapID, in.Room.SelectedSavedMapID = id, id, id, id
		built(t, in)
	}
	for _, id := range []string{"", "+1", "01", "-0", "32768", "-32769", "1.0", " 1"} {
		in := fixture()
		in.Map.CurrentSavedMapID, in.Map.SelectedSavedMapID, in.Room.SelectedSavedMapID = id, id, id
		expectCode(t, in, cleanplan.CodeMapUnproven)
	}
	for _, segment := range []int{1, 63} {
		in := fixture()
		in.Room.SegmentID = segment
		built(t, in)
	}
	for _, segment := range []int{-1, 0, 64} {
		in := fixture()
		in.Room.SegmentID = segment
		expectCode(t, in, cleanplan.CodeRoomInvalid)
	}
}

func TestIdentitySnapshotAndMapProofFailures(t *testing.T) {
	cases := []struct {
		name string
		edit func(*cleanplan.Observations)
		code cleanplan.Code
	}{
		{"wrong model", func(v *cleanplan.Observations) { v.Map.Snapshot.Binding.Model = "dreame.vacuum.other" }, cleanplan.CodeModelUnsupported},
		{"blank identity", func(v *cleanplan.Observations) { v.Map.Snapshot.Binding.AccountID = "" }, cleanplan.CodeIdentityInvalid},
		{"oversized identity", func(v *cleanplan.Observations) { v.Map.Snapshot.Binding.DeviceID = strings.Repeat("x", 129) }, cleanplan.CodeIdentityInvalid},
		{"not an opaque identity", func(v *cleanplan.Observations) { v.Map.Snapshot.Binding.AccountID = "private@example.invalid" }, cleanplan.CodeIdentityInvalid},
		{"account collision", func(v *cleanplan.Observations) { v.Catalog.Snapshot.Binding.AccountID = "other-account" }, cleanplan.CodeIdentityMismatch},
		{"device collision", func(v *cleanplan.Observations) { v.Room.Snapshot.Binding.DeviceID = "other-device" }, cleanplan.CodeIdentityMismatch},
		{"generation collision", func(v *cleanplan.Observations) { v.Settings.Snapshot.ID = "other-snapshot" }, cleanplan.CodeSnapshotMismatch},
		{"invalid generation", func(v *cleanplan.Observations) { v.Map.Snapshot.ID = "" }, cleanplan.CodeSnapshotInvalid},
		{"stale reader flag", func(v *cleanplan.Observations) { v.Map.Snapshot.Stale = true }, cleanplan.CodeSnapshotStale},
		{"stale catalog", func(v *cleanplan.Observations) {
			v.Catalog.Snapshot.ReadAtMillis = v.NowMillis - cleanplan.MaxAgeMillis - 1
		}, cleanplan.CodeSnapshotStale},
		{"future observation", func(v *cleanplan.Observations) { v.State.Snapshot.ReadAtMillis = v.NowMillis + 1 }, cleanplan.CodeSnapshotStale},
		{"missing observation time", func(v *cleanplan.Observations) { v.Room.Snapshot.ReadAtMillis = 0 }, cleanplan.CodeSnapshotStale},
		{"invalid clock", func(v *cleanplan.Observations) { v.NowMillis = 0 }, cleanplan.CodeSnapshotInvalid},
		{"no catalog match", func(v *cleanplan.Observations) { v.Catalog.Matches = 0 }, cleanplan.CodeCatalogUnproven},
		{"duplicate catalog target", func(v *cleanplan.Observations) { v.Catalog.Matches = 2 }, cleanplan.CodeCatalogAmbiguous},
		{"unknown current source", func(v *cleanplan.Observations) { v.Map.Source = cleanplan.CurrentFrameUnknown }, cleanplan.CodeMapUnproven},
		{"incremental frame", func(v *cleanplan.Observations) { v.Map.FrameType = 80 }, cleanplan.CodeMapUnproven},
		{"unexpected model map version", func(v *cleanplan.Observations) { v.Map.Version = 0 }, cleanplan.CodeMapUnproven},
		{"saved linkage collision", func(v *cleanplan.Observations) { v.Map.CurrentSavedMapID = "1" }, cleanplan.CodeMapUnproven},
		{"missing saved status", func(v *cleanplan.Observations) { v.Map.SavedStatus.Quality = cleanplan.Missing }, cleanplan.CodeObservationMissing},
		{"unknown markers", func(v *cleanplan.Observations) { v.Map.Markers = cleanplan.Invalid }, cleanplan.CodeMapIneligible},
		{"not located on saved map", func(v *cleanplan.Observations) { v.Map.SavedStatus.Value = 1 }, cleanplan.CodeMapIneligible},
		{"temporary", func(v *cleanplan.Observations) { v.Map.Temporary = true }, cleanplan.CodeMapIneligible},
		{"new", func(v *cleanplan.Observations) { v.Map.New = true }, cleanplan.CodeMapIneligible},
		{"restored", func(v *cleanplan.Observations) { v.Map.Restored = true }, cleanplan.CodeMapIneligible},
		{"empty", func(v *cleanplan.Observations) { v.Map.Empty = true }, cleanplan.CodeMapIneligible},
		{"missing room", func(v *cleanplan.Observations) { v.Room.Matches = 0 }, cleanplan.CodeRoomUnproven},
		{"ambiguous alias", func(v *cleanplan.Observations) { v.Room.Matches = 2 }, cleanplan.CodeRoomAmbiguous},
		{"wrong floor room", func(v *cleanplan.Observations) { v.Room.SelectedSavedMapID = "0" }, cleanplan.CodeRoomInvalid},
		{"not current member", func(v *cleanplan.Observations) { v.Room.InCurrent = false }, cleanplan.CodeRoomInvalid},
		{"not saved member", func(v *cleanplan.Observations) { v.Room.InSelectedSaved = false }, cleanplan.CodeRoomInvalid},
		{"hidden", func(v *cleanplan.Observations) { v.Room.Visible = false }, cleanplan.CodeRoomInvalid},
		{"unmapped", func(v *cleanplan.Observations) { v.Room.Mapped = false }, cleanplan.CodeRoomInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			tc.edit(&in)
			expectCode(t, in, tc.code)
		})
	}
	// The trusted reader may use a proven advanced cached read and version 2.
	in := fixture()
	in.Map.Source, in.Map.Version = cleanplan.AdvancedCachedRead, 2
	built(t, in)
}

func TestMissingAndUnsupportedRequiredObservationsNeverBecomeZero(t *testing.T) {
	fields := []struct {
		name  string
		field func(*cleanplan.Observations) *cleanplan.IntObservation
	}{
		{"state", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.State }},
		{"status", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.Status }},
		{"task", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.Task }},
		{"error", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.Error }},
		{"battery", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.Battery }},
		{"charging", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.State.Charging }},
		{"mode", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.Settings.Mode }},
		{"tank", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.Settings.Tank }},
		{"fan", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.Settings.Fan }},
		{"water", func(v *cleanplan.Observations) *cleanplan.IntObservation { return &v.Settings.Water }},
	}
	for _, field := range fields {
		for _, quality := range []cleanplan.Quality{cleanplan.Missing, cleanplan.Unsupported, cleanplan.Invalid, 255} {
			t.Run(fmt.Sprintf("%s-%d", field.name, quality), func(t *testing.T) {
				in := fixture()
				field.field(&in).Quality = quality
				code := cleanplan.CodeObservationInvalid
				if quality == cleanplan.Missing {
					code = cleanplan.CodeObservationMissing
				} else if quality == cleanplan.Unsupported {
					code = cleanplan.CodeObservationUnsupported
				}
				expectCode(t, in, code)
			})
		}
	}
	for _, quality := range []cleanplan.Quality{cleanplan.Missing, cleanplan.Unsupported, cleanplan.Invalid} {
		in := fixture()
		in.State.CleaningPaused.Quality = quality
		if _, err := cleanplan.Build(in); err == nil {
			t.Fatal("missing paused evidence was accepted")
		}
	}
}

func TestRestingStateGateAndBatteryWithoutInventedThreshold(t *testing.T) {
	for _, battery := range []int{0, 1, 14, 15, 100} {
		in := fixture()
		in.State.Battery = integer(battery)
		built(t, in)
	}
	for _, battery := range []int{-1, 101} {
		in := fixture()
		in.State.Battery = integer(battery)
		expectCode(t, in, cleanplan.CodeObservationInvalid)
	}
	for _, task := range []int{1, 3, 5, 6, 8, 10, 11, 27} {
		in := fixture()
		in.State.Task = integer(task)
		expectCode(t, in, cleanplan.CodeStateNotResting)
	}
	for _, status := range []int{-1, 1, 2, 3, 12, 15, 18, 21, 1501} {
		in := fixture()
		in.State.Status = integer(status)
		expectCode(t, in, cleanplan.CodeStateNotResting)
	}
	for _, state := range []int{-1, 1, 3, 4, 5, 11, 14, 19, 31, 32, 33} {
		in := fixture()
		in.State.State = integer(state)
		expectCode(t, in, cleanplan.CodeStateNotResting)
	}
	in := fixture()
	in.State.CleaningPaused = boolean(true)
	expectCode(t, in, cleanplan.CodeStateNotResting)
	in = fixture()
	in.State.Error = integer(1)
	expectCode(t, in, cleanplan.CodeStateNotResting)
	in = fixture()
	in.State.Charging = integer(2)
	expectCode(t, in, cleanplan.CodeStateConflict)
	in.State.State, in.State.Status = integer(2), integer(0)
	built(t, in) // Idle away from the dock is a resting state too.
	in.State.Charging = integer(5)
	expectCode(t, in, cleanplan.CodeStateNotResting)
	for _, drainage := range []int{1, 2, 3} {
		in = fixture()
		in.State.Drainage = integer(drainage)
		expectCode(t, in, cleanplan.CodeStateNotResting)
	}
	in = fixture()
	in.State.Drainage = integer(0)
	built(t, in)
	in.State.Drainage = cleanplan.IntObservation{Quality: cleanplan.Unsupported, Value: 1}
	expectCode(t, in, cleanplan.CodeObservationInvalid)
}

func TestPlainSettingsHaveNoCoercionOrFallback(t *testing.T) {
	for _, pair := range [][2]int{{0, 0}, {1, 1}, {1, 10}, {2, 1}, {2, 10}} {
		in := fixture()
		in.Settings.Mode, in.Settings.Tank = integer(pair[0]), integer(pair[1])
		built(t, in)
	}
	cases := []struct {
		edit func(*cleanplan.Observations)
		code cleanplan.Code
	}{
		{func(v *cleanplan.Observations) { v.Settings.Encoding = cleanplan.EncodingUnknown }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Mode = integer(3) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Fan = integer(-1) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Fan = integer(4) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Water = integer(0) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Water = integer(4) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Tank = integer(0) }, cleanplan.CodeSettingsConflict},
		{func(v *cleanplan.Observations) { v.Settings.Mode = integer(0) }, cleanplan.CodeSettingsConflict},
		{func(v *cleanplan.Observations) { v.Settings.Tank = integer(99) }, cleanplan.CodeSettingsConflict},
		{func(v *cleanplan.Observations) { v.Settings.Customized = integer(2) }, cleanplan.CodeSettingsUnsupported},
		{func(v *cleanplan.Observations) { v.Settings.Customized.Quality = cleanplan.Missing }, cleanplan.CodeObservationMissing},
		{func(v *cleanplan.Observations) { v.PerRoom = roomSettings(*v) }, cleanplan.CodeSettingsConflict},
		{func(v *cleanplan.Observations) {
			v.Settings.Customized = cleanplan.IntObservation{Quality: cleanplan.Unsupported, Value: 1}
		}, cleanplan.CodeObservationInvalid},
	}
	for _, tc := range cases {
		in := fixture()
		tc.edit(&in)
		expectCode(t, in, tc.code)
	}
	in := fixture()
	in.Settings.Customized = cleanplan.IntObservation{Quality: cleanplan.Unsupported}
	built(t, in) // Explicit source-known absent capability, not a missing read.
}

func TestCustomizedNeedsGenuineRoomEntryAndRawWaterConversion(t *testing.T) {
	for _, encoded := range []int{2, 3, 4} {
		in := fixture()
		in.Settings.Customized = integer(1)
		in.PerRoom = roomSettings(in)
		in.PerRoom.EncodedWater = integer(encoded)
		draft := built(t, in)
		preview, _ := draft.Preview()
		if preview.Fan != 3 || preview.Water != encoded-1 || preview.SettingsSource != "genuine_observed_room" {
			t.Fatal("planner used globals or confused raw cleanset water with row water")
		}
		payload, err := draft.LogicalPayloadJSON(in.NowMillis)
		if err != nil || !bytes.Contains(payload, []byte(fmt.Sprintf("[7,1,3,%d,1]", encoded-1))) {
			t.Fatal("per-room settings did not reach the fixed row")
		}
	}
	cases := []struct {
		name string
		edit func(*cleanplan.Observations)
		code cleanplan.Code
	}{
		{"missing entry", func(v *cleanplan.Observations) { v.PerRoom = nil }, cleanplan.CodePerRoomRequired},
		{"unknown provenance", func(v *cleanplan.Observations) { v.PerRoom.Origin = cleanplan.EntryUnknown }, cleanplan.CodePerRoomUnobserved},
		{"synthesized defaults", func(v *cleanplan.Observations) { v.PerRoom.Origin = cleanplan.SynthesizedEntry }, cleanplan.CodePerRoomUnobserved},
		{"other room", func(v *cleanplan.Observations) { v.PerRoom.SegmentID = 9 }, cleanplan.CodeRoomInvalid},
		{"other map", func(v *cleanplan.Observations) { v.PerRoom.SelectedSavedMapID = "0" }, cleanplan.CodeRoomInvalid},
		{"other account", func(v *cleanplan.Observations) { v.PerRoom.Snapshot.Binding.AccountID = "other-account" }, cleanplan.CodeIdentityMismatch},
		{"other generation", func(v *cleanplan.Observations) { v.PerRoom.Snapshot.ID = "other-generation" }, cleanplan.CodeSnapshotMismatch},
		{"stale entry", func(v *cleanplan.Observations) { v.PerRoom.Snapshot.Stale = true }, cleanplan.CodeSnapshotStale},
		{"missing fan cannot use global", func(v *cleanplan.Observations) { v.PerRoom.Fan.Quality = cleanplan.Missing }, cleanplan.CodeObservationMissing},
		{"unsupported water", func(v *cleanplan.Observations) { v.PerRoom.EncodedWater.Quality = cleanplan.Unsupported }, cleanplan.CodeObservationUnsupported},
		{"row-water-as-encoded", func(v *cleanplan.Observations) { v.PerRoom.EncodedWater = integer(1) }, cleanplan.CodeSettingsUnsupported},
		{"bad encoded water", func(v *cleanplan.Observations) { v.PerRoom.EncodedWater = integer(5) }, cleanplan.CodeSettingsUnsupported},
		{"zero repeat not clamped", func(v *cleanplan.Observations) { v.PerRoom.Repeat = integer(0) }, cleanplan.CodeRepeatMismatch},
		{"two passes not silently changed", func(v *cleanplan.Observations) { v.PerRoom.Repeat = integer(2) }, cleanplan.CodeRepeatMismatch},
		{"advanced per-room mode", func(v *cleanplan.Observations) { v.PerRoom.Mode = integer(1) }, cleanplan.CodeSettingsUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fixture()
			in.Settings.Customized = integer(1)
			in.PerRoom = roomSettings(in)
			tc.edit(&in)
			expectCode(t, in, tc.code)
		})
	}
}

func TestPayloadRechecksAgeAndZeroDraftCannotBecomePayload(t *testing.T) {
	in := fixture()
	draft := built(t, in)
	if _, err := draft.LogicalPayloadJSON(in.Map.Snapshot.ReadAtMillis + cleanplan.MaxAgeMillis); err != nil {
		t.Fatal("freshness boundary rejected")
	}
	for _, now := range []int64{0, -1, in.Map.Snapshot.ReadAtMillis - 1, in.NowMillis - 1, in.Map.Snapshot.ReadAtMillis + cleanplan.MaxAgeMillis + 1} {
		payload, err := draft.LogicalPayloadJSON(now)
		if payload != nil || cleanplan.ErrorCode(err) != cleanplan.CodeSnapshotStale {
			t.Fatal("stale payload serialized")
		}
	}
	if preview, err := draft.Preview(); err != nil || preview.Kind != "DRAFT" {
		t.Fatal("an older owner review should remain a draft, not become permission")
	}
	for _, invalid := range []*cleanplan.Draft{nil, {}} {
		if payload, err := invalid.LogicalPayloadJSON(in.NowMillis); payload != nil || cleanplan.ErrorCode(err) != cleanplan.CodeDraftInvalid {
			t.Fatal("zero/unchecked draft serialized")
		}
		if _, err := invalid.Preview(); cleanplan.ErrorCode(err) != cleanplan.CodeDraftInvalid {
			t.Fatal("zero/unchecked draft acquired a preview")
		}
	}
	if _, err := json.Marshal(&cleanplan.Draft{}); err == nil {
		t.Fatal("zero draft acquired a JSON representation")
	}
}

func TestFormattingJSONAndErrorsDoNotExposePrivateProof(t *testing.T) {
	in := fixture()
	in.Settings.Customized = integer(1)
	in.PerRoom = roomSettings(in)
	draft := built(t, in)
	values := []any{in.Catalog.Snapshot.Binding, in.Map.Snapshot, in.Catalog, in.Map, in.Room,
		in.State, in.Settings, *in.PerRoom, in, draft, *draft}
	for _, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			text := fmt.Sprintf(verb, value)
			if strings.Contains(text, "synthetic-") || strings.Contains(text, "-32768") || strings.Contains(text, "selects") {
				t.Fatal("formatted proof or draft leaked private input")
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil || bytes.Contains(encoded, []byte("synthetic-")) || bytes.Contains(encoded, []byte("-32768")) || bytes.Contains(encoded, []byte("selects")) {
			t.Fatal("JSON formatting leaked proof or logical payload")
		}
	}
	var observation cleanplan.Observations
	if err := json.Unmarshal([]byte(`{"verified":true,"authorized":true}`), &observation); cleanplan.ErrorCode(err) != cleanplan.CodeUntrustedInput {
		t.Fatal("untrusted JSON became backend observations")
	}
	if err := json.Unmarshal([]byte(`{"version":1,"segmentID":7}`), draft); cleanplan.ErrorCode(err) != cleanplan.CodeUntrustedInput {
		t.Fatal("untrusted JSON became a draft")
	}
	if _, err := draft.LogicalPayloadJSON(in.NowMillis); cleanplan.ErrorCode(err) != cleanplan.CodeDraftInvalid {
		t.Fatal("failed JSON input left a reusable constructed draft")
	}
	if cleanplan.ErrorCode(errors.New("private underlying input")) != cleanplan.CodeInvalidInput ||
		(&cleanplan.Error{}).Error() != string(cleanplan.CodeInvalidInput) {
		t.Fatal("unknown error contents escaped the fixed error code boundary")
	}
}
