package cloud

import (
	"encoding/json"
	"os"
	"testing"

	"alisa-dreame-readonly/internal/bridge"
)

// This test only reads an AST-oracle-generated synthetic specification. It
// deliberately has no client, token, URL, signature, send or network operation.
func TestFutureRoomContractIsSyntheticAndCurrentReaderDeniesWrites(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/room-cleaning-contract.synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Synthetic     bool `json:"synthetic"`
		Transmittable bool `json:"transmittable"`
		Requires      bool `json:"requires_real_device_map_verification"`
		Binding       struct {
			Device    string `json:"device_id"`
			MapID     int    `json:"saved_map_id"`
			SegmentID int    `json:"segment_id"`
		} `json:"binding"`
		RPC struct {
			DID  string `json:"did"`
			ID   int    `json:"id"`
			Data struct {
				DID    string `json:"did"`
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					DID  string `json:"did"`
					SIID int    `json:"siid"`
					AIID int    `json:"aiid"`
					In   []struct {
						PIID  int             `json:"piid"`
						Value json.RawMessage `json:"value"`
					} `json:"in"`
				} `json:"params"`
			} `json:"data"`
		} `json:"unsigned_rpc"`
	}
	if json.Unmarshal(raw, &fixture) != nil || !fixture.Synthetic || fixture.Transmittable || !fixture.Requires {
		t.Fatal("not a future synthetic-only fixture")
	}
	if fixture.Binding.MapID != 42 || fixture.Binding.SegmentID != 23 || fixture.RPC.DID != fixture.Binding.Device || fixture.RPC.Data.DID != fixture.Binding.Device || fixture.RPC.Data.Params.DID != fixture.Binding.Device || fixture.RPC.ID != fixture.RPC.Data.ID {
		t.Fatal("fixture binding mismatch")
	}
	p := fixture.RPC.Data.Params
	if fixture.RPC.Data.Method != "action" || p.SIID != 4 || p.AIID != 1 || len(p.In) != 2 || p.In[0].PIID != 1 || string(p.In[0].Value) != "18" || p.In[1].PIID != 10 {
		t.Fatal("source baseline action contract changed")
	}
	var encoded string
	if json.Unmarshal(p.In[1].Value, &encoded) != nil {
		t.Fatal("cleaning properties are not encoded JSON text")
	}
	var selection struct {
		Selects [][]int `json:"selects"`
	}
	if json.Unmarshal([]byte(encoded), &selection) != nil || len(selection.Selects) != 1 || len(selection.Selects[0]) != 5 {
		t.Fatal("source select row layout changed")
	}
	for i, want := range []int{23, 1, 2, 1, 1} {
		if selection.Selects[0][i] != want {
			t.Fatal("synthetic source selection mismatch")
		}
	}
	for _, op := range []string{"action", "clean_segment", "clean_room", "set_properties"} {
		if validateRequest(bridge.Request{Schema: 1, Op: op, Device: fixture.Binding.Device}) == nil {
			t.Fatal("read-only runtime accepts a write operation")
		}
	}
}
