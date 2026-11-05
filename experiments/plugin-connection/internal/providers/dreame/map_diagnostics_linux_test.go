//go:build linux

package dreame

import (
	"context"
	"strings"
	"testing"

	"smart-home-plugin/internal/provider"
)

func TestDiagnosticRoomsStillUseFreshOwnedCatalogAndDenyControl(t *testing.T) {
	a, dir := setup(t)
	mode(t, dir, "map_diagnostics")
	raw, err := a.Execute(context.Background(), dir, provider.Rooms, provider.Params{DeviceID: "fixture-1"})
	if err != nil || !strings.Contains(string(raw), `"encrypted_inline_unsupported"`) {
		t.Fatal("owned diagnostic result unavailable")
	}
	_, err = a.Execute(context.Background(), dir, provider.Rooms, provider.Params{DeviceID: "foreign-device"})
	code(t, err, "device_not_found")
	_, err = a.Execute(context.Background(), dir, provider.CleanRooms, provider.Params{DeviceID: "fixture-1", MapID: "synthetic-map", RoomIDs: []string{"synthetic-room"}})
	code(t, err, "invalid_params")
	if events(t, dir) != "devices\nrooms\ndevices\n" {
		t.Fatal("diagnostics bypassed membership or dispatched control")
	}
}
