package voice

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

const syntheticProjectID = "g-p-00000000000000000000000000000000"

func TestProjectRunOption(t *testing.T) {
	for _, args := range [][]string{nil, {"--project-id="}} {
		settings, err := parseRunSettings(args)
		if err != nil || settings.ProjectID != "" {
			t.Fatal("empty project option changed the default", err)
		}
	}
	for _, id := range []string{syntheticProjectID, "g-p-0123456789abcdef0123456789abcdef"} {
		settings, err := parseRunSettings([]string{"--project-id=" + id, "--voice=orbit"})
		if err != nil || settings.ProjectID != id || settings.Voice != "orbit" {
			t.Fatal("valid project option was not preserved", err)
		}
	}
	var out, diagnostics bytes.Buffer
	if err := Run(t.Context(), []string{"run", "--help"}, nil, &out, &diagnostics); err != nil || !strings.Contains(out.String(), "--project-id") {
		t.Fatal("project option missing from help", err)
	}
}

func TestInvalidProjectRunOptionDoesNotDiscloseValue(t *testing.T) {
	for _, id := range []string{
		"g-p-" + strings.Repeat("0", 31),
		"g-p-" + strings.Repeat("0", 33),
		"G-P-" + strings.Repeat("0", 32),
		"g-p-" + strings.Repeat("A", 32),
		"g-p-" + strings.Repeat("g", 32),
		"g-" + strings.Repeat("0", 32),
		" " + syntheticProjectID,
		syntheticProjectID + "\n",
		"g-p-0000000000000000000000000000000é",
		"private-project-marker;$(echo unexpected)",
	} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), []string{"run", "--project-id=" + id}, nil, &out, &diagnostics)
		if err == nil || strings.Contains(err.Error()+out.String()+diagnostics.String(), id) {
			t.Fatal("invalid project option accepted or disclosed")
		}
	}
}

func TestLedFeedbackRunOptionIsLocalOnly(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--led-feedback"}, true},
		{[]string{"--led-feedback=true"}, true},
		{[]string{"--led-feedback=false"}, false},
	} {
		settings, err := parseRunSettings(tc.args)
		if err != nil || settings.LedFeedback != tc.want {
			t.Fatal("LED feedback option changed", err)
		}
	}
	var out, diagnostics bytes.Buffer
	err := Run(t.Context(), []string{"run", "--led-feedback=private-marker"}, nil, &out, &diagnostics)
	if err == nil || strings.Contains(err.Error()+out.String()+diagnostics.String(), "private-marker") {
		t.Fatal("invalid LED feedback option accepted or disclosed")
	}
	var payloads []map[string]any
	for _, settings := range []sessionSettings{{}, {LedFeedback: true}} {
		data, err := encodeSession(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC), settings)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		delete(payload, "voice_session_id")
		delete(payload, "voice_status_request_id")
		payloads = append(payloads, payload)
	}
	if !reflect.DeepEqual(payloads[0], payloads[1]) {
		t.Fatal("local LED feedback option entered the Voice payload")
	}
}

func TestProjectPayloadIsOptInAndStartsFreshSession(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	encode := func(settings sessionSettings) map[string]any {
		t.Helper()
		before := settings
		data, err := encodeSession(now, settings)
		if err != nil {
			t.Fatal(err)
		}
		if settings != before {
			t.Fatal("encoding changed the settings snapshot")
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	ordinary := encode(sessionSettings{})
	project := encode(sessionSettings{ProjectID: syntheticProjectID})
	second := encode(sessionSettings{ProjectID: syntheticProjectID})
	if !reflect.DeepEqual(ordinary["conversation_mode"], map[string]any{"kind": "primary_assistant"}) {
		t.Fatal("default conversation mode changed")
	}
	if !reflect.DeepEqual(project["conversation_mode"], map[string]any{"kind": "gizmo_interaction", "gizmo_id": syntheticProjectID}) {
		t.Fatal("project conversation mode differs from observed wire shape")
	}
	if project["voice_session_id"] == second["voice_session_id"] || project["voice_session_id"] != project["voice_status_request_id"] {
		t.Fatal("project calls reused a voice session identifier")
	}
	for _, key := range []string{"conversation_id", "parent_message_id", "project_id", "gizmo_id"} {
		if _, exists := project[key]; exists {
			t.Fatal("project routing added an unobserved top-level field")
		}
	}
	for _, payload := range []map[string]any{ordinary, project} {
		delete(payload, "conversation_mode")
		delete(payload, "voice_session_id")
		delete(payload, "voice_status_request_id")
	}
	if !reflect.DeepEqual(ordinary, project) {
		t.Fatal("project routing changed unrelated Voice payload fields")
	}
}
