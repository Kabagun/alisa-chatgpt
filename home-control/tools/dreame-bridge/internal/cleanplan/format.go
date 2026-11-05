package cleanplan

import (
	"encoding/json"
	"fmt"
	"io"
)

func redacted(state fmt.State, name string) {
	_, _ = io.WriteString(state, "cleanplan."+name+"{redacted}")
}
func redactedJSON() ([]byte, error) { return []byte(`{"redacted":true}`), nil }

func (Binding) Format(s fmt.State, _ rune)          { redacted(s, "Binding") }
func (Snapshot) Format(s fmt.State, _ rune)         { redacted(s, "Snapshot") }
func (CatalogEvidence) Format(s fmt.State, _ rune)  { redacted(s, "CatalogEvidence") }
func (MapEvidence) Format(s fmt.State, _ rune)      { redacted(s, "MapEvidence") }
func (RoomEvidence) Format(s fmt.State, _ rune)     { redacted(s, "RoomEvidence") }
func (StateEvidence) Format(s fmt.State, _ rune)    { redacted(s, "StateEvidence") }
func (SettingsEvidence) Format(s fmt.State, _ rune) { redacted(s, "SettingsEvidence") }
func (PerRoomSettings) Format(s fmt.State, _ rune)  { redacted(s, "PerRoomSettings") }
func (Observations) Format(s fmt.State, _ rune)     { redacted(s, "Observations") }

func (Binding) MarshalJSON() ([]byte, error)          { return redactedJSON() }
func (Snapshot) MarshalJSON() ([]byte, error)         { return redactedJSON() }
func (CatalogEvidence) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (MapEvidence) MarshalJSON() ([]byte, error)      { return redactedJSON() }
func (RoomEvidence) MarshalJSON() ([]byte, error)     { return redactedJSON() }
func (StateEvidence) MarshalJSON() ([]byte, error)    { return redactedJSON() }
func (SettingsEvidence) MarshalJSON() ([]byte, error) { return redactedJSON() }
func (PerRoomSettings) MarshalJSON() ([]byte, error)  { return redactedJSON() }
func (Observations) MarshalJSON() ([]byte, error)     { return redactedJSON() }

func (o *Observations) UnmarshalJSON([]byte) error {
	if o != nil {
		*o = Observations{}
	}
	return fail(CodeUntrustedInput)
}

func (d *Draft) UnmarshalJSON([]byte) error {
	if d != nil {
		*d = Draft{}
	}
	return fail(CodeUntrustedInput)
}

// Even explicit JSON formatting of a Draft returns only the safe preview.
func (d Draft) MarshalJSON() ([]byte, error) {
	preview, err := (&d).Preview()
	if err != nil {
		return nil, err
	}
	return json.Marshal(preview)
}

func (Draft) Format(state fmt.State, _ rune) { redacted(state, "Draft") }

func (e *Error) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code Code `json:"code"`
	}{Code: e.Code()})
}
