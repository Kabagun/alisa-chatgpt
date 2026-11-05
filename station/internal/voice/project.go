package voice

import "strings"

// validProjectID accepts only the ID shape observed in an owner-provided
// website Voice request. Keep identifiers out of validation diagnostics.
func validProjectID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "g-p-") {
		return false
	}
	for _, c := range id[4:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// projectConversationMode uses the observed website wire shape. Empty keeps
// the ordinary assistant payload; it never selects an existing conversation.
func projectConversationMode(id string) map[string]string {
	if id == "" {
		return map[string]string{"kind": "primary_assistant"}
	}
	return map[string]string{"kind": "gizmo_interaction", "gizmo_id": id}
}
