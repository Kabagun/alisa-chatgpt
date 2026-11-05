package cleanplan

import (
	"errors"
	"strconv"
)

const (
	Model = "dreame.vacuum.r2422"
	// MaxAgeMillis is an engineering guard, not a vendor freshness guarantee.
	MaxAgeMillis     int64 = 30_000
	maxIdentityBytes       = 128
)

// Quality preserves absence, explicit lack of support, and invalid reads.
type Quality uint8

const (
	Missing Quality = iota
	Observed
	Unsupported
	Invalid
)

type IntObservation struct {
	Quality Quality
	Value   int
}

type BoolObservation struct {
	Quality Quality
	Value   bool
}

// Binding contains private, bounded backend identifiers, not credentials or
// account emails. Identifiers must be ASCII letters/digits or '-', '_', '.'.
type Binding struct {
	AccountID string
	DeviceID  string
	Model     string
}

// Snapshot.ID is the trusted reader's operation/snapshot identity, shared by all
// facts used in one plan. ReadAtMillis is injected by that reader, not robot time.
type Snapshot struct {
	Binding      Binding
	ID           string
	ReadAtMillis int64
	Stale        bool
}

type CatalogEvidence struct {
	Snapshot Snapshot
	// Matches counts exact target matches, not all devices in the account.
	Matches int
}

type CurrentFrameSource uint8

const (
	CurrentFrameUnknown CurrentFrameSource = iota
	RefreshResponse
	AdvancedCachedRead
)

type MapEvidence struct {
	Snapshot           Snapshot
	Source             CurrentFrameSource
	FrameType          int
	Version            int
	CurrentMapID       string
	CurrentSavedMapID  string
	SelectedSavedMapID string
	SavedStatus        IntObservation
	Markers            Quality
	Temporary          bool
	New                bool
	Restored           bool
	Empty              bool
}

type RoomEvidence struct {
	Snapshot           Snapshot
	SelectedSavedMapID string
	SegmentID          int
	Matches            int
	InCurrent          bool
	InSelectedSaved    bool
	Visible            bool
	Mapped             bool
}

type StateEvidence struct {
	Snapshot       Snapshot
	State          IntObservation
	Status         IntObservation
	Task           IntObservation
	Error          IntObservation
	Battery        IntObservation
	Charging       IntObservation
	CleaningPaused BoolObservation
	// Unsupported means the pinned model capability declares no drainage
	// facility, or the trusted reader explicitly established non-support.
	Drainage IntObservation
}

type SettingsEncoding uint8

const (
	EncodingUnknown SettingsEncoding = iota
	PlainWaterVolume
)

type SettingsEvidence struct {
	Snapshot Snapshot
	Encoding SettingsEncoding
	Mode     IntObservation
	Tank     IntObservation
	Fan      IntObservation
	Water    IntObservation
	// Unsupported is an explicitly established absent capability, not Missing.
	Customized IntObservation
}

type EntryOrigin uint8

const (
	EntryUnknown EntryOrigin = iota
	GenuineObservedEntry
	SynthesizedEntry
)

type PerRoomSettings struct {
	Snapshot           Snapshot
	SelectedSavedMapID string
	SegmentID          int
	Origin             EntryOrigin
	Fan                IntObservation
	// EncodedWater is the genuine raw cleanset value (2..4), not row water.
	EncodedWater IntObservation
	Repeat       IntObservation
	// An observed per-room mode indicates an advanced encoding outside scope.
	Mode IntObservation
}

// Observations is not a wire schema. JSON unmarshalling is deliberately denied.
type Observations struct {
	NowMillis int64
	Catalog   CatalogEvidence
	Map       MapEvidence
	Room      RoomEvidence
	State     StateEvidence
	Settings  SettingsEvidence
	// Omit when customized cleaning is disabled. No per-room fallback is guessed.
	PerRoom *PerRoomSettings
}

type Code string

const (
	CodeInvalidInput           Code = "cleanplan_invalid_input"
	CodeUntrustedInput         Code = "cleanplan_untrusted_input"
	CodeModelUnsupported       Code = "cleanplan_model_unsupported"
	CodeIdentityInvalid        Code = "cleanplan_identity_invalid"
	CodeIdentityMismatch       Code = "cleanplan_identity_mismatch"
	CodeSnapshotInvalid        Code = "cleanplan_snapshot_invalid"
	CodeSnapshotMismatch       Code = "cleanplan_snapshot_mismatch"
	CodeSnapshotStale          Code = "cleanplan_snapshot_stale"
	CodeCatalogUnproven        Code = "cleanplan_catalog_unproven"
	CodeCatalogAmbiguous       Code = "cleanplan_catalog_ambiguous"
	CodeMapUnproven            Code = "cleanplan_map_unproven"
	CodeMapIneligible          Code = "cleanplan_map_ineligible"
	CodeRoomUnproven           Code = "cleanplan_room_unproven"
	CodeRoomAmbiguous          Code = "cleanplan_room_ambiguous"
	CodeRoomInvalid            Code = "cleanplan_room_invalid"
	CodeObservationMissing     Code = "cleanplan_observation_missing"
	CodeObservationUnsupported Code = "cleanplan_observation_unsupported"
	CodeObservationInvalid     Code = "cleanplan_observation_invalid"
	CodeStateNotResting        Code = "cleanplan_state_not_resting"
	CodeStateConflict          Code = "cleanplan_state_conflict"
	CodeSettingsUnsupported    Code = "cleanplan_settings_unsupported"
	CodeSettingsConflict       Code = "cleanplan_settings_conflict"
	CodePerRoomRequired        Code = "cleanplan_per_room_required"
	CodePerRoomUnobserved      Code = "cleanplan_per_room_unobserved"
	CodeRepeatMismatch         Code = "cleanplan_repeat_mismatch"
	CodeDraftInvalid           Code = "cleanplan_draft_invalid"
)

// Error never contains observation contents, identifiers, or an underlying error.
type Error struct{ code Code }

func (e *Error) Code() Code {
	if e == nil || !knownCode(e.code) {
		return CodeInvalidInput
	}
	return e.code
}

func (e *Error) Error() string { return string(e.Code()) }
func fail(code Code) error     { return &Error{code: code} }

// ErrorCode maps unrelated errors to a fixed safe code without formatting them.
func ErrorCode(err error) Code {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code()
	}
	return CodeInvalidInput
}

func knownCode(code Code) bool {
	switch code {
	case CodeInvalidInput, CodeUntrustedInput, CodeModelUnsupported,
		CodeIdentityInvalid, CodeIdentityMismatch, CodeSnapshotInvalid,
		CodeSnapshotMismatch, CodeSnapshotStale, CodeCatalogUnproven,
		CodeCatalogAmbiguous, CodeMapUnproven, CodeMapIneligible,
		CodeRoomUnproven, CodeRoomAmbiguous, CodeRoomInvalid,
		CodeObservationMissing, CodeObservationUnsupported, CodeObservationInvalid,
		CodeStateNotResting, CodeStateConflict, CodeSettingsUnsupported,
		CodeSettingsConflict, CodePerRoomRequired, CodePerRoomUnobserved,
		CodeRepeatMismatch, CodeDraftInvalid:
		return true
	}
	return false
}

type settingsSource uint8

const (
	globalSettings settingsSource = iota + 1
	perRoomSettings
)

// Draft can only acquire valid private fields through Build. Its zero value
// cannot serialize a payload. It is a proposal, never authorization or dispatch.
type Draft struct {
	version    uint8
	deviceID   string
	segmentID  int
	mode       int
	fan        int
	water      int
	battery    int
	source     settingsSource
	oldestRead int64
	builtAt    int64
}

// Preview is safe for a concrete settings review. Friendly target/room names
// remain the caller's separately protected binding. Firmware is always unproven.
type Preview struct {
	Kind               string `json:"kind"`
	Operation          string `json:"operation"`
	Model              string `json:"model"`
	Mode               int    `json:"mode"`
	Fan                int    `json:"fan"`
	Water              int    `json:"water"`
	Repeat             int    `json:"repeat"`
	Order              int    `json:"order"`
	Battery            int    `json:"battery"`
	SettingsSource     string `json:"settings_source"`
	FirmwareAcceptance string `json:"firmware_acceptance"`
}

func identifier(value string) bool {
	if len(value) < 1 || len(value) > maxIdentityBytes {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func mapID(value string) bool {
	n, err := strconv.ParseInt(value, 10, 16)
	return err == nil && strconv.FormatInt(n, 10) == value
}

func checkBinding(binding Binding) error {
	if binding.Model != Model {
		return fail(CodeModelUnsupported)
	}
	if !identifier(binding.AccountID) || !identifier(binding.DeviceID) {
		return fail(CodeIdentityInvalid)
	}
	return nil
}

func checkSnapshot(snapshot Snapshot, expected Snapshot, now int64) error {
	if err := checkBinding(snapshot.Binding); err != nil {
		return err
	}
	if snapshot.Binding != expected.Binding {
		return fail(CodeIdentityMismatch)
	}
	if !identifier(snapshot.ID) {
		return fail(CodeSnapshotInvalid)
	}
	if snapshot.ID != expected.ID {
		return fail(CodeSnapshotMismatch)
	}
	if snapshot.Stale || snapshot.ReadAtMillis <= 0 || now < snapshot.ReadAtMillis ||
		now-snapshot.ReadAtMillis > MaxAgeMillis {
		return fail(CodeSnapshotStale)
	}
	return nil
}

func observed(quality Quality) error {
	switch quality {
	case Observed:
		return nil
	case Missing:
		return fail(CodeObservationMissing)
	case Unsupported:
		return fail(CodeObservationUnsupported)
	default:
		return fail(CodeObservationInvalid)
	}
}

// Build checks one coherent trusted-reader snapshot. It has no clock, network,
// filesystem, secret, authorization, or command-dispatch dependencies.
func Build(in Observations) (*Draft, error) {
	if in.NowMillis <= 0 {
		return nil, fail(CodeSnapshotInvalid)
	}
	base := in.Map.Snapshot
	if err := checkBinding(base.Binding); err != nil {
		return nil, err
	}
	if !identifier(base.ID) {
		return nil, fail(CodeSnapshotInvalid)
	}
	oldest := in.NowMillis
	for _, snapshot := range [...]Snapshot{
		in.Catalog.Snapshot, base, in.Room.Snapshot, in.State.Snapshot, in.Settings.Snapshot,
	} {
		if err := checkSnapshot(snapshot, base, in.NowMillis); err != nil {
			return nil, err
		}
		if snapshot.ReadAtMillis < oldest {
			oldest = snapshot.ReadAtMillis
		}
	}
	if in.Catalog.Matches == 0 {
		return nil, fail(CodeCatalogUnproven)
	}
	if in.Catalog.Matches != 1 {
		return nil, fail(CodeCatalogAmbiguous)
	}
	if in.Map.Source != RefreshResponse && in.Map.Source != AdvancedCachedRead ||
		in.Map.FrameType != 73 || in.Map.Version != 1 && in.Map.Version != 2 ||
		!mapID(in.Map.CurrentMapID) || !mapID(in.Map.CurrentSavedMapID) ||
		!mapID(in.Map.SelectedSavedMapID) ||
		in.Map.CurrentSavedMapID != in.Map.SelectedSavedMapID {
		return nil, fail(CodeMapUnproven)
	}
	if err := observed(in.Map.SavedStatus.Quality); err != nil {
		return nil, err
	}
	if in.Map.SavedStatus.Value != 2 || in.Map.Markers != Observed ||
		in.Map.Temporary || in.Map.New || in.Map.Restored || in.Map.Empty {
		return nil, fail(CodeMapIneligible)
	}
	if in.Room.Matches == 0 {
		return nil, fail(CodeRoomUnproven)
	}
	if in.Room.Matches != 1 {
		return nil, fail(CodeRoomAmbiguous)
	}
	if in.Room.SegmentID < 1 || in.Room.SegmentID > 63 ||
		!mapID(in.Room.SelectedSavedMapID) ||
		in.Room.SelectedSavedMapID != in.Map.SelectedSavedMapID ||
		!in.Room.InCurrent || !in.Room.InSelectedSaved || !in.Room.Visible || !in.Room.Mapped {
		return nil, fail(CodeRoomInvalid)
	}
	if err := checkState(in.State); err != nil {
		return nil, err
	}
	fan, water, source, err := checkSettings(in.Settings, in.PerRoom, in.Room, base, in.NowMillis)
	if err != nil {
		return nil, err
	}
	if in.PerRoom != nil && in.PerRoom.Snapshot.ReadAtMillis < oldest {
		oldest = in.PerRoom.Snapshot.ReadAtMillis
	}
	return &Draft{
		version: 1, deviceID: base.Binding.DeviceID, segmentID: in.Room.SegmentID,
		mode: in.Settings.Mode.Value, fan: fan, water: water,
		battery: in.State.Battery.Value, source: source, oldestRead: oldest, builtAt: in.NowMillis,
	}, nil
}

func checkState(state StateEvidence) error {
	for _, value := range [...]IntObservation{
		state.State, state.Status, state.Task, state.Error, state.Battery, state.Charging,
	} {
		if err := observed(value.Quality); err != nil {
			return err
		}
	}
	if err := observed(state.CleaningPaused.Quality); err != nil {
		return err
	}
	if state.Battery.Value < 0 || state.Battery.Value > 100 {
		return fail(CodeObservationInvalid)
	}
	// There is deliberately no invented minimum battery threshold.
	if state.Error.Value != 0 || state.Task.Value != 0 || state.CleaningPaused.Value {
		return fail(CodeStateNotResting)
	}
	if state.State.Value != 2 && state.State.Value != 6 && state.State.Value != 13 ||
		state.Status.Value != 0 && state.Status.Value != 6 && state.Status.Value != 14 && state.Status.Value != 17 ||
		state.Charging.Value != 1 && state.Charging.Value != 2 && state.Charging.Value != 3 {
		return fail(CodeStateNotResting)
	}
	if (state.State.Value == 6 || state.State.Value == 13 || state.Status.Value == 6) &&
		state.Charging.Value == 2 {
		return fail(CodeStateConflict)
	}
	switch state.Drainage.Quality {
	case Unsupported:
		// Explicit model/reader non-support is different from a missing read.
		if state.Drainage.Value != 0 {
			return fail(CodeObservationInvalid)
		}
	case Observed:
		if state.Drainage.Value != 0 {
			return fail(CodeStateNotResting)
		}
	default:
		return observed(state.Drainage.Quality)
	}
	return nil
}

func checkSettings(settings SettingsEvidence, perRoom *PerRoomSettings, room RoomEvidence,
	base Snapshot, now int64) (int, int, settingsSource, error) {
	for _, value := range [...]IntObservation{settings.Mode, settings.Tank, settings.Fan, settings.Water} {
		if err := observed(value.Quality); err != nil {
			return 0, 0, 0, err
		}
	}
	if settings.Encoding != PlainWaterVolume || settings.Mode.Value < 0 || settings.Mode.Value > 2 ||
		settings.Fan.Value < 0 || settings.Fan.Value > 3 || settings.Water.Value < 1 || settings.Water.Value > 3 {
		return 0, 0, 0, fail(CodeSettingsUnsupported)
	}
	if settings.Mode.Value == 0 && settings.Tank.Value != 0 ||
		settings.Mode.Value != 0 && settings.Tank.Value != 1 && settings.Tank.Value != 10 {
		return 0, 0, 0, fail(CodeSettingsConflict)
	}
	customized := false
	switch settings.Customized.Quality {
	case Unsupported:
		if settings.Customized.Value != 0 {
			return 0, 0, 0, fail(CodeObservationInvalid)
		}
	case Observed:
		if settings.Customized.Value != 0 && settings.Customized.Value != 1 {
			return 0, 0, 0, fail(CodeSettingsUnsupported)
		}
		customized = settings.Customized.Value == 1
	default:
		return 0, 0, 0, observed(settings.Customized.Quality)
	}
	if !customized {
		if perRoom != nil {
			return 0, 0, 0, fail(CodeSettingsConflict)
		}
		return settings.Fan.Value, settings.Water.Value, globalSettings, nil
	}
	if perRoom == nil {
		return 0, 0, 0, fail(CodePerRoomRequired)
	}
	if err := checkSnapshot(perRoom.Snapshot, base, now); err != nil {
		return 0, 0, 0, err
	}
	if perRoom.Origin != GenuineObservedEntry {
		return 0, 0, 0, fail(CodePerRoomUnobserved)
	}
	if perRoom.SegmentID != room.SegmentID || !mapID(perRoom.SelectedSavedMapID) ||
		perRoom.SelectedSavedMapID != room.SelectedSavedMapID {
		return 0, 0, 0, fail(CodeRoomInvalid)
	}
	for _, value := range [...]IntObservation{perRoom.Fan, perRoom.EncodedWater, perRoom.Repeat} {
		if err := observed(value.Quality); err != nil {
			return 0, 0, 0, err
		}
	}
	if perRoom.Mode.Quality != Missing && perRoom.Mode.Quality != Unsupported || perRoom.Mode.Value != 0 ||
		perRoom.Fan.Value < 0 || perRoom.Fan.Value > 3 ||
		perRoom.EncodedWater.Value < 2 || perRoom.EncodedWater.Value > 4 {
		return 0, 0, 0, fail(CodeSettingsUnsupported)
	}
	if perRoom.Repeat.Value != 1 {
		return 0, 0, 0, fail(CodeRepeatMismatch)
	}
	return perRoom.Fan.Value, perRoom.EncodedWater.Value - 1, perRoomSettings, nil
}

func (d *Draft) valid() bool {
	return d != nil && d.version == 1 && identifier(d.deviceID) &&
		d.segmentID >= 1 && d.segmentID <= 63 && d.mode >= 0 && d.mode <= 2 &&
		d.fan >= 0 && d.fan <= 3 && d.water >= 1 && d.water <= 3 &&
		d.battery >= 0 && d.battery <= 100 && d.oldestRead > 0 &&
		d.builtAt >= d.oldestRead && d.builtAt-d.oldestRead <= MaxAgeMillis &&
		(d.source == globalSettings || d.source == perRoomSettings)
}

// Preview exposes bounded settings only. It neither expires an owner review nor
// permits dispatch. A later dispatch must rebuild and compare a fresh plan.
func (d *Draft) Preview() (Preview, error) {
	if !d.valid() {
		return Preview{}, fail(CodeDraftInvalid)
	}
	source := "observed_global"
	if d.source == perRoomSettings {
		source = "genuine_observed_room"
	}
	return Preview{
		Kind: "DRAFT", Operation: "single_room", Model: Model,
		Mode: d.mode, Fan: d.fan, Water: d.water, Repeat: 1, Order: 1,
		Battery: d.battery, SettingsSource: source, FirmwareAcceptance: "unverified",
	}, nil
}
