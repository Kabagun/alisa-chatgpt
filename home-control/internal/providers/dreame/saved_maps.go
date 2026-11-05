package dreame

import "strconv"

type savedMapReadDiagnostic struct {
	Outcome           string `json:"outcome"`
	VersionHint       int    `json:"version_hint"`
	VersionSource     string `json:"version_source"`
	MapSavingProbe    string `json:"map_saving_probe"`
	ListEntries       int    `json:"list_entries"`
	FramesExamined    int    `json:"frames_examined"`
	FramesDecoded     int    `json:"frames_decoded"`
	FrameFailures     int    `json:"frame_failures"`
	BlobGetAttempts   int    `json:"blob_get_attempts"`
	BlobDownloads     int    `json:"blob_downloads"`
	AcceptedBlobBytes int    `json:"accepted_blob_bytes"`
	SelectedMatched   bool   `json:"selected_matched"`
	LastError         string `json:"last_error"`
}

func savedMapReadDiagnosticValid(raw []byte, s savedMapReadDiagnostic, locator savedObjectDiagnostic, mapCount int, selected, reason string) bool {
	if !exactKeys(raw, "outcome", "version_hint", "version_source", "map_saving_probe", "list_entries", "frames_examined", "frames_decoded", "frame_failures", "blob_get_attempts", "blob_downloads", "accepted_blob_bytes", "selected_matched", "last_error") {
		return false
	}
	if !enum(s.Outcome, "not_referenced", "reference_invalid", "model_version_unavailable", "model_iv_unavailable", "locator_unavailable", "list_unavailable", "list_invalid", "empty_list", "selected_id_missing", "selected_map_unavailable", "selected_map_available", "budget_exhausted", "read_deadline") || !enum(s.VersionSource, "unavailable", "model_default", "map_saving_nonnull") || !enum(s.MapSavingProbe, "value_present", "null_value", "property_unavailable", "missing", "request_unavailable", "response_invalid") {
		return false
	}
	if s.VersionHint < -1 || s.VersionHint > 3 || (s.VersionHint == -1) != (s.VersionSource == "unavailable") || s.VersionSource == "map_saving_nonnull" && (s.VersionHint != 0 || s.MapSavingProbe != "value_present") {
		return false
	}
	for _, n := range []int{s.ListEntries, s.FramesExamined, s.FramesDecoded, s.FrameFailures} {
		if n < 0 || n > 32 {
			return false
		}
	}
	if s.FramesExamined > s.ListEntries || s.FramesDecoded > s.FramesExamined || s.FrameFailures > s.FramesExamined || s.BlobGetAttempts < 0 || s.BlobGetAttempts > 4 || s.BlobDownloads < 0 || s.BlobDownloads > s.BlobGetAttempts || s.AcceptedBlobBytes < 0 || s.AcceptedBlobBytes > 7<<20 || s.BlobDownloads == 0 && s.AcceptedBlobBytes != 0 || locator.FetchPerformed != (s.BlobGetAttempts > 0) {
		return false
	}
	if s.BlobGetAttempts > 0 && locator.Outcome != "metadata_available" {
		return false
	}
	if !enum(s.LastError, "", "saved_map_read_failed", "map_blob_host_denied", "map_blob_dns_unavailable", "map_blob_address_denied", "map_blob_timeout", "map_blob_transport_failed", "map_blob_redirect_denied", "map_blob_http_failed", "map_blob_encoding_unsupported", "map_blob_response_limit", "map_blob_url_invalid", "saved_map_limit", "saved_map_invalid_json", "saved_map_duplicate_field", "saved_map_invalid_list", "saved_map_invalid_frame", "saved_map_invalid_base64", "saved_map_invalid_crypto", "saved_map_invalid_compression", "saved_map_unsupported_version", "saved_map_unsupported_frame", "saved_map_invalid_metadata", "saved_map_reference_unavailable", "map_locator_unavailable", "saved_frame_unverified", "saved_room_metadata_invalid") {
		return false
	}
	if mapCount == 1 {
		id, err := strconv.Atoi(selected)
		return err == nil && id >= -32768 && id <= 32767 && strconv.Itoa(id) == selected && s.SelectedMatched && s.Outcome == "selected_map_available" && reason == "saved_metadata_available" && s.FramesDecoded > 0
	}
	return mapCount == 0 && selected == "" && !s.SelectedMatched && s.Outcome != "selected_map_available" && reason == "saved_metadata_unavailable"
}
