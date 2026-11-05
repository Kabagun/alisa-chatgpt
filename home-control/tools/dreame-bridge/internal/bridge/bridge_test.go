package bridge

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

const devicesJSON = `{"code":0,"data":{"page":{"records":[{"did":"fixture-vacuum-1","model":"dreame.vacuum.synthetic","customName":"Example robot","mac":"must-not-leak","masterUid":"must-not-leak"}]}}}`
const statusJSON = `{"code":0,"data":{"result":[{"siid":2,"piid":1,"code":0,"value":6},{"siid":2,"piid":2,"code":0,"value":0},{"siid":3,"piid":1,"code":0,"value":82},{"siid":3,"piid":2,"code":0,"value":1},{"siid":4,"piid":1,"code":-1},{"siid":4,"piid":7,"code":0,"value":0}]}}`

func encodedFrame(id, width, height int, frame byte, meta string) string {
	raw := make([]byte, 27+width*height)
	binary.LittleEndian.PutUint16(raw, uint16(id))
	raw[4] = frame
	binary.LittleEndian.PutUint16(raw[19:], uint16(width))
	binary.LittleEndian.PutUint16(raw[21:], uint16(height))
	raw = append(raw, meta...)
	var out bytes.Buffer
	z := zlib.NewWriter(&out)
	z.Write(raw)
	z.Close()
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

func exampleFixture() *Fixture {
	return &Fixture{Schema: 1, Synthetic: true, ObservedAt: "2026-01-01T00:00:00Z", Devices: json.RawMessage(devicesJSON), Status: map[string]json.RawMessage{"fixture-vacuum-1": json.RawMessage(statusJSON)}, Maps: map[string][]SavedMap{"fixture-vacuum-1": {{ID: 42, Version: 1, Active: true, Data: encodedFrame(42, 2, 2, 'I', `{"seg_inf":{"7":{"type":1,"roomID":"living-unique"},"3":{"type":4,"roomID":"kitchen-unique","name":"0JrRg9GF0L3Rjw=="}}}`)}}}}
}

func TestReadOperations(t *testing.T) {
	f := exampleFixture()
	for _, op := range []string{"devices", "status", "rooms"} {
		req := Request{Schema: 1, Op: op}
		if op != "devices" {
			req.Device = "fixture-vacuum-1"
		}
		r := Execute(f, req)
		if !r.OK || !r.ReadOnly || r.Mode != "offline_fixture" {
			t.Fatalf("%s: %+v", op, r)
		}
		encoded, _ := json.Marshal(r)
		if bytes.Contains(encoded, []byte("must-not-leak")) {
			t.Fatal("private upstream fields leaked")
		}
	}
	r := Execute(f, Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"}).Data.(Rooms)
	if len(r.Items) != 2 || r.Items[0].SegmentID != 3 || r.Items[0].Name != "Кухня" || r.Items[0].UniqueID != "kitchen-unique" || r.VerifiedForControl || !r.MetadataOnly {
		t.Fatalf("rooms: %+v", r)
	}
	s := Execute(f, Request{Schema: 1, Op: "status", Device: "fixture-vacuum-1"}).Data.(Status)
	if s.Values["battery_percent"] != 82 || len(s.Missing) != 1 || s.Missing[0] != "status" {
		t.Fatalf("status: %+v", s)
	}
}

func TestMapFrameDiagnosticsAreEnumOnlyAndBounded(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{encodedFrame(42, 2, 2, 'I', `{"seg_inf":{}}`), "full"},
		{encodedFrame(42, 2, 2, 'P', `{}`), "incremental"},
		{encodedFrame(42, 2, 2, 'W', `{}`), "other"},
		{"opaque,synthetic-private-key", "encrypted_suffix"},
		{"invalid-private-frame", "invalid"},
		{strings.Repeat("x", MaxFixture+1), "invalid"},
	} {
		if got := InspectMapFrameKind(tc.input); got != tc.want {
			t.Fatalf("frame kind = %s, want %s", got, tc.want)
		}
	}
	params := MapDiagnosticReadParams()
	if len(params) != 2 || params[0]["siid"] != 6 || params[0]["piid"] != 3 || params[1]["siid"] != 6 || params[1]["piid"] != 8 {
		t.Fatal("map read selectors changed")
	}
	for _, p := range params {
		if len(p) != 3 || p["value"] != nil || p["aiid"] != nil {
			t.Fatal("map diagnostic can write")
		}
	}
}

func TestDenyCommandsAndSelectors(t *testing.T) {
	f := exampleFixture()
	cases := []Request{
		{Schema: 1, Op: "clean", Device: "fixture-vacuum-1", MapID: 42},
		{Schema: 1, Op: "send"}, {Schema: 1, Op: "set_properties"}, {Schema: 1, Op: "shell"},
		{Schema: 1, Op: "devices", Device: "anything"}, {Schema: 1, Op: "status", Device: "fixture-vacuum-1", MapID: 42},
		{Schema: 1, Op: "rooms", Device: "https://example.test"}, {Schema: 1, Op: "rooms", Device: "missing"},
		{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1", MapID: -1}, {Schema: 2, Op: "devices"},
	}
	for _, c := range cases {
		if r := Execute(f, c); r.OK {
			t.Fatalf("accepted %+v", c)
		}
	}
	for _, input := range []string{`{"schema":1,"op":"status","url":"https://example.test"}`, `{"schema":1,"op":"status","token":"secret"}`, `{} {}`, strings.Repeat("x", MaxRequest+1)} {
		var req Request
		if DecodeOne(strings.NewReader(input), MaxRequest, &req, true) == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if Execute(nil, Request{Schema: 1, Op: "devices"}).OK {
		t.Fatal("missing fixture accepted")
	}
	capabilities := Execute(nil, Request{Schema: 1, Op: "capabilities"})
	encoded, _ := json.Marshal(capabilities)
	if !capabilities.OK || !bytes.Contains(encoded, []byte(`"network_enabled":false`)) {
		t.Fatal("wrong capabilities")
	}
}

func TestFixtureGateAndBounds(t *testing.T) {
	f := exampleFixture()
	encoded, _ := json.Marshal(f)
	if _, err := LoadFixture(bytes.NewReader(encoded)); err != nil {
		t.Fatal(err)
	}
	f.Synthetic = false
	encoded, _ = json.Marshal(f)
	if _, err := LoadFixture(bytes.NewReader(encoded)); err == nil {
		t.Fatal("nonfixture accepted")
	}
	if _, err := LoadFixture(strings.NewReader(strings.Repeat(" ", MaxFixture+1))); err == nil {
		t.Fatal("unbounded fixture")
	}
	if _, err := ParseDevices([]byte(`{"code":0}`)); err == nil {
		t.Fatal("incomplete envelope accepted")
	}
}

func TestMapValidation(t *testing.T) {
	valid := exampleFixture().Maps["fixture-vacuum-1"][0]
	cases := []SavedMap{
		{ID: 41, Version: 1, Data: valid.Data},
		{ID: 42, Version: 4, Data: valid.Data},
		{ID: 42, Version: 1, Data: valid.Data + ",private-map-key"},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'P', `{"seg_inf":{}}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', `{"seg_inf":{"0":{"type":4}}}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', `{"seg_inf":{"03":{"type":4}}}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', `{"seg_inf":{"3":{"type":99}}}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', `{"seg_inf":{"3":{"name":"@@@"}}}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', `{"rism":"nested-not-supported"}`)},
		{ID: 42, Version: 1, Data: encodedFrame(42, 2, 2, 'I', strings.Repeat("x", maxMap))},
	}
	for i, c := range cases {
		if _, err := ParseRooms(c); err == nil {
			t.Fatalf("accepted case %d", i)
		}
	}
	f := exampleFixture()
	f.Maps["fixture-vacuum-1"] = append(f.Maps["fixture-vacuum-1"], valid)
	if Execute(f, Request{Schema: 1, Op: "rooms", Device: "fixture-vacuum-1"}).OK {
		t.Fatal("ambiguous active map accepted")
	}
}

func TestStatusValidation(t *testing.T) {
	for _, raw := range []string{
		`{"code":0}`, `{"code":401,"data":{"result":[]}}`,
		`{"code":0,"data":{"result":[{"siid":3,"piid":1,"code":0,"value":101}]}}`,
		`{"code":0,"data":{"result":[{"siid":3,"piid":1,"code":0,"value":null}]}}`,
		`{"code":0,"data":{"result":[{"siid":3,"piid":1,"code":0,"value":1},{"siid":3,"piid":1,"code":0,"value":2}]}}`,
	} {
		if _, err := ParseStatus("fixture-vacuum-1", "2026-01-01T00:00:00Z", []byte(raw)); err == nil {
			t.Fatal("invalid status accepted")
		}
	}
	params := StatusReadParams()
	if len(params) != 6 {
		t.Fatal("unexpected read params")
	}
	for _, p := range params {
		if len(p) != 3 || p["value"] != nil || p["aiid"] != nil {
			t.Fatal("non-read selector")
		}
	}
}

func BenchmarkRooms(b *testing.B) {
	m := exampleFixture().Maps["fixture-vacuum-1"][0]
	for b.Loop() {
		if _, err := ParseRooms(m); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzMapDecoder(f *testing.F) {
	f.Add(exampleFixture().Maps["fixture-vacuum-1"][0].Data)
	f.Add("invalid")
	f.Fuzz(func(t *testing.T, data string) {
		if len(data) > MaxFixture {
			return
		}
		ParseRooms(SavedMap{ID: 42, Version: 1, Data: data})
	})
}
