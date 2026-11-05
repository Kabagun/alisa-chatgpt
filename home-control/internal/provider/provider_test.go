package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCredentialsRedacted(t *testing.T) {
	c := Credentials{Username: "synthetic-user", Password: "synthetic-password", Region: "eu"}
	b, _ := json.Marshal(c)
	for _, s := range []string{string(b), fmt.Sprint(c), fmt.Sprintf("%+v %#v %s %q", c, c, c, c), fmt.Sprintf("%+v", &c)} {
		if strings.Contains(s, c.Username) || strings.Contains(s, c.Password) {
			t.Fatal("credential format exposed private fields")
		}
	}
}
func TestParamsFixedOperations(t *testing.T) {
	for _, tc := range []struct {
		op    Operation
		p     Params
		valid bool
	}{
		{Devices, Params{}, true}, {Devices, Params{DeviceID: "foreign"}, false},
		{Status, Params{DeviceID: "device"}, true}, {Status, Params{DeviceID: "device", MapID: "map"}, false},
		{Rooms, Params{}, false}, {CleanRooms, Params{DeviceID: "device", MapID: "map", RoomIDs: []string{"1", "2"}}, true},
		{CleanRooms, Params{DeviceID: "device", MapID: "map", RoomIDs: []string{"1", "1"}}, false},
		{Operation("arbitrary_rpc"), Params{}, false},
	} {
		if (tc.p.Validate(tc.op) == nil) != tc.valid {
			t.Fatal("operation shape validation changed")
		}
	}
}
