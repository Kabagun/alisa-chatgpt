package dreame

type currentMapDiagnostic struct {
	Outcome              string                      `json:"outcome"`
	TargetProfileMatched bool                        `json:"target_profile_matched"`
	ActionRequested      bool                        `json:"action_requested"`
	ActionAcknowledged   bool                        `json:"action_acknowledged"`
	ActionRequests       int                         `json:"action_requests"`
	InlineOutputs        int                         `json:"inline_outputs"`
	ReferenceOutputs     int                         `json:"reference_outputs"`
	OtherOutputs         int                         `json:"other_outputs"`
	PassivePolls         int                         `json:"passive_polls"`
	Freshness            string                      `json:"freshness"`
	FullFrame            bool                        `json:"full_frame"`
	SavedStatusPresent   bool                        `json:"saved_status_present"`
	SavedStatus          int                         `json:"saved_status"`
	MarkersValid         bool                        `json:"markers_valid"`
	Temporary            bool                        `json:"temporary"`
	NestedTemporary      bool                        `json:"nested_temporary"`
	New                  bool                        `json:"new"`
	Restored             bool                        `json:"restored"`
	Empty                bool                        `json:"empty"`
	LinkagePresent       bool                        `json:"linkage_present"`
	LinkageMatches       bool                        `json:"linkage_matches"`
	DescriptorUnchanged  bool                        `json:"descriptor_unchanged"`
	ActiveVerified       bool                        `json:"active_verified"`
	LastError            string                      `json:"last_error"`
	DuplicateCategory    string                      `json:"duplicate_category,omitempty"`
	Selection            *currentSelectionDiagnostic `json:"selection,omitempty"`
}

type currentSelectionDiagnostic struct {
	Present bool `json:"present"`
	Valid   bool `json:"valid"`
	Count   int  `json:"count"`
}

func currentMapDiagnosticValid(raw []byte, c currentMapDiagnostic, mapCount int, active bool) bool {
	keys := []string{"outcome", "target_profile_matched", "action_requested", "action_acknowledged", "action_requests", "inline_outputs", "reference_outputs", "other_outputs", "passive_polls", "freshness", "full_frame", "saved_status_present", "saved_status", "markers_valid", "temporary", "nested_temporary", "new", "restored", "empty", "linkage_present", "linkage_matches", "descriptor_unchanged", "active_verified", "last_error"}
	if field(raw, "duplicate_category") != nil {
		keys = append(keys, "duplicate_category")
	}
	if selectionRaw := field(raw, "selection"); selectionRaw != nil {
		keys = append(keys, "selection")
		s := c.Selection
		var typed struct {
			Present *bool `json:"present"`
			Valid   *bool `json:"valid"`
			Count   *int  `json:"count"`
		}
		if decode(selectionRaw, &typed, true) != nil || typed.Present == nil || typed.Valid == nil || typed.Count == nil {
			return false
		}
		if s == nil || !exactKeys(selectionRaw, "present", "valid", "count") || !c.FullFrame || s.Count < 0 || s.Count > 63 || s.Valid && !s.Present || !s.Valid && s.Count != 0 {
			return false
		}
	}
	if !exactKeys(raw, keys...) {
		return false
	}
	if !enum(c.DuplicateCategory, "", "list", "frame_metadata", "frame_rooms", "frame_version", "frame_binding", "segment_alias") || c.DuplicateCategory != "" && (c.LastError != "saved_map_duplicate_field" || c.ActiveVerified) {
		return false
	}
	if !enum(c.Outcome, "not_attempted", "model_unsupported", "saved_map_unavailable", "profile_unavailable", "read_deadline", "action_unavailable", "action_response_invalid", "action_rejected", "action_output_failed", "legacy_unsupported", "output_unsupported", "poll_unavailable", "poll_response_invalid", "freshness_unverified", "map_unavailable", "stale_reference", "reference_invalid", "locator_unavailable", "fetch_failed", "budget_exhausted", "decode_failed", "candidate_conflict", "markers_invalid", "format_unverified", "linkage_missing", "linkage_mismatch", "descriptor_unavailable", "descriptor_changed", "active_map_verified") || !enum(c.Freshness, "none", "action_response", "cached_unverified", "cached_unchanged", "cached_reference_changed") {
		return false
	}
	if c.ActionRequests < 0 || c.ActionRequests > 1 || c.ActionRequested != (c.ActionRequests == 1) || c.ActionAcknowledged && !c.ActionRequested || c.InlineOutputs < 0 || c.InlineOutputs > 1 || c.ReferenceOutputs < 0 || c.ReferenceOutputs > 1 || c.OtherOutputs < 0 || c.OtherOutputs > 8 || c.PassivePolls < 0 || c.PassivePolls > 3 || c.SavedStatus < -1 || c.SavedStatus > 2 || c.ActiveVerified != active {
		return false
	}
	if c.LinkageMatches && !c.LinkagePresent || c.ActiveVerified != (c.Outcome == "active_map_verified") {
		return false
	}
	if c.ActionRequested && !c.TargetProfileMatched || (c.InlineOutputs+c.ReferenceOutputs+c.OtherOutputs > 0 || c.PassivePolls > 0) && !c.ActionAcknowledged || c.FullFrame && (!c.ActionAcknowledged || !enum(c.Freshness, "action_response", "cached_reference_changed")) || c.LinkagePresent && !c.FullFrame || c.DescriptorUnchanged && !c.LinkageMatches {
		return false
	}
	if !enum(c.LastError, "", "saved_map_read_failed", "map_blob_host_denied", "map_blob_dns_unavailable", "map_blob_address_denied", "map_blob_timeout", "map_blob_transport_failed", "map_blob_redirect_denied", "map_blob_http_failed", "map_blob_encoding_unsupported", "map_blob_response_limit", "map_blob_url_invalid", "saved_map_limit", "saved_map_invalid_json", "saved_map_duplicate_field", "saved_map_invalid_list", "saved_map_invalid_frame", "saved_map_invalid_base64", "saved_map_invalid_crypto", "saved_map_invalid_compression", "saved_map_unsupported_version", "saved_map_unsupported_frame", "saved_map_invalid_metadata") {
		return false
	}
	if !active {
		return true
	}
	return mapCount == 1 && c.TargetProfileMatched && c.ActionRequested && c.ActionAcknowledged && c.FullFrame && c.SavedStatusPresent && c.SavedStatus == 2 && c.MarkersValid && !c.Temporary && !c.NestedTemporary && !c.New && !c.Restored && !c.Empty && c.LinkagePresent && c.LinkageMatches && c.DescriptorUnchanged && enum(c.Freshness, "action_response", "cached_reference_changed") && (c.Freshness != "cached_reference_changed" || c.PassivePolls > 0)
}
