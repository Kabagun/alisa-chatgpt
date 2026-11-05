package cloud

import (
	"context"
	"crypto/sha256"
	"errors"
	"strconv"

	"alisa-dreame-readonly/internal/bridge"
	"alisa-dreame-readonly/internal/savedmap"
)

const maxMapBlobGets = 4
const maxAcceptedMapBytes = 7 << 20

type SavedMapReadDiagnostics struct {
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

func safeMapReadError(err error) string {
	var frameError *savedmap.Error
	if errors.As(err, &frameError) {
		return string(frameError.Code)
	}
	var fault *bridge.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "map_blob_host_denied", "map_blob_dns_unavailable", "map_blob_address_denied", "map_blob_timeout", "map_blob_transport_failed", "map_blob_redirect_denied", "map_blob_http_failed", "map_blob_encoding_unsupported", "map_blob_response_limit", "map_blob_url_invalid":
			return fault.Code
		}
	}
	return "saved_map_read_failed"
}
func (c *Client) fetchSavedBlob(ctx context.Context, locator privateMapURL, limit int, d *MapDiagnostics) ([]byte, bool) {
	s := d.SavedMaps
	if ctx.Err() != nil {
		s.Outcome = "read_deadline"
		return nil, false
	}
	if s.BlobGetAttempts >= maxMapBlobGets || s.AcceptedBlobBytes+limit > maxAcceptedMapBytes {
		s.Outcome = "budget_exhausted"
		return nil, false
	}
	if c.mapBlobs == nil || !c.mapBlobs.approved(locator.value) {
		s.LastError = "map_blob_host_denied"
		return nil, false
	}
	s.BlobGetAttempts++
	d.SavedObject.FetchPerformed = true
	body, err := c.mapBlobs.fetch(ctx, locator.value, limit)
	if err != nil {
		s.LastError = safeMapReadError(err)
		return nil, false
	}
	s.BlobDownloads++
	s.AcceptedBlobBytes += len(body)
	return body, true
}
func (c *Client) readSavedMaps(ctx context.Context, device, model string, locator privateMapURL, result *MapMetadata) error {
	d := result.Diagnostics
	probe := d.mapSavingProbe
	if probe == "" {
		probe = "request_unavailable"
	}
	s := &SavedMapReadDiagnostics{Outcome: "not_referenced", VersionHint: -1, VersionSource: "unavailable", MapSavingProbe: probe}
	d.SavedMaps = s
	version, ok := c.mapProfile.ModelVersions[model]
	if !ok {
		s.Outcome = "model_version_unavailable"
		return nil
	}
	s.VersionHint = version
	s.VersionSource = "model_default"
	if d.mapSavingValue {
		s.VersionHint = 0
		s.VersionSource = "map_saving_nonnull"
	}
	iv := []byte(c.mapProfile.ModelIVs[model])
	defer clear(iv)
	if len(iv) != 16 {
		s.Outcome = "model_iv_unavailable"
		return nil
	}
	if locator.value == "" {
		if d.SavedObject.Outcome == "reference_invalid" {
			s.Outcome = "reference_invalid"
		} else if d.SavedObject.Outcome != "not_referenced" {
			s.Outcome = "locator_unavailable"
		}
		return nil
	}
	listBytes, ok := c.fetchSavedBlob(ctx, locator, 1<<20, d)
	if !ok {
		if s.Outcome != "budget_exhausted" && s.Outcome != "read_deadline" {
			s.Outcome = "list_unavailable"
		}
		return nil
	}
	list, err := savedmap.ParseList(listBytes)
	clear(listBytes)
	if err != nil {
		s.Outcome = "list_invalid"
		s.LastError = safeMapReadError(err)
		return nil
	}
	s.ListEntries = len(list.Entries)
	if s.ListEntries == 0 {
		s.Outcome = "empty_list"
		return nil
	}
	if list.CurrentID == "" {
		s.Outcome = "selected_id_missing"
		return nil
	}
	s.Outcome = "selected_map_unavailable"
	for _, entry := range list.Entries {
		if ctx.Err() != nil {
			s.Outcome = "read_deadline"
			break
		}
		encoded := entry.EncodedFrame
		if encoded == "" {
			if list.Server != 1 || entry.ObjectReference == "" {
				s.FramesExamined++
				s.FrameFailures++
				s.LastError = "saved_map_reference_unavailable"
				continue
			}
			if s.BlobGetAttempts >= maxMapBlobGets {
				s.Outcome = "budget_exhausted"
				break
			}
			frameLocator, metadata, err := c.locateMapObject(ctx, device, model, privateMapReference{name: entry.ObjectReference})
			if err != nil {
				return err
			}
			if metadata.Outcome != "metadata_available" {
				s.FramesExamined++
				s.FrameFailures++
				s.LastError = "map_locator_unavailable"
				continue
			}
			body, ok := c.fetchSavedBlob(ctx, frameLocator, 2<<20, d)
			if !ok {
				if s.Outcome == "budget_exhausted" || s.Outcome == "read_deadline" {
					break
				}
				s.FramesExamined++
				s.FrameFailures++
				continue
			}
			encoded = string(body)
			clear(body)
		}
		s.FramesExamined++
		if len(encoded) > 2<<20 {
			s.FrameFailures++
			s.LastError = "saved_map_limit"
			continue
		}
		frameHash := sha256.Sum256([]byte(encoded))
		frame, err := savedmap.DecodeFrame(encoded, "", iv, s.VersionHint)
		encoded = ""
		if err != nil {
			s.FrameFailures++
			s.LastError = safeMapReadError(err)
			continue
		}
		s.FramesDecoded++
		if !frame.Saved {
			s.FrameFailures++
			s.LastError = "saved_frame_unverified"
			continue
		}
		if frame.MapID != list.CurrentID {
			continue
		}
		id, err := strconv.Atoi(frame.MapID)
		if err != nil || id < -32768 || id > 32767 || strconv.Itoa(id) != frame.MapID || frame.Version < 0 || frame.Version > 3 || len(frame.Rooms) > 63 {
			s.FrameFailures++
			s.LastError = "saved_room_metadata_invalid"
			continue
		}
		rooms := bridge.Rooms{DeviceID: device, MapID: id, MapVersion: frame.Version, MetadataOnly: true, Items: []bridge.Room{}}
		seen := map[int]bool{}
		valid := true
		for _, room := range frame.Rooms {
			if room.ID < 1 || room.ID > 63 || seen[room.ID] || !privateMapText(room.Name, 256) || (room.UniqueID != "" && !privateMapText(room.UniqueID, 128)) || room.Type < 0 || room.Type > 65535 {
				valid = false
				break
			}
			seen[room.ID] = true
			rooms.Items = append(rooms.Items, bridge.Room{SegmentID: room.ID, UniqueID: room.UniqueID, Name: room.Name, Type: room.Type})
		}
		if !valid {
			s.FrameFailures++
			s.LastError = "saved_room_metadata_invalid"
			continue
		}
		result.Maps = []bridge.Rooms{rooms}
		result.SelectedSavedMapID = frame.MapID
		result.savedFrameHash = frameHash
		s.SelectedMatched = true
		s.Outcome = "selected_map_available"
		break
	}
	return nil
}
